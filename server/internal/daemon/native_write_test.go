package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeRecoveryUsesConfirmedBusinessReceiptWithoutRepeatingWrite(t *testing.T) {
	for _, known := range []bool{true, false} {
		for _, initial := range []agentstate.ToolState{agentstate.ToolStarted, agentstate.ToolUnknown} {
			t.Run(map[bool]string{true: "confirmed", false: "unknown"}[known]+"/"+string(initial), func(t *testing.T) {
				call := llm.ToolCall{ID: "create-once", Name: "multica_create_issue", Arguments: json.RawMessage(`{"title":"Capture"}`)}
				current := agentstate.Session{ID: uuid.NewString(), LeaseToken: uuid.NewString(), Version: 3, Checkpoint: agentstate.Checkpoint{InputTaskID: "run", Messages: []llm.ModelMessage{{Role: "user", Content: "Capture"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolStarted}}}}
				current.Checkpoint.Tools[0].State = initial
				reads := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/session"):
						_ = json.NewEncoder(w).Encode(current)
					case strings.HasSuffix(r.URL.Path, "/write-result"):
						reads++
						result := agentstate.WriteResult{Known: known}
						if known {
							result.Status, result.Body = http.StatusCreated, json.RawMessage(`{"id":"created"}`)
							current.Checkpoint.Tools[0].State = agentstate.ToolSucceeded
							output := `{"id":"created"}`
							current.Checkpoint.Tools[0].Result = &output
							current.Version++
							result.Session = &current
						}
						_ = json.NewEncoder(w).Encode(result)
					default:
						t.Errorf("recovery attempted a business write: %s", r.URL)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer srv.Close()
				store := newNativeStore(NewClient(srv.URL), Task{ID: "run"})
				store.journalDir = t.TempDir()
				session, err := store.Acquire(context.Background(), t.TempDir())
				if err != nil || reads != 1 {
					t.Fatalf("recovery failed: reads=%d error=%v", reads, err)
				}
				if known {
					if session.Version != 4 || session.Checkpoint.Tools[0].State != agentstate.ToolSucceeded || *session.Checkpoint.Tools[0].Result != `{"id":"created"}` {
						t.Fatalf("receipt not saved: %+v", session.Checkpoint)
					}
				} else {
					if session.Version != 3 || session.Checkpoint.Tools[0].State != initial {
						t.Fatalf("unknown operation was promoted: %+v", session.Checkpoint)
					}
					session.Checkpoint.Recover()
					if session.Checkpoint.Tools[0].State != agentstate.ToolUnknown {
						t.Fatal("unconfirmed operation became replayable")
					}
				}
			})
		}
	}
}
