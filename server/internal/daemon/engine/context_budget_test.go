package engine

import (
	"github.com/multica-ai/multica/server/pkg/llm"
	"strings"
	"testing"
)

func TestMeasuredContextPreservesLongHistoryUntilActualPressure(t *testing.T) {
	req := llm.Request{Messages: []llm.ModelMessage{{Role: "user", Content: strings.Repeat("training history ", 1000)}}, MaxOutputTokens: 1024}
	var b contextBudget
	conservative := b.estimate(req)
	b.observe(req, &llm.Usage{InputTokens: 2500})
	next := req
	next.Messages = append(append([]llm.ModelMessage{}, req.Messages...), llm.ModelMessage{Role: "assistant", Content: "job shell-123 is running"})
	if got := b.estimate(next); got >= conservative || got < 2500 {
		t.Fatalf("provider measurement ignored: %d vs %d", got, conservative)
	}
	next.Messages[0].Content = "changed projection"
	if b.estimate(next) >= 2500 {
		t.Fatal("replaced history reused stale usage")
	}
	b.observe(req, nil)
	if b.estimate(req) != conservative {
		t.Fatal("missing usage treated as zero")
	}
}
