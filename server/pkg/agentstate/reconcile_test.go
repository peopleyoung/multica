package agentstate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestReconcileUnknownShellRequiresExplicitEvidenceAndPreservesHistory(t *testing.T) {
	call := llm.ToolCall{ID: "legacy", Name: "shell", Arguments: json.RawMessage(`{"command":"unrepeatable operation"}`)}
	cp := Checkpoint{Messages: []llm.ModelMessage{{Role: "user", Content: "work"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []ToolRecord{{Call: call, State: ToolUnknown}}}
	next, err := cp.ReconcileShell("legacy", "failed", "operator", "Remote job exited; log confirms FileNotFoundError for required weights.", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(cp.Messages, next.Messages) || cp.Tools[0].State != ToolUnknown || next.Tools[0].State != ToolFailed || next.Tools[0].Result == nil {
		t.Fatal("resolution rewrote history or did not preserve original")
	}
	if err := next.Validate(4096); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition(cp, next, 4096); err == nil {
		t.Fatal("ordinary daemon save can resolve unknown operation")
	}
	for _, tt := range []struct{ call, outcome, operator, evidence string }{
		{"legacy", "failed", "operator", ""}, {"legacy", "failed", "", "verified"}, {"legacy", "unknown", "operator", "verified"}, {"wrong", "failed", "operator", "verified"},
	} {
		if _, err := cp.ReconcileShell(tt.call, tt.outcome, tt.operator, tt.evidence, time.Now()); err == nil {
			t.Fatalf("invalid reconciliation accepted: %+v", tt)
		}
	}
	cp.Tools[0].Call.Name = "http_request"
	cp.Messages[1].ToolCalls[0].Name = "http_request"
	if _, err := cp.ReconcileShell("legacy", "failed", "operator", "verified remote failure", time.Now()); err == nil {
		t.Fatal("shell recovery resolved an external write")
	}
}
