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

type memoryStore struct {
	session     agentstate.Session
	failStarted bool
}

func (s *memoryStore) Acquire(context.Context, string) (agentstate.Session, error) {
	return clone(s.session), nil
}
func (s *memoryStore) Save(_ context.Context, next agentstate.Session) (agentstate.Session, error) {
	if next.Version != s.session.Version {
		return agentstate.Session{}, errors.New("version conflict")
	}
	if err := agentstate.ValidateTransition(s.session.Checkpoint, next.Checkpoint, 65536); err != nil {
		return agentstate.Session{}, err
	}
	if s.failStarted && len(next.Checkpoint.Tools) > 0 && next.Checkpoint.Tools[0].State == agentstate.ToolStarted {
		return agentstate.Session{}, errors.New("storage unavailable")
	}
	next.Version++
	next.Checkpoint.Version = next.Version
	s.session = clone(next)
	return clone(next), nil
}
func (s *memoryStore) Renew(context.Context, agentstate.Session) error   { return nil }
func (s *memoryStore) Release(context.Context, agentstate.Session) error { return nil }
func (s *memoryStore) Compact(context.Context, agentstate.Session) (agentstate.Session, error) {
	return agentstate.Session{}, errors.New("fixture compaction unavailable")
}
func clone(s agentstate.Session) agentstate.Session {
	raw, _ := json.Marshal(s)
	var next agentstate.Session
	_ = json.Unmarshal(raw, &next)
	next.Checkpoint.Version = next.Version
	return next
}

type modelFunc func(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error)

func (f modelFunc) Complete(ctx context.Context, req llm.Request, lease agentstate.Session, emit func(llm.Event) error) (*llm.Response, error) {
	return f(ctx, req, lease, emit)
}

type interruptedHost struct {
	ToolHost
	file string
}

type compactStore struct {
	*memoryStore
	compactions int
}

type unchangedCompactStore struct {
	*memoryStore
	compactions int
}

func (s *unchangedCompactStore) Compact(_ context.Context, next agentstate.Session) (agentstate.Session, error) {
	s.compactions++
	return next, nil
}

func TestEngineStopsIneffectiveCompactionBeforeAnotherModelCall(t *testing.T) {
	host, err := tools.New(t.TempDir(), tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &unchangedCompactStore{memoryStore: &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{Messages: []llm.ModelMessage{{Role: "user", Content: strings.Repeat("x", 25000)}}}}}}
	model := modelFunc(func(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error) {
		t.Fatal("oversized context reached inference")
		return nil, nil
	})
	b := New(model, store, host, Config{TaskID: "root", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "continue", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "failed" || !strings.Contains(result.Error, "did not reduce") || store.compactions != 1 {
		t.Fatalf("ineffective summaries repeated: %+v calls=%d", result, store.compactions)
	}
	if store.session.Checkpoint.Messages[0].Content != strings.Repeat("x", 25000) {
		t.Fatal("compaction erased durable history")
	}
}

func (s *compactStore) Compact(_ context.Context, next agentstate.Session) (agentstate.Session, error) {
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}
	_, cut, err := next.Checkpoint.SummaryInput(caps)
	if err != nil {
		return agentstate.Session{}, err
	}
	next.Checkpoint = next.Checkpoint.WithSummary(cut, "Earlier read-only investigation completed. Continue the current objective.")
	next.Version++
	next.Checkpoint.Version = next.Version
	s.session = clone(next)
	s.compactions++
	return clone(next), nil
}

func TestEngineCompactsBeforeInferenceAndRetainsDurableHistory(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &compactStore{memoryStore: &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{Messages: []llm.ModelMessage{{Role: "system", Content: "permissions"}, {Role: "user", Content: "earlier objective"}, {Role: "assistant", Content: strings.Repeat("x", 24000)}}}}}}
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		if store.compactions != 1 || req.Messages[len(req.Messages)-1].Content != "current objective" {
			t.Error("inference missed compaction or current request")
		}
		for _, message := range req.Messages {
			if len(message.Content) > 1000 {
				t.Error("uncompacted history reached model")
			}
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "done"}}, nil
	})
	b := New(model, store, host, Config{Model: "fixture", TaskID: "current", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "current objective", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" {
		t.Fatal(result)
	}
	if len(store.session.Checkpoint.Messages) != 5 || len(store.session.Checkpoint.Messages[2].Content) != 24000 {
		t.Fatal("audit history erased during compaction")
	}
}

