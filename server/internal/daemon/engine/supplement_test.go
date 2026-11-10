package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestSupplementIsSavedBeforeAcceptance(t *testing.T) {
	dir := t.TempDir()
	host, err := tools.New(dir, tools.Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}
	entered, release := make(chan struct{}), make(chan struct{})
	step := 0
	model := modelFunc(func(_ context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
		step++
		if step == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		} else {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "user" || last.Content != "additional objective" || len(store.session.Checkpoint.SupplementIDs) != 1 {
				t.Error("additional input was not durably accepted")
			}
		}
		return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "result"}}, nil
	})
	backend := New(model, store, host, Config{Model: "fixture", TaskID: "task", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
	session, err := backend.Execute(ctx, "original objective", agent.ExecOptions{Cwd: dir, EnableTaskSupplement: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	accepted := make(chan error, 1)
	go func() { accepted <- session.SupplementWithID(ctx, "comment-1", "additional objective") }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for session.SupplementReady() {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline.C:
			t.Fatal("input was not queued")
		}
	}
	select {
	case <-accepted:
		t.Fatal("accepted input before checkpoint")
	default:
	}
	close(release)
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	if result := <-session.Result; result.Status != "completed" {
		t.Fatal(result)
	}
	if session.SupplementReady() {
		t.Fatal("completed turn accepts input")
	}
	if err := session.SupplementWithID(ctx, "late", "late guidance"); err == nil {
		t.Fatal("late input accepted")
	}
	count := 0
	for _, m := range store.session.Checkpoint.Messages {
		if strings.Contains(m.Content, "additional objective") {
			count++
		}
	}
	if count != 1 || step != 2 {
		t.Fatalf("inputs=%d steps=%d", count, step)
	}
}

type supplementStore struct {
	*memoryStore
	beforeSave func(context.Context) error
}

func (s *supplementStore) Save(ctx context.Context, next agentstate.Session) (agentstate.Session, error) {
	if len(next.Checkpoint.SupplementIDs) > len(s.session.Checkpoint.SupplementIDs) && s.beforeSave != nil {
		if err := s.beforeSave(ctx); err != nil {
			return agentstate.Session{}, err
		}
	}
	return s.memoryStore.Save(ctx, next)
}

type supplementToolHost struct {
	ToolHost
	entered, release chan struct{}
}

func (h supplementToolHost) Execute(ctx context.Context, _ llm.ToolCall) (string, error) {
	close(h.entered)
	select {
	case <-h.release:
		return "tool result", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestSupplementSafeStepAndFailureSemantics(t *testing.T) {
	for _, mode := range []string{"tool group", "checkpoint failure", "cancel pending", "cancel during commit", "duplicate after recovery"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dir := t.TempDir()
			host, err := tools.New(dir, tools.Permissions{}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			store := &supplementStore{memoryStore: &memoryStore{session: agentstate.Session{ID: "session", LeaseToken: "lease"}}}
			if mode == "checkpoint failure" {
				store.beforeSave = func(context.Context) error { return errors.New("storage unavailable") }
			}
			commitEntered, commitRelease := make(chan struct{}), make(chan struct{})
			if mode == "cancel during commit" {
				store.beforeSave = func(ctx context.Context) error {
					close(commitEntered)
					select {
					case <-commitRelease:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			if mode == "duplicate after recovery" {
				store.session.Checkpoint = agentstate.Checkpoint{InputTaskID: "task", SupplementIDs: []string{"comment-1"}, Messages: []llm.ModelMessage{{Role: "user", Content: "original objective"}, {Role: "user", Content: "additional objective"}}}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var toolHost ToolHost = host
			if mode == "tool group" {
				toolHost = supplementToolHost{ToolHost: host, entered: entered, release: release}
			}
			steps := 0
			model := modelFunc(func(ctx context.Context, req llm.Request, _ agentstate.Session, _ func(llm.Event) error) (*llm.Response, error) {
				steps++
				if steps == 1 {
					if mode == "tool group" {
						return &llm.Response{Message: llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"file"}`)}}}}, nil
					}
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				} else {
					last := req.Messages[len(req.Messages)-1]
					if last.Role != "user" || last.Content != "additional objective" || len(store.session.Checkpoint.SupplementIDs) != 1 || len(store.session.Checkpoint.Tools) != 0 {
						t.Error("additional input not saved at a complete tool step")
					}
					if mode == "tool group" && req.Messages[len(req.Messages)-2].ToolCallID != "read-1" {
						t.Error("input split a tool group")
					}
				}
				return &llm.Response{Message: llm.ModelMessage{Role: "assistant", Content: "result"}}, nil
			})
			backend := New(model, store, toolHost, Config{Model: "fixture", TaskID: "task", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 65536, MaxTools: 10}})
			session, err := backend.Execute(ctx, "original objective", agent.ExecOptions{Cwd: dir, EnableTaskSupplement: true})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			inputCtx, cancelInput := context.WithCancel(ctx)
			defer cancelInput()
			accepted := make(chan error, 1)
			go func() { accepted <- session.SupplementWithID(inputCtx, "comment-1", "additional objective") }()
			for session.SupplementReady() {
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case err := <-accepted:
				t.Fatalf("accepted before safe step: %v", err)
			default:
			}
			if mode == "cancel pending" {
				cancelInput()
				if err := <-accepted; !errors.Is(err, context.Canceled) {
					t.Fatalf("pending cancellation: %v", err)
				}
			}
			close(release)
			if mode == "cancel during commit" {
				select {
				case <-commitEntered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				cancelInput()
				close(commitRelease)
			}
			if mode != "cancel pending" {
				err := <-accepted
				if (err != nil) != (mode == "checkpoint failure") {
					t.Fatalf("acceptance error: %v", err)
				}
			}
			for range session.Messages {
			}
			result := <-session.Result
			wantStatus, wantSteps, wantInputs := "completed", 2, 1
			if mode == "checkpoint failure" {
				wantStatus, wantSteps, wantInputs = "failed", 1, 0
			} else if mode == "cancel pending" {
				wantSteps, wantInputs = 1, 0
			} else if mode == "duplicate after recovery" {
				wantSteps = 1
			}
			if result.Status != wantStatus || steps != wantSteps || len(store.session.Checkpoint.SupplementIDs) != wantInputs {
				t.Fatalf("result=%+v steps=%d inputs=%v", result, steps, store.session.Checkpoint.SupplementIDs)
			}
		})
	}
}
