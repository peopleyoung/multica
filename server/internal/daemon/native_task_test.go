package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeRunTaskPreparesAndCompletesWithoutAIExecutable(t *testing.T) {
	checkpoint := agentstate.Session{ID: uuid.NewString()}
	modelCalls, starts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/daemon/native/config":
			_ = json.NewEncoder(w).Encode(llm.Deployment{Configured: true, Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 32}})
		case strings.HasSuffix(r.URL.Path, "/native/session"):
			var req agentstate.SessionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			checkpoint.LeaseToken = req.LeaseToken
			_ = json.NewEncoder(w).Encode(checkpoint)
		case strings.HasSuffix(r.URL.Path, "/native/checkpoint/save"):
			var req agentstate.CheckpointRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if err := agentstate.ValidateTransition(checkpoint.Checkpoint, req.Session.Checkpoint, 65536); err != nil {
				t.Error(err)
			}
			checkpoint = req.Session
			checkpoint.Version++
			checkpoint.Checkpoint.Version = checkpoint.Version
			_ = json.NewEncoder(w).Encode(checkpoint)
		case strings.HasSuffix(r.URL.Path, "/native/checkpoint/renew"):
			_ = json.NewEncoder(w).Encode(checkpoint)
		case strings.HasSuffix(r.URL.Path, "/native/checkpoint/release"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/native/inference"):
			modelCalls++
			var req llm.NativeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.SessionID != checkpoint.ID || req.SessionVersion != checkpoint.Version || !checkpoint.Checkpoint.MatchesMessages(req.Input.Messages) {
				t.Error("inference was not bound to the durable session")
			}
			if len(req.Input.Messages) < 2 || req.Input.Messages[0].Role != "system" || !strings.Contains(req.Input.Messages[0].Content, "native-agent-instructions-regression") {
				t.Error("platform brief was not delivered inline")
			}
			for _, tool := range req.Input.Tools {
				if tool.Name == "shell" || tool.Name == "apply_patch" || tool.Name == "make_directory" {
					t.Error("host limits granted powers without agent opt-in")
				}
			}
			if !strings.Contains(req.Input.Messages[0].Content, "Shell execution: disabled") || !strings.Contains(req.Input.Messages[0].Content, "Local file writes: disabled") {
				t.Error("actual runtime permissions were not delivered to the model")
			}
			if len(req.Input.Messages) >= 2 {
				refs := req.Input.Messages[1].Attachments
				if len(refs) != 2 || refs[0].ID != "selected-image" || refs[1].ID != "cloned-image" {
					t.Errorf("quick-create media did not reach durable model input: %+v", refs)
				}
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(llm.Frame{Event: &llm.Event{Text: "done"}})
			_ = json.NewEncoder(w).Encode(llm.Frame{Response: &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "done"}}})
		default:
			if strings.HasSuffix(r.URL.Path, "/start") {
				starts++
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"test-native-rt": {ID: "test-native-rt", Provider: "native"}},
		cfg: Config{NativeRuntime: true, NativeAllowWrite: true, NativeAllowShell: true, WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{"native": {Path: "/missing/native-has-no-executable"}},
		},
	}
	task := Task{ID: "test-native-task", NativeInputTaskID: "test-native-task", WorkspaceID: "test-native-ws", RuntimeID: "test-native-rt", AgentID: "test-native-agent", AuthToken: "mat_test_native", RemoteMCPDaemonToken: "mdt_test_native", Agent: &AgentData{ID: "test-native-agent", Name: "test", Instructions: "native-agent-instructions-regression"},
		QuickCreatePrompt: "Create an issue from these images", QuickCreateAttachmentIDs: []string{"selected-image"},
		QuickCreateSourceContext: json.RawMessage(`{"source_issue":{"attachments":[]},"comment_thread":[{"attachments":[{"id":"cloned-image","source_attachment_id":"mutable-original","content_type":"image/png"}]}]}`),
	}
	result, err := d.runTask(context.Background(), task, "native", 0, logger)
	if err != nil || result.Status != "completed" || result.Comment != "done" || modelCalls != 1 || starts != 1 {
		t.Fatalf("native run failed: result=%+v err=%v modelCalls=%d starts=%d", result, err, modelCalls, starts)
	}
}
