package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConnectionBothProtocolsAndAdmission(t *testing.T) {
	for _, protocol := range []string{"chat_completions", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			var requests atomic.Int32
			ledger := &recordedAdmission{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				step := requests.Add(1)
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request body")
					return
				}
				if body["model"] != "probe-model" || body["stream"] != true || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("probe did not use configured native transport")
				}
				if step == 2 && body["tool_choice"] == nil {
					t.Error("probe tool choice is not explicit")
				}
				if step == 3 {
					raw, _ := json.Marshal(body)
					if !strings.Contains(string(raw), "probe-call") || !strings.Contains(string(raw), `\"ok\":true`) || body["tool_choice"] != nil || body["tools"] != nil {
						t.Error("simulated tool result missing, or final step still forces tools")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == "responses" {
					if r.URL.Path != "/responses" || body["store"] != false {
						t.Error("probe did not use stateless Responses")
					}
					var output []any
					if step == 2 {
						output = []any{map[string]any{"type": "function_call", "id": "fc_probe", "call_id": "probe-call", "name": "multica_connection_probe", "arguments": "{}", "status": "completed"}}
					} else {
						output = []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}}
					}
					responseEvent(w, "response.completed", map[string]any{"response": map[string]any{"id": "resp_probe", "status": "completed", "output": output}})
				} else {
					if r.URL.Path != "/chat/completions" {
						t.Error("probe did not use Chat Completions")
					}
					if step == 2 {
						writeChunk(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "probe-call", "type": "function", "function": map[string]any{"name": "multica_connection_probe", "arguments": "{}"}}}}, "tool_calls")
					} else {
						writeChunk(w, map[string]any{"role": "assistant", "content": "OK"}, "stop")
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer srv.Close()
			caps := fixtureCapabilities()
			caps.API = protocol
			client := New(Config{BaseURL: srv.URL, APIKey: "fixture-key", DefaultModel: "probe-model", Capabilities: caps, Admission: ledger, MaxRetries: retries(0)})
			ctx := WithCallScope(context.Background(), CallScope{WorkspaceID: "workspace", Purpose: "connection_test"})
			result := client.TestConnection(ctx)
			if !result.OK || result.Stage != "complete" || result.Code != "ok" || result.DurationMs < 0 || requests.Load() != 3 {
				t.Fatalf("invalid connection result: %+v requests=%d", result, requests.Load())
			}
			if len(ledger.calls) != 3 || len(ledger.results) != 3 {
				t.Fatal("probe bypassed shared admission or accounting")
			}
			for _, call := range ledger.calls {
				if call.Scope.Purpose != "connection_test" || call.RetryAttempt != 0 || call.Scope.TaskID != "" {
					t.Fatalf("probe was recorded as business execution: %+v", call.Scope)
				}
			}
		})
	}
}

func TestConnectionFailureStopsAndSanitizes(t *testing.T) {
	for _, tc := range []struct {
		name, stage, code string
		failAt, status    int
	}{
		{"auth", "text", "model_authentication_failed", 1, 401},
		{"rejected tools", "tool_call", "model_request_rejected", 2, 400},
		{"tools ignored", "tool_call", "model_tool_call_failed", 2, 200},
		{"broken continuation", "tool_result", "model_upstream_error", 3, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				step := int(requests.Add(1))
				if step == tc.failAt && tc.status != 200 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, `{"error":{"message":"private-fixture-key","type":"api_error"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if step == 2 && tc.failAt != 2 {
					writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "probe-call", "type": "function", "function": map[string]any{"name": "multica_connection_probe", "arguments": "{}"}}}}, "tool_calls")
				} else {
					writeChunk(w, map[string]any{"content": "OK"}, "stop")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			client := New(Config{BaseURL: server.URL, DefaultModel: "probe-model", Capabilities: fixtureCapabilities(), MaxRetries: retries(0)})
			result := client.TestConnection(context.Background())
			raw, _ := json.Marshal(result)
			if result.OK || result.Stage != tc.stage || result.Code != tc.code || requests.Load() != int32(tc.failAt) || strings.Contains(string(raw), "private-fixture-key") {
				t.Fatalf("unsafe result or request replay: %+v requests=%d", result, requests.Load())
			}
		})
	}
}

func TestConnectionCancellationNeverDials(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := New(Config{BaseURL: srv.URL, DefaultModel: "probe-model", Capabilities: fixtureCapabilities(), MaxRetries: retries(0)}).TestConnection(ctx)
	if result.OK || result.Code != "model_step_cancelled" || requests.Load() != 0 {
		t.Fatalf("canceled probe still ran: %+v", result)
	}
}
