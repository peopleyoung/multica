// Package agentstate defines durable native model steps and tool history.
package agentstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/multica-ai/multica/server/pkg/llm"
)

type ToolState string

const (
	ToolPlanned   ToolState = "planned"
	ToolStarted   ToolState = "started"
	ToolSucceeded ToolState = "succeeded"
	ToolFailed    ToolState = "failed"
	ToolUnknown   ToolState = "unknown"
)

// ToolRecord is saved before execution begins. Started means a side effect may
// have happened; recovery never treats it as permission to invoke again. A
// result pointer distinguishes a valid empty tool result from a missing one.
type ToolRecord struct {
	Call   llm.ToolCall `json:"call"`
	State  ToolState    `json:"state"`
	Result *string      `json:"result,omitempty"`
}

// Checkpoint is authoritative model history, distinct from display messages.
// The repository will compare Version under an execution lease; a local copy
// alone never authorizes new side effects. Scope and leases belong to storage.
type Checkpoint struct {
	Version       int64              `json:"-"`
	InputTaskID   string             `json:"input_task_id,omitempty"`
	Messages      []llm.ModelMessage `json:"messages"`
	Tools         []ToolRecord       `json:"tools,omitempty"`
	Summary       *ContextSummary    `json:"summary,omitempty"`
	SupplementIDs []string           `json:"supplement_ids,omitempty"`
}

// MatchesMessages compares model inputs to authoritative history despite jsonb
// whitespace/key normalization, without weakening integer identity.
func (c Checkpoint) MatchesMessages(messages []llm.ModelMessage) bool {
	return sameJSON(c.ModelMessages(), messages)
}

// Validate checks that every outstanding model call has exactly one durable
// operation record. Tool result messages are added as a complete group after
// all records finish, so model steps never observe half a tool group.
func (c Checkpoint) Validate(maxArgumentBytes int) error {
	seenInputs := make(map[string]bool, len(c.SupplementIDs))
	for _, id := range c.SupplementIDs {
		if id == "" || len(id) > 200 || seenInputs[id] {
			return errors.New("agentstate: additional input identities must be nonempty and unique")
		}
		seenInputs[id] = true
	}
	if err := c.validateSummary(maxArgumentBytes); err != nil {
		return err
	}
	if c.Version < 0 {
		return errors.New("engine: negative checkpoint version")
	}
	if len(c.Messages) == 0 && len(c.Tools) == 0 {
		return nil
	}
	if err := llm.ValidateHistory(c.Messages, maxArgumentBytes, true); err != nil {
		return err
	}
	pending := make(map[string]llm.ToolCall)
	for _, m := range c.Messages {
		for _, call := range m.ToolCalls {
			pending[call.ID] = call
		}
		if m.Role == "tool" {
			delete(pending, m.ToolCallID)
		}
	}
	for _, record := range c.Tools {
		call, ok := pending[record.Call.ID]
		if !ok || !sameJSON(call, record.Call) {
			return errors.New("engine: tool record does not match an outstanding model call")
		}
		delete(pending, record.Call.ID)
		switch record.State {
		case ToolPlanned, ToolStarted, ToolUnknown:
			if record.Result != nil {
				return errors.New("engine: unfinished operation has a result")
			}
		case ToolSucceeded, ToolFailed:
			if record.Result == nil {
				return errors.New("engine: finished operation is missing its result")
			}
		default:
			return fmt.Errorf("engine: unsupported tool state %q", record.State)
		}
	}
	if len(pending) != 0 {
		return errors.New("engine: outstanding tool calls lack durable operation records")
	}
	if len(c.Tools) > 0 && c.Messages[len(c.Messages)-1].Role != "assistant" {
		return errors.New("agentstate: checkpoint must preserve a complete outstanding tool group")
	}
	return nil
}

