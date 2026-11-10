package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestModelCallMetricsKeepPurposeCostAndUnknownUsageSeparate(t *testing.T) {
	m := NewBusinessMetrics()
	call := llm.Call{Scope: llm.CallScope{WorkspaceID: "private-workspace", TaskID: "private-run", Purpose: "summary"}, Model: "gpt-5.6-sol", RetryAttempt: 1}
	m.RecordModelAdmission("summary", "allowed")
	m.RecordModelCall(call, llm.CallResult{Outcome: "completed", Duration: time.Second, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10, CachedTokens: 20}})
	if got := testutil.ToFloat64(m.modelCalls.tokens.WithLabelValues("summary", "input")); got != 100 {
		t.Fatalf("cached input was counted twice: %v", got)
	}
	if got := testutil.ToFloat64(m.modelCalls.retries.WithLabelValues("summary")); got != 1 {
		t.Fatalf("retry count=%v", got)
	}
	want := float64(80)*5/1_000_000 + float64(20)*0.5/1_000_000 + float64(10)*30/1_000_000
	if got := testutil.ToFloat64(m.modelCalls.cost.WithLabelValues("summary")); got != want {
		t.Fatalf("estimated cost=%v want=%v", got, want)
	}
	call.Model = "private-model-alias"
	m.RecordModelCall(call, llm.CallResult{Outcome: "completed", Usage: &llm.Usage{InputTokens: 2, OutputTokens: 1}})
	m.RecordModelCall(call, llm.CallResult{Outcome: "failed"})
	if testutil.ToFloat64(m.modelCalls.unpriced.WithLabelValues("summary")) != 1 || testutil.ToFloat64(m.modelCalls.unknown.WithLabelValues("summary")) != 1 {
		t.Fatal("unknown usage or unknown pricing was treated as zero cost")
	}
	for _, family := range GatherForTest(t, m) {
		if !strings.HasPrefix(family.GetName(), "multica_model_api_") {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if strings.Contains(label.GetValue(), "private-") {
					t.Fatalf("audit identifier leaked into metric label: %s", label.GetValue())
				}
			}
		}
	}
}