func (h interruptedHost) Execute(context.Context, llm.ToolCall) (string, error) {
	if err := os.WriteFile(h.file, []byte("partial side effect"), 0600); err != nil {
		return "", err
	}
	return "", context.DeadlineExceeded
}

func TestEngineInterruptedToolCanBeInvestigatedOnRetry(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	steps := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		steps++
		if steps == 1 {
			return &llm.Response{Message: llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "write", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","old_text":"","new_text":"write"}`)}}}}, nil
		}
		found := false
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "TOOL_OUTCOME_UNKNOWN") {
				found = true
			}
		}
		if !found {
			t.Error("lost interrupted outcome")
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "Need to inspect the partial side effect."}}, nil
	})
	b := New(model, store, interruptedHost{ToolHost: host, file: filepath.Join(dir, "a.txt")}, Config{TaskID: "retry-root", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	for i := 0; i < 2; i++ {
		session, err := b.Execute(context.Background(), "write", agent.ExecOptions{Cwd: dir})
		if err != nil {
			t.Fatal(err)
		}
		for range session.Messages {
		}
		result := <-session.Result
		if i == 0 && (result.Status != "failed" || len(store.session.Checkpoint.Tools) != 1 || store.session.Checkpoint.Tools[0].State != agentstate.ToolUnknown) {
			t.Fatalf("lost unknown result: %+v", result)
		}
		if i == 1 && (result.Status != "completed" || steps != 2 || len(store.session.Checkpoint.Tools) != 0) {
			t.Fatalf("investigation blocked: %+v", result)
		}
	}
}

func TestEngineShellWaitYieldsAndResumesOneCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture command")
	}
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	steps := 0
	var jobID string
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		steps++
		message := llm.ModelMessage{Role: "assistant"}
		switch steps {
		case 1:
			message.ToolCalls = []llm.ToolCall{{ID: "long-shell", Name: "shell", Arguments: json.RawMessage(`{"command":"printf x >> launched.txt; sleep 0.3; printf finished","wait_ms":1}`)}}
		case 2:
			var result map[string]any
			if err := json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &result); err != nil {
				t.Fatal(err)
			}
			if result["status"] != "running" {
				t.Fatal("wait expiry killed the command")
			}
			jobID, _ = result["job_id"].(string)
			args, _ := json.Marshal(map[string]any{"job_id": jobID, "wait_ms": 2000})
			message.ToolCalls = []llm.ToolCall{{ID: "collect", Name: "job_output", Arguments: args}}
		case 3:
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, `"status":"completed"`) {
				t.Fatal("missing job settlement")
			}
			message.Content = "done"
		default:
			t.Fatal("resumed completed task reran the model")
		}
		return &llm.Response{Message: message}, nil
	})
	b := New(model, store, host, Config{TaskID: "root", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	for range 2 {
		session, err := b.Execute(context.Background(), "Run the command.", agent.ExecOptions{Cwd: dir})
		if err != nil {
			t.Fatal(err)
		}
		for range session.Messages {
		}
		if result := <-session.Result; result.Status != "completed" {
			t.Fatalf("job loop failed: %+v", result)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "launched.txt")); err != nil || string(raw) != "x" {
		t.Fatal("command side effect repeated")
	}
}

func TestEngineRestoresStartedShellReceiptWithoutReexecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir, journal := t.TempDir(), t.TempDir()
	host, err := tools.New(dir, tools.Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "launch", Name: "shell", Arguments: json.RawMessage(`{"command":"printf x >> launched; printf result","wait_ms":2000}`)}
	initial := agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{InputTaskID: "root", Messages: []llm.ModelMessage{{Role: "user", Content: "run once"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolStarted}}}}
	if _, err := host.Execute(agentstate.WithWriteClaim(context.Background(), initial, call.ID), call); err != nil {
		t.Fatal(err)
	}
	_ = host.Close()
	host, err = tools.New(dir, tools.Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: initial}
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		if !strings.Contains(req.Messages[len(req.Messages)-1].Content, `"output":"result"`) {
			t.Fatal("recovered job not paired with model call")
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "done"}}, nil
	})
	b := New(model, store, host, Config{TaskID: "root", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "run once", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" {
		t.Fatalf("receipt recovery failed: %+v", result)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "launched")); err != nil || string(raw) != "x" {
		t.Fatal("recovery repeated a shell effect")
	}
}

