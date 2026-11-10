package agentstate

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestRecoveryDoesNotReplayUncertainOperation(t *testing.T) {
	call := llm.ToolCall{ID: "write-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt"}`)}
	c := Checkpoint{Messages: []llm.ModelMessage{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []ToolRecord{{Call: call, State: ToolPlanned}}}
	if err := c.Validate(1024); err != nil {
		t.Fatal(err)
	}
	if err := c.Tools[0].Start(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after a write but before its result checkpoint.
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var restored Checkpoint
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Recover()
	if err := restored.Validate(1024); err != nil {
		t.Fatal(err)
	}
	if restored.Tools[0].State != ToolUnknown || restored.Tools[0].Start() == nil || restored.Tools[0].Finish("late", false) == nil {
		t.Fatal("an uncertain operation must require reconciliation")
	}
}

func TestCheckpointRejectsUnpairedRecords(t *testing.T) {
	call := llm.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{}`)}
	for _, tc := range []struct {
		name    string
		records []ToolRecord
	}{
		{"missing", nil},
		{"duplicate", []ToolRecord{{Call: call, State: ToolPlanned}, {Call: call, State: ToolPlanned}}},
		{"missing result", []ToolRecord{{Call: call, State: ToolSucceeded}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Checkpoint{Messages: []llm.ModelMessage{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: tc.records}
			if c.Validate(1024) == nil {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
	r := ToolRecord{Call: call, State: ToolPlanned}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish("", false); err != nil {
		t.Fatal(err)
	}
	c := Checkpoint{Messages: []llm.ModelMessage{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []ToolRecord{r}}
	if err := c.Validate(1024); err != nil {
		t.Fatal(err)
	}
	if r.Start() == nil {
		t.Fatal("completed operation can be replayed")
	}
}

func TestCheckpointTransitionPreservesJSONMeaning(t *testing.T) {
	before := Checkpoint{Messages: []llm.ModelMessage{{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "one", Name: "patch", Arguments: json.RawMessage(`{"a":1,"b":9007199254740993}`)}}}}, Tools: []ToolRecord{{Call: llm.ToolCall{ID: "one", Name: "patch", Arguments: json.RawMessage(`{"a":1,"b":9007199254740993}`)}, State: ToolPlanned}}}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var after Checkpoint
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	after.Messages[0].ToolCalls[0].Arguments = json.RawMessage(`{ "b": 9007199254740993, "a": 1 }`)
	after.Tools[0].Call.Arguments = json.RawMessage(`{ "b": 9007199254740993, "a": 1 }`)
	after.Tools[0].State = ToolStarted
	if err := ValidateTransition(before, after, 1024); err != nil {
		t.Fatal(err)
	}
	after.Tools[0].Call.Arguments = json.RawMessage(`{"a":1,"b":9007199254740992}`)
	after.Messages[0].ToolCalls[0].Arguments = after.Tools[0].Call.Arguments
	if ValidateTransition(before, after, 1024) == nil {
		t.Fatal("changed large integer was accepted as unchanged history")
	}
}
