package agentstate

import (
	"encoding/json"

	"github.com/multica-ai/multica/server/pkg/llm"
)

// InterruptedResult preserves uncertainty rather than inventing an execution
// outcome. Its exact bytes are validated when closing an interrupted group.
func InterruptedResult(record ToolRecord) string {
	code, guidance := "TOOL_OUTCOME_UNKNOWN", "The call started but no result was durably confirmed. Do not replay it blindly. Inspect external state first; retry only read-only or demonstrably idempotent work. If effects cannot be verified, report the uncertainty and ask the user."
	if record.State == ToolPlanned {
		code, guidance = "TOOL_NOT_STARTED", "This call was not started because its tool group was interrupted. Re-evaluate it after verifying the preceding operation; it has not been executed automatically."
	}
	raw, _ := json.Marshal(struct {
		Code     string `json:"code"`
		CallID   string `json:"call_id"`
		Tool     string `json:"tool"`
		Replayed bool   `json:"replayed"`
		Guidance string `json:"guidance"`
	}{code, record.Call.ID, record.Call.Name, false, guidance})
	return string(raw)
}

func hasUnknown(records []ToolRecord) bool {
	for _, record := range records {
		if record.State == ToolUnknown {
			return true
		}
	}
	return false
}

// CloseInterruptedGroup balances the transcript without changing an unknown
// operation into a terminal outcome or executing any unstarted remainder.
// The append-only transcript retains each call and its explicit uncertainty.
func (c *Checkpoint) CloseInterruptedGroup() bool {
	if !hasUnknown(c.Tools) {
		return false
	}
	for _, record := range c.Tools {
		if record.State == ToolStarted {
			return false
		}
	}
	for _, record := range c.Tools {
		content := InterruptedResult(record)
		if record.Result != nil {
			content = *record.Result
		}
		c.Messages = append(c.Messages, llm.ModelMessage{Role: "tool", ToolCallID: record.Call.ID, Content: content})
	}
	c.Tools = nil
	return true
}
