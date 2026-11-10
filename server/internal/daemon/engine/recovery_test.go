package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestUnknownGroupContinuesInvestigationWithoutReplayingOrDispatchingRemainder(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	calls := []llm.ToolCall{
		{ID: "uncertain", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"old","old_text":"","new_text":"must not replay"}`)},
		{ID: "not-started", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"later","old_text":"","new_text":"must not start"}`)},
	}
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{
		Messages: []llm.ModelMessage{{Role: "user", Content: "old request"}, {Role: "assistant", ToolCalls: calls}},
		Tools:    []agentstate.ToolRecord{{Call: calls[0], State: agentstate.ToolUnknown}, {Call: calls[1], State: agentstate.ToolPlanned}},
	}}}
	steps := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		steps++
		if len(req.Messages) < 5 || !strings.Contains(req.Messages[2].Content, "TOOL_OUTCOME_UNKNOWN") || !strings.Contains(req.Messages[3].Content, "TOOL_NOT_STARTED") || req.Messages[len(req.Messages)-1].Content != "inspect state" {
			return nil, errors.New("missing recovery facts or current input")
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "External state needs verification before any new write."}}, nil
	})
	b := New(model, store, host, Config{TaskID: "new", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	s, err := b.Execute(context.Background(), "inspect state", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range s.Messages {
	}
	if result := <-s.Result; result.Status != "completed" || steps != 1 {
		t.Fatalf("investigation blocked: %+v steps=%d", result, steps)
	}
	for _, name := range []string{"old", "later"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("recovery executed an old operation")
		}
	}
}

func TestCompletionDuringInferenceIsLoggedBeforeFinishing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{Shell: true}, os.Environ(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	steps := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		steps++
		if steps == 1 {
			return &llm.Response{Message: llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "start", Name: "shell", Arguments: json.RawMessage(`{"command":"while [ ! -f gate ]; do sleep 0.01; done; printf done","run_in_background":true}`)}}}}, nil
		}
		if steps == 2 {
			if err := os.WriteFile(filepath.Join(dir, "gate"), []byte("release"), 0600); err != nil {
				return nil, err
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				ids, err := host.PendingJobs(agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "session"}, ""))
				if err != nil {
					return nil, err
				}
				if len(ids) == 0 {
					break
				}
				if time.Now().After(deadline) {
					return nil, errors.New("fixture job did not settle")
				}
				time.Sleep(5 * time.Millisecond)
			}
			return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "premature answer"}}, nil
		}
		found := false
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, "[native job completed ") {
				found = true
			}
		}
		if !found {
			t.Error("model missed completion notice")
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "observed completion"}}, nil
	})
	b := New(model, store, host, Config{TaskID: "task", Model: "fixture", MaxSteps: 4, Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	s, err := b.Execute(context.Background(), "work", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range s.Messages {
	}
	if result := <-s.Result; result.Status != "completed" || steps != 3 {
		t.Fatalf("completion not integrated: %+v steps=%d", result, steps)
	}
	count, retained := 0, false
	for _, m := range store.session.Checkpoint.Messages {
		if strings.HasPrefix(m.Content, "[native job completed ") {
			count++
		}
		if m.Content == "premature answer" {
			retained = true
		}
	}
	if count != 1 || !retained {
		t.Fatal("completion duplicated or prior model response lost")
	}
}
