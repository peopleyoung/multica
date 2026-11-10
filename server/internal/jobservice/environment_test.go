package jobservice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

func TestRemoteEnvironmentSharesCommandsFilesRulesArtifactsAndRecoversAdmission(t *testing.T) {
	cfg, old := fixture(t)
	_ = old.Close()
	cfg.Limits.WriteFiles = true
	s, err := New(cfg, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.WriteFile(filepath.Join(cfg.WorkDir, "AGENTS.md"), []byte("remote rules"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600)
	_ = os.Symlink(outside, filepath.Join(cfg.WorkDir, "escape"))
	var drop atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environment/tools" && drop.Swap(false) {
			s.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		s.ServeHTTP(w, r)
	}))
	defer server.Close()
	external, err := tools.NewExternal(nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: server.URL, TokenEnv: "JOBS_TOKEN", AllowWrite: true}}}, nativeruntime.Limits{HTTPOrigins: []string{server.URL}}, map[string]string{"JOBS_TOKEN": tokenA})
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	external.ObserveSubmission = func(context.Context, string) error { observations++; return nil }
	host, err := tools.NewRemoteEnvironment(context.Background(), external, tools.Permissions{Shell: true, WriteFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "remote-session"}, "write")
	execute := func(name, args string) (string, error) {
		return host.Execute(ctx, llm.ToolCall{ID: "once", Name: name, Arguments: json.RawMessage(args)})
	}
	if rules, err := host.Rules(ctx, "."); err != nil || !strings.Contains(rules, "remote rules") {
		t.Fatalf("rules=%s err=%v", rules, err)
	}
	if _, err := execute("apply_patch", `{"path":"marker","old_text":"","new_text":"file"}`); err != nil {
		t.Fatal(err)
	}
	drop.Store(true)
	call := llm.ToolCall{ID: "once", Name: "shell", Arguments: json.RawMessage(`{"command":"printf command >> marker; printf artifact > result","wait_ms":2000}`)}
	if _, err := host.Execute(ctx, call); !errors.Is(err, tools.ErrOutcomeUnknown) {
		t.Fatalf("lost reply: %v", err)
	}
	out, found, err := host.RecoverTool(ctx, call)
	if err != nil || !found {
		t.Fatalf("recovery %s %v", out, err)
	}
	if observations != 2 {
		t.Fatalf("recovered shell was not watched: %d", observations)
	}
	if got, err := execute("read_file", `{"path":"marker"}`); err != nil || got != "filecommand" {
		t.Fatalf("split filesystem or repeated command: %s %v", got, err)
	}
	if data, err := host.ReadArtifact("result"); err != nil || string(data) != "artifact" {
		t.Fatalf("remote artifact %s %v", data, err)
	}
	for _, path := range []string{"../secret", "escape/secret"} {
		args, _ := json.Marshal(map[string]string{"path": path})
		if _, err := execute("read_file", string(args)); err == nil {
			t.Fatalf("path escaped: %s", path)
		}
	}
	request(t, s, tokenB, "GET", "/v1/submissions/"+strings.Repeat("0", 64), nil, 404)
	if runtime.GOOS == "linux" {
		out, err := execute("terminal", `{"action":"open"}`)
		if err != nil {
			t.Fatal(err)
		}
		var term struct {
			ID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(out), &term); err != nil {
			t.Fatal(err)
		}
		send, _ := json.Marshal(map[string]any{"action": "send", "session_id": term.ID, "text": "printf remote-terminal > terminal-file\n", "wait_ms": 100})
		if _, err := execute("terminal", string(send)); err != nil {
			t.Fatal(err)
		}
		if out, err := execute("read_file", `{"path":"terminal-file"}`); err != nil || out != "remote-terminal" {
			t.Fatalf("terminal used a different work directory: %s %v", out, err)
		}
		closeArgs, _ := json.Marshal(map[string]string{"action": "close", "session_id": term.ID})
		if _, err := execute("terminal", string(closeArgs)); err != nil {
			t.Fatal(err)
		}
		if _, err := execute("terminal", `{"action":"open"}`); err != nil {
			t.Fatal(err)
		}
		if err := host.Close(); err != nil {
			t.Fatal(err)
		}
		if err := host.Close(); err != nil {
			t.Fatal("repeated teardown was not idempotent")
		}
		list := request(t, s, tokenA, "POST", "/v1/environment/tools", map[string]any{"name": "terminal", "scope": "remote-session", "arguments": map[string]string{"action": "list"}}, 200)
		if list["output"] != "[]" {
			t.Fatal("remote terminal survived host teardown")
		}
	}

	readonly, err := tools.NewRemoteEnvironment(ctx, external, tools.Permissions{})
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if _, err := readonly.Execute(ctx, call); err == nil {
		t.Fatal("read-only run launched a command")
	}
}

func TestRemoteEnvironmentTerminalOwnershipUsesNamespaceAndSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("interactive terminals require Linux")
	}
	_, s := fixture(t)
	open := request(t, s, tokenA, "POST", "/v1/environment/tools", map[string]any{"name": "terminal", "scope": "session-a", "arguments": map[string]string{"action": "open"}}, 200)
	var terminal struct {
		ID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(open["output"].(string)), &terminal); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ token, scope string }{{tokenA, "session-b"}, {tokenB, "session-a"}} {
		request(t, s, v.token, "POST", "/v1/environment/tools", map[string]any{"name": "terminal", "scope": v.scope, "arguments": map[string]string{"action": "read", "session_id": terminal.ID}}, 400)
	}
	request(t, s, tokenA, "POST", "/v1/environment/tools", map[string]any{"name": "terminal", "scope": "session-a", "arguments": map[string]string{"action": "close", "session_id": terminal.ID}}, 200)
}