func TestEngineKeepsTaskOpenForPendingLocalJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	step := 0
	var id string
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		step++
		m := llm.ModelMessage{Role: "assistant"}
		switch step {
		case 1:
			m.ToolCalls = []llm.ToolCall{{ID: "launch", Name: "shell", Arguments: json.RawMessage(`{"command":"sleep 0.4; printf completed","run_in_background":true}`)}}
		case 2:
			var result map[string]any
			_ = json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &result)
			id, _ = result["job_id"].(string)
			m.Content = "all done"
		case 3:
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, id) {
				t.Fatal("no durable reminder for pending job")
			}
			args, _ := json.Marshal(map[string]any{"job_id": id, "wait_ms": 2000})
			m.ToolCalls = []llm.ToolCall{{ID: "poll", Name: "job_output", Arguments: args}}
		case 4:
			m.Content = "collected"
		default:
			t.Fatal("unexpected model step")
		}
		return &llm.Response{Message: m}, nil
	})
	b := New(model, store, host, Config{TaskID: "root", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "run and collect", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || result.Output != "collected" || step != 4 {
		t.Fatalf("task finished before owned work: %+v steps=%d", result, step)
	}
}

func TestEngineOperatorVerifiedOutcomeContinuesNewInput(t *testing.T) {
	host, err := tools.New(t.TempDir(), tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	call := llm.ToolCall{ID: "legacy-shell", Name: "shell", Arguments: json.RawMessage(`{"command":"must never be replayed"}`)}
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{InputTaskID: "old-task", Messages: []llm.ModelMessage{{Role: "user", Content: "prior task"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolUnknown}}}}}
	steps := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		steps++
		if req.Messages[len(req.Messages)-1].Content != "new human input" {
			t.Fatal("lost new task input")
		}
		found := false
		for _, m := range req.Messages {
			if m.Role == "tool" && m.ToolCallID == call.ID && strings.Contains(m.Content, "operator_verified") {
				found = true
			}
		}
		if !found {
			t.Fatal("lost verified prior outcome")
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "continue from verified state"}}, nil
	})
	b := New(model, store, host, Config{TaskID: "new-task", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	run := func() agent.Result {
		t.Helper()
		s, err := b.Execute(context.Background(), "new human input", agent.ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for range s.Messages {
		}
		return <-s.Result
	}
	next, err := store.session.Checkpoint.ReconcileShell(call.ID, "failed", "operator", "Remote job has exited; its log confirms missing weights.", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	store.session.Checkpoint = next
	store.session.Version++
	if result := run(); result.Status != "completed" || steps != 1 || len(store.session.Checkpoint.Tools) != 0 {
		t.Fatalf("verified session still blocked: %+v", result)
	}
}

func TestEngineReadWriteAndResume(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	host, err := tools.New(dir, tools.Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	step := 0
	model := modelFunc(func(_ context.Context, req llm.Request, lease agentstate.Session, emit func(llm.Event) error) (*llm.Response, error) {
		step++
		response := &llm.Response{Message: llm.ModelMessage{Role: "assistant"}}
		switch step {
		case 1:
			response.Message.ToolCalls = []llm.ToolCall{{ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)}}
		case 2:
			if last := req.Messages[len(req.Messages)-1]; last.Role != "tool" || last.ToolCallID != "read-1" || last.Content != "before" {
				t.Fatalf("unpaired read result: %+v", last)
			}
			response.Message.ToolCalls = []llm.ToolCall{{ID: "write-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","old_text":"before","new_text":"after"}`)}}
		case 3:
			response.Message.Content = "Updated."
			if err := emit(llm.Event{Text: "Updated."}); err != nil {
				return nil, err
			}
		case 4:
			if len(req.Messages) < 7 || req.Messages[len(req.Messages)-2].Content != "Updated." {
				t.Fatal("conversation history lost on resume")
			}
			response.Message.Content = "I remember."
		default:
			t.Fatal("unexpected extra model request")
		}
		return response, nil
	})
	cfg := Config{Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}}
	backend := New(model, store, host, cfg)
	run := func(prompt string) agent.Result {
		session, err := backend.Execute(context.Background(), prompt, agent.ExecOptions{Cwd: dir, SystemPrompt: "Use the available tools."})
		if err != nil {
			t.Fatal(err)
		}
		for range session.Messages {
		}
		return <-session.Result
	}
	first := run("Update the file.")
	if first.Status != "completed" || first.Output != "Updated." {
		t.Fatalf("%+v", first)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(data) != "after" {
		t.Fatalf("file=%q error=%v", data, err)
	}
	second := run("Do you remember?")
	if second.Status != "completed" || second.Output != "I remember." {
		t.Fatalf("%+v", second)
	}
}

func TestEngineDoesNotExecuteWithoutDurableIntent(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}, failStarted: true}
	model := modelFunc(func(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error) {
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "write-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","old_text":"","new_text":"must not run"}`)}}}}, nil
	})
	backend := New(model, store, host, Config{Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := backend.Execute(context.Background(), "write", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "failed" {
		t.Fatalf("%+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("tool executed without confirmed durable intent")
	}
}

func TestEngineReusesCompletedRunWithoutRepeatingInputOrInference(t *testing.T) {
	host, err := tools.New(t.TempDir(), tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{
		InputTaskID: "same-run", Messages: []llm.ModelMessage{{Role: "user", Content: "original"}, {Role: "assistant", Content: "Already completed."}},
	}}}
	model := modelFunc(func(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error) {
		t.Error("completed run requested another model step")
		return nil, errors.New("unexpected inference")
	})
	b := New(model, store, host, Config{TaskID: "same-run", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "original", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || result.Output != "Already completed." || len(store.session.Checkpoint.Messages) != 2 {
		t.Fatalf("duplicate run changed history: %+v", result)
	}
}

func TestEngineNeverReplaysAnUnconfirmedWrite(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	call := llm.ToolCall{ID: "write-once", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","old_text":"","new_text":"must not replay"}`)}
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease", Checkpoint: agentstate.Checkpoint{
		InputTaskID: "interrupted", Messages: []llm.ModelMessage{{Role: "user", Content: "write"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}},
		Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolStarted}},
	}}}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("already written before crash"), 0600); err != nil {
		t.Fatal(err)
	}
	model := modelFunc(func(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error) {
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "Verify external state before another write."}}, nil
	})
	b := New(model, store, host, Config{TaskID: "retry", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "retry", agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" || len(store.session.Checkpoint.Tools) != 0 || !strings.Contains(store.session.Checkpoint.Messages[2].Content, "TOOL_OUTCOME_UNKNOWN") {
		t.Fatalf("unknown operation not preserved: %+v", result)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(data) != "already written before crash" {
		t.Fatal("unconfirmed write was replayed")
	}
}

type cleanupStore struct {
	*memoryStore
	started chan struct{}
	finish  chan struct{}
}

func (s *cleanupStore) Release(ctx context.Context, _ agentstate.Session) error {
	close(s.started)
	select {
	case <-s.finish:
		return errors.New("release unavailable")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestEnginePublishesTerminalBeforeCleanup(t *testing.T) {
	host, err := tools.New(t.TempDir(), tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &cleanupStore{memoryStore: &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}, started: make(chan struct{}), finish: make(chan struct{})}
	model := modelFunc(func(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error) {
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "done"}}, nil
	})
	b := New(model, store, host, Config{Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), "finish", agent.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.started:
		if !session.TerminalObserved() {
			t.Error("terminal outcome was hidden behind cleanup")
		}
	case <-time.After(3 * time.Second):
		t.Error("cleanup did not start")
	}
	close(store.finish)
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || result.Output != "done" {
		t.Fatalf("cleanup reclassified durable result: %+v", result)
	}
}

func TestEngineUsesProviderMeasurementAcrossSteps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("observed"), 0600); err != nil {
		t.Fatal(err)
	}
	host, err := tools.New(dir, tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	store := &unchangedCompactStore{memoryStore: &memoryStore{session: agentstate.Session{ID: "measured-session", LeaseToken: "lease"}}}
	calls := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		calls++
		if calls == 1 {
			return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: strings.Repeat("analysis ", 1250), ToolCalls: []llm.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"marker"}`)}}}, Usage: &llm.Usage{InputTokens: 2000, OutputTokens: 2000}}, nil
		}
		raw, _ := json.Marshal(req)
		if len(raw) < 19000 {
			t.Fatal("fixture did not cross old byte threshold")
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "finished"}, Usage: &llm.Usage{InputTokens: 4100}}, nil
	})
	b := New(model, store, host, Config{TaskID: "measured", Model: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 8192, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := b.Execute(context.Background(), strings.Repeat("objective ", 1000), agent.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" || store.compactions != 0 || calls != 2 {
		t.Fatalf("premature compaction status=%s compactions=%d calls=%d error=%s", result.Status, store.compactions, calls, result.Error)
	}
}
