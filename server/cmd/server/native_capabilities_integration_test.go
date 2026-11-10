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
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

// A fake CVAT endpoint and model exercise the real router, durable native loop,
// task authorization, external credentials, script output and comment delivery.
func TestNativeExternalDataToScriptArtifactAndRequestedReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	machine, token, taskToken := "native-capabilities-"+uuid.NewString(), "mdt_"+uuid.NewString(), "mat_"+uuid.NewString()
	runtimeID := fx.Runtime(t, "native capability integration", testutil.Cols{"provider": "native", "daemon_id": machine})
	externalCalls := 0
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalCalls++
		if r.Header.Get("Authorization") != "Token fixture-cvat-secret" || r.Method != "GET" || r.URL.Path != "/api/tasks" {
			t.Error("external service request was not scoped and authenticated")
		}
		_, _ = w.Write([]byte(`{"count":1,"results":[{"id":17,"name":"verified dataset"}]}`))
	}))
	defer external.Close()
	cfg := nativeruntime.Config{WriteFiles: true, Shell: true, Services: []nativeruntime.Service{{Name: "cvat", BaseURL: external.URL + "/api", TokenEnv: "CVAT_TOKEN", AuthScheme: "Token"}}}
	configRaw, _ := json.Marshal(cfg)
	agentID := fx.Agent(t, "native capability integration", runtimeID, testutil.Cols{"runtime_config": string(configRaw)})
	issueID := fx.Issue(t, "Verify external data and produce a report")
	threadID := fx.Comment(t, issueID, "Reply here with the report.")
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	taskID := fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "trigger_comment_id": threadID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
	fx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	fx.Insert(t, "task_token", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": testUserID, "agent_id": agentID, "task_id": taskID, "token_hash": auth.HashToken(taskToken), "expires_at": time.Now().Add(time.Hour)})
	fx.Cleanup(t, "DELETE FROM native_session WHERE agent_id=$1", agentID)
	fx.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1 AND source_task_id=$2", issueID, taskID)
	step := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []struct{ Role, Content string }
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Error("bad model input")
			w.WriteHeader(400)
			return
		}
		for _, m := range input.Messages {
			if strings.Contains(m.Content, "fixture-cvat-secret") {
				t.Error("external credential reached the model")
			}
		}
		step++
		delta, finish := map[string]any{}, "tool_calls"
		call := func(name string, args any) {
			raw, _ := json.Marshal(args)
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": fmt.Sprintf("capability-%d", step), "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}}
		}
		switch step {
		case 1:
			call("native_runtime_info", map[string]any{})
		case 2:
			if !strings.Contains(input.Messages[len(input.Messages)-1].Content, `"shell":true`) {
				t.Error("effective diagnostics missing")
			}
			call("http_request", map[string]any{"service": "cvat", "method": "GET", "path": "tasks?page_size=1"})
		case 3:
			if !strings.Contains(input.Messages[len(input.Messages)-1].Content, "verified dataset") {
				t.Error("external evidence missing")
			}
			call("make_directory", map[string]any{"path": "artifacts/reports"})
		case 4:
			call("apply_patch", map[string]any{"path": "artifacts/verify.sh", "old_text": "", "new_text": "printf 'dataset 17 verified\\n' > artifacts/reports/result.txt\n"})
		case 5:
			call("shell", map[string]any{"command": "/bin/sh artifacts/verify.sh"})
		case 6:
			call("read_file", map[string]any{"path": "artifacts/reports/result.txt"})
		case 7:
			if input.Messages[len(input.Messages)-1].Content != "dataset 17 verified\n" {
				t.Error("script output was not verified")
			}
			call("multica_create_comment", map[string]any{"issue_id": issueID, "content": "Verified dataset 17; generated artifacts/reports/result.txt."})
		case 8:
			if strings.HasPrefix(input.Messages[len(input.Messages)-1].Content, "Tool error:") {
				t.Error("comment delivery failed")
			}
			delta["content"], finish = "Verified dataset 17 and delivered the report.", "stop"
		default:
			t.Error("unexpected model repeat")
			delta["content"], finish = "stop", "stop"
		}
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
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
	task := daemon.Task{ID: taskID, NativeInputTaskID: taskID, WorkspaceID: testWorkspaceID, AgentID: agentID, RuntimeID: runtimeID, IssueID: issueID, TriggerCommentID: threadID, Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), RemoteMCPDaemonToken: token, AuthToken: taskToken, Agent: &daemon.AgentData{RuntimeConfig: configRaw}}
	backend, closeBackend, err := daemon.NewNativeBackend(ctx, daemon.NewClient(api.URL), task, daemon.NativeBackendOptions{WorkDir: dir, JournalDir: t.TempDir(), Permissions: tools.Permissions{WriteFiles: true, Shell: true}, HTTPOrigins: []string{external.URL}, ServiceEnv: map[string]string{"CVAT_TOKEN": "fixture-cvat-secret"}, Env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBackend()
	session, err := backend.Execute(ctx, "Verify the external dataset, run the verification script and reply in the requested thread.", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || step != 8 || externalCalls != 1 {
		t.Fatalf("capability workflow failed: status=%s error=%s steps=%d external_calls=%d", result.Status, result.Error, step, externalCalls)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "artifacts/reports/result.txt"))
	if err != nil || string(raw) != "dataset 17 verified\n" {
		t.Fatal("verified artifact missing")
	}
	if count := fx.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1 AND parent_id=$2 AND source_task_id=$3", issueID, threadID, taskID); count != 1 {
		t.Fatalf("report delivery count=%d", count)
	}
}
