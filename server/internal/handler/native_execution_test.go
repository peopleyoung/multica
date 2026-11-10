package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Exercise the production daemon backend through HTTP authorization, SQL
// checkpoints, streaming inference, and real confined file tools. No AI CLI
// or external model service participates in this acceptance test.
func TestNativeExecutionHTTPCheckpointAndChatContinuation(t *testing.T) {
	const token = "mdt_native_execution_fixture"
	const machine = "native-execution-machine"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native execution", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native execution", runtimeID)
	chatID := dbfx.ChatSession(t, agentID)
	dbfx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	step := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "fixture-model" || r.Header.Get("Authorization") != "Bearer upstream-only-secret" {
			t.Error("upstream did not use the deployment model and credential")
		}
		step++
		delta := map[string]any{}
		finish := "tool_calls"
		switch step {
		case 1:
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "read", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"a.txt"}`}}}
		case 2:
			if last := req.Messages[len(req.Messages)-1]; last.Role != "tool" || last.Content != "before" {
				t.Errorf("read result not delivered to the model: %+v", last)
			}
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "write", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": `{"path":"a.txt","old_text":"before","new_text":"after"}`}}}
		case 3:
			delta["content"], finish = "Updated.", "stop"
		case 4:
			if len(req.Messages) < 7 || req.Messages[len(req.Messages)-2].Content != "Updated." {
				t.Error("second chat turn lost durable model and tool history")
			}
			delta["content"], finish = "I remember.", "stop"
		default:
			t.Error("unexpected extra model step")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
	}))
	defer upstream.Close()
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 32}
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{APIKey: "upstream-only-secret", BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: caps})}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine)))
		})
	})
	router.Get("/api/daemon/native/config", h.NativeModelConfig)
	router.Post("/api/daemon/tasks/{taskId}/native/inference", h.NativeInference)
	router.Post("/api/daemon/tasks/{taskId}/native/session", h.AcquireNativeSession)
	router.Post("/api/daemon/tasks/{taskId}/native/checkpoint/{operation}", h.NativeCheckpoint)
	api := httptest.NewServer(router)
	defer api.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := t.TempDir()
	var sessionID string
	for i, prompt := range []string{"Update the file.", "Do you remember?"} {
		taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
		task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, AgentID: agentID, WorkspaceID: testWorkspaceID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token}
		backend, closeBackend, err := daemon.NewNativeBackend(context.Background(), daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: journal, Permissions: tools.Permissions{WriteFiles: true}})
		if err != nil {
			t.Fatal(err)
		}
		session, err := backend.Execute(context.Background(), prompt, agent.ExecOptions{Cwd: dir, SystemPrompt: "Use the available tools."})
		if err != nil {
			closeBackend()
			t.Fatal(err)
		}
		toolResults := 0
		for message := range session.Messages {
			if message.Type == agent.MessageToolResult {
				toolResults++
			}
		}
		result := <-session.Result
		closeBackend()
		if result.Status != "completed" || result.SessionID == "" {
			t.Fatalf("turn %d: %+v", i, result)
		}
		if i == 0 {
			sessionID = result.SessionID
			if toolResults != 2 || result.Output != "Updated." {
				t.Fatalf("missing tool/result events: %+v count=%d", result, toolResults)
			}
		} else if result.SessionID != sessionID || result.Output != "I remember." {
			t.Fatalf("chat did not continue the same session: %+v", result)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(raw) != "after" || step != 4 {
		t.Fatalf("disk/model mismatch: file=%q steps=%d err=%v", raw, step, err)
	}
}
