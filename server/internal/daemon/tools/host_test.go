package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func invoke(t *testing.T, h *Host, name string, args map[string]string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return h.Execute(context.Background(), llm.ToolCall{ID: "test", Name: name, Arguments: raw})
}

func TestHostFileLoopAndConfinement(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := New(dir, Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := invoke(t, h, "apply_patch", map[string]string{"path": "a.txt", "old_text": "", "new_text": "hello world"}); err != nil {
		t.Fatal(err)
	}
	if text, err := invoke(t, h, "read_file", map[string]string{"path": "a.txt"}); err != nil || text != "hello world" {
		t.Fatalf("%q %v", text, err)
	}
	if _, err := invoke(t, h, "apply_patch", map[string]string{"path": "a.txt", "old_text": "world", "new_text": "Multica"}); err != nil {
		t.Fatal(err)
	}
	if text, err := invoke(t, h, "search_text", map[string]string{"path": ".", "text": "Multica"}); err != nil || !strings.Contains(text, "a.txt:1:hello Multica") {
		t.Fatalf("%q %v", text, err)
	}
	if _, err := invoke(t, h, "apply_patch", map[string]string{"path": "a.txt", "old_text": "", "new_text": "overwrite"}); err == nil {
		t.Fatal("create overwrote an existing file")
	}
	for _, path := range []string{"../private.txt", filepath.Join(outside, "private.txt")} {
		if _, err := invoke(t, h, "read_file", map[string]string{"path": path}); err == nil {
			t.Fatal("escaped work directory")
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
			t.Fatal(err)
		}
		if _, err := invoke(t, h, "read_file", map[string]string{"path": "escape/private.txt"}); err == nil {
			t.Fatal("symlink escaped work directory")
		}
		if _, err := invoke(t, h, "apply_patch", map[string]string{"path": "escape/private.txt", "old_text": "secret", "new_text": "changed"}); err == nil {
			t.Fatal("patch escaped through a symlink")
		}
	}
}

func TestHostPermissionsArgumentsAndOutputLimit(t *testing.T) {
	dir := t.TempDir()
	h, err := New(dir, Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, name := range []string{"shell", "apply_patch"} {
		if _, err := h.Execute(context.Background(), llm.ToolCall{Name: name, Arguments: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("unauthorized tool executed")
		}
		for _, def := range h.Definitions() {
			if def.Name == name {
				t.Fatal("unauthorized tool advertised")
			}
		}
	}
	for _, args := range []string{`{"path":null}`, `{"path":1}`, `{"path":".","extra":"value"}`, `[]`} {
		if _, err := h.Execute(context.Background(), llm.ToolCall{Name: "read_file", Arguments: json.RawMessage(args)}); err == nil {
			t.Fatal("invalid tool arguments accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(strings.Repeat("x", outputLimit+100)), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := invoke(t, h, "read_file", map[string]string{"path": "long.txt"})
	if err != nil || !strings.Contains(text, `"has_more":true`) || len(text) > outputLimit+512 {
		t.Fatalf("unbounded output: %d %v", len(text), err)
	}
}

func TestHostShellAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture command")
	}
	h, err := New(t.TempDir(), Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if out, err := invoke(t, h, "shell", map[string]string{"command": "printf hello"}); err != nil || !strings.Contains(out, `"output":"hello"`) {
		t.Fatalf("%q %v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = h.Execute(ctx, llm.ToolCall{ID: "cancelled", Name: "shell", Arguments: json.RawMessage(`{"command":"sleep 30"}`)})
	if err == nil || time.Since(start) > 8*time.Second {
		t.Fatalf("cancellation did not stop process tree: %v", err)
	}
}

func TestMakeDirectoryCreatesNestedArtifactsWithoutShell(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	h, err := New(dir, Permissions{WriteFiles: true}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for range 2 {
		if _, err := invoke(t, h, "make_directory", map[string]string{"path": "artifacts/reports"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := invoke(t, h, "apply_patch", map[string]string{"path": "artifacts/reports/result.txt", "old_text": "", "new_text": "verified"}); err != nil {
		t.Fatal(err)
	}
	if raw, err := h.ReadArtifact("artifacts/reports/result.txt"); err != nil || string(raw) != "verified" {
		t.Fatal("artifact creation failed")
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
			t.Fatal(err)
		}
		if _, err := invoke(t, h, "make_directory", map[string]string{"path": "escape/forbidden"}); err == nil {
			t.Fatal("directory escaped through symlink")
		}
		if _, err := os.Stat(filepath.Join(outside, "forbidden")); !os.IsNotExist(err) {
			t.Fatal("outside directory was created")
		}
	}
	readonly, err := New(dir, Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if _, err := invoke(t, readonly, "make_directory", map[string]string{"path": "forbidden"}); err == nil {
		t.Fatal("read-only host created directory")
	}
}
