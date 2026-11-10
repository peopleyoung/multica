package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func fixtureCapabilities() Capabilities {
	return Capabilities{StreamingToolCalls: true, JSONObject: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 512, MaxTools: 4}
}

func fixtureRequest() Request {
	return Request{Messages: []ModelMessage{{Role: "user", Content: "Read the file."}}, MaxOutputTokens: 100,
		Tools: []Tool{{Name: "read_file", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}}
}

func writeChunk(w http.ResponseWriter, delta map[string]any, finish any) {
	chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func TestNativeImageInputUsesHydratedBytesAndDeploymentCapability(t *testing.T) {
	calls := 0
	srv := stubUpstream(t, func(w http.ResponseWriter, body map[string]any) {
		calls++
		parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,Zml4dHVyZQ==" {
			t.Error("image was not hydrated from bytes")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk(w, map[string]any{"content": "image analyzed"}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	caps := fixtureCapabilities()
	req := Request{Messages: []ModelMessage{{Role: "user", Content: "analyze", Images: []ImageInput{{ContentType: "image/png", Data: []byte("fixture")}}}}, MaxOutputTokens: 100}
	client := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: caps})
	if _, err := client.Complete(context.Background(), req, nil); err == nil || calls != 0 {
		t.Fatal("unverified image capability sent upstream")
	}
	caps.ImageInput = true
	client = New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: caps})
	if _, err := client.Complete(context.Background(), req, nil); err != nil || calls != 1 {
		t.Fatal(err)
	}
}

func TestCompleteToolRoundTrip(t *testing.T) {
	requests := 0
	srv := stubUpstream(t, func(w http.ResponseWriter, body map[string]any) {
		requests++
		if body["model"] != "fixture-model" || body["chat_template_kwargs"] != nil {
			t.Errorf("native requests must use the deployment model and their own thinking policy: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"pa`}}}}, nil)
			writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `th":"README.md"}`}}}}, nil)
			writeChunk(w, map[string]any{}, "tool_calls")
		} else {
			messages := body["messages"].([]any)
			last := messages[len(messages)-1].(map[string]any)
			if last["role"] != "tool" || last["tool_call_id"] != "call-1" || last["content"] != "file contents" {
				t.Errorf("tool result not paired in outbound history: %v", messages)
			}
			writeChunk(w, map[string]any{"content": "Done."}, "stop")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":22,\"completion_tokens\":3}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: fixtureCapabilities(), DisableThinking: true})
	req := fixtureRequest()
	first, err := c.Complete(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.ToolCalls) != 1 || string(first.Message.ToolCalls[0].Arguments) != `{"path":"README.md"}` || first.Usage != nil {
		t.Fatalf("unexpected complete call or missing-usage semantics: %+v", first)
	}
	req.Messages = append(req.Messages, first.Message, ModelMessage{Role: "tool", ToolCallID: "call-1", Content: "file contents"})
	var display strings.Builder
	last, err := c.Complete(context.Background(), req, func(e Event) error { display.WriteString(e.Text); return nil })
	if err != nil || last == nil || last.Message.Content != "Done." || display.String() != "Done." || last.Usage == nil || last.Usage.InputTokens != 22 || requests != 2 {
		t.Fatalf("unexpected final step: result=%+v error=%v requests=%d", last, err, requests)
	}
}

func TestCompleteRejectsUnsafePartialSteps(t *testing.T) {
	for _, tc := range []struct{ name, args, finish, tool string }{
		{"disconnected", `{}`, "", "read_file"},
		{"truncated", `{}`, "length", "read_file"},
		{"invalid JSON", `{"path":`, "tool_calls", "read_file"},
		{"non-object arguments", `[]`, "tool_calls", "read_file"},
		{"unknown tool", `{}`, "tool_calls", "shell"},
		{"argument limit", `{"path":"` + strings.Repeat("x", 600) + `"}`, "tool_calls", "read_file"},
		{"finish mismatch", `{}`, "stop", "read_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := stubUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": tc.tool, "arguments": tc.args}}}}, nil)
				if tc.finish != "" {
					writeChunk(w, map[string]any{}, tc.finish)
				}
			})
			c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: fixtureCapabilities()})
			result, err := c.Complete(context.Background(), fixtureRequest(), nil)
			if err == nil || result != nil {
				t.Fatalf("unsafe step became executable: %+v, %v", result, err)
			}
		})
	}
}

func TestCompleteRejectsEmptyFinalResponse(t *testing.T) {
	for _, content := range []string{"", "\n \t"} {
		srv := stubUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeChunk(w, map[string]any{"content": content}, "stop")
		})
		c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: fixtureCapabilities()})
		response, err := c.Complete(context.Background(), fixtureRequest(), nil)
		if !errors.Is(err, ErrEmptyResponse) || response != nil || FailureCode(err) != "model_response_empty" {
			t.Fatalf("empty final accepted: response=%v err=%v", response, err)
		}
		srv.Close()
	}
}

func TestCompleteCancellationAndConsumerFailure(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelContext), func(t *testing.T) {
			srv := stubUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeChunk(w, map[string]any{"content": "partial"}, nil)
				w.(http.Flusher).Flush()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("consumer stopped")
			c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: fixtureCapabilities()})
			result, err := c.Complete(ctx, fixtureRequest(), func(Event) error {
				if cancelContext {
					cancel()
					return nil
				}
				return failure
			})
			if err == nil || result != nil {
				t.Fatalf("partial step returned after cancellation: %+v %v", result, err)
			}
			if !cancelContext && !errors.Is(err, failure) {
				t.Fatal(err)
			}
		})
	}
}

func TestCompleteDisabledAndUnverifiedNeverDial(t *testing.T) {
	for _, configured := range []bool{false, true} {
		calls := 0
		srv := stubUpstream(t, func(http.ResponseWriter, map[string]any) { calls++ })
		cfg := Config{}
		if configured {
			cfg.BaseURL = srv.URL
		}
		_, err := New(cfg).Complete(context.Background(), fixtureRequest(), nil)
		if err == nil || calls != 0 {
			t.Fatalf("unconfigured capabilities reached upstream: calls=%d error=%v", calls, err)
		}
	}
}

func TestCompleteUpstreamFailuresNeverChangeDeploymentModel(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			srv := stubUpstream(t, func(w http.ResponseWriter, body map[string]any) {
				requests++
				if body["model"] != "fixture-model" {
					t.Error("upstream failure changed the configured model")
				}
				w.WriteHeader(status)
				fmt.Fprintln(w, `{"error":{"message":"private upstream diagnostic"}}`)
			})
			c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: fixtureCapabilities(), MaxRetries: retries(0)})
			result, err := c.Complete(context.Background(), fixtureRequest(), nil)
			if result != nil || err == nil || requests != 1 {
				t.Fatalf("failed request replayed or accepted: result=%+v err=%v requests=%d", result, err, requests)
			}
		})
	}
}

func TestCompleteUpstreamStreamErrorRejectsPartialCallsWithoutReplay(t *testing.T) {
	requests := 0
	srv := stubUpstream(t, func(w http.ResponseWriter, _ map[string]any) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk(w, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "pending", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":`}}}}, nil)
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"private upstream diagnostic\",\"type\":\"upstream_error\"}}\n\n")
	})
	c := New(Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: fixtureCapabilities()})
	result, err := c.Complete(context.Background(), fixtureRequest(), nil)
	if result != nil || err == nil || requests != 1 || FailureCode(err) != "model_upstream_error" {
		t.Fatalf("stream failure was replayed, accepted or misclassified: response=%v requests=%d code=%s", result, requests, FailureCode(err))
	}
}
