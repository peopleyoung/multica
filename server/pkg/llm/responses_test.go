package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responsesCapabilities(t *testing.T) Capabilities {
	t.Helper()
	caps := fixtureCapabilities()
	if err := json.Unmarshal([]byte(`{"api":"responses"}`), &caps); err != nil {
		t.Fatal(err)
	}
	return caps
}

func responseEvent(w http.ResponseWriter, kind string, value map[string]any) {
	value["type"] = kind
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
}

func TestResponsesNativeToolRoundTripAndSummary(t *testing.T) {
	requests := 0
	ledger := &recordedAdmission{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/responses" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"requires Responses for tool calls"}}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["store"] != false || body["stream"] != true || body["max_output_tokens"] != float64(100) || body["max_completion_tokens"] != nil || body["previous_response_id"] != nil {
			t.Errorf("incorrect stateless Responses request: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		output := []any{}
		if requests == 1 {
			tools := body["tools"].([]any)
			tool := tools[0].(map[string]any)
			if tool["name"] != "read_file" || tool["strict"] != false || tool["function"] != nil {
				t.Errorf("wrong Responses function schema: %v", tool)
			}
			output = append(output, map[string]any{"type": "reasoning", "id": "rs_fixture", "summary": []any{}, "encrypted_content": "opaque-reasoning"}, map[string]any{"type": "function_call", "id": "fc_fixture", "call_id": "call-1", "name": "read_file", "arguments": `{"path":"README.md"}`, "status": "completed"})
			responseEvent(w, "response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_fixture", "call_id": "call-1", "name": "read_file", "arguments": ""}})
			responseEvent(w, "response.function_call_arguments.delta", map[string]any{"output_index": 1, "item_id": "fc_fixture", "delta": `{"path":`})
		} else {
			if requests == 2 {
				input := body["input"].([]any)
				foundReasoning, foundResult := false, false
				for _, v := range input {
					m := v.(map[string]any)
					if m["type"] == "reasoning" && m["encrypted_content"] == "opaque-reasoning" {
						foundReasoning = true
					}
					if m["type"] == "function_call_output" && m["call_id"] == "call-1" && m["output"] == "fixture result" {
						foundResult = true
					}
				}
				if !foundReasoning || !foundResult {
					t.Error("reasoning or tool result was lost during stateless replay")
				}
			} else if body["tools"] != nil {
				t.Error("summary must not acquire execution tools")
			}
			output = append(output, map[string]any{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Done."}}})
			responseEvent(w, "response.output_text.delta", map[string]any{"delta": "Done.", "output_index": 0, "content_index": 0, "item_id": "msg_fixture"})
		}
		responseEvent(w, "response.completed", map[string]any{"response": map[string]any{"id": "resp_fixture", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 22, "output_tokens": 7, "input_tokens_details": map[string]any{"cached_tokens": 3}}}})
	}))
	defer srv.Close()
	client := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-gpt", Capabilities: responsesCapabilities(t), Admission: ledger, MaxRetries: retries(0)})
	req := fixtureRequest()
	first, err := client.Complete(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("configured GPT cannot use Native tools: %v", err)
	}
	if len(first.Message.ToolCalls) != 1 || string(first.Message.ToolCalls[0].Arguments) != `{"path":"README.md"}` {
		t.Fatalf("tool call lost: %+v", first)
	}
	req.Messages = append(req.Messages, first.Message, ModelMessage{Role: "tool", ToolCallID: "call-1", Content: "fixture result"})
	var text strings.Builder
	final, err := client.Complete(context.Background(), req, func(e Event) error { text.WriteString(e.Text); return nil })
	if err != nil || final.Message.Content != "Done." || text.String() != "Done." {
		t.Fatalf("GPT tool result continuation failed: response=%+v error=%v", final, err)
	}
	req.Tools = nil
	if _, err := client.Complete(context.Background(), req, nil); err != nil {
		t.Fatalf("summary transport differs from execution: %v", err)
	}
	if len(ledger.calls) != 3 || len(ledger.results) != 3 {
		t.Fatalf("Responses bypassed admission: calls=%d results=%d", len(ledger.calls), len(ledger.results))
	}
	for _, result := range ledger.results {
		if result.Usage == nil || *result.Usage != (Usage{InputTokens: 22, OutputTokens: 7, CachedTokens: 3}) {
			t.Fatalf("Responses billing usage lost: %+v", result)
		}
	}
}

