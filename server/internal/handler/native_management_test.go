package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeManagementUsesAuthorizedServicesAndDurableReceipts(t *testing.T) {
	const machine, daemonToken, taskToken = "native-management", "mdt_management_fixture", "mat_management_fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native management", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native management", runtimeID)
	chatID := dbfx.ChatSession(t, agentID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "originator_user_id": testUserID, "accountable_user_id": testUserID})
	skillID := insertHandlerTestSkill(t, "native-management", "Useful workflow")
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "task_id": taskID, "user_id": testUserID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
	dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
	foreignWorkspace := dbfx.Workspace(t, "Foreign native configuration", "foreign-native-"+uuid.NewString())
	foreign := dbfx.Insert(t, "autopilot", testutil.Cols{"workspace_id": foreignWorkspace, "title": "Outside workspace", "description": "Untouched", "assignee_id": agentID, "execution_mode": "run_only", "created_by_type": "member", "created_by_id": testUserID})
	type operation struct {
		resource, action string
		values           map[string]any
	}
	operations := []operation{
		{"project", "create", map[string]any{"title": "Native managed project", "description": "Shared outcome"}},
		{"project", "update", map[string]any{"status": "in_progress", "due_date": "2026-12-01"}},
		{"project", "add_resource", map[string]any{"resource_type": "github_repo", "resource_ref": map[string]any{"url": "https://github.com/multica-ai/multica", "ref": "main"}}},
		{"project", "list_resources", nil},
		{"agent", "create", map[string]any{"name": "Native specialist", "instructions": "Complete the assigned work", "skill_ids": []string{skillID}}},
		{"agent", "update", map[string]any{"description": "Configured through native tools"}},
		{"agent", "add_skills", map[string]any{"skill_ids": []string{skillID}}},
		{"agent", "get", nil},
		{"squad", "create", map[string]any{"name": "Native team", "leader_id": "$agent"}},
		{"squad", "update", map[string]any{"description": "Configured team"}},
		{"squad", "add_member", map[string]any{"member_type": "agent", "member_id": agentID}},
		{"squad", "list_members", nil},
		{"autopilot", "create", map[string]any{"title": "Native digest", "description": "Prepare the digest", "assignee_id": "$agent", "execution_mode": "run_only", "project_id": "$project"}},
		{"autopilot", "update", map[string]any{"status": "paused"}},
		{"autopilot", "add_trigger", map[string]any{"kind": "schedule", "cron_expression": "0 9 * * *", "timezone": "Asia/Shanghai"}},
		{"autopilot", "add_trigger", map[string]any{"kind": "webhook", "label": "External kickoff"}},
		{"autopilot", "get", nil},
		{"foreign", "update", map[string]any{"description": "Must not change"}},
	}
	ids := map[string]string{"foreign": foreign}
	step := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if step > 0 {
			previous := operations[step-1]
			content := input.Messages[len(input.Messages)-1].Content
			if previous.resource == "foreign" {
				if !strings.Contains(content, "HTTP 404") {
					t.Errorf("cross-workspace write did not fail closed: %s", content)
				}
			} else if strings.HasPrefix(content, "Tool error:") {
				t.Errorf("%s/%s failed: %s", previous.resource, previous.action, content)
			}
			if previous.action == "create" {
				var created struct{ ID string }
				if json.Unmarshal([]byte(content), &created) != nil || created.ID == "" {
					t.Errorf("%s create returned no id: %s", previous.resource, content)
				}
				ids[previous.resource] = created.ID
				registerNativeManagementCleanup(t, previous.resource, created.ID)
			}
			if strings.Contains(content, "webhook_token") || strings.Contains(content, "webhook_url") || strings.Contains(content, "mcp_config") || strings.Contains(content, "runtime_config") {
				t.Errorf("configuration credentials entered model history: %s", content)
			}
		}
		delta, finish := map[string]any{"content": "Configuration completed."}, "stop"
		if step < len(operations) {
			op := operations[step]
			args := map[string]any{"action": op.action}
			if op.action != "create" {
				args["id"] = ids[op.resource]
			}
			if op.values != nil {
				args["values"] = op.values
			}
			raw, _ := json.Marshal(args)
			for resource, id := range ids {
				raw = []byte(strings.ReplaceAll(string(raw), "$"+resource, id))
			}
			resource := op.resource
			if resource == "foreign" {
				resource = "autopilot"
			}
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("configure-%d", step), "type": "function", "function": map[string]any{"name": "multica_manage_" + resource, "arguments": string(raw)}}}}, "tool_calls"
		}
		step++
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	h := &Handler{
		Queries: testHandler.Queries, DB: testHandler.DB, TxStarter: testHandler.TxStarter,
		TaskService: testHandler.TaskService, AutopilotService: testHandler.AutopilotService,
		Hub: testHandler.Hub, Bus: testHandler.Bus, Analytics: testHandler.Analytics,
	}
	h.LLM = llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
	api := httptest.NewServer(nativeBusinessTestRouter(h, machine))
	defer api.Close()
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, AgentID: agentID, RuntimeID: runtimeID, ChatSessionID: chatID, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: daemonToken, AuthToken: taskToken}
	dir := t.TempDir()
	backend, closeBackend, err := daemon.NewNativeBackend(context.Background(), daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(context.Background(), "Apply the previewed configuration approved by the human.", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || step != len(operations)+1 {
		t.Fatalf("configuration loop failed: %+v step=%d", result, step)
	}
	var storedRuntime, model, description string
	if err := testPool.QueryRow(context.Background(), "SELECT runtime_id::text, COALESCE(model,''), description FROM agent WHERE id=$1", ids["agent"]).Scan(&storedRuntime, &model, &description); err != nil || storedRuntime != runtimeID || model != "" || description != "Configured through native tools" {
		t.Fatalf("native agent configuration lost runtime/model binding: runtime=%s model=%q description=%q error=%v", storedRuntime, model, description, err)
	}
	var receiptJSON, checkpointJSON []byte
	if err := testPool.QueryRow(context.Background(), "SELECT write_outcomes, checkpoint FROM native_session WHERE agent_id=$1 AND scope_id=$2", agentID, chatID).Scan(&receiptJSON, &checkpointJSON); err != nil {
		t.Fatal(err)
	}
	var receipts map[string]agentstate.WriteOutcome
	if err := json.Unmarshal(receiptJSON, &receipts); err != nil || len(receipts) != 14 {
		t.Fatalf("configuration writes lacked receipts: %s error=%v", receiptJSON, err)
	}
	if strings.Contains(string(receiptJSON)+string(checkpointJSON), "webhook_token") || strings.Contains(string(receiptJSON)+string(checkpointJSON), "webhook_url") {
		t.Fatal("webhook credentials reached durable native state")
	}
	if err := testPool.QueryRow(context.Background(), "SELECT description FROM autopilot WHERE id=$1", foreign).Scan(&description); err != nil || description != "Untouched" {
		t.Fatalf("cross-workspace configuration was modified: %q error=%v", description, err)
	}
	var creator string
	if err := testPool.QueryRow(context.Background(), "SELECT created_by_id::text FROM autopilot WHERE id=$1", ids["autopilot"]).Scan(&creator); err != nil || creator != testUserID {
		t.Fatalf("automation did not preserve human originator attribution: %s error=%v", creator, err)
	}
}

func registerNativeManagementCleanup(t *testing.T, resource, id string) {
	t.Helper()
	if id == "" {
		return
	}
	switch resource {
	case "project":
		dbfx.Cleanup(t, "DELETE FROM project WHERE id=$1", id)
		dbfx.Cleanup(t, "DELETE FROM project_resource WHERE project_id=$1", id)
	case "agent":
		dbfx.Cleanup(t, "DELETE FROM agent WHERE id=$1", id)
		dbfx.Cleanup(t, "DELETE FROM agent_skill WHERE agent_id=$1", id)
		dbfx.Cleanup(t, "DELETE FROM agent_invocation_target WHERE agent_id=$1", id)
	case "squad":
		dbfx.Cleanup(t, "DELETE FROM squad WHERE id=$1", id)
		dbfx.Cleanup(t, "DELETE FROM squad_member WHERE squad_id=$1", id)
	case "autopilot":
		dbfx.Cleanup(t, "DELETE FROM autopilot WHERE id=$1", id)
		dbfx.Cleanup(t, "DELETE FROM autopilot_trigger WHERE autopilot_id=$1", id)
		dbfx.Cleanup(t, "DELETE FROM autopilot_rule_version WHERE autopilot_id=$1", id)
	}
}
