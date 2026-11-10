package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

func TestExternalServiceIsConfinedAndCredentialsStayPrivate(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Token fixture-private-token" || r.URL.Path != "/api/tasks/" || r.URL.Query().Get("page") != "1" {
			t.Error("service request lost endpoint or authentication")
		}
		_, _ = w.Write([]byte(`{"results":[],"echo":"fixture-private-token"}`))
	}))
	defer srv.Close()
	cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "cvat", BaseURL: srv.URL + "/api", TokenEnv: "CVAT_TOKEN", AuthScheme: "Token"}}}
	if _, err := NewExternal(cfg, nativeruntime.Limits{}, map[string]string{"CVAT_TOKEN": "fixture-private-token"}); err == nil {
		t.Fatal("unapproved host accepted")
	}
	limits := nativeruntime.Limits{HTTPOrigins: []string{srv.URL}}
	if _, err := NewExternal(cfg, limits, nil); err == nil {
		t.Fatal("missing credentials accepted")
	}
	h, err := NewExternal(cfg, limits, map[string]string{"CVAT_TOKEN": "fixture-private-token"})
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(method, p string) (string, error) {
		raw, _ := json.Marshal(map[string]string{"service": "cvat", "method": method, "path": p})
		return h.Execute(context.Background(), llm.ToolCall{Name: "http_request", Arguments: raw})
	}
	out, err := invoke("GET", "tasks/?page=1")
	if err != nil || !strings.Contains(out, "200") || strings.Contains(out, "fixture-private-token") {
		t.Fatalf("unsafe service result: %s %v", out, err)
	}
	for _, p := range []string{"/api/tasks", "//attacker.invalid/tasks", "https://attacker.invalid", "../tasks", "tasks/../secrets", "%2e%2e/tasks", "tasks%2f..%2fsecrets", "tasks#secret", "tasks\\other"} {
		if _, err := invoke("GET", p); err == nil {
			t.Errorf("unsafe path accepted: %s", p)
		}
	}
	if _, err := invoke("POST", "tasks"); err == nil {
		t.Fatal("read-only service allowed write")
	}
	if calls != 1 {
		t.Fatalf("invalid requests reached network: %d", calls)
	}
}

func TestExternalWritesNeverRetryUncertainResults(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusFound} {
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Location", "/again")
			w.WriteHeader(status)
		}))
		cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "cvat", BaseURL: srv.URL, AllowWrite: true}}}
		h, err := NewExternal(cfg, nativeruntime.Limits{HTTPOrigins: []string{srv.URL}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.Execute(context.Background(), llm.ToolCall{Name: "http_request", Arguments: json.RawMessage(`{"service":"cvat","method":"POST","path":"tasks","body":{"name":"demo"}}`)})
		if !errors.Is(err, ErrOutcomeUnknown) || calls != 1 {
			t.Fatalf("uncertain write was repeatable: status=%d calls=%d err=%v", status, calls, err)
		}
		srv.Close()
	}
}
