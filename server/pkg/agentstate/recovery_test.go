package agentstate

import (
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestInterruptedGroupCanOnlyAppendCanonicalUnknownFacts(t *testing.T) {
	call := llm.ToolCall{ID: "uncertain", Name: "shell", Arguments: json.RawMessage(`{"command":"work"}`)}
	before := Checkpoint{Messages: []llm.ModelMessage{{Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []ToolRecord{{Call: call, State: ToolUnknown}}}
	after := before
	after.Messages = append([]llm.ModelMessage(nil), before.Messages...)
	if !after.CloseInterruptedGroup() {
		t.Fatal("did not close unknown group")
	}
	if err := ValidateTransition(before, after, 4096); err != nil {
		t.Fatal(err)
	}
	if before.Tools[0].State != ToolUnknown || before.Tools[0].Result != nil {
		t.Fatal("recovery invented a terminal outcome")
	}
	after.Messages[len(after.Messages)-1].Content = "The command failed; repeat it."
	if ValidateTransition(before, after, 4096) == nil {
		t.Fatal("allowed fabricated outcome")
	}
	before.Tools[0].State = ToolStarted
	if before.CloseInterruptedGroup() {
		t.Fatal("closed a live started call")
	}
}
