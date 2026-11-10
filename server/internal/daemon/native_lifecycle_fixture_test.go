package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// newNativeLifecycleFixture exercises environment lifecycle tests through the
// native checkpoint and inference protocol without launching an AI executable.
func newNativeLifecycleFixture(t *testing.T, shellCommands ...string) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	checkpoint := agentstate.Session{ID: uuid.NewString()}
	commandIndex := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/daemon/native/config":
			// Environment tests need room for the full tool catalog and issue
			// workflow; context-budget behavior is covered by engine tests.
			_ = json.NewEncoder(w).Encode(llm.Deployment{Configured: true, Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 65536, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 64}})
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
			w.Header().Set("Content-Type", "application/x-ndjson")
			message := llm.ModelMessage{Role: "assistant", Content: "done"}
			if commandIndex < len(shellCommands) {
				args, _ := json.Marshal(map[string]string{"command": shellCommands[commandIndex]})
				message = llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: uuid.NewString(), Name: "shell", Arguments: args}}}
				commandIndex++
			}
			_ = json.NewEncoder(w).Encode(llm.Frame{Response: &llm.Response{Message: message}})
		default:
			_, _ = w.Write([]byte("{}"))
		}
	}
}
