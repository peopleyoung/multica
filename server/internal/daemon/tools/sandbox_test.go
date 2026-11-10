package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxRefusesMissingEnforcementAndConflictingWrites(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	limits := ServiceLimits{MaxActive: 1, MaxRecords: 4, LogBytes: 1 << 20, Sandbox: SandboxPolicy{Mode: "workspace_write"}}
	if host, err := NewServiceHost(t.TempDir(), nil, t.TempDir(), limits); err == nil {
		host.Close()
		t.Fatal("silently fell back to unconfined execution")
	}
	limits.Sandbox.Mode = "read_only"
	limits.WriteFiles = true
	if host, err := NewServiceHost(t.TempDir(), nil, t.TempDir(), limits); err == nil {
		host.Close()
		t.Fatal("read-only sandbox granted file writes")
	}
}

func TestSandboxFunctionalConfinement(t *testing.T) {
	dir := t.TempDir()
	h, err := New(dir, Permissions{}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	policy := SandboxPolicy{Mode: "read_only"}
	if err := h.configureSandbox(context.Background(), policy); err != nil {
		t.Skipf("kernel/runner cannot enforce confinement (correctly refused): %v", err)
	}
	cmd, err := policy.command(dir, []string{"SECRET=must-not-inherit"}, "test ! -e /home && test -z \"$SECRET\" && ! touch forbidden", false)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox read-only, visibility or environment policy failed: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("sandbox write escaped")
	}
	cmd, err = SandboxPolicy{Mode: "workspace_write"}.command(dir, nil, "printf allowed > allowed && test ! -e /home", false)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("workspace sandbox failed: %s %v", strings.TrimSpace(string(out)), err)
	}
}
