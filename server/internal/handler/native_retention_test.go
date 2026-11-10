package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/modelcontrol"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeHistoryAndReceiptsFollowScopeDeletion(t *testing.T) {
	for _, kind := range []string{"issue", "chat", "builder"} {
		t.Run(kind, func(t *testing.T) {
			runtimeID := dbfx.Runtime(t, "native retention", testutil.Cols{"provider": "native"})
			cols := testutil.Cols{}
			if kind == "builder" {
				cols["kind"], cols["system_key"] = "system", "agent_builder:"+uuid.NewString()
			}
			agentID := dbfx.Agent(t, "native retention", runtimeID, cols)
			scopeType := "chat"
			scopeID := dbfx.ChatSession(t, agentID)
			path, param, remove := "/api/chat/sessions/"+scopeID, "sessionId", testHandler.DeleteChatSession
			if kind == "issue" {
				scopeType, scopeID = "issue", dbfx.Issue(t, "native retention")
				path, param, remove = "/api/issues/"+scopeID, "id", testHandler.DeleteIssue
			}
			insert := func(agent, scope, id string) string {
				return dbfx.Insert(t, "native_session", testutil.Cols{
					"workspace_id": testWorkspaceID, "agent_id": agent, "scope_type": scope, "scope_id": id,
					"daemon_id": "native-retention", "work_dir": "/fixture",
					"checkpoint":     []byte(`{"messages":[{"role":"user","content":"retained request"}],"tools":[]}`),
					"write_outcomes": []byte(`{"operation":{"state":"finished","digest":"fixture","call_id":"create","status":201,"body":{"id":"created-resource"}}}`),
				})
			}
			deleted := insert(agentID, scopeType, scopeID)
			// A builder owns its hidden agent, including run-scoped history.
			// An ordinary scope delete must leave the agent's other scopes intact.
			other := insert(agentID, "run", uuid.NewString())
			neighbourAgent := dbfx.Agent(t, "native retention neighbour", runtimeID)
			neighbour := insert(neighbourAgent, "run", uuid.NewString())
			r := testutil.WithURLParams(newRequest(http.MethodDelete, path, nil), param, scopeID)
			testutil.Call(t, remove, withChatTestWorkspaceCtx(t, r)).Want(http.StatusNoContent)
			var remaining int
			if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM native_session WHERE id=$1", deleted).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("deleted scope retained model history or write receipts: count=%d error=%v", remaining, err)
			}
			wantOther := 1
			if kind == "builder" {
				wantOther = 0
			}
			if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM native_session WHERE id=$1", other).Scan(&remaining); err != nil || remaining != wantOther {
				t.Fatalf("agent history retention: count=%d want=%d error=%v", remaining, wantOther, err)
			}
			var history, receipts string
			if err := testPool.QueryRow(context.Background(), "SELECT checkpoint->'messages'->0->>'content', write_outcomes->'operation'->'body'->>'id' FROM native_session WHERE id=$1", neighbour).Scan(&history, &receipts); err != nil || history != "retained request" || receipts != "created-resource" {
				t.Fatalf("scope deletion damaged another agent's history: history=%q receipt=%q error=%v", history, receipts, err)
			}
		})
	}
}

func TestNativeAcquireFencesWorkspaceTeardownAndCancellation(t *testing.T) {
	for _, lock := range []string{"workspace", "run"} {
		t.Run(lock, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			workspaceID := dbfx.Workspace(t, "Native acquire fence", "native-acquire-"+uuid.NewString())
			runtimeID := dbfx.Runtime(t, "native acquire fence", testutil.Cols{"workspace_id": workspaceID, "provider": "native"})
			agentID := dbfx.Agent(t, "native acquire fence", runtimeID, testutil.Cols{"workspace_id": workspaceID})
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "running", "dispatched_at": dispatched})
			dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1", workspaceID)
			holder, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			query, id := "SELECT id FROM workspace WHERE id=$1 FOR UPDATE", workspaceID
			if lock == "run" {
				query, id = "SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE", taskID
			}
			if _, err := holder.Exec(ctx, query, id); err != nil {
				t.Fatal(err)
			}
			writer, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(context.Background())
			var pid int32
			if err := writer.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := testHandler.Queries.WithTx(writer).AcquireNativeSession(ctx, db.AcquireNativeSessionParams{
					WorkspaceID: parseUUID(workspaceID), AgentID: parseUUID(agentID), ScopeType: "run", ScopeID: task.ID,
					DaemonID: "native-acquire-fence", WorkDir: "/fixture", ActiveTaskID: task.ID, ActiveDispatch: task.DispatchedAt, LeaseToken: parseUUID(uuid.NewString()),
				})
				done <- err
			}()
			waitForBlockedWriter(t, ctx, pid)
			if _, err := holder.Exec(ctx, "UPDATE agent_task_queue SET status='cancelled' WHERE id=$1", taskID); err != nil {
				t.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != pgx.ErrNoRows {
					t.Fatalf("cancelled run acquired native history after %s fence: %v", lock, err)
				}
			case <-ctx.Done():
				t.Fatal("acquire did not return after cancellation")
			}
		})
	}
}

func TestNativeWorkspaceDeletionRemovesHistoryAndReceipts(t *testing.T) {
	workspaceID := dbfx.Workspace(t, "Native retention", "native-retention-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	runtimeID := dbfx.Runtime(t, "native retention", testutil.Cols{"workspace_id": workspaceID, "provider": "native"})
	agentID := dbfx.Agent(t, "native retention", runtimeID, testutil.Cols{"workspace_id": workspaceID})
	controller := modelcontrol.New(testPool, modelcontrol.DefaultLimits(), nil)
	call := llm.Call{Scope: llm.CallScope{WorkspaceID: workspaceID, TaskID: uuid.NewString(), Purpose: "execution"}, Model: "fixture", ReservedTokens: 100}
	finish, err := controller.Acquire(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{workspaceID, testWorkspaceID} {
		dbfx.Insert(t, "native_session", testutil.Cols{
			"workspace_id": workspace, "agent_id": agentID, "scope_type": "run", "scope_id": uuid.NewString(),
			"daemon_id": "native-retention", "work_dir": "/fixture",
			"write_outcomes": []byte(`{"operation":{"state":"finished","digest":"fixture"}}`),
		})
	}
	r := testutil.WithURLParams(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID, nil), "id", workspaceID)
	testutil.Call(t, testHandler.DeleteWorkspace, r).Want(http.StatusNoContent)
	if err := finish(context.Background(), llm.CallResult{Outcome: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if got := dbfx.Count(t, "SELECT count(*) FROM model_call WHERE workspace_id=$1", workspaceID); got != 0 {
		t.Fatal("workspace retained model audit or late completion recreated it")
	}
	if _, err := controller.Acquire(context.Background(), call); !errors.Is(err, llm.ErrAdmissionUnavailable) {
		t.Fatalf("deleted workspace acquired model budget: %v", err)
	}
	var count int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM native_session WHERE workspace_id=$1", workspaceID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("workspace retained native history: count=%d error=%v", count, err)
	}
	// A corrupt cross-workspace agent reference must not widen the teardown.
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("workspace teardown removed another workspace's native history: count=%d error=%v", count, err)
	}
}
