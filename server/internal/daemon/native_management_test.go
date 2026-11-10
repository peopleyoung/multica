package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeManagementBindsRuntimeAndWriteIdentity(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/agents" || r.Header.Get("Authorization") != "Bearer mat_run" || r.Header.Get("X-Workspace-ID") != "workspace" {
			t.Error("management used a different operation or authority")
		}
		var claim agentstate.WriteClaim
		if json.Unmarshal([]byte(r.Header.Get(agentstate.NativeWriteHeader)), &claim) != nil || claim.SessionID != "session" || claim.CallID != "configure" || claim.Version != 7 {
			t.Error("configuration omitted the engine-owned write identity")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["runtime_id"] != "native-runtime" || body["model"] != nil || body["custom_env"] != nil {
			t.Errorf("configuration did not bind the native runtime: %+v", body)
		}
		_, _ = w.Write([]byte(`{"id":"new-agent","name":"Specialist","mcp_config":{"token":"private-token"},"runtime_config":{"password":"private-token"},"future_secret":"private-token"}`))
	}))
	defer srv.Close()
	h := &nativeToolHost{control: NewClient(srv.URL), task: Task{WorkspaceID: "workspace", AuthToken: "mat_run", RuntimeID: "native-runtime"}}
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "session", LeaseToken: "lease", Version: 7}, "configure")
	output, err := h.Execute(ctx, llm.ToolCall{Name: "multica_manage_agent", Arguments: json.RawMessage(`{"action":"create","values":{"name":"Specialist","instructions":"Review changes"}}`)})
	if err != nil || requests != 1 || strings.Contains(output, "private-token") || !strings.Contains(output, "new-agent") {
		t.Fatalf("unsafe configuration output=%s requests=%d error=%v", output, requests, err)
	}
	for _, tc := range []struct{ name, args string }{
		{"multica_manage_agent", `{"action":"create","values":{"name":"X","instructions":"Y","model":"other"}}`},
		{"multica_manage_agent", `{"action":"create","values":{"name":"X","instructions":"Y","runtime_id":"other"}}`},
		{"multica_manage_agent", `{"action":"create","values":{"name":"X","instructions":"Y","custom_env":{"SECRET":"x"}}}`},
		{"multica_manage_agent", `{"action":"create","values":{"name":"X"}}`},
		{"multica_manage_squad", `{"action":"create","values":{"name":"X"}}`},
		{"multica_manage_project", `{"action":"update","id":"../secrets","values":{"title":"X"}}`},
		{"multica_manage_project", `{"action":"get","id":"project","values":{"title":"X"}}`},
		{"multica_manage_project", `{"action":"update","id":"project","values":{"lead_id":"agent"}}`},
		{"multica_manage_project", `{"action":"create","id":"other","values":{"title":"X"}}`},
		{"multica_manage_autopilot", `{"action":"add_trigger","id":"automation","values":{"kind":"webhook","signing_secret":"x"}}`},
		{"multica_manage_autopilot", `{"action":"trigger","id":"automation"}`},
		{"multica_manage_autopilot", `{"action":"update","id":"automation","values":{"subscribers":[{"user_id":"other"}]}}`},
		{"multica_manage_agent", `{"action":"get","id":"agent","url":"https://other.invalid"}`},
	} {
		if _, err := h.Execute(ctx, llm.ToolCall{Name: tc.name, Arguments: json.RawMessage(tc.args)}); err == nil {
			t.Fatalf("invalid configuration reached API: %s %s", tc.name, tc.args)
		}
	}
	if requests != 1 {
		t.Fatalf("rejected configurations reached API: %d", requests)
	}
}

func TestNativeManagementDefinitionsResolveLocally(t *testing.T) {
	for _, def := range nativeManagementDefinitions() {
		var schema jsonschema.Schema
		if err := json.Unmarshal(def.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if _, err := schema.Resolve(nil); err != nil {
			t.Fatalf("%s schema does not resolve: %v", def.Name, err)
		}
	}
}
