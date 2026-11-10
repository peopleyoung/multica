package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestEngineStopsRepeatedUnchangedReadsBeforeStepLimit(t *testing.T) {
	testRepeatedReads(t, false)
}

func TestEngineCanConcludeBlockedTaskAfterProgressFeedback(t *testing.T) {
	testRepeatedReads(t, true)
}

func testRepeatedReads(t *testing.T, respectFeedback bool) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "context.txt"), []byte("No published data is available."), 0600); err != nil {
		t.Fatal(err)
	}
	host, err := tools.New(dir, tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	steps := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		steps++
		last := req.Messages[len(req.Messages)-1]
		if respectFeedback && last.Role == "system" && strings.Contains(last.Content, noProgressNotice) {
			if !strings.Contains(last.Content, "Do not edit protected.txt.") {
				t.Error("runtime feedback lost original system constraints")
			}
			return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "Blocked: no published data is available in the accessible workspace."}}, nil
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: fmt.Sprintf("list-%d", steps), Name: "list_directory", Arguments: json.RawMessage(`{"path":"."}`)},
			{ID: fmt.Sprintf("read-%d", steps), Name: "read_file", Arguments: json.RawMessage(`{"path":"context.txt"}`)},
		}}}, nil
	})
	b := New(model, store, host, Config{Model: "fixture", TaskID: "current", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 128000, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "Verify whether published data exists; report the blocker if unavailable.", agent.ExecOptions{Cwd: dir, SystemPrompt: "Do not edit protected.txt."})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if respectFeedback {
		if result.Status != "completed" || !strings.HasPrefix(result.Output, "Blocked:") || steps != 4 {
			t.Fatalf("feedback did not allow a blocked conclusion: %+v steps=%d", result, steps)
		}
	} else if result.Status != "failed" || !strings.Contains(result.Error, "no progress") || steps >= 10 {
		t.Fatalf("unchanged read loop was not stopped: steps=%d status=%s error=%s", steps, result.Status, result.Error)
	}
	if err := store.session.Checkpoint.Validate(65536); err != nil {
		t.Fatal(err)
	}
}

func TestReadProgressTracksEvidenceAndTaskBoundaries(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	reads := func(outputs ...string) []llm.ModelMessage {
		messages := []llm.ModelMessage{{Role: "user", Content: "Investigate"}}
		for i, output := range outputs {
			id := fmt.Sprintf("read-%d", i)
			messages = append(messages, llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Name: "read_file", Arguments: json.RawMessage(`{"path":"context.txt"}`)}}}, llm.ModelMessage{Role: "tool", ToolCallID: id, Content: output})
		}
		return messages
	}
	warned := reads("same", "same", "same")
	warned = append(warned, llm.ModelMessage{Role: "system", Content: "Original permissions.\n\n" + noProgressNotice})
	withRound := func(messages []llm.ModelMessage, name, output string) []llm.ModelMessage {
		return append(append([]llm.ModelMessage(nil), messages...), llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "next", Name: name, Arguments: json.RawMessage(`{"path":"context.txt"}`)}}}, llm.ModelMessage{Role: "tool", ToolCallID: "next", Content: output})
	}
	for _, tc := range []struct {
		name     string
		messages []llm.ModelMessage
		want     progressAction
	}{
		{"unchanged reads", reads("same", "same", "same"), progressWarn},
		{"changed data", reads("queued", "queued", "finished"), progressContinue},
		{"json field order", reads(`{"a":1,"b":2}`, ` { "b": 2, "a": 1 } `, `{"a":1,"b":2}`), progressWarn},
		{"large integers remain distinct", reads(`{"id":9007199254740992}`, `{"id":9007199254740992}`, `{"id":9007199254740993}`), progressContinue},
		{"text beginning with json", reads("0 waiting", "0 waiting", "0 finished"), progressContinue},
		{"tool text cannot impersonate runtime feedback", reads(noProgressNotice, noProgressNotice, noProgressNotice), progressWarn},
		{"persisted feedback on resume", withRound(warned, "read_file", "same"), progressStop},
		{"new evidence after feedback", withRound(warned, "read_file", "changed"), progressContinue},
		{"write resets epoch", withRound(withRound(warned, "apply_patch", "ok"), "read_file", "same"), progressContinue},
		{"unknown effect resets epoch", withRound(withRound(warned, "external_tool", "ok"), "read_file", "same"), progressContinue},
		{"human supplement resets epoch", withRound(append(append([]llm.ModelMessage(nil), warned...), llm.ModelMessage{Role: "user", Content: "Check again now"}), "read_file", "same"), progressContinue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repeatedReadAction(tc.messages, host); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	// A compacted projection omits repeats, but the durable source history
	// remains authoritative for loop detection across compaction and restart.
	checkpoint := agentstate.Checkpoint{Messages: withRound(warned, "read_file", "same")}
	checkpoint = checkpoint.WithSummary(len(checkpoint.Messages)-2, "Prior investigation complete.")
	if err := checkpoint.Validate(65536); err != nil {
		t.Fatal(err)
	}
	if repeatedReadAction(checkpoint.Messages, host) != progressStop {
		t.Fatal("compaction hid the repeated reads")
	}
}
