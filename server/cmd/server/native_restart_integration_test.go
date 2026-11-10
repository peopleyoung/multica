package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Interrupt a live model stream by shutting down the API's TCP listener and
// connections, then rebuild its router and native backend against the same DB.
func TestNativeAPIRestartRecoversSavedToolWithoutReplayingWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	machine, token := "native-restart-"+uuid.NewString(), "mdt_"+uuid.NewString()
	runtimeID := fx.Runtime(t, "native restart", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := fx.Agent(t, "native restart", runtimeID)
	issueID := fx.Issue(t, "native API restart")
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
	fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	fx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, WorkspaceID: testWorkspaceID, AgentID: agentID, RuntimeID: runtimeID, IssueID: issueID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token}
	opened, stopped := make(chan struct{}), make(chan struct{})
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
			t.Error("restart changed deployment model")
		}
		step++
		w.Header().Set("Content-Type", "text/event-stream")
		switch step {
		case 1:
			delta := map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "write-once", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": `{"path":"once.txt","old_text":"before","new_text":"after"}`}}}}
			frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": "tool_calls"}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
		case 2:
			if last := input.Messages[len(input.Messages)-1]; last.Role != "tool" {
				t.Error("write result was not checkpointed before next model step")
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"incomplete reply\"}}]}\n\n")
			w.(http.Flusher).Flush()
			close(opened)
			<-r.Context().Done()
			close(stopped)
		case 3:
			var history strings.Builder
			for _, message := range input.Messages {
				history.WriteString(message.Content)
			}
			if strings.Contains(history.String(), "incomplete reply") || !strings.Contains(history.String(), "Recover the native restart fixture") {
				t.Error("restart lost input or saved incomplete assistant output")
			}
			if input.Messages[len(input.Messages)-1].Role != "tool" {
				t.Error("restart lost completed tool history")
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Recovered after API restart.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			t.Error("API restart repeated a model or tool step")
		}
	}))
	defer upstream.Close()
	defer cancel()
	t.Setenv("MULTICA_LLM_API_KEY", "fixture-key")
	t.Setenv("MULTICA_LLM_BASE_URL", upstream.URL)
	t.Setenv("MULTICA_LLM_DEFAULT_MODEL", "fixture-model")
	newAPI := func(address string) *httptest.Server {
		router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 32}})
		server := httptest.NewUnstartedServer(router)
		if address != "" {
			if err := server.Listener.Close(); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			server.Listener = listener
		}
		server.Start()
		return server
	}
	api := newAPI("")
	defer api.Close()
	defer cancel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "once.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir(), Permissions: tools.Permissions{WriteFiles: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(ctx, "Recover the native restart fixture", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	interrupted := make(chan agent.Result, 1)
	go func() {
		for range session.Messages {
		}
		interrupted <- <-session.Result
	}()
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal("stream did not open after saved file write")
	}
	address := api.Listener.Addr().String()
	api.CloseClientConnections()
	api.Close()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("API shutdown left upstream stream alive")
	}
	select {
	case result := <-interrupted:
		if result.Status == "completed" {
			t.Fatal("truncated model stream was reported completed")
		}
	case <-ctx.Done():
		t.Fatal("API shutdown did not settle native run")
	}
	closeBackend()
	if data, err := os.ReadFile(filepath.Join(dir, "once.txt")); err != nil || string(data) != "after" {
		t.Fatal("write was not present before API restart")
	}
	// Model a process crash leaving an unfinished model reservation. The
	// interrupted handler may finish its own accounting asynchronously, so an
	// explicit orphan makes recovery independent of that scheduling race.
	fx.Insert(t, "model_call", testutil.Cols{
		"workspace_id": testWorkspaceID, "task_id": taskID, "purpose": "execution",
		"scope_key": "run:" + taskID, "model": "fixture-model",
		"reserved_tokens": 1, "charged_tokens": 1,
	})
	// Advance both lease intervals without sleeping: the native session lease
	// and the independent model admission lease each fence crash recovery.
	// Their canonical tests own exclusivity before expiry.
	fx.Exec(t, "UPDATE native_session SET lease_until=now()-interval '1 second' WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID)
	fx.Exec(t, "UPDATE model_call SET lease_until=now()-interval '1 second' WHERE workspace_id=$1 AND task_id=$2 AND outcome='active'", testWorkspaceID, taskID)
	restarted := newAPI(address)
	defer restarted.Close()
	defer cancel()
	recovered, closeRecovered, err := daemon.NewNativeBackend(ctx, daemon.NewClient(restarted.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir(), Permissions: tools.Permissions{WriteFiles: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRecovered()
	continued, err := recovered.Execute(ctx, "Recover the native restart fixture", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range continued.Messages {
	}
	result := <-continued.Result
	if result.Status != "completed" || result.Output != "Recovered after API restart." || step != 3 {
		t.Fatalf("native recovery failed: %+v steps=%d", result, step)
	}
	var checkpoint string
	if err := testPool.QueryRow(ctx, "SELECT checkpoint::text FROM native_session WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agentID).Scan(&checkpoint); err != nil {
		t.Fatal(err)
	}
	var saved agentstate.Checkpoint
	if err := json.Unmarshal([]byte(checkpoint), &saved); err != nil {
		t.Fatal(err)
	}
	calls, results := 0, 0
	for _, message := range saved.Messages {
		for _, call := range message.ToolCalls {
			if call.ID == "write-once" && call.Name == "apply_patch" {
				calls++
			}
		}
		if message.Role == "tool" && message.ToolCallID == "write-once" {
			results++
		}
	}
	if err := saved.Validate(65536); err != nil {
		t.Fatal(err)
	}
	if len(saved.Tools) != 0 || calls != 1 || results != 1 || strings.Contains(checkpoint, "incomplete reply") {
		t.Fatal("API restart duplicated write history or committed partial output")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "once.txt")); err != nil || string(data) != "after" {
		t.Fatal("API restart changed saved file result")
	}
}
