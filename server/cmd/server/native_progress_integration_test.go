package main

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
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeRepeatedReadsCanReportBlockerThroughRequestedThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	machine, token, taskToken := "native-progress-"+uuid.NewString(), "mdt_"+uuid.NewString(), "mat_"+uuid.NewString()
	runtimeID := fx.Runtime(t, "native progress", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native progress", runtimeID)
	issueID := fx.Issue(t, "Verify external data", testutil.Cols{"status": "todo"})
	threadID := fx.Comment(t, issueID, "Report the blocker in this thread.")
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
	fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	fx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": testUserID, "agent_id": agentID, "task_id": taskID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
	fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1 AND source_task_id=$2", issueID, taskID)
	const blocker = "Blocked: external annotation-platform access is unavailable; local workspace evidence cannot establish whether a published dataset exists."
	steps, sawFeedback := 0, false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		steps++
		delta, finish := map[string]any{}, "tool_calls"
		call := func(index int, name string, args map[string]any) map[string]any {
			raw, _ := json.Marshal(args)
			return map[string]any{"index": index, "id": fmt.Sprintf("step-%d-%d", steps, index), "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}
		}
		switch {
		case steps <= 3:
			delta["tool_calls"] = []any{call(0, "multica_get_issue", map[string]any{"issue_id": issueID}), call(1, "list_directory", map[string]any{"path": "."})}
		case steps == 4:
			last := input.Messages[len(input.Messages)-1]
			sawFeedback = last.Role == "system" && strings.Contains(last.Content, "Runtime progress check:") && strings.Contains(last.Content, "Never edit protected.txt.")
			if !sawFeedback {
				t.Error("repeated reads did not produce durable feedback preserving constraints")
			}
			delta["tool_calls"] = []any{call(0, "multica_create_comment", map[string]any{"issue_id": issueID, "parent_id": threadID, "content": blocker})}
		default:
			delta["content"], finish = blocker, "stop"
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	t.Setenv("MULTICA_LLM_API_KEY", "fixture-key")
	t.Setenv("MULTICA_LLM_BASE_URL", upstream.URL)
	t.Setenv("MULTICA_LLM_DEFAULT_MODEL", "fixture-model")
	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 128000, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 128}
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: caps})
	api := httptest.NewServer(router)
	defer api.Close()
	dir := t.TempDir()
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, WorkspaceID: testWorkspaceID, AgentID: agentID, RuntimeID: runtimeID, IssueID: issueID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token, AuthToken: taskToken}
	backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(ctx, "Verify the dataset; report a capability blocker in the requested thread.", agent.ExecOptions{Cwd: dir, SystemPrompt: "Never edit protected.txt."})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || steps != 5 || !sawFeedback {
		t.Fatalf("progress recovery failed: result=%+v steps=%d feedback=%v", result, steps, sawFeedback)
	}
	if count := fx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1 AND parent_id=$2 AND author_type='agent' AND author_id=$3 AND source_task_id=$4 AND content=$5", issueID, threadID, agentID, taskID, blocker); count != 1 {
		t.Fatalf("blocker was not posted exactly once to its requested thread: %d", count)
	}
	var status string
	fx.QueryRow(t, "SELECT status FROM issue WHERE id=$1", issueID).Scan(&status)
	if status != "todo" {
		t.Fatalf("unfulfilled objective was marked complete: %s", status)
	}
	var raw []byte
	// Release clears active_task_id; read the agent's issue scope instead.
	fx.QueryRow(t, "SELECT checkpoint FROM native_session WHERE agent_id=$1 AND scope_id=$2", agentID, issueID).Scan(&raw)
	var saved agentstate.Checkpoint
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if err := saved.Validate(4096); err != nil {
		t.Fatal(err)
	}
}
