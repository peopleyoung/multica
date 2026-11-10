package daemon

import (
	"io"
	"log/slog"
	"strings"
	"testing"

)

func TestNativeModeNeverProbesExternalCLIs(t *testing.T) {
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_NATIVE_RUNTIME", "")
	t.Setenv("MULTICA_NATIVE_ALLOW_WRITE", "")
	t.Setenv("MULTICA_NATIVE_ALLOW_SHELL", "")
	// No fake binary is staged: even executable lookup must be bypassed.
	cfg, err := LoadConfig(Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NativeRuntime || len(cfg.Agents) != 1 || cfg.NativeAllowWrite || cfg.NativeAllowShell {
		t.Fatalf("unexpected native config: %+v", cfg.Agents)
	}
	d := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runtimes := d.nativeRuntimes()
	if len(runtimes) != 1 || runtimes[0]["type"] != "native" {
		t.Fatalf("unexpected runtimes: %+v", runtimes)
	}
}

func TestNativeToolsDoNotInheritServerCredentials(t *testing.T) {
	env := nativeToolEnv([]string{"MULTICA_TOKEN=owner", "MULTICA_LLM_API_KEY=deployment-secret", "OPENAI_API_KEY=ambient-secret", "PATH=/usr/bin"}, map[string]string{"MULTICA_TOKEN": "task-credential", "MULTICA_WORKSPACE_ID": "workspace", "MULTICA_LLM_API_KEY": "forbidden"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "forbidden") || strings.Contains(joined, "owner") || !strings.Contains(joined, "MULTICA_TOKEN=task-credential") {
		t.Fatal("native tool environment leaked unrelated credentials")
	}
}