// ValidateTransition permits append-only history and forward-only operation
// states. A lease holder cannot turn an uncertain/completed write into a newly
// planned call by saving an older checkpoint under a newer version.
func ValidateTransition(before, after Checkpoint, maxArgumentBytes int) error {
	if len(after.SupplementIDs) < len(before.SupplementIDs) || !sameJSON(before.SupplementIDs, after.SupplementIDs[:len(before.SupplementIDs)]) && len(before.SupplementIDs) > 0 {
		return errors.New("agentstate: accepted additional inputs cannot be removed")
	}
	if len(after.SupplementIDs) > len(before.SupplementIDs) && (len(after.SupplementIDs) != len(before.SupplementIDs)+1 || len(after.Messages) != len(before.Messages)+1 || after.Messages[len(after.Messages)-1].Role != "user" || len(after.Tools) > 0 || after.InputTaskID != before.InputTaskID) {
		return errors.New("agentstate: additional input requires a complete user message")
	}
	if !sameJSON(before.Summary, after.Summary) {
		return errors.New("agentstate: summaries require server-authorized compaction")
	}
	if err := before.Validate(maxArgumentBytes); err != nil {
		return err
	}
	if err := after.Validate(maxArgumentBytes); err != nil {
		return err
	}
	if len(after.Messages) < len(before.Messages) || !sameJSON(before.Messages, after.Messages[:len(before.Messages)]) {
		// Empty nil and empty slices describe the same initial history.
		if len(before.Messages) != 0 {
			return errors.New("agentstate: model history cannot be rewritten")
		}
	}
	if len(after.Messages) == len(before.Messages) {
		if before.InputTaskID != after.InputTaskID {
			return errors.New("agentstate: input identity requires an appended user message")
		}
		if len(before.Tools) != len(after.Tools) {
			return errors.New("agentstate: tool operations cannot be replaced")
		}
		for i, old := range before.Tools {
			next := after.Tools[i]
			if !sameJSON(old.Call, next.Call) {
				return errors.New("agentstate: tool identity cannot change")
			}
			if sameJSON(old, next) {
				continue
			}
			allowed := old.State == ToolPlanned && next.State == ToolStarted || old.State == ToolStarted && (next.State == ToolUnknown || next.State == ToolSucceeded || next.State == ToolFailed)
			if !allowed {
				return errors.New("agentstate: invalid tool state transition")
			}
		}
		return nil
	}
	added := after.Messages[len(before.Messages):]
	if before.InputTaskID != after.InputTaskID && (after.InputTaskID == "" || added[len(added)-1].Role != "user" || len(after.Tools) != 0) {
		return errors.New("agentstate: new input must end in an ordinary user message")
	}
	if len(before.Tools) > 0 {
		if len(after.Tools) != 0 || len(added) != len(before.Tools) {
			return errors.New("agentstate: tool results must be appended as a complete group")
		}
		for i, record := range before.Tools {
			content := ""
			switch {
			case (record.State == ToolSucceeded || record.State == ToolFailed) && record.Result != nil:
				content = *record.Result
			case hasUnknown(before.Tools) && (record.State == ToolUnknown || record.State == ToolPlanned):
				content = InterruptedResult(record)
			default:
				return errors.New("agentstate: tool result must match its durable outcome")
			}
			if added[i].Role != "tool" || added[i].ToolCallID != record.Call.ID || added[i].Content != content {
				return errors.New("agentstate: tool result must match its durable outcome")
			}
		}
		return nil
	}
	for _, m := range added {
		if m.Role != "user" && m.Role != "system" && m.Role != "assistant" {
			return errors.New("agentstate: unexpected appended message")
		}
	}
	for _, record := range after.Tools {
		if record.State != ToolPlanned {
			return errors.New("agentstate: new operations must start planned")
		}
	}
	return nil
}

// PostgreSQL jsonb normalizes object key order and whitespace. Compare parsed
// JSON while preserving exact numbers; float64 would hide large integer edits.
func sameJSON(a, b any) bool {
	decode := func(v any) (any, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var out any
		err = dec.Decode(&out)
		return out, err
	}
	left, err := decode(a)
	if err != nil {
		return false
	}
	right, err := decode(b)
	return err == nil && reflect.DeepEqual(left, right)
}

// Recover marks operations whose completion was not saved as unknown. Only
// planned operations may be invoked. An interrupted group can be closed with
// explicit unknown results so the model can investigate without replaying it.
func (c *Checkpoint) Recover() {
	for i := range c.Tools {
		if c.Tools[i].State == ToolStarted {
			c.Tools[i].State = ToolUnknown
		}
	}
}

func (r *ToolRecord) Start() error {
	if r.State != ToolPlanned || r.Result != nil {
		return errors.New("engine: only a planned tool operation can start")
	}
	r.State = ToolStarted
	return nil
}

func (r *ToolRecord) Finish(result string, failed bool) error {
	if r.State != ToolStarted {
		return errors.New("engine: only a started tool operation can finish")
	}
	r.State = ToolSucceeded
	if failed {
		r.State = ToolFailed
	}
	r.Result = &result
	return nil
}
