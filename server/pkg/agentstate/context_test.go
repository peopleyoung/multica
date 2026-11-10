package agentstate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func contextFixture() Checkpoint {
	return Checkpoint{InputTaskID: "task", Messages: []llm.ModelMessage{
		{Role: "system", Content: "permissions"}, {Role: "user", Content: "original objective"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"a","unused": 1}`)}}},
		{Role: "tool", ToolCallID: "read", Content: "observed result"}, {Role: "assistant", Content: "earlier final"}, {Role: "user", Content: "current request"},
	}}
}

func TestSummaryPreservesAuditHistoryAndCompleteToolGroups(t *testing.T) {
	before := contextFixture()
	after := before.WithSummary(4, "Read completed; no writes.")
	if err := after.Validate(4096); err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != 6 || !sameJSON(before.Messages, after.Messages) {
		t.Fatal("summary erased audit history")
	}
	if err := ValidateTransition(before, after, 4096); err == nil {
		t.Fatal("ordinary save rewrote context projection")
	}
	projection := after.ModelMessages()
	if projection[0].Content != "permissions" || projection[len(projection)-1].Content != "current request" || !after.MatchesMessages(projection) {
		t.Fatal("projection lost current task or permissions")
	}
	if err := llm.ValidateHistory(projection, 4096, false); err != nil {
		t.Fatal(err)
	}
	broken := before.WithSummary(3, "split tool group")
	if err := broken.Validate(4096); err == nil {
		t.Fatal("split call/result accepted")
	}
	// PostgreSQL jsonb sorts argument keys and changes whitespace.
	after.Messages[2].ToolCalls[0].Arguments = json.RawMessage(`{"unused":1,"path":"a"}`)
	if err := after.Validate(4096); err != nil {
		t.Fatalf("digest changed after jsonb normalization: %v", err)
	}
}

func TestSummaryRequestUsesBoundedPrefixAndNoTools(t *testing.T) {
	cp := contextFixture()
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}
	request, cut, err := cp.SummaryInput(caps)
	if err != nil || cut < 4 || len(request.Tools) > 0 {
		t.Fatalf("cut=%d err=%v", cut, err)
	}
	if err := llm.ValidateHistory(cp.Messages[:cut], 4096, false); err != nil {
		t.Fatal(err)
	}
	if err := request.Validate(caps); err != nil {
		t.Fatal(err)
	}
	cp.Messages[0].Content = strings.Repeat("huge", 4096)
	if _, _, err := cp.SummaryInput(caps); err == nil {
		t.Fatal("oversized indivisible message accepted")
	}
}

func TestBlankStoredSummaryRecoversFromDurableHistory(t *testing.T) {
	cp := contextFixture().WithSummary(4, "\n \t")
	if err := cp.Validate(4096); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(cp.ModelMessages(), cp.Messages) {
		t.Fatal("blank summary hid original history")
	}
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}
	request, cut, err := cp.SummaryInput(caps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Messages[1].Content, "original objective") || !strings.Contains(request.Messages[1].Content, "observed result") {
		t.Fatal("recovery omitted earlier facts")
	}
	if err := cp.WithSummary(cut, "Recovered objective and observations.").Validate(4096); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryExcludesOpaqueReasoningButRetainsDurableState(t *testing.T) {
	cp := contextFixture()
	state := &llm.ReasoningState{Source: strings.Repeat("a", 64), Items: []llm.ReasoningItem{{ID: "rs_fixture", EncryptedContent: strings.Repeat("opaque-private-replay", 1000)}}}
	cp.Messages[2].Reasoning = state
	caps := llm.Capabilities{API: "responses", StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}
	request, cut, err := cp.SummaryInput(caps)
	if err != nil || cut < 4 {
		t.Fatalf("opaque replay consumed the summary budget: cut=%d error=%v", cut, err)
	}
	if strings.Contains(request.Messages[1].Content, "opaque-private-replay") || !strings.Contains(request.Messages[1].Content, "observed result") {
		t.Fatal("summary leaked opaque replay or lost the tool outcome")
	}
	if cp.Messages[2].Reasoning != state {
		t.Fatal("summary erased durable replay state")
	}
}
