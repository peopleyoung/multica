package modelcontrol

import "testing"

func TestParseLimits(t *testing.T) {
	limits, err := ParseLimits(`{"workspace_concurrent":2}`)
	if err != nil || limits.WorkspaceConcurrent != 2 || limits.GlobalConcurrent != 16 {
		t.Fatalf("partial limits did not retain deployment defaults: %+v %v", limits, err)
	}
	for _, input := range []string{`null`, `[]`, `{"global_concurrent":0}`, `{"workspace_tokens_per_minute":-1}`, `{"global_tokens_per_minute":1000000000001}`, `{"unknown":1}`, `{} {}`} {
		if _, err := ParseLimits(input); err == nil {
			t.Fatalf("invalid limit accepted: %s", input)
		}
	}
}
