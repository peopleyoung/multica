package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The native backend must cross the API summary boundary, retain the full
// audit history, and continue a tool group using the compacted projection.
func TestNativeCompactionContinuesToolsThroughServerRouter(t *testing.T) {
	testNativeCompactionThroughServerRouter(t, 0)
}

func TestNativeCompactionRenewsLeaseDuringSlowSummary(t *testing.T) {
	testNativeCompactionThroughServerRouter(t, 46*time.Second)
}

func testNativeCompactionThroughServerRouter(t *testing.T, summaryDelay time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), summaryDelay+25*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	machine, token := "native-compact-"+uuid.NewString(), "mdt_"+uuid.NewString()
	runtimeID := fx.Runtime(t, "native compact", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native compact", runtimeID)
	issueID := fx.Issue(t, "native compact history")
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
	fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "current.txt"), []byte("current file bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	oldResult := strings.Repeat("prior investigation ", 1200)
	checkpoint := agentstate.Checkpoint{InputTaskID: uuid.NewString(), Messages: []llm.ModelMessage{
		{Role: "system", Content: "Preserve the user's constraints. Never edit protected.txt."},
		{Role: "user", Content: "Investigate the earlier state; keep exact resource identifier RESOURCE-123."},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "old-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"old.txt"}`)}}},
		{Role: "tool", ToolCallID: "old-read", Content: oldResult},
		{Role: "assistant", Content: "Earlier investigation complete."},
	}}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := fx.Insert(t, "native_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": "issue", "scope_id": issueID, "daemon_id": machine, "work_dir": dir, "checkpoint": raw})
	summaries, steps := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model    string
			Messages []struct {
				Role, Content string
				ToolCallID    string `json:"tool_call_id"`
				ToolCalls     []struct {
					ID       string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
			Tools []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if input.Model != "fixture-model" {
			t.Error("summary or execution selected another model")
		}
		summary := len(input.Tools) == 0 && len(input.Messages) > 0 && strings.HasPrefix(input.Messages[0].Content, "Summarize historical agent work")
		delta, finish := map[string]any{}, "stop"
		if summary {
			summaries++
			select {
			case <-time.After(summaryDelay):
			case <-r.Context().Done():
				return
			}
			var history []llm.ModelMessage
			if len(input.Messages) != 2 {
				t.Error("summary request lost historical prefix")
			} else if err := json.Unmarshal([]byte(input.Messages[1].Content), &history); err != nil {
				t.Error(err)
			}
			if err := llm.ValidateHistory(history, 4096, false); err != nil {
				t.Errorf("summary split a tool group: %v", err)
			}
			if len(history) < 4 || history[3].ToolCallID != "old-read" || history[3].Content != oldResult {
				t.Error("summary did not include the completed historical tool group")
			}
			delta["content"] = "Earlier read completed. Preserve RESOURCE-123; never edit protected.txt. Continue the latest request."
		} else {
			steps++
			if summaries != 1 {
				t.Error("execution did not compact before inference")
			}
			var projected []llm.ModelMessage
			for _, m := range input.Messages {
				message := llm.ModelMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
				for _, call := range m.ToolCalls {
					message.ToolCalls = append(message.ToolCalls, llm.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments)})
				}
				projected = append(projected, message)
			}
			if err := llm.ValidateHistory(projected, 4096, false); err != nil {
				t.Errorf("projection contains orphaned tool results: %v", err)
			}
			var contents strings.Builder
			for _, m := range input.Messages {
				contents.WriteString(m.Content)
			}
			for _, required := range []string{"RESOURCE-123", "Never edit protected.txt.", "Read current.txt and report its bytes.", "Historical context summary"} {
				if !strings.Contains(contents.String(), required) {
					t.Errorf("compacted request lost %q", required)
				}
			}
			if strings.Contains(contents.String(), oldResult) {
				t.Error("uncompacted tool output reached inference")
			}
			if steps == 1 {
				delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "current-read", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"current.txt"}`}}}}, "tool_calls"
			} else {
				if steps != 2 || input.Messages[len(input.Messages)-1].Role != "tool" || !strings.Contains(input.Messages[len(input.Messages)-1].Content, "current file bytes") {
					t.Error("tool execution did not continue after compaction")
				}
				delta["content"] = "Read current file; preserved RESOURCE-123 and protected.txt."
			}
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
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 32}
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: caps})
	api := httptest.NewServer(router)
	defer api.Close()
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, WorkspaceID: testWorkspaceID, AgentID: agentID, RuntimeID: runtimeID, IssueID: issueID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token}
	backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(ctx, "Read current.txt and report its bytes.", agent.ExecOptions{Cwd: dir, SystemPrompt: "Never edit protected.txt."})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || summaries != 1 || steps != 2 {
		t.Fatalf("native compact continuation failed: result=%+v summaries=%d steps=%d", result, summaries, steps)
	}
	var savedRaw []byte
	fx.QueryRow(t, "SELECT checkpoint FROM native_session WHERE id=$1", sessionID).Scan(&savedRaw)
	var saved agentstate.Checkpoint
	if err := json.Unmarshal(savedRaw, &saved); err != nil {
		t.Fatal(err)
	}
	if err := saved.Validate(4096); err != nil {
		t.Fatal(err)
	}
	if saved.Summary == nil || len(saved.Messages) < len(checkpoint.Messages)+4 || saved.Messages[3].Content != oldResult || saved.Messages[3].ToolCallID != "old-read" || len(saved.Tools) != 0 {
		t.Fatal("compaction erased history or lost completed tool groups")
	}
	currentCalls, currentResults := 0, 0
	for _, message := range saved.Messages {
		for _, call := range message.ToolCalls {
			if call.ID == "current-read" {
				currentCalls++
			}
		}
		if message.Role == "tool" && message.ToolCallID == "current-read" {
			currentResults++
		}
	}
	if currentCalls != 1 || currentResults != 1 {
		t.Fatal("compaction duplicated or lost the new tool group")
	}
}
