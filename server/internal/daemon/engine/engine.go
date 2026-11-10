// Package engine runs a single native agent loop using durable model/tool steps.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type Model interface {
	Complete(context.Context, llm.Request, agentstate.Session, func(llm.Event) error) (*llm.Response, error)
}
type Store interface {
	Acquire(context.Context, string) (agentstate.Session, error)
	Save(context.Context, agentstate.Session) (agentstate.Session, error)
	Renew(context.Context, agentstate.Session) error
	Release(context.Context, agentstate.Session) error
	Compact(context.Context, agentstate.Session) (agentstate.Session, error)
}
type ToolHost interface {
	Definitions() []llm.Tool
	Execute(context.Context, llm.ToolCall) (string, error)
	IsReadOnly(llm.ToolCall) bool
}

type Config struct {
	Model              string
	TaskID             string
	Capabilities       llm.Capabilities
	MaxSteps           int
	SystemInstructions string
	InputContext       string
	InputAttachments   []llm.AttachmentRef
}

type Backend struct {
	model Model
	store Store
	tools ToolHost
	cfg   Config
}

func New(model Model, store Store, tools ToolHost, cfg Config) *Backend {
	return &Backend{model: model, store: store, tools: tools, cfg: cfg}
}

func (b *Backend) Execute(ctx context.Context, prompt string, opts agent.ExecOptions) (*agent.Session, error) {
	opts.SystemPrompt += b.cfg.SystemInstructions
	prompt = b.cfg.InputContext + prompt
	if err := b.cfg.Capabilities.Validate(); err != nil {
		return nil, err
	}
	if b.cfg.Model == "" || b.model == nil || b.store == nil || b.tools == nil {
		return nil, errors.New("native execution dependencies are missing")
	}
	if opts.Model != "" && opts.Model != b.cfg.Model {
		return nil, errors.New("native execution cannot override the deployment model")
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		return b.start(ctx, cancel, prompt, opts), nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return b.start(ctx, cancel, prompt, opts), nil
}

func (b *Backend) start(ctx context.Context, cancel context.CancelFunc, prompt string, opts agent.ExecOptions) *agent.Session {
	messages := make(chan agent.Message, 64)
	results := make(chan agent.Result, 1)
	var terminal atomic.Bool
	inputs := &supplements{}
	go func() {
		defer cancel()
		defer inputs.close()
		start := time.Now()
		result := b.run(ctx, prompt, opts, func(m agent.Message) error {
			select {
			case messages <- m:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, func() { terminal.Store(true) }, inputs)
		result.DurationMs = time.Since(start).Milliseconds()
		terminal.Store(true)
		close(messages)
		results <- result
		close(results)
	}()
	session := &agent.Session{Messages: messages, Result: results, TerminalObserved: terminal.Load}
	if opts.EnableTaskSupplement {
		session.SupplementWithID = inputs.offer
		session.SupplementReady = inputs.accepts
	}
	return session
}

func (b *Backend) run(ctx context.Context, prompt string, opts agent.ExecOptions, emit func(agent.Message) error, observeTerminal func(), inputs *supplements) (result agent.Result) {
	result.Status = "failed"
	fail := func(err error) agent.Result {
		result.Error = err.Error()
		if ctx.Err() != nil {
			result.Status = "cancelled"
		}
		return result
	}
	session, err := b.store.Acquire(ctx, opts.Cwd)
	if err != nil {
		return fail(err)
	}
	result.SessionID = session.ID
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var mu sync.Mutex // Serializes renew/save network operations and versions.
	lease := agentstate.Session{ID: session.ID, Version: session.Version, LeaseToken: session.LeaseToken}
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				err := b.store.Renew(ctx, lease)
				mu.Unlock()
				if err != nil {
					cancel(fmt.Errorf("native session lease lost: %w", err))
					return
				}
			}
		}
	}()
	defer func() {
		inputs.close()
		cancel(nil)
		<-leaseDone
		releaseCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		// The durable outcome is already decided. A failed release leaves a
		// bounded lease to expire; it must not reclassify a completed run.
		_ = b.store.Release(releaseCtx, session)
	}()
	defer observeTerminal()
	save := func() error {
		mu.Lock()
		defer mu.Unlock()
		// Save must still stage an observed tool result locally after
		// cancellation. The cancelled context prevents a new network step.
		next, err := b.store.Save(ctx, session)
		if err != nil {
			return fmt.Errorf("native checkpoint was not confirmed: %w", err)
		}
		session = next
		lease.Version = next.Version
		return nil
	}
	if err := emit(agent.Message{Type: agent.MessageStatus, Status: "running", SessionID: session.ID}); err != nil {
		return fail(err)
	}
	if err := session.Checkpoint.Validate(b.cfg.Capabilities.MaxToolArgumentBytes); err != nil {
		return fail(err)
	}
	// A managed shell admission is already the durable effect of its tool call.
	// Recover its receipt before classifying unobserved operations as unknown.
	// This path performs no execution and cannot reopen an unknown write.
	if recovery, ok := b.tools.(interface {
		RecoverTool(context.Context, llm.ToolCall) (string, bool, error)
	}); ok {
		for i := range session.Checkpoint.Tools {
			record := session.Checkpoint.Tools[i]
			if record.State != agentstate.ToolStarted {
				continue
			}
			output, found, err := recovery.RecoverTool(agentstate.WithWriteClaim(ctx, session, record.Call.ID), record.Call)
			if err != nil {
				return fail(err)
			}
			if !found {
				continue
			}
			if err := session.Checkpoint.Tools[i].Finish(output, false); err != nil {
				return fail(err)
			}
			if err := save(); err != nil {
				return fail(err)
			}
		}
	}
	session.Checkpoint.Recover()
	if err := save(); err != nil {
		return fail(err)
	}
	if session.Checkpoint.CloseInterruptedGroup() {
		if err := save(); err != nil {
			return fail(err)
		}
	}
	// Resume any wholly planned tool group before introducing the new prompt.
	// Never lose the new input by saving it between calls and their results.
	runTools := func() error {
		for i := range session.Checkpoint.Tools {
			record := &session.Checkpoint.Tools[i]
			if record.State == agentstate.ToolSucceeded || record.State == agentstate.ToolFailed {
				continue
			}
			if err := record.Start(); err != nil {
				return err
			}
			call := record.Call
			if err := save(); err != nil {
				return err
			} // Durable intent precedes execution.
			var input map[string]any
			_ = json.Unmarshal(call.Arguments, &input)
			if err := emit(agent.Message{Type: agent.MessageToolUse, Tool: call.Name, CallID: call.ID, Input: input}); err != nil {
				return err
			}
			output, toolErr := b.tools.Execute(agentstate.WithWriteClaim(ctx, session, call.ID), call)
			// An interrupted command may already have changed external state.
			// Preserve uncertainty before giving the model a chance to investigate.
			if toolErr != nil && (ctx.Err() != nil || errors.Is(toolErr, tools.ErrOutcomeUnknown) || errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded)) {
				session.Checkpoint.Tools[i].State = agentstate.ToolUnknown
				if err := save(); err != nil {
					return err
				}
				if ctx.Err() != nil || errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded) {
					return errors.New("native tool interrupted; outcome is unknown and requires reconciliation")
				}
				// Platform receipts can establish the outcome without executing
				// the write again. The server alone commits that transition.
				if receipts, ok := b.store.(interface {
					ReconcileToolOutcomes(context.Context, agentstate.Session) (agentstate.Session, error)
				}); ok {
					next, err := receipts.ReconcileToolOutcomes(ctx, session)
					if err != nil {
						return err
					}
					mu.Lock()
					session = next
					lease.Version = next.Version
					mu.Unlock()
					known := session.Checkpoint.Tools[i]
					if known.Result != nil && (known.State == agentstate.ToolSucceeded || known.State == agentstate.ToolFailed) {
						if err := emit(agent.Message{Type: agent.MessageToolResult, Tool: call.Name, CallID: call.ID, Output: *known.Result}); err != nil {
							return err
						}
						continue
					}
				}
				unknownOutput := agentstate.InterruptedResult(session.Checkpoint.Tools[i])
				session.Checkpoint.CloseInterruptedGroup()
				if err := save(); err != nil {
					return err
				}
				return emit(agent.Message{Type: agent.MessageToolResult, Tool: call.Name, CallID: call.ID, Output: unknownOutput})
			}
			if toolErr != nil {
				if output != "" {
					output += "\n"
				}
				output += "Tool error: " + toolErr.Error()
			}
			if len(output) > 128<<10 {
				output = output[:128<<10] + "\n[tool result truncated]"
			}
			// Save may replace the checkpoint allocation, so reacquire the record.
			if err := session.Checkpoint.Tools[i].Finish(output, toolErr != nil); err != nil {
				return err
			}
			if err := save(); err != nil {
				return err
			}
			if err := emit(agent.Message{Type: agent.MessageToolResult, Tool: call.Name, CallID: call.ID, Output: output}); err != nil {
				return err
			}
		}
		for _, record := range session.Checkpoint.Tools {
			if record.Result == nil {
				return errors.New("native tool result is missing")
			}
			session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "tool", ToolCallID: record.Call.ID, Content: *record.Result})
		}
		session.Checkpoint.Tools = nil
		return save()
	}
	if len(session.Checkpoint.Tools) > 0 {
		if err := runTools(); err != nil {
			return fail(err)
		}
	}
	inputAlreadySaved := b.cfg.TaskID != "" && session.Checkpoint.InputTaskID == b.cfg.TaskID
	if inputAlreadySaved && len(session.Checkpoint.Messages) > 0 {
		last := session.Checkpoint.Messages[len(session.Checkpoint.Messages)-1]
		if last.Role == "assistant" && len(last.ToolCalls) == 0 {
			result.Status = "completed"
			result.Output = last.Content
			return result
		}
	}
	if !inputAlreadySaved {
		lastSystem := ""
		for _, message := range session.Checkpoint.Messages {
			if message.Role == "system" {
				lastSystem = message.Content
			}
		}
		if opts.SystemPrompt != "" && opts.SystemPrompt != lastSystem {
			session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "system", Content: opts.SystemPrompt})
		}
		session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "user", Content: prompt, Attachments: b.cfg.InputAttachments})
		session.Checkpoint.InputTaskID = b.cfg.TaskID
		if err := save(); err != nil {
			return fail(err)
		}
	}
	steps := b.cfg.MaxSteps
	inputs.enable()
	acceptInput := func(final bool) (bool, error) {
		input := inputs.take(final)
		if input == nil {
			return false, nil
		}
		for _, id := range session.Checkpoint.SupplementIDs {
			if id == input.id {
				input.done <- nil
				return false, nil
			}
		}
		if err := input.ctx.Err(); err != nil {
			input.done <- err
			return false, nil
		}
		session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "user", Content: input.text})
		session.Checkpoint.SupplementIDs = append(session.Checkpoint.SupplementIDs, input.id)
		err := save()
		input.done <- err
		return true, err
	}
	if steps <= 0 {
		steps = 32
	}
	pendingJobReminder := false
	collectNotices := func(completedStep *llm.ModelMessage) (bool, error) {
		host, ok := b.tools.(interface {
			JobNotifications(context.Context, []llm.ModelMessage) ([]string, error)
		})
		if !ok {
			return false, nil
		}
		notices, err := host.JobNotifications(agentstate.WithWriteClaim(ctx, session, ""), session.Checkpoint.Messages)
		if err != nil {
			return false, err
		}
		if len(notices) > 0 && completedStep != nil {
			session.Checkpoint.Messages = append(session.Checkpoint.Messages, *completedStep)
		}
		for _, notice := range notices {
			session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "user", Content: notice})
		}
		if len(notices) > 0 {
			return true, save()
		}
		return false, nil
	}
	var contextMeasure contextBudget
	for range steps {
		if _, err := acceptInput(false); err != nil {
			return fail(err)
		}
		if _, err := collectNotices(nil); err != nil {
			return fail(err)
		}
		switch repeatedReadAction(session.Checkpoint.Messages, b.tools) {
		case progressStop:
			return fail(errors.New("native task made no progress: repeated unchanged reads after runtime feedback; report missing data or permissions before retrying"))
		case progressWarn:
			instructions := ""
			for _, message := range session.Checkpoint.Messages {
				if message.Role == "system" {
					instructions, _, _ = strings.Cut(message.Content, "\n\n"+noProgressNotice)
				}
			}
			session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "system", Content: instructions + "\n\n" + noProgressNotice})
			if err := save(); err != nil {
				return fail(err)
			}
		}
		request := llm.Request{Messages: session.Checkpoint.ModelMessages(), Tools: b.tools.Definitions(), MaxOutputTokens: b.cfg.Capabilities.MaxOutputTokens}
		// Use measured provider usage for an unchanged prefix, with byte-sized
		// reservations for new input and headroom for output/protocol overhead.
		raw, err := json.Marshal(request)
		if err != nil {
			return fail(err)
		}
		budget := b.cfg.Capabilities.ContextTokens - request.MaxOutputTokens - 1024
		for attempts := 0; contextMeasure.estimate(request) > budget*4/5; attempts++ {
			if attempts >= 4 {
				return fail(errors.New("native context cannot fit after compaction; reduce input or tool inventory"))
			}
			// Compact performs a model request; renewals must continue while it
			// runs. Only updating the returned version needs serialization.
			previousSize := len(raw)
			next, compactErr := b.store.Compact(ctx, session)
			mu.Lock()
			if compactErr == nil {
				session = next
				lease.Version = next.Version
			}
			mu.Unlock()
			if compactErr != nil {
				return fail(fmt.Errorf("native context compaction failed; original history is preserved: %w", compactErr))
			}
			request.Messages = session.Checkpoint.ModelMessages()
			raw, err = json.Marshal(request)
			if err != nil {
				return fail(err)
			}
			if len(raw) >= previousSize && contextMeasure.estimate(request) > budget*4/5 {
				return fail(errors.New("native context compaction did not reduce request size; original durable history is preserved"))
			}
		}
		mu.Lock()
		err = b.store.Renew(ctx, lease)
		mu.Unlock()
		if err != nil {
			return fail(err)
		}
		response, err := b.model.Complete(ctx, request, lease, func(event llm.Event) error { return emit(agent.Message{Type: agent.MessageText, Content: event.Text}) })
		if err != nil {
			return fail(err)
		}
		if response == nil || response.Message.Role != "assistant" {
			return fail(errors.New("native model returned an invalid step"))
		}
		contextMeasure.observe(request, response.Usage)
		if response.Usage != nil {
			if result.Usage == nil {
				result.Usage = make(map[string]agent.TokenUsage)
			}
			u := result.Usage[b.cfg.Model]
			u.InputTokens += response.Usage.InputTokens - response.Usage.CachedTokens
			u.CacheReadTokens += response.Usage.CachedTokens
			u.OutputTokens += response.Usage.OutputTokens
			result.Usage[b.cfg.Model] = u
		}
		if len(response.Message.ToolCalls) == 0 {
			// A job may have settled during inference. Deliver its committed
			// completion before accepting an answer produced without that fact.
			if added, err := collectNotices(&response.Message); err != nil {
				return fail(err)
			} else if added {
				continue
			}
			if jobs, ok := b.tools.(interface {
				PendingJobs(context.Context) ([]string, error)
			}); ok {
				ids, err := jobs.PendingJobs(agentstate.WithWriteClaim(ctx, session, ""))
				if err != nil {
					return fail(err)
				}
				if len(ids) > 0 {
					if pendingJobReminder {
						return fail(errors.New("native model tried to finish with local jobs still running; collect or stop jobs before completing"))
					}
					pendingJobReminder = true
					notice := "Local jobs are still running: " + strings.Join(ids, ", ") + ". Use job_output with wait_ms=30000 to collect results or job_kill to stop them before completing. Ending this task stops its local jobs. Do not relaunch commands."
					session.Checkpoint.Messages = append(session.Checkpoint.Messages, response.Message, llm.ModelMessage{Role: "user", Content: notice})
					if err := save(); err != nil {
						return fail(err)
					}
					continue
				}
			}
		}
		session.Checkpoint.Messages = append(session.Checkpoint.Messages, response.Message)
		for _, call := range response.Message.ToolCalls {
			session.Checkpoint.Tools = append(session.Checkpoint.Tools, agentstate.ToolRecord{Call: call, State: agentstate.ToolPlanned})
		}
		if err := save(); err != nil {
			return fail(err)
		}
		if len(response.Message.ToolCalls) == 0 {
			accepted, err := acceptInput(true)
			if err != nil {
				return fail(err)
			}
			if accepted {
				continue
			}
			result.Status = "completed"
			result.Output = response.Message.Content
			return result
		}
		if err := runTools(); err != nil {
			return fail(err)
		}
	}
	return fail(errors.New("native model step limit reached"))
}
