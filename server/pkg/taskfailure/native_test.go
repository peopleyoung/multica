package taskfailure

import "testing"

func TestNativeUnknownOutcomeIsReconciliationFailure(t *testing.T) {
	for _, text := range []string{
		"native tool outcome is unknown; reconcile the prior operation before continuing",
		"native tool interrupted; outcome is unknown and requires reconciliation",
		"native tool outcome is unknown (tool shell, session fixture); reconciliation required",
	} {
		if got := Classify(text).String(); got != "native_tool_outcome_unknown" {
			t.Errorf("%q classified as %q", text, got)
		}
	}
}

func TestNativeModelFailuresKeepActionableCategories(t *testing.T) {
	for _, tc := range []struct {
		text   string
		reason Reason
	}{
		{"native model request rejected; check API protocol and model capabilities", ReasonAgentMissingConfig},
		{"native model authentication failed; check provider credentials and access", ReasonAgentProviderAuthOrAccess},
		{"native model upstream returned an error", ReasonAgentProviderServerError},
		{`native context compaction failed: {"code":"model_request_rejected"}`, ReasonAgentMissingConfig},
		{`native context compaction failed: {"code":"model_rate_limited"}`, ReasonAgentProviderCapacityOrRateLimit},
	} {
		if got := Classify(tc.text); got != tc.reason {
			t.Errorf("%q: got %s want %s", tc.text, got, tc.reason)
		}
	}
}
