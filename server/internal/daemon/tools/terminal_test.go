//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestTerminalPersistsREPLStateInterruptsForegroundAndClosesChildren(t *testing.T) {
	dir := t.TempDir()
	h, err := New(dir, Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "terminal-owner"}, "")
	invoke := func(args map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, err := h.Execute(ctx, llm.ToolCall{Name: "terminal", Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	id := invoke(map[string]any{"action": "open"})["session_id"].(string)
	send := func(text string) { invoke(map[string]any{"action": "send", "session_id": id, "text": text}) }
	await := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			v := invoke(map[string]any{"action": "read", "session_id": id, "wait_ms": 20})
			if strings.Contains(v["output"].(string), want) {
				return
			}
		}
		t.Fatalf("terminal did not produce %q", want)
	}
	send("stty -echo\nexport NATIVE_TEST=retained\nprintf 'state=%s\\n' \"$NATIVE_TEST\"\n")
	await("state=retained")
	send("sleep 60 & echo $! > child.pid; sleep 60\n")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "child.pid")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	invoke(map[string]any{"action": "interrupt", "session_id": id})
	send("printf 'after-interrupt=%s\\n' \"$NATIVE_TEST\"\n")
	await("after-interrupt=retained")
	invoke(map[string]any{"action": "resize", "session_id": id, "rows": 30, "cols": 90})
	invoke(map[string]any{"action": "close", "session_id": id})
	pid, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
	// A killed child may briefly be a zombie until reaped; neither is live work.
	if stat, err := os.ReadFile("/proc/" + strings.TrimSpace(string(pid)) + "/stat"); err == nil {
		end := strings.LastIndexByte(string(stat), ')')
		if !strings.HasPrefix(strings.TrimSpace(string(stat[end+1:])), "Z") {
			t.Fatal("terminal child survived close")
		}
	}
	raw, _ := json.Marshal(map[string]string{"action": "read", "session_id": id})
	if _, err := h.Execute(ctx, llm.ToolCall{Name: "terminal", Arguments: raw}); err == nil {
		t.Fatal("closed handle remains usable")
	}
}

func TestTerminalOutputOffsetsAndExpiryAreBounded(t *testing.T) {
	s := &terminalSession{id: "terminal-test", status: "running", start: 100, output: []byte("0123456789")}
	raw, err := s.observe(0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"output_truncated":true`) || !strings.Contains(raw, `"next_offset":110`) {
		t.Fatal(raw)
	}
	if _, err := s.observe(111); err == nil {
		t.Fatal("future offset accepted")
	}
}

func TestTerminalUnconsumedInputHasBoundedWait(t *testing.T) {
	h, err := New(t.TempDir(), Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "input-owner"}, "")
	invoke := func(args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return h.Execute(ctx, llm.ToolCall{Name: "terminal", Arguments: raw})
	}
	out, err := invoke(map[string]any{"action": "open"})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(map[string]any{"action": "send", "session_id": v.ID, "text": "stty -icanon -echo; printf input-ready; sleep 60\n", "wait_ms": 100}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	blocked := false
	for i := 0; i < 10; i++ {
		_, err := invoke(map[string]any{"action": "send", "session_id": v.ID, "text": strings.Repeat("x", 8192)})
		if err != nil {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("fixture never filled terminal input")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("unconsumed terminal input blocked without a deadline")
	}
}
