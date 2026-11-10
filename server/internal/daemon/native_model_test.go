package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeModelClaimAndStream(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer mdt_fixture" || r.URL.Path != "/api/daemon/tasks/run-1/native/inference" {
			t.Error("incorrect model request identity")
		}
		var req llm.NativeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Attempt != 2 || req.DispatchedAt != "2026-11-05T00:00:00Z" {
			t.Errorf("missing claim generation: %+v", req)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":{"text":"ok"}}`)
		fmt.Fprintln(w, `{"response":{"message":{"role":"assistant","content":"ok"}}}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.SetToken("mdt_fixture")
	var display strings.Builder
	result, err := c.CompleteNativeModel(context.Background(), Task{ID: "run-1", Attempt: 2, DispatchedAt: "2026-11-05T00:00:00Z"}, agentstate.Session{ID: "session", LeaseToken: "lease", Version: 7}, llm.Request{Messages: []llm.ModelMessage{{Role: "user", Content: "hi"}}}, func(e llm.Event) error { display.WriteString(e.Text); return nil })
	if err != nil || result == nil || display.String() != "ok" || requests != 1 {
		t.Fatalf("result=%+v error=%v requests=%d", result, err, requests)
	}
}

func TestNativeModelIncompleteStreamNeverRetries(t *testing.T) {
	for _, body := range []string{
		`{"event":{"text":"partial"}}`,
		`{"error":"private upstream details"}`,
		`{"event":{"text":"partial"},"response":{"message":{"role":"assistant"}}}`,
		`{"response":{"message":{"role":"assistant","tool_calls":[{"id":"one","name":"shell","arguments":{}}]}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/x-ndjson")
				fmt.Fprintln(w, body)
			}))
			defer srv.Close()
			c := NewClient(srv.URL)
			result, err := c.CompleteNativeModel(context.Background(), Task{ID: "one"}, agentstate.Session{ID: "session", LeaseToken: "lease", Version: 7}, llm.Request{Messages: []llm.ModelMessage{{Role: "user", Content: "hi"}}}, nil)
			if result != nil || err == nil || requests != 1 || strings.Contains(err.Error(), "private upstream") {
				t.Fatalf("result=%+v error=%v requests=%d", result, err, requests)
			}
		})
	}
}

func TestNativeModelReportsRecognizedFailureCodes(t *testing.T) {
	for code, diagnostic := range map[string]string{
		"model_step_cancelled":      "cancelled or timed out",
		"model_quota_exceeded":      "quota exceeded",
		"model_response_incomplete": "response incomplete",
		"model_stream_interrupted":  "stream interrupted",
		"model_transport_error":     "connection failed",
		"model_upstream_error":      "upstream returned an error",
	} {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_ = json.NewEncoder(w).Encode(llm.Frame{Error: code})
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL).CompleteNativeModel(context.Background(), Task{ID: "fixture"}, agentstate.Session{}, llm.Request{}, nil)
			if err == nil || !strings.Contains(err.Error(), diagnostic) {
				t.Fatalf("expected diagnostic %q, got %v", diagnostic, err)
			}
		})
	}
}
