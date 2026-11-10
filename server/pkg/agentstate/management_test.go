package agentstate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManagementResponseKeepsConfigurationAndExcludesCredentials(t *testing.T) {
	for _, tc := range []struct{ tool, input, want string }{
		{"multica_manage_agent", ` {"id":"agent","name":"Specialist","runtime_config":{"token":"private"},"mcp_config":{"key":"private"},"future_secret":"private"}`, "Specialist"},
		{"multica_manage_agent", `[{"id":"skill","name":"Review","enabled":true,"config":{"token":"private"}}]`, "Review"},
		{"multica_manage_autopilot", `{"autopilot":{"id":"automation","title":"Digest"},"triggers":[{"id":"trigger","kind":"webhook","webhook_token":"private","webhook_url":"private","future_secret":"private"}],"collaborators":["private"]}`, "Digest"},
		{"multica_manage_autopilot", `{"autopilots":[{"id":"automation","title":"Digest","future_secret":"private"}]}`, "Digest"},
	} {
		body, err := ProjectManagementResponse(tc.tool, json.RawMessage(tc.input))
		if err != nil || strings.Contains(string(body), "private") || !strings.Contains(string(body), tc.want) {
			t.Fatalf("management projection=%s error=%v", body, err)
		}
	}
}
