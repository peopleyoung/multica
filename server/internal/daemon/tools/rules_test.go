package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRulesFollowDirectoryPrecedenceAndRejectEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"AGENTS.md": "root rule", "src/AGENTS.md": "nested rule"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	host, err := New(root, Permissions{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	got, err := host.Rules(context.Background(), "src")
	if err != nil || strings.Index(got, "root rule") < 0 || strings.Index(got, "nested rule") < strings.Index(got, "root rule") {
		t.Fatalf("%s: %v", got, err)
	}
	for _, path := range []string{"../", "/tmp"} {
		if _, err := host.Rules(context.Background(), path); err == nil {
			t.Fatal("escaped rules root")
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("outside secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err == nil {
		if _, err := host.Rules(context.Background(), "escape"); err == nil {
			t.Fatal("followed rules outside root")
		}
	}
}
