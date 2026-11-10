package metrics

import (
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/prometheus/client_golang/prometheus"
	"strings"
)

// Transport attempts are separate from daemon-reported run usage. These
// counters include auxiliary calls and must not be added to run-token counters.
type modelCallMetrics struct {
	tools     *prometheus.CounterVec
	admission *prometheus.CounterVec
	calls     *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	tokens    *prometheus.CounterVec
	unknown   *prometheus.CounterVec
	retries   *prometheus.CounterVec
	cost      *prometheus.CounterVec
	unpriced  *prometheus.CounterVec
}

func newModelCallMetrics() *modelCallMetrics {
	return &modelCallMetrics{
		tools:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_tool_events_total", Help: "Persisted tool start and result events; duplicate daemon callbacks are excluded."}, []string{"family", "event"}),
		admission: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_admission_total", Help: "Shared model admission decisions by purpose and result."}, []string{"purpose", "result"}),
		calls:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_attempts_total", Help: "Finished upstream HTTP attempts, including SDK retries and auxiliary calls."}, []string{"purpose", "outcome"}),
		latency:   prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "multica_model_api_duration_seconds", Help: "Upstream HTTP attempt duration including streamed response consumption.", Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}}, []string{"purpose", "outcome"}),
		tokens:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_tokens_total", Help: "Upstream-reported input, output and cached input tokens; cached is a subset of input."}, []string{"purpose", "type"}),
		unknown:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_unknown_usage_total", Help: "Attempts without valid upstream usage; these are not zero-token calls."}, []string{"purpose"}),
		retries:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_retries_total", Help: "Finished SDK retry attempts (initial requests excluded)."}, []string{"purpose"}),
		cost:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_estimated_cost_usd_total", Help: "Estimated cost from the local model rate table, not an authoritative provider charge."}, []string{"purpose"}),
		unpriced:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "multica_model_api_unpriced_usage_total", Help: "Reported usage without a known model price; unknown price does not mean free."}, []string{"purpose"}),
	}
}

func (m *modelCallMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.tools, m.admission, m.calls, m.latency, m.tokens, m.unknown, m.retries, m.cost, m.unpriced}
}

func (m *BusinessMetrics) RecordToolEvent(tool, event string) {
	if m == nil || m.modelCalls == nil || event != "tool_use" && event != "tool_result" {
		return
	}
	family := "other"
	switch {
	case strings.HasPrefix(tool, "multica_"):
		family = "multica"
	case strings.HasPrefix(tool, "mcp_"):
		family = "mcp"
	case tool == "shell_command":
		family = "shell"
	case tool == "read_file" || tool == "list_files" || tool == "search" || tool == "apply_patch":
		family = "files"
	case tool == "read_rules" || tool == "load_skill":
		family = "context"
	}
	m.modelCalls.tools.WithLabelValues(family, event).Inc()
}

func modelPurpose(purpose string) string {
	switch purpose {
	case "execution", "summary", "title", "quick_actions", "connection_test":
		return purpose
	default:
		return "other"
	}
}

func (m *BusinessMetrics) RecordModelAdmission(purpose, result string) {
	if m == nil || m.modelCalls == nil {
		return
	}
	if result != "allowed" && result != "denied" {
		result = "error"
	}
	m.modelCalls.admission.WithLabelValues(modelPurpose(purpose), result).Inc()
}

func (m *BusinessMetrics) RecordModelCall(call llm.Call, result llm.CallResult) {
	if m == nil || m.modelCalls == nil {
		return
	}
	purpose := modelPurpose(call.Scope.Purpose)
	outcome := result.Outcome
	if outcome != "completed" && outcome != "cancelled" {
		outcome = "failed"
	}
	c := m.modelCalls
	c.calls.WithLabelValues(purpose, outcome).Inc()
	c.latency.WithLabelValues(purpose, outcome).Observe(result.Duration.Seconds())
	if call.RetryAttempt > 0 {
		c.retries.WithLabelValues(purpose).Inc()
	}
	if result.Usage == nil {
		c.unknown.WithLabelValues(purpose).Inc()
		return
	}
	u := result.Usage
	c.tokens.WithLabelValues(purpose, "input").Add(float64(u.InputTokens))
	c.tokens.WithLabelValues(purpose, "output").Add(float64(u.OutputTokens))
	c.tokens.WithLabelValues(purpose, "cached_input").Add(float64(u.CachedTokens))
	if price, known := PriceForModelAlias(call.Model); known {
		cost := tokenCostUSD(max(0, u.InputTokens-u.CachedTokens), price.InputPerM) + tokenCostUSD(u.CachedTokens, price.CacheReadPerM) + tokenCostUSD(u.OutputTokens, price.OutputPerM)
		c.cost.WithLabelValues(purpose).Add(cost)
	} else {
		c.unpriced.WithLabelValues(purpose).Inc()
	}
}
