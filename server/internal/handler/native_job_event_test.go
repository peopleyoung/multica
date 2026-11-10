package handler

import (
	"context"
	"errors"
	"github.com/multica-ai/multica/server/internal/auth"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

func TestNativeJobFeedbackIsAtomicIdempotentAndRechecksAccess(t *testing.T) {
	for _, scenario := range []string{"completed", "failed", "interrupted", "cancelled", "reassigned", "closed", "nonmember", "active", "retry-active", "invocation-revoked", "squad-member", "squad-removed", "runtime-changed", "archived", "foreign-daemon", "foreign-workspace", "queue-failure"} {
		t.Run(scenario, func(t *testing.T) {
			const machine = "job-feedback-fixture"
			runtimeID := dbfx.Runtime(t, "Native feedback", testutil.Cols{"provider": "native", "daemon_id": machine})
			agentCols := testutil.Cols{}
			if scenario == "archived" {
				agentCols["archived_at"] = time.Now()
			}
			if scenario == "invocation-revoked" {
				agentCols["owner_id"] = dbfx.User(t, "Other owner", uuid.NewString()+"@example.test")
				agentCols["permission_mode"] = "private"
			}
			if scenario == "nonmember" {
				agentCols["owner_id"] = dbfx.User(t, "Removed job owner", uuid.NewString()+"@example.test")
			}
			agentID := dbfx.Agent(t, "Native feedback", runtimeID, agentCols)
			issueCols := testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID}
			if scenario == "reassigned" {
				issueCols["assignee_id"] = uuid.NewString()
			}
			if scenario == "closed" {
				issueCols["status"] = "done"
			}
			squadID := ""
			if scenario == "squad-member" || scenario == "squad-removed" {
				leader := dbfx.Agent(t, "Current coordinator", runtimeID)
				squadID = dbfx.Squad(t, "Job squad", leader)
				if scenario == "squad-member" {
					dbfx.SquadMember(t, squadID, "agent", agentID)
				}
				issueCols["assignee_type"] = "squad"
				issueCols["assignee_id"] = squadID
			}
			issueID := dbfx.Issue(t, "Observe existing training", issueCols)
			status := "completed"
			if scenario == "cancelled" {
				status = "cancelled"
			}
			if scenario == "active" {
				status = "running"
			}
			human := testUserID
			if scenario == "nonmember" {
				human = agentCols["owner_id"].(string)
			}
			taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": status, "originator_user_id": human, "accountable_user_id": human})
			if squadID != "" {
				dbfx.Exec(t, "UPDATE agent_task_queue SET squad_id=$1 WHERE id=$2", squadID, taskID)
			}
			if scenario == "runtime-changed" {
				other := dbfx.Runtime(t, "Changed runtime")
				dbfx.Exec(t, "UPDATE agent SET runtime_id=$1 WHERE id=$2", other, agentID)
			}

			dbfx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
			dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1 AND id<>$2", issueID, taskID)
			h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, TxStarter: testPool, Bus: testHandler.Bus}
			invoke := func(want int) map[string]any {
				t.Helper()
				caller := machine
				ws := testWorkspaceID
				if scenario == "foreign-daemon" {
					caller = "other-executor"
				}
				if scenario == "foreign-workspace" {
					ws = uuid.NewString()
				}
				ctx := middleware.WithDaemonContext(context.Background(), ws, caller)
				event := nativeruntime.JobEvent{Key: strings.Repeat("a", 64), JobID: "job-training", Status: "completed"}
				if scenario == "failed" || scenario == "interrupted" {
					event.Status = scenario
				}
				req := testutil.WithURLParams(testutil.JSONRequest("POST", "/native/job-event", event).WithContext(ctx), "taskId", taskID)
				var out map[string]any
				testutil.Call(t, h.NativeJobEvent, req).Want(want).JSON(&out)
				return out
			}
			if scenario == "foreign-daemon" || scenario == "foreign-workspace" {
				want := http.StatusForbidden
				if scenario == "foreign-workspace" {
					want = http.StatusNotFound
				}
				invoke(want)
				return
			}
			if scenario == "active" {
				if out := invoke(200); out["acknowledged"] != false {
					t.Fatal(out)
				}
				if n := dbfx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1", issueID); n != 0 {
					t.Fatal("active source triggered feedback")
				}
				dbfx.Exec(t, "UPDATE agent_task_queue SET status='completed' WHERE id=$1", taskID)
			}
			if scenario == "retry-active" {
				retry := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "originator_user_id": human, "accountable_user_id": human})
				if out := invoke(200); out["acknowledged"] != false {
					t.Fatal("active retry was overlapped")
				}
				dbfx.Exec(t, "UPDATE agent_task_queue SET status='completed' WHERE id=$1", retry)
			}

			if scenario == "queue-failure" {
				h.TxStarter = failNativeJobQueueStarter{inner: testPool}
				invoke(503)
				if n := dbfx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1", issueID); n != 0 {
					t.Fatal("queue failure committed a receipt without follow-up")
				}
				h.TxStarter = testPool
			}
			first := invoke(200)
			second := invoke(200)
			if first["acknowledged"] != true || second["duplicate"] != true || first["comment_id"] != second["comment_id"] {
				t.Fatalf("lost receipt dedup failed: %v %v", first, second)
			}
			want := 1
			if scenario == "cancelled" || scenario == "reassigned" || scenario == "closed" || scenario == "nonmember" || scenario == "invocation-revoked" || scenario == "squad-removed" || scenario == "runtime-changed" || scenario == "archived" {
				want = 0
			}
			if n := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND delegated_from_task_id=$2", issueID, taskID); n != want {
				t.Fatalf("follow-up count %d want %d", n, want)
			}
			if want == 1 {
				if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND delegated_from_task_id=$2 AND originator_user_id=$3 AND accountable_user_id=$3 AND status='queued'`, issueID, taskID, human); n != 1 {
					t.Fatal("follow-up lost human provenance")
				}
			}
			if n := dbfx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1", issueID); n != 1 {
				t.Fatalf("feedback comments %d", n)
			}
			issue, err := h.Queries.GetIssue(context.Background(), parseUUID(issueID))
			if err != nil || issue.Status != issueCols["status"] {
				t.Fatal("process observation changed business acceptance")
			}
		})
	}
}

type failNativeJobQueueStarter struct{ inner txStarter }

func (s failNativeJobQueueStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failNativeJobQueueTx{tx}, nil
}

type failNativeJobQueueTx struct{ pgx.Tx }

func (tx failNativeJobQueueTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "-- name: CreateAgentTask") {
		return failNativeJobQueueRow{}
	}
	return tx.Tx.QueryRow(ctx, query, args...)
}

type failNativeJobQueueRow struct{}

func (failNativeJobQueueRow) Scan(...any) error { return errors.New("injected queue storage failure") }

func TestNativeJobFeedbackUsesRealDaemonControlAuthentication(t *testing.T) {
	const machine = "job-auth-fixture"
	runtimeID := dbfx.Runtime(t, "Native feedback auth", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "Native auth worker", runtimeID)
	issueID := dbfx.Issue(t, "Existing training auth", testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "completed", "originator_user_id": testUserID, "accountable_user_id": testUserID})
	dbfx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)
	dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1 AND id<>$2", issueID, taskID)
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, TxStarter: testPool, Bus: testHandler.Bus}
	endpoint := middleware.DaemonAuth(h.Queries, nil, nil, nil)(http.HandlerFunc(h.NativeJobEvent))
	createPAT := func(userID string) string {
		t.Helper()
		raw, err := auth.GeneratePATToken()
		if err != nil {
			t.Fatal(err)
		}
		dbfx.Insert(t, "personal_access_token", testutil.Cols{"user_id": userID, "name": "observer fixture", "token_hash": auth.HashToken(raw), "token_prefix": raw[:12]})
		return raw
	}
	ownerPAT := createPAT(testUserID)
	otherUser := dbfx.User(t, "Other runtime owner", uuid.NewString()+"@example.test")
	dbfx.Member(t, testWorkspaceID, otherUser, "admin")
	otherPAT := createPAT(otherUser)
	sameMDT := "mdt_" + uuid.NewString()
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(sameMDT), "expires_at": time.Now().Add(time.Hour)})
	otherMDT := "mdt_" + uuid.NewString()
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": "other-daemon", "token_hash": auth.HashToken(otherMDT), "expires_at": time.Now().Add(time.Hour)})
	for _, tc := range []struct {
		token string
		want  int
	}{{"mul_invalid", 401}, {otherPAT, 403}, {otherMDT, 403}, {sameMDT, 200}, {ownerPAT, 200}, {ownerPAT, 200}} {
		req := testutil.WithURLParams(testutil.JSONRequest("POST", "/native/job-event", nativeruntime.JobEvent{Key: strings.Repeat("c", 64), JobID: "job-existing", Status: "completed"}), "taskId", taskID)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-User-ID", testUserID) // Forging the owner must not bypass authentication.
		var out map[string]any
		testutil.Call(t, endpoint.ServeHTTP, req).Want(tc.want).JSON(&out)
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE delegated_from_task_id=$1", taskID); n != 1 {
		t.Fatalf("real authenticated feedback queued %d tasks", n)
	}
}
