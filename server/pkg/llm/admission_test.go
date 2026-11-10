package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type recordedAdmission struct {
	mu      sync.Mutex
	calls   []Call
	results []CallResult
	deny    bool
}

func (a *recordedAdmission) Acquire(_ context.Context, call Call) (func(context.Context, CallResult) error, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deny {
		return nil, ErrQuotaExceeded
	}
	a.calls = append(a.calls, call)
	return func(_ context.Context, result CallResult) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.results = append(a.results, result)
		return nil
	}, nil
}

func TestAdmissionAccountsForNativeAuxiliaryAndEverySDKRetry(t *testing.T) {
	for _, kind := range []string{"execution", "summary", "title", "quick_actions"} {
		t.Run(kind, func(t *testing.T) {
			admission := &recordedAdmission{}
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					w.Header().Set("Retry-After-Ms", "1")
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, `{"error":{"message":"private upstream diagnostic"}}`)
					return
				}
				if kind == "execution" || kind == "summary" {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`)
				}
			}))
			defer upstream.Close()
			retries, _ := Retries(1)
			client := New(Config{Admission: admission, BaseURL: upstream.URL, DefaultModel: "fixture", MaxRetries: retries, Capabilities: fixtureCapabilities()})
			ctx := WithCallScope(context.Background(), CallScope{WorkspaceID: "workspace", TaskID: "run", SessionID: "session", Purpose: kind})
			var err error
			if kind == "execution" || kind == "summary" {
				_, err = client.Complete(ctx, Request{Messages: []ModelMessage{{Role: "user", Content: "private prompt"}}, MaxOutputTokens: 100}, nil)
			} else if kind == "title" {
				_, err = client.GenerateText(ctx, "", "", "private prompt")
			} else {
				_, err = client.GenerateJSON(ctx, "", "JSON", "private prompt", 0, 100)
			}
			if err != nil {
				t.Fatal(err)
			}
			admission.mu.Lock()
			defer admission.mu.Unlock()
			if requests != 2 || len(admission.calls) != 2 || len(admission.results) != 2 {
				t.Fatalf("attempts=%d reservations=%d finishes=%d", requests, len(admission.calls), len(admission.results))
			}
			for i, call := range admission.calls {
				if call.Scope.Purpose != kind || call.Scope.WorkspaceID != "workspace" || call.Model != "fixture" || call.RetryAttempt != i || call.ReservedTokens < 100 {
					t.Fatalf("attempt lost accounting identity: %+v", call)
				}
			}
			if admission.results[0].Outcome != "failed" || admission.results[0].Usage != nil {
				t.Fatalf("failed attempt fabricated usage: %+v", admission.results[0])
			}
			result := admission.results[1]
			if result.Outcome != "completed" || result.Usage == nil || *result.Usage != (Usage{InputTokens: 11, OutputTokens: 7, CachedTokens: 3}) {
				t.Fatalf("reported usage lost or counted twice: %+v", result)
			}
		})
	}
}

func TestDeniedAdmissionNeverReachesUpstream(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	client := New(Config{Admission: &recordedAdmission{deny: true}, BaseURL: upstream.URL})
	_, err := client.GenerateText(context.Background(), "", "", "private prompt")
	if !errors.Is(err, ErrQuotaExceeded) || requests != 0 {
		t.Fatalf("denied model call reached upstream: calls=%d error=%v", requests, err)
	}
}

func TestAdmissionDistinguishesMissingInvalidAndZeroUsage(t *testing.T) {
	for _, tc := range []struct {
		usage string
		known bool
	}{
		{``, false}, {`,"usage":{}`, false}, {`,"usage":{"prompt_tokens":-1,"completion_tokens":3}`, false},
		{`,"usage":{"prompt_tokens":1,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}`, false},
		{`,"usage":{"prompt_tokens":0,"completion_tokens":0}`, true},
	} {
		t.Run(tc.usage, func(t *testing.T) {
			a := &recordedAdmission{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]%s}`, tc.usage)
			}))
			defer upstream.Close()
			_, err := New(Config{Admission: a, BaseURL: upstream.URL}).GenerateText(context.Background(), "", "", "input")
			if err != nil {
				t.Fatal(err)
			}
			if len(a.results) != 1 || (a.results[0].Usage != nil) != tc.known {
				t.Fatalf("missing/invalid usage treated as zero: %+v", a.results)
			}
		})
	}
}
