package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeSquadTriggersAndEvaluationRecoveryThroughServerRouter(t *testing.T) {
	for _, tc := range []struct {
		name, source, outcome string
		loseResponse          bool
	}{
		{"assignment", "assignment", "no_action", false},
		{"comment", "comment", "no_action", false},
		{"mention", "mention", "no_action", false},
		{"worker_comment", "worker_comment", "no_action", false},
		{"child_done", "child_done", "no_action", false},
		{"action", "assignment", "action", false},
		{"failed", "assignment", "failed", false},
		{"no_action_response_lost", "assignment", "no_action", true},
		{"no_action_response_lost_live", "assignment", "no_action", true},
		{"action_response_lost", "assignment", "action", true},
		{"failed_response_lost", "assignment", "failed", true},
		{"leader_changed_during_model", "assignment", "no_action", false},
		{"cancelled_during_model", "assignment", "no_action", false},
		{"comment_before_evaluation", "assignment", "no_action", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			ctx := context.Background()
			machine, token := "native-squad-"+uuid.NewString(), "mdt_"+uuid.NewString()
			// Each scenario owns its quota window as well as its business data.
			bootstrap := testutil.New(testPool, "", testUserID)
			workspaceID := bootstrap.Workspace(t, "Native squad acceptance", "native-squad-"+uuid.NewString())
			fx := testutil.New(testPool, workspaceID, testUserID)
			fx.Member(t, workspaceID, testUserID, "owner")
			fx.Cleanup(t, "DELETE FROM model_call WHERE workspace_id=$1", workspaceID)
			runtimeID := fx.Runtime(t, "native squad", testutil.Cols{"provider": "native", "daemon_id": machine})
			agentID := fx.Agent(t, "native squad leader", runtimeID)
			squadID := fx.Squad(t, "native squad", agentID)
			fx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "leader"})
			fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": workspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
			fx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", workspaceID, agentID)
			fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
			fx.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", agentID)
			fx.Exec(t, `UPDATE workspace SET issue_counter=GREATEST(issue_counter,
				(SELECT COALESCE(MAX(number),0) FROM issue WHERE workspace_id=$1)) WHERE id=$1`, workspaceID)
			step := 0
			var beforeFirstModelResult func()
			evaluationStep := 1
			if tc.name == "comment_before_evaluation" {
				evaluationStep = 2
			}
			workerTurn, workerStep := false, 0
			workerIssue, workerThread := "", ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Model    string
					Messages []struct{ Role, Content string }
					Tools    []struct{ Function struct{ Name string } }
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input.Model != "fixture-model" {
					t.Error("squad evaluation selected another model")
				}
				if workerTurn {
					workerStep++
					for _, tool := range input.Tools {
						if tool.Function.Name == "multica_record_squad_evaluation" {
							t.Error("worker received the leader evaluation tool")
						}
					}
					name, args := "", map[string]any{"issue_id": workerIssue}
					if source == "worker_comment" && workerStep == 1 {
						name, args["content"], args["parent_id"] = "multica_create_comment", "Native worker result", workerThread
					} else if source == "child_done" && workerStep == 1 {
						name = "multica_get_issue"
					} else if source == "child_done" && workerStep == 2 {
						var child handler.IssueResponse
						if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &child); err != nil {
							t.Error(err)
						}
						name, args["status"], args["expected_revision"] = "multica_update_issue_status", "done", child.Revision
					}
					delta, finish := map[string]any{"content": "Native worker result"}, "stop"
					if name != "" {
						raw, _ := json.Marshal(args)
						delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("worker-%d", workerStep), "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}}}, "tool_calls"
					}
					frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
					return
				}
				step++
				if step == 1 && beforeFirstModelResult != nil {
					beforeFirstModelResult()
				}
				delta, finish := map[string]any{"content": "Evaluation recorded."}, "stop"
				if step == 1 {
					found := false
					for _, tool := range input.Tools {
						found = found || tool.Function.Name == "multica_record_squad_evaluation"
					}
					var prompt strings.Builder
					for _, message := range input.Messages {
						prompt.WriteString(message.Content)
					}
					if !found || !strings.Contains(prompt.String(), "multica_record_squad_evaluation") || !strings.Contains(prompt.String(), "Squad Operating Protocol") {
						t.Error("claim lost squad briefing or native evaluation tool")
					}
				}
				if tc.name == "comment_before_evaluation" && step == 1 {
					var issueID string
					if err := testPool.QueryRow(ctx, "SELECT issue_id::text FROM agent_task_queue WHERE agent_id=$1 AND status='running'", agentID).Scan(&issueID); err != nil {
						t.Error(err)
					}
					args, _ := json.Marshal(map[string]string{"issue_id": issueID, "content": "Native leader business result"})
					delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "leader-comment", "type": "function", "function": map[string]any{"name": "multica_create_comment", "arguments": string(args)}}}}, "tool_calls"
				} else if step == evaluationStep {
					args, _ := json.Marshal(map[string]string{"outcome": tc.outcome, "reason": "Evaluation fixture result"})
					delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "squad-evaluation", "type": "function", "function": map[string]any{"name": "multica_record_squad_evaluation", "arguments": string(args)}}}}, "tool_calls"
				} else if tc.name == "leader_changed_during_model" {
					if !strings.Contains(input.Messages[len(input.Messages)-1].Content, "Tool error:") {
						t.Error("former leader did not receive its rejected evaluation result")
					}
				} else if step != evaluationStep+1 || !strings.Contains(input.Messages[len(input.Messages)-1].Content, "squad_leader_evaluated") {
					t.Errorf("evaluation did not reach the existing service: %+v", input.Messages)
				}
				frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
			}))
			defer upstream.Close()
			t.Setenv("MULTICA_LLM_API_KEY", "fixture-key")
			t.Setenv("MULTICA_LLM_BASE_URL", upstream.URL)
			t.Setenv("MULTICA_LLM_DEFAULT_MODEL", "fixture-model")
			router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
			call := func(credential, method, path string, body any) *http.Request {
				req := testutil.JSONRequest(method, path, body)
				req.Header.Set("Authorization", "Bearer "+credential)
				req.Header.Set("X-Workspace-ID", workspaceID)
				req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
				return req
			}
			body := map[string]any{"title": "Native squad evaluation", "status": "todo", "assignee_type": "squad", "assignee_id": squadID}
			if source != "assignment" && source != "child_done" {
				body["status"] = "backlog"
			}
			if source == "mention" {
				body["assignee_type"], body["assignee_id"] = "member", testUserID
			}
			var issue handler.IssueResponse
			testutil.Call(t, router.ServeHTTP, call(testToken, http.MethodPost, "/api/issues", body)).Want(http.StatusCreated).JSON(&issue)
			fx.Cleanup(t, "DELETE FROM issue WHERE id=$1", issue.ID)
			fx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issue.ID)
			fx.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", issue.ID)
			if source == "child_done" {
				// Isolate the child completion trigger from the initial assignment.
				fx.Exec(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", issue.ID)
			}
			if source == "comment" || source == "mention" {
				content := "Please evaluate this update."
				if source == "mention" {
					content = fmt.Sprintf("[@native squad](mention://squad/%s) Please evaluate this update.", squadID)
				}
				testutil.Call(t, router.ServeHTTP, call(testToken, http.MethodPost, "/api/issues/"+issue.ID+"/comments", map[string]any{"content": content})).Want(http.StatusCreated)
			}
			if source == "worker_comment" || source == "child_done" {
				workerTurn = true
				workerID := fx.Agent(t, "native squad worker", runtimeID)
				fx.SquadMember(t, squadID, "agent", workerID)
				fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", workerID)
				fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", workerID)
				fx.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", workerID)
				workerIssue = issue.ID
				if source == "worker_comment" {
					var trigger handler.CommentResponse
					testutil.Call(t, router.ServeHTTP, call(testToken, http.MethodPost, "/api/issues/"+issue.ID+"/comments", map[string]any{"content": fmt.Sprintf("[@worker](mention://agent/%s) Report your result in this thread.", workerID), "suppress_agent_ids": []string{agentID}})).Want(http.StatusCreated).JSON(&trigger)
					workerThread = trigger.ID
				} else {
					var child handler.IssueResponse
					testutil.Call(t, router.ServeHTTP, call(testToken, http.MethodPost, "/api/issues", map[string]any{"title": "Native child objective", "parent_issue_id": issue.ID, "status": "todo", "assignee_type": "agent", "assignee_id": workerID})).Want(http.StatusCreated).JSON(&child)
					workerIssue = child.ID
					fx.Cleanup(t, "DELETE FROM issue WHERE id=$1", child.ID)
					fx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", child.ID)
					fx.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", child.ID)
				}
				var claimedWorker struct{ Tasks []daemon.Task }
				testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1})).Want(http.StatusOK).JSON(&claimedWorker)
				if len(claimedWorker.Tasks) != 1 || claimedWorker.Tasks[0].AgentID != workerID || claimedWorker.Tasks[0].IsLeaderTask || claimedWorker.Tasks[0].IssueID != workerIssue {
					t.Fatalf("trigger did not claim the native worker: %+v", claimedWorker)
				}
				task := claimedWorker.Tasks[0]
				testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/start", map[string]any{"runtime_id": runtimeID, "dispatched_at": task.DispatchedAt})).Want(http.StatusOK)
				api := httptest.NewServer(router)
				defer api.Close()
				dir := t.TempDir()
				backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				defer closeBackend()
				session, err := backend.Execute(ctx, daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir, SystemPrompt: task.Agent.Instructions})
				if err != nil {
					t.Fatal(err)
				}
				for range session.Messages {
				}
				result := <-session.Result
				wantSteps := 2
				if source == "child_done" {
					wantSteps = 3
				}
				if result.Status != "completed" || workerStep != wantSteps {
					t.Fatalf("native worker failed: %+v step=%d", result, workerStep)
				}
				testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
				if source == "worker_comment" {
					if got := fx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1 AND author_id=$2 AND source_task_id=$3 AND parent_id=$4", issue.ID, workerID, task.ID, workerThread); got != 1 {
						t.Fatalf("worker reply lost its thread or run provenance: %d", got)
					}
				} else if got := fx.Count(t, "SELECT count(*) FROM issue WHERE id=$1 AND status='done'", workerIssue); got != 1 {
					t.Fatal("worker completion did not finish the child objective")
				}
				workerTurn = false
			}
			var claimed struct{ Tasks []daemon.Task }
			testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1})).Want(http.StatusOK).JSON(&claimed)
			if len(claimed.Tasks) != 1 || !claimed.Tasks[0].IsLeaderTask || claimed.Tasks[0].SquadID != squadID || claimed.Tasks[0].IssueID != issue.ID {
				t.Fatalf("trigger did not claim a native squad leader: %+v", claimed)
			}
			task := claimed.Tasks[0]
			testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/start", map[string]any{"runtime_id": runtimeID, "dispatched_at": task.DispatchedAt})).Want(http.StatusOK)
			if tc.name == "leader_changed_during_model" {
				newLeader := fx.Agent(t, "replacement native squad leader", runtimeID)
				beforeFirstModelResult = func() {
					recorded := httptest.NewRecorder()
					router.ServeHTTP(recorded, call(testToken, http.MethodPut, "/api/squads/"+squadID, map[string]string{"leader_id": newLeader}))
					if recorded.Code != http.StatusOK {
						t.Errorf("leadership change failed: HTTP %d", recorded.Code)
					}
				}
			} else if tc.name == "cancelled_during_model" {
				beforeFirstModelResult = func() {
					recorded := httptest.NewRecorder()
					router.ServeHTTP(recorded, call(testToken, http.MethodPost, "/api/tasks/"+task.ID+"/cancel", nil))
					if recorded.Code != http.StatusOK {
						t.Errorf("leader cancellation failed: HTTP %d", recorded.Code)
					}
				}
			}
			var evaluationRequests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Model a receipt outage as well as the lost write response for
				// cases that exercise recovery across a new API/executor pair.
				if tc.loseResponse && tc.name != "no_action_response_lost_live" && evaluationRequests.Load() > 0 && strings.HasSuffix(r.URL.Path, "/write-result") {
					http.Error(w, "fixture receipt unavailable", http.StatusServiceUnavailable)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/squad-evaluated") {
					router.ServeHTTP(w, r)
					return
				}
				evaluationRequests.Add(1)
				if !tc.loseResponse {
					router.ServeHTTP(w, r)
					return
				}
				// Commit through the real route, then lose the response before the
				// daemon can observe it. No handler or receipt is stubbed here.
				recorded := httptest.NewRecorder()
				router.ServeHTTP(recorded, r)
				if recorded.Code != http.StatusCreated {
					t.Errorf("evaluation failed before response loss: HTTP %d", recorded.Code)
				}
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			}))
			defer api.Close()
			dir := t.TempDir()
			backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBackend()
			session, err := backend.Execute(ctx, daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir, SystemPrompt: task.Agent.Instructions})
			if err != nil {
				t.Fatal(err)
			}
			for range session.Messages {
			}
			result := <-session.Result
			if tc.name == "cancelled_during_model" {
				if result.Status == "completed" || step != 1 || evaluationRequests.Load() != 0 {
					t.Fatalf("cancelled leader continued with a write: status=%s steps=%d evaluation_requests=%d", result.Status, step, evaluationRequests.Load())
				}
				for range 2 {
					testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
				}
				if got := fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='cancelled'", task.ID); got != 1 {
					t.Fatal("late completion overwrote the cancelled run")
				}
				if got := fx.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='squad_leader_evaluated'", issue.ID); got != 0 {
					t.Fatal("cancelled leader recorded an evaluation")
				}
				if got := fx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1 AND source_task_id=$2", issue.ID, task.ID); got != 0 {
					t.Fatal("cancelled leader synthesized a result comment")
				}
				return
			}
			if tc.loseResponse && tc.name != "no_action_response_lost_live" {
				if result.Status != "failed" || step != 1 || evaluationRequests.Load() != 1 {
					t.Fatalf("uncertain evaluation did not stop execution: %+v steps=%d requests=%d", result, step, evaluationRequests.Load())
				}
				if got := fx.Count(t, "SELECT count(*) FROM native_session WHERE agent_id=$1 AND checkpoint->'tools'->0->>'state'='unknown' AND (SELECT count(*) FROM jsonb_each(write_outcomes))=1", agentID); got != 1 {
					t.Fatal("lost response did not retain its unknown checkpoint and confirmed receipt")
				}
				closeBackend()
				api.Close()
				// New API and executor instances have no memory of the completed
				// write. Recovery must use SQL, even with an empty local journal.
				restarted, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
				recoveryAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/squad-evaluated") {
						evaluationRequests.Add(1)
					}
					restarted.ServeHTTP(w, r)
				}))
				defer recoveryAPI.Close()
				resumed, closeResumed, err := daemon.NewNativeBackend(ctx, daemon.NewClient(recoveryAPI.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				defer closeResumed()
				session, err := resumed.Execute(ctx, daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir, SystemPrompt: task.Agent.Instructions})
				if err != nil {
					t.Fatal(err)
				}
				for range session.Messages {
				}
				result = <-session.Result
			}
			if result.Status != "completed" || step != evaluationStep+1 {
				t.Fatalf("native squad evaluation failed: %+v step=%d", result, step)
			}
			testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
			testutil.Call(t, router.ServeHTTP, call(token, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
			wantEvaluations := 1
			if tc.name == "leader_changed_during_model" {
				wantEvaluations = 0
			}
			if got := fx.Count(t, "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND actor_id=$2 AND action='squad_leader_evaluated' AND details->>'outcome'=$4 AND details->>'task_id'=$3", issue.ID, agentID, task.ID, tc.outcome); got != wantEvaluations {
				t.Fatalf("leader verdict not recorded exactly once: %d", got)
			}
			wantComments := 1
			if tc.outcome == "no_action" && tc.name != "comment_before_evaluation" && tc.name != "leader_changed_during_model" {
				wantComments = 0
			}
			if got := fx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1 AND author_type='agent' AND author_id=$2 AND source_task_id=$3", issue.ID, agentID, task.ID); got != wantComments {
				t.Fatalf("%s completion synthesized %d comments; want %d", tc.outcome, got, wantComments)
			}
			if evaluationRequests.Load() != 1 {
				t.Fatalf("recovery repeated the evaluation write: %d requests", evaluationRequests.Load())
			}
			wantReceipts := 1
			if tc.name == "comment_before_evaluation" {
				wantReceipts = 2
			}
			if got := fx.Count(t, "SELECT count(*) FROM native_session WHERE agent_id=$1 AND (SELECT count(*) FROM jsonb_each(write_outcomes))=$2", agentID, wantReceipts); got != 1 {
				t.Fatalf("evaluation lost its durable write receipt: %d", got)
			}
			if got := fx.Count(t, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND status='queued'", agentID); got != 0 {
				t.Fatalf("leader verdict triggered %d new runs", got)
			}
		})
	}
}
