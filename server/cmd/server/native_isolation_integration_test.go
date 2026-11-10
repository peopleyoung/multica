package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/remotemcp"
	"github.com/multica-ai/multica/server/pkg/remotemcp/remotemcptest"
)

// Force overlapping model requests for two workspaces on the same daemon.
// Both runs use identical relative paths and MCP server/tool names.
func TestNativeConcurrentRunsIsolateWorkspaceFilesHistoryAndMCPCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	type run struct {
		task                daemon.Task
		dir, marker, secret string
		config              json.RawMessage
		mcp                 *remotemcptest.Server
	}
	runs := make([]run, 2)
	root := testutil.New(testPool, testWorkspaceID, testUserID)
	machine := "native-isolation-" + uuid.NewString()
	var origins []string
	for i := range runs {
		current := &runs[i]
		current.marker, current.secret = fmt.Sprintf("private-run-%d", i), "fixture-mcp-"+uuid.NewString()
		ws := root.Workspace(t, current.marker, "native-isolation-"+uuid.NewString())
		root.Member(t, ws, testUserID, "owner")
		fx := testutil.New(testPool, ws, testUserID)
		runtimeID := fx.Runtime(t, current.marker, testutil.Cols{"provider": "native", "daemon_id": machine})
		agentID := fx.Agent(t, current.marker, runtimeID)
		issueID := fx.Issue(t, current.marker)
		dispatched := time.Now().UTC().Truncate(time.Microsecond)
		taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
		token := "mdt_" + uuid.NewString()
		fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": ws, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
		fx.Cleanup(t, "DELETE FROM native_session WHERE workspace_id=$1", ws)
		fx.Cleanup(t, "DELETE FROM model_call WHERE workspace_id=$1", ws)
		current.task = daemon.Task{ID: taskID, NativeInputTaskID: taskID, WorkspaceID: ws, AgentID: agentID, RuntimeID: runtimeID, IssueID: issueID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token}
		current.dir = t.TempDir()
		if err := os.WriteFile(filepath.Join(current.dir, "private.txt"), []byte(current.marker), 0600); err != nil {
			t.Fatal(err)
		}
		current.mcp = remotemcptest.NewServer()
		t.Cleanup(current.mcp.Close)
		target, err := url.Parse(current.mcp.URL)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+current.secret {
				t.Error("MCP credential crossed run boundary")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			r.Header.Set("Authorization", "Bearer "+remotemcptest.Credential)
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(endpoint.Close)
		origins = append(origins, endpoint.URL)
		current.config, err = json.Marshal(map[string]any{"mcpServers": map[string]any{"same_server": map[string]any{"type": "http", "url": endpoint.URL, "headers": map[string]string{"Authorization": "Bearer " + current.secret}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(remotemcp.DevOriginsEnv, strings.Join(origins, ","))
	var mu sync.Mutex
	steps := map[string]int{}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model    string
			Messages []struct{ Role, Content string }
			Tools    []struct {
				Function struct{ Name, Description string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var prompt strings.Builder
		for _, message := range input.Messages {
			prompt.WriteString(message.Content)
		}
		var owner *run
		for i := range runs {
			if strings.Contains(prompt.String(), runs[i].marker) {
				if owner != nil {
					t.Error("concurrent model history crossed workspace boundary")
				}
				owner = &runs[i]
			}
			if strings.Contains(prompt.String(), runs[i].secret) || strings.Contains(prompt.String(), runs[i].task.RemoteMCPDaemonToken) {
				t.Error("run credential reached model history")
			}
		}
		if owner == nil || input.Model != "fixture-model" {
			t.Error("missing run identity or fixed model")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		step := steps[owner.marker]
		steps[owner.marker]++
		mu.Unlock()
		if step == 0 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
		}
		name, args := "read_file", map[string]any{"path": "private.txt"}
		switch step {
		case 1:
			if last := input.Messages[len(input.Messages)-1]; last.Role != "tool" || last.Content != owner.marker {
				t.Error("file read crossed work directory")
			}
			name = ""
			for _, tool := range input.Tools {
				if strings.Contains(tool.Function.Description, "same_server / fixture.write") {
					name = tool.Function.Name
				}
			}
			if name == "" {
				t.Error("MCP tool missing")
			}
			args = map[string]any{"value": owner.marker}
		case 2:
			name, args = "apply_patch", map[string]any{"path": "private.txt", "old_text": owner.marker, "new_text": owner.marker + " updated"}
		}
		delta, finish := map[string]any{"content": owner.marker + " completed"}, "stop"
		if step < 3 {
			arguments, _ := json.Marshal(args)
			delta, finish = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("same-call-%d", step), "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}}, "tool_calls"
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer upstream.Close()
	defer cancel()
	t.Setenv("MULTICA_LLM_API_KEY", "fixture-upstream")
	t.Setenv("MULTICA_LLM_BASE_URL", upstream.URL)
	t.Setenv("MULTICA_LLM_DEFAULT_MODEL", "fixture-model")
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{LLMCapabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 131072, MaxOutputTokens: 2048, MaxToolArgumentBytes: 65536, MaxTools: 64}})
	api := httptest.NewServer(router)
	defer api.Close()
	defer cancel()
	results := make([]chan agent.Result, len(runs))
	journal := t.TempDir()
	for i := range runs {
		current := &runs[i]
		backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), current.task, daemon.NativeBackendOptions{WorkDir: current.dir, JournalDir: journal, MCPConfig: current.config, Permissions: tools.Permissions{WriteFiles: true}})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBackend()
		session, err := backend.Execute(ctx, current.marker, agent.ExecOptions{Cwd: current.dir})
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan agent.Result, 1)
		results[i] = result
		go func() {
			for range session.Messages {
			}
			result <- <-session.Result
		}()
	}
	for range runs {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("model requests did not overlap")
		}
	}
	close(release)
	sessionIDs := map[string]bool{}
	for i, resultCh := range results {
		var result agent.Result
		select {
		case result = <-resultCh:
		case <-ctx.Done():
			t.Fatal("concurrent execution did not finish")
		}
		current := &runs[i]
		if result.Status != "completed" || result.Output != current.marker+" completed" || result.SessionID == "" || sessionIDs[result.SessionID] {
			t.Fatalf("concurrent run lost identity: %+v", result)
		}
		sessionIDs[result.SessionID] = true
		data, err := os.ReadFile(filepath.Join(current.dir, "private.txt"))
		if err != nil || string(data) != current.marker+" updated" {
			t.Fatalf("isolated file mismatch: %q error=%v", data, err)
		}
		if writes := current.mcp.Writes(); len(writes) != 1 || writes[0] != current.marker {
			t.Fatalf("MCP write crossed run boundary: %v", writes)
		}
		var checkpoint string
		if err := testPool.QueryRow(ctx, "SELECT checkpoint::text FROM native_session WHERE workspace_id=$1 AND agent_id=$2 AND id=$3", current.task.WorkspaceID, current.task.AgentID, result.SessionID).Scan(&checkpoint); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(checkpoint, current.marker) || strings.Contains(checkpoint, runs[1-i].marker) || strings.Contains(checkpoint, current.secret) {
			t.Error("durable history crossed workspace or credential boundary")
		}
	}
}
