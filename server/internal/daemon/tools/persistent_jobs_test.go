package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

func TestPersistentServiceRejectsMalformedSuccessWithoutRepeatingSubmission(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"job_id":"shell-a","status":"invented"}`} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: server.URL, TokenEnv: "JOBS_TOKEN", AllowWrite: true}}}
			h, err := NewExternal(cfg, nativeruntime.Limits{HTTPOrigins: []string{server.URL}}, map[string]string{"JOBS_TOKEN": "private-fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "session"}, "submit")
			_, err = h.Execute(ctx, llm.ToolCall{Name: "persistent_job", ID: "submit", Arguments: json.RawMessage(`{"action":"submit","command":"fixture-only"}`)})
			if !errors.Is(err, ErrOutcomeUnknown) || calls != 1 {
				t.Fatalf("malformed success trusted or retried: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestPersistentServiceSubmissionIdentityCannotBeChosenByModel(t *testing.T) {
	keys := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key string `json:"key"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid admission request")
		}
		keys = append(keys, body.Key)
		_, _ = w.Write([]byte(`{"job_id":"shell-1234567890abcdef","status":"running"}`))
	}))
	defer server.Close()
	cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: server.URL, TokenEnv: "JOBS_TOKEN", AllowWrite: true}}}
	h, err := NewExternal(cfg, nativeruntime.Limits{HTTPOrigins: []string{server.URL}}, map[string]string{"JOBS_TOKEN": "private-fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{Name: "persistent_job", ID: "submit", Arguments: json.RawMessage(`{"action":"submit","command":"fixture-only"}`)}
	if _, err := h.Execute(context.Background(), call); err == nil {
		t.Fatal("submission without engine identity accepted")
	}
	for _, owner := range []string{"a", "a", "b"} {
		ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: owner}, "submit")
		if _, err := h.Execute(ctx, call); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] == keys[2] {
		t.Fatal("submission keys do not follow execution identity")
	}
	call.Arguments = json.RawMessage(`{"action":"submit","command":"fixture-only","key":"model-selected"}`)
	if _, err := h.Execute(agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "a"}, "submit"), call); err == nil || len(keys) != 3 {
		t.Fatal("model-controlled admission identity reached service")
	}
	if _, err := h.Execute(context.Background(), llm.ToolCall{Name: "http_request", Arguments: json.RawMessage(`{"service":"multica_jobs","method":"POST","path":"v1/jobs","body":{"key":"model-selected","command":"fixture-only"}}`)}); err == nil || len(keys) != 3 {
		t.Fatal("generic HTTP bypassed engine-owned submission identity")
	}
}
