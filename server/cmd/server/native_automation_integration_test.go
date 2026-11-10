package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
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
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Production routing, dispatch, claim credentials, native execution, completion
// and terminal listeners participate. The only model is a local HTTP fixture.
func TestNativeAutomationDispatchThroughServerRouter(t *testing.T) {
	for _, source := range []string{"schedule", "webhook"} {
		for _, mode := range []string{"run_only", "create_issue"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				machine, daemonToken := "native-automation-"+uuid.NewString(), "mdt_"+uuid.NewString()
				fx := testutil.New(testPool, testWorkspaceID, testUserID)
				runtimeID := fx.Runtime(t, "native automation", testutil.Cols{"provider": "native", "daemon_id": machine})
				agentID := fx.Agent(t, "native automation", runtimeID)
				fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
				fx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
				fx.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", agentID)
				fx.Cleanup(t, "DELETE FROM task_token WHERE agent_id=$1", agentID)
				fx.Exec(t, `UPDATE workspace SET issue_counter=GREATEST(issue_counter,
					(SELECT COALESCE(MAX(number),0) FROM issue WHERE workspace_id=$1)) WHERE id=$1`, testWorkspaceID)
				step := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var input struct {
						Model    string
						Messages []struct{ Role, Content string }
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.Model != "fixture-model" {
						t.Error("automation selected another model")
					}
					if mode == "run_only" && step == 0 || mode == "create_issue" && step == 1 {
						var prompt strings.Builder
						for _, message := range input.Messages {
							prompt.WriteString(message.Content)
						}
						if !strings.Contains(prompt.String(), "Produce the native digest") || source == "webhook" && !strings.Contains(prompt.String(), "native-fixture-event") {
							t.Error("dispatch lost automation instructions or webhook payload")
						}
					}
					delta, finish := map[string]any{"content": "Native digest completed."}, "stop"
					if mode == "create_issue" && step < 2 {
						var issue struct {
							ID       string
							Revision int64
						}
						name, arguments := "multica_get_issue", map[string]any{}
						if step == 0 {
							var issueID string
							if err := testPool.QueryRow(ctx, "SELECT issue_id::text FROM agent_task_queue WHERE agent_id=$1 ORDER BY created_at DESC LIMIT 1", agentID).Scan(&issueID); err != nil {
								t.Error(err)
							}
							arguments["issue_id"] = issueID
						} else {
							if err := json.Unmarshal([]byte(input.Messages[len(input.Messages)-1].Content), &issue); err != nil {
								t.Error(err)
							}
							name, arguments = "multica_update_issue_status", map[string]any{"issue_id": issue.ID, "status": "done", "expected_revision": issue.Revision}
						}
						args, _ := json.Marshal(arguments)
						delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("automation-%d", step), "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}}}}, "tool_calls"
					}
					step++
					frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
				}))
				defer upstream.Close()
				t.Setenv("MULTICA_LLM_API_KEY", "fixture-key")
				t.Setenv("MULTICA_LLM_BASE_URL", upstream.URL)
				t.Setenv("MULTICA_LLM_DEFAULT_MODEL", "fixture-model")
				bus := events.New()
				router, h := NewRouterWithOptions(testPool, realtime.NewHub(), bus, analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
				registerAutopilotListeners(bus, h.AutopilotService)
				call := func(token, method, path string, body any) *http.Request {
					req := testutil.JSONRequest(method, path, body)
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("X-Workspace-ID", testWorkspaceID)
					req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
					return req
				}
				var automation handler.AutopilotResponse
				testutil.Call(t, router.ServeHTTP, call(testToken, http.MethodPost, "/api/autopilots", map[string]any{"title": "Native automation", "description": "Produce the native digest", "assignee_id": agentID, "execution_mode": mode, "issue_title_template": "Native digest"})).Want(http.StatusCreated).JSON(&automation)
				fx.Cleanup(t, "DELETE FROM autopilot WHERE id=$1", automation.ID)
				fx.Cleanup(t, "DELETE FROM autopilot_run WHERE autopilot_id=$1", automation.ID)
				fx.Cleanup(t, "DELETE FROM autopilot_rule_version WHERE autopilot_id=$1", automation.ID)
				fx.Cleanup(t, "DELETE FROM autopilot_trigger WHERE autopilot_id=$1", automation.ID)
				fx.Cleanup(t, "DELETE FROM webhook_delivery WHERE autopilot_id=$1", automation.ID)
				var trigger handler.AutopilotTriggerResponse
				triggerBody := map[string]any{"kind": source}
				if source == "schedule" {
					triggerBody["cron_expression"], triggerBody["timezone"] = "0 9 * * *", "Asia/Shanghai"
				}
				testutil.Call(t, router.ServeHTTP, call(testToken, http.MethodPost, "/api/autopilots/"+automation.ID+"/triggers", triggerBody)).Want(http.StatusCreated).JSON(&trigger)
				ap, err := h.Queries.GetAutopilot(ctx, parseUUID(automation.ID))
				if err != nil {
					t.Fatal(err)
				}
				if source == "schedule" {
					planned := time.Now().UTC().Truncate(time.Microsecond)
					first, err := h.AutopilotService.DispatchAutopilotForPlan(ctx, ap, parseUUID(trigger.ID), source, nil, planned)
					if err != nil {
						t.Fatal(err)
					}
					duplicate, err := h.AutopilotService.DispatchAutopilotForPlan(ctx, ap, parseUUID(trigger.ID), source, nil, planned)
					if err != nil || first == nil || duplicate == nil || first.ID != duplicate.ID {
						t.Fatalf("schedule replay changed run: first=%+v duplicate=%+v error=%v", first, duplicate, err)
					}
				} else {
					if trigger.WebhookToken == nil {
						t.Fatal("test webhook has no credential")
					}
					for range 2 {
						req := testutil.JSONRequest(http.MethodPost, "/api/webhooks/autopilots/"+*trigger.WebhookToken, map[string]any{"event": "native-fixture-event", "data": map[string]string{"objective": "daily digest"}})
						req.Header.Set("X-GitHub-Delivery", "native-fixture-delivery")
						testutil.Call(t, router.ServeHTTP, req).Want(http.StatusOK)
					}
					worked, err := h.WebhookDeliveryWorker.ProcessNext(ctx)
					if err != nil || !worked {
						t.Fatalf("webhook dispatch failed: worked=%t error=%v", worked, err)
					}
				}
				var claimed struct{ Tasks []daemon.Task }
				testutil.Call(t, router.ServeHTTP, call(daemonToken, http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1})).Want(http.StatusOK).JSON(&claimed)
				if len(claimed.Tasks) != 1 {
					t.Fatalf("native automation was not claimable: %+v", claimed)
				}
				task := claimed.Tasks[0]
				testutil.Call(t, router.ServeHTTP, call(daemonToken, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/start", map[string]any{"runtime_id": runtimeID, "dispatched_at": task.DispatchedAt})).Want(http.StatusOK)
				api := httptest.NewServer(router)
				defer api.Close()
				dir := t.TempDir()
				backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				defer closeBackend()
				session, err := backend.Execute(ctx, daemon.BuildPrompt(task, "native"), agent.ExecOptions{Cwd: dir})
				if err != nil {
					t.Fatal(err)
				}
				for range session.Messages {
				}
				result := <-session.Result
				if result.Status != "completed" || result.Output != "Native digest completed." {
					t.Fatalf("native automation failed: %+v", result)
				}
				testutil.Call(t, router.ServeHTTP, call(daemonToken, http.MethodPost, "/api/daemon/tasks/"+task.ID+"/complete", handler.TaskCompleteRequest{Output: result.Output, SessionID: result.SessionID, WorkDir: dir})).Want(http.StatusOK)
				var run db.AutopilotRun
				if err := testPool.QueryRow(ctx, "SELECT id, status, result FROM autopilot_run WHERE autopilot_id=$1", automation.ID).Scan(&run.ID, &run.Status, &run.Result); err != nil || run.Status != "completed" {
					t.Fatalf("native completion did not reach automation: status=%s error=%v", run.Status, err)
				}
				var count int
				if err := testPool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1", agentID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("event replay duplicated native work: tasks=%d error=%v", count, err)
				}
				if mode == "run_only" && !strings.Contains(string(run.Result), result.Output) {
					t.Fatalf("run-only result lost native output: %s", run.Result)
				}
				if task.IssueID != "" {
					fx.Cleanup(t, "DELETE FROM issue WHERE id=$1", task.IssueID)
				}
			})
		}
	}
}