func TestResponsesNeverExecutesPartialOrInvalidCalls(t *testing.T) {
	for _, tc := range []struct{ name, event, output string }{
		{"disconnected", "", `[]`},
		{"incomplete", "response.incomplete", `[]`},
		{"failed", "response.failed", `[]`},
		{"unknown tool", "response.completed", `[{"type":"function_call","call_id":"call-1","name":"shell","arguments":"{}"}]`},
		{"invalid arguments", "response.completed", `[{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{"}]`},
		{"empty output", "response.completed", `[]`},
		{"refusal", "response.completed", `[{"type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"private reason"}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				w.Header().Set("Content-Type", "text/event-stream")
				responseEvent(w, "response.function_call_arguments.delta", map[string]any{"output_index": 0, "delta": `{"path":"README.md"}`})
				if tc.event != "" {
					var output []any
					if err := json.Unmarshal([]byte(tc.output), &output); err != nil {
						t.Error(err)
					}
					responseEvent(w, tc.event, map[string]any{"response": map[string]any{"status": "completed", "output": output}})
				}
			}))
			defer srv.Close()
			client := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-gpt", Capabilities: responsesCapabilities(t)})
			result, err := client.Complete(context.Background(), fixtureRequest(), nil)
			if result != nil || err == nil || count != 1 {
				t.Fatalf("partial call became executable or stream replayed: result=%+v error=%v requests=%d", result, err, count)
			}
		})
	}
}

func TestResponsesReasoningCannotCrossEndpointModelOrCredential(t *testing.T) {
	for _, change := range []string{"endpoint", "model", "credential"} {
		t.Run(change, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Input []map[string]any `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				for _, item := range body.Input {
					if item["type"] == "reasoning" {
						t.Error("opaque reasoning leaked to another configured provider")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				responseEvent(w, "response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}}}})
			}))
			defer srv.Close()
			cfg := Config{BaseURL: srv.URL, APIKey: "fixture-key", DefaultModel: "fixture-model", Capabilities: responsesCapabilities(t)}
			old := New(cfg)
			switch change {
			case "endpoint":
				cfg.BaseURL += "/other"
			case "model":
				cfg.DefaultModel = "another-model"
			case "credential":
				cfg.APIKey = "another-fixture-key"
			}
			messages := []ModelMessage{{Role: "user", Content: "Hello"}, {Role: "assistant", Content: "Prior answer", Reasoning: &ReasoningState{Source: old.responseSource, Items: []ReasoningItem{{ID: "rs_fixture", EncryptedContent: "private-opaque-state", Summary: []ReasoningSummary{}}}}}, {Role: "user", Content: "Continue"}}
			client := New(cfg)
			if _, err := client.Complete(context.Background(), Request{Messages: messages, MaxOutputTokens: 100}, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResponsesAPIValidationAndHistoryState(t *testing.T) {
	caps := fixtureCapabilities()
	caps.API = "unsupported-protocol"
	if caps.Validate() == nil {
		t.Fatal("unknown API accepted")
	}
	valid := ModelMessage{Role: "assistant", Content: "OK", Reasoning: &ReasoningState{Source: strings.Repeat("a", 64), Items: []ReasoningItem{{ID: "rs_fixture", EncryptedContent: "opaque"}}}}
	if err := ValidateHistory([]ModelMessage{valid}, 512, false); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.Role = "user"
	if ValidateHistory([]ModelMessage{bad}, 512, false) == nil {
		t.Fatal("user supplied reasoning replay state")
	}
	bad = valid
	bad.Reasoning = &ReasoningState{Source: "not-a-provider", Items: valid.Reasoning.Items}
	if ValidateHistory([]ModelMessage{bad}, 512, false) == nil {
		t.Fatal("unbound reasoning accepted")
	}
}

func TestResponsesAuxiliaryGenerationUsesConfiguredProtocol(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonMode), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" {
					t.Error("auxiliary generation ignored the configured protocol")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if jsonMode {
					text := body["text"].(map[string]any)
					format := text["format"].(map[string]any)
					if format["type"] != "json_object" {
						t.Error("JSON response format was lost")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				responseEvent(w, "response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "{}"}}}}}})
			}))
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-gpt", Capabilities: responsesCapabilities(t)})
			var text string
			var err error
			if jsonMode {
				text, err = c.GenerateJSON(context.Background(), "", "JSON", "Reply with a JSON object.", 0, 100)
			} else {
				text, err = c.GenerateText(context.Background(), "", "", "Hello")
			}
			if err != nil || text != "{}" {
				t.Fatalf("configured auxiliary model failed: text=%q error=%v", text, err)
			}
		})
	}
}
