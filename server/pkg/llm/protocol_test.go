package llm

import (
	"encoding/json"
	"testing"
)

func TestHistoryPairsEveryCallExactlyOnce(t *testing.T) {
	call := ToolCall{ID: "one", Name: "read_file", Arguments: json.RawMessage(`{}`)}
	assistant := ModelMessage{Role: "assistant", ToolCalls: []ToolCall{call}}
	tool := ModelMessage{Role: "tool", ToolCallID: "one", Content: ""}
	for _, tc := range []struct {
		name           string
		messages       []ModelMessage
		pending, valid bool
	}{
		{"complete", []ModelMessage{assistant, tool, {Role: "user", Content: "continue"}}, false, true},
		{"checkpoint", []ModelMessage{assistant}, true, true},
		{"missing result", []ModelMessage{assistant}, false, false},
		{"interrupted group", []ModelMessage{assistant, {Role: "user", Content: "continue"}}, true, false},
		{"orphan", []ModelMessage{tool}, true, false},
		{"duplicate result", []ModelMessage{assistant, tool, tool}, false, false},
		{"reused call", []ModelMessage{assistant, tool, assistant}, true, false},
		{"invalid role", []ModelMessage{{Role: "developer"}}, false, false},
		{"non-assistant call", []ModelMessage{{Role: "user", ToolCalls: []ToolCall{call}}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHistory(tc.messages, 512, tc.pending)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestCapabilityAndConfigurationValidation(t *testing.T) {
	valid := `{"streaming_tool_calls":true,"json_object":true,"context_tokens":8192,"max_output_tokens":1024,"max_tool_argument_bytes":512,"max_tools":4}`
	for _, raw := range []string{"", valid} {
		if _, err := ParseCapabilities(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`null`, `{}`, `[]`, valid + ` {}`, `{"unknown":true}`, `{"streaming_tool_calls":true,"context_tokens":100,"max_output_tokens":100}`} {
		if _, err := ParseCapabilities(raw); err == nil {
			t.Fatalf("invalid capabilities accepted: %s", raw)
		}
	}
	for _, base := range []string{"localhost:1234", "https://user:secret@example.com", "https://example.com?key=secret", "ftp://example.com"} {
		if (Config{BaseURL: base}).Validate() == nil {
			t.Fatal("invalid endpoint accepted")
		}
	}
	if (Config{Capabilities: fixtureCapabilities()}).Validate() == nil {
		t.Fatal("native execution guessed its model")
	}
}
