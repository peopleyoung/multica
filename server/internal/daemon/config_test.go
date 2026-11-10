package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestPatternsFromEnv_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("MULTICA_GC_ARTIFACT_PATTERNS", "")
	defaults := []string{"node_modules", ".next", ".turbo"}
	got := patternsFromEnv("MULTICA_GC_ARTIFACT_PATTERNS", defaults)
	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("expected defaults %v, got %v", defaults, got)
	}
	// Ensure callers get a copy, not a shared backing array.
	got[0] = "mutated"
	if defaults[0] == "mutated" {
		t.Fatal("patternsFromEnv must not return a slice aliased with defaults")
	}
}

// A localhost server URL is not the official cloud host, so this exercises the
// self-host branch of defaultGCCompletedTaskTTL: retention stays unbounded until
// an operator opts in, and a daemon upgrade never starts deleting on its own.
func TestLoadConfig_CompletedTaskTTLDefaultsDisabledOnSelfHostAndReadsEnv(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "")

	overrides := Overrides{
		ServerURL:      "http://localhost:0",
		WorkspacesRoot: t.TempDir(),
	}
	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with default completed-task TTL: %v", err)
	}
	if cfg.GCCompletedTaskTTL != 0 {
		t.Fatalf("GCCompletedTaskTTL = %s, want disabled", cfg.GCCompletedTaskTTL)
	}

	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "36h")
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with completed-task TTL: %v", err)
	}
	if cfg.GCCompletedTaskTTL != 36*time.Hour {
		t.Fatalf("GCCompletedTaskTTL = %s, want 36h", cfg.GCCompletedTaskTTL)
	}

	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "not-a-duration")
	if _, err := LoadConfig(overrides); err == nil || !strings.Contains(err.Error(), "MULTICA_GC_COMPLETED_TASK_TTL") {
		t.Fatalf("LoadConfig invalid completed-task TTL error = %v, want named validation error", err)
	}
}

func TestLoadConfig_WSClaimPollIntervalPrecedence(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_DAEMON_WS_CLAIM_POLL_INTERVAL", "")
	base := Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()}

	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatalf("LoadConfig default: %v", err)
	}
	if cfg.WSClaimPollInterval != DefaultWSClaimPollInterval {
		t.Fatalf("default WSClaimPollInterval = %s, want %s", cfg.WSClaimPollInterval, DefaultWSClaimPollInterval)
	}

	t.Setenv("MULTICA_DAEMON_WS_CLAIM_POLL_INTERVAL", "75s")
	cfg, err = LoadConfig(base)
	if err != nil {
		t.Fatalf("LoadConfig env: %v", err)
	}
	if cfg.WSClaimPollInterval != 75*time.Second {
		t.Fatalf("env WSClaimPollInterval = %s, want 75s", cfg.WSClaimPollInterval)
	}

	base.WSClaimPollInterval = 2 * time.Minute
	cfg, err = LoadConfig(base)
	if err != nil {
		t.Fatalf("LoadConfig override: %v", err)
	}
	if cfg.WSClaimPollInterval != 2*time.Minute {
		t.Fatalf("override WSClaimPollInterval = %s, want 2m", cfg.WSClaimPollInterval)
	}

	base.WSClaimPollInterval = 0
	t.Setenv("MULTICA_DAEMON_WS_CLAIM_POLL_INTERVAL", "0s")
	if _, err := LoadConfig(base); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("LoadConfig zero WS claim poll error = %v, want positive-duration validation", err)
	}
}

func TestLoadConfig_CompletedTaskTTLDefaultsBoundedOnOfficialCloud(t *testing.T) {
	stageFakeAgent(t)
	redirectTestHome(t, t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "")

	overrides := Overrides{
		ServerURL:      "https://" + officialCloudHost,
		WorkspacesRoot: t.TempDir(),
	}
	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig on official cloud: %v", err)
	}
	if cfg.GCCompletedTaskTTL != 14*24*time.Hour {
		t.Fatalf("GCCompletedTaskTTL = %s, want 14d on official cloud", cfg.GCCompletedTaskTTL)
	}

	// An explicit 0 has to win on cloud too — otherwise the only way back to the
	// previous retention behavior would be downgrading the daemon.
	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "0")
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with cloud opt-out: %v", err)
	}
	if cfg.GCCompletedTaskTTL != 0 {
		t.Fatalf("GCCompletedTaskTTL = %s, want an explicit 0 to disable the cloud default", cfg.GCCompletedTaskTTL)
	}

	t.Setenv("MULTICA_GC_COMPLETED_TASK_TTL", "36h")
	cfg, err = LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig with cloud override: %v", err)
	}
	if cfg.GCCompletedTaskTTL != 36*time.Hour {
		t.Fatalf("GCCompletedTaskTTL = %s, want the env override to win on cloud", cfg.GCCompletedTaskTTL)
	}
}

func TestDefaultGCCompletedTaskTTLOnlyBoundsOfficialCloudHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		serverURL string
		want      time.Duration
	}{
		{"official cloud", "https://api.multica.ai", DefaultGCCompletedTaskTTLCloud},
		{"official cloud with port and path", "https://API.Multica.AI:443/api", DefaultGCCompletedTaskTTLCloud},
		// Staging and previews inherit the self-host value for the same reason
		// officialCloudHost excludes them from the auto-update default.
		{"staging", "https://api-staging.multica.ai", DefaultGCCompletedTaskTTLSelfHost},
		{"self-host", "https://multica.example.com", DefaultGCCompletedTaskTTLSelfHost},
		{"localhost", "http://localhost:8080", DefaultGCCompletedTaskTTLSelfHost},
		{"unparseable", "://nope", DefaultGCCompletedTaskTTLSelfHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultGCCompletedTaskTTL(tc.serverURL); got != tc.want {
				t.Fatalf("defaultGCCompletedTaskTTL(%q) = %s, want %s", tc.serverURL, got, tc.want)
			}
		})
	}
}

func TestRepoMaintenanceKillSwitchDefaultsOnAndCanDisable(t *testing.T) {
	t.Setenv("MULTICA_GC_REPO_MAINTENANCE_ENABLED", "")
	if !boolFromEnv("MULTICA_GC_REPO_MAINTENANCE_ENABLED", true) {
		t.Fatal("repo maintenance kill switch should default to enabled")
	}

	t.Setenv("MULTICA_GC_REPO_MAINTENANCE_ENABLED", "false")
	if boolFromEnv("MULTICA_GC_REPO_MAINTENANCE_ENABLED", true) {
		t.Fatal("repo maintenance kill switch should accept false")
	}
}

func TestPatternsFromEnv_DropsSeparatorBearingEntries(t *testing.T) {
	t.Setenv("MULTICA_GC_ARTIFACT_PATTERNS", "node_modules, .next ,foo/bar, ../etc, ,target")
	got := patternsFromEnv("MULTICA_GC_ARTIFACT_PATTERNS", nil)
	want := []string{"node_modules", ".next", "target"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

// lookPathInPath is a thin wrapper used by the test above; matches what
// exec.LookPath would do but lets the test be explicit about which call it's
// asserting against.
func lookPathInPath(name string) (string, error) {
	return exec.LookPath(name)
}

func TestIsOfficialCloudServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{"canonical cloud https", "https://api.multica.ai", true},
		{"canonical cloud with trailing slash stripped", "https://api.multica.ai/", true},
		{"canonical cloud case-insensitive", "https://API.Multica.AI", true},
		{"cloud over plain http (unusual but match host)", "http://api.multica.ai", true},
		{"localhost is self-host", "http://localhost:8080", false},
		{"loopback ip is self-host", "http://127.0.0.1:8080", false},
		{"lan ip is self-host", "http://192.168.0.28:8080", false},
		{"third-party host is self-host", "https://multica.example.com", false},
		// Staging / preview / future subdomains deliberately follow the
		// safer self-host default until explicitly opted in.
		{"multica.ai apex is not the api host", "https://multica.ai", false},
		{"staging subdomain is self-host", "https://staging.multica.ai", false},
		{"preview subdomain is self-host", "https://api-preview.multica.ai", false},
		// Malformed inputs must not falsely match.
		{"empty string is self-host", "", false},
		{"garbage string is self-host", "::not a url::", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOfficialCloudServer(tc.url); got != tc.want {
				t.Errorf("isOfficialCloudServer(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// stageFakeAgent writes an executable `claude` script into a temp dir and
// points PATH (and the daemon-id env var) so LoadConfig can run end-to-end
// without poking the host's real agent installation. Returns the staged PATH
// so tests that need to add their own dirs can extend it.
func stageFakeAgent(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell not available on Windows")
	}
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("MULTICA_DAEMON_ID", "11111111-1111-1111-1111-111111111111")
	// Clear any inherited env-var override so the test sees the URL-based
	// default, not whatever the developer happens to have exported.
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "")
	return binDir
}

// TestLoadConfig_AutoUpdateDefault_SelfHostOff is the regression guard for
// MUL-2381: a daemon pointed at any non-cloud server URL must default
// AutoUpdateEnabled to false, because self-host operators frequently run a
// fork and the upstream GitHub release would silently overwrite it.
func TestLoadConfig_AutoUpdateDefault_SelfHostOff(t *testing.T) {
	stageFakeAgent(t)
	cfg, err := LoadConfig(Overrides{
		ServerURL:      "http://localhost:8080",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpdateEnabled {
		t.Fatalf("AutoUpdateEnabled = true for self-host (localhost) server, want false")
	}
}

// TestLoadConfig_ToolWatchdogDefaultsToIdleWatchdog pins the merge: the
// in-flight-tool budget is no longer an independent constant, so an operator
// who raises the idle budget cannot accidentally leave tool calls capped at a
// lower, invisible ceiling.
func TestLoadConfig_ToolWatchdogDefaultsToIdleWatchdog(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("MULTICA_AGENT_IDLE_WATCHDOG", "")
	t.Setenv("MULTICA_AGENT_TOOL_WATCHDOG", "")

	load := func(t *testing.T) Config {
		t.Helper()
		cfg, err := LoadConfig(Overrides{
			ServerURL:      "http://localhost:8080",
			WorkspacesRoot: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg
	}

	cfg := load(t)
	if cfg.AgentIdleWatchdog != DefaultAgentIdleWatchdog {
		t.Fatalf("AgentIdleWatchdog = %s, want default %s", cfg.AgentIdleWatchdog, DefaultAgentIdleWatchdog)
	}
	if cfg.AgentToolWatchdog != cfg.AgentIdleWatchdog {
		t.Fatalf("AgentToolWatchdog = %s, want it to track AgentIdleWatchdog %s", cfg.AgentToolWatchdog, cfg.AgentIdleWatchdog)
	}

	// Raising only the idle budget must carry the tool budget with it.
	t.Setenv("MULTICA_AGENT_IDLE_WATCHDOG", "6h")
	cfg = load(t)
	if cfg.AgentIdleWatchdog != 6*time.Hour || cfg.AgentToolWatchdog != 6*time.Hour {
		t.Fatalf("idle=%s tool=%s, want both 6h", cfg.AgentIdleWatchdog, cfg.AgentToolWatchdog)
	}

	// An explicit tool override still wins, so "tools may run longer than the
	// model may think" stays expressible.
	t.Setenv("MULTICA_AGENT_TOOL_WATCHDOG", "12h")
	cfg = load(t)
	if cfg.AgentIdleWatchdog != 6*time.Hour {
		t.Fatalf("AgentIdleWatchdog = %s, want 6h", cfg.AgentIdleWatchdog)
	}
	if cfg.AgentToolWatchdog != 12*time.Hour {
		t.Fatalf("AgentToolWatchdog = %s, want 12h from env", cfg.AgentToolWatchdog)
	}

	// Zero keeps its distinct meaning: never force-stop while a tool is in
	// flight. It must NOT be re-derived from the idle budget.
	t.Setenv("MULTICA_AGENT_TOOL_WATCHDOG", "0")
	cfg = load(t)
	if cfg.AgentToolWatchdog != 0 {
		t.Fatalf("AgentToolWatchdog = %s, want 0 from env", cfg.AgentToolWatchdog)
	}

	// Disabling the suite disables both.
	t.Setenv("MULTICA_AGENT_IDLE_WATCHDOG", "0")
	t.Setenv("MULTICA_AGENT_TOOL_WATCHDOG", "")
	cfg = load(t)
	if cfg.AgentIdleWatchdog != 0 || cfg.AgentToolWatchdog != 0 {
		t.Fatalf("idle=%s tool=%s, want both 0", cfg.AgentIdleWatchdog, cfg.AgentToolWatchdog)
	}
}

// TestLoadConfig_AutoUpdateDefault_CloudOn confirms the symmetric case: a
// daemon pointed at Multica's hosted cloud keeps the historical opt-in
// auto-update default. We pass the WSS form of the URL to also exercise that
// NormalizeServerBaseURL maps it through to the http host the detector
// inspects.
func TestLoadConfig_AutoUpdateDefault_CloudOn(t *testing.T) {
	stageFakeAgent(t)
	cfg, err := LoadConfig(Overrides{
		ServerURL:      "wss://api.multica.ai/ws",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AutoUpdateEnabled {
		t.Fatalf("AutoUpdateEnabled = false for Multica Cloud server, want true")
	}
}

// TestLoadConfig_AutoUpdateEnv_ForcesOnForSelfHost lets a self-host operator
// re-enable auto-update via env var, overriding the new conservative default.
func TestLoadConfig_AutoUpdateEnv_ForcesOnForSelfHost(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "true")
	cfg, err := LoadConfig(Overrides{
		ServerURL:      "http://localhost:8080",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AutoUpdateEnabled {
		t.Fatalf("AutoUpdateEnabled = false after explicit MULTICA_DAEMON_AUTO_UPDATE=true, want true")
	}
}

// TestLoadConfig_AutoUpdateEnv_ForcesOffForCloud covers the inverse: a cloud
// user can still opt out via env var.
func TestLoadConfig_AutoUpdateEnv_ForcesOffForCloud(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "false")
	cfg, err := LoadConfig(Overrides{
		ServerURL:      "https://api.multica.ai",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpdateEnabled {
		t.Fatalf("AutoUpdateEnabled = true after explicit MULTICA_DAEMON_AUTO_UPDATE=false, want false")
	}
}

// TestLoadConfig_AutoUpdate_NoFlagWinsOverCloudDefault keeps the legacy CLI
// flag working: --no-auto-update (translated into overrides.DisableAutoUpdate)
// forces auto-update off even when the cloud default and env var would enable.
func TestLoadConfig_AutoUpdate_NoFlagWinsOverCloudDefault(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "true")
	cfg, err := LoadConfig(Overrides{
		ServerURL:         "https://api.multica.ai",
		WorkspacesRoot:    t.TempDir(),
		DisableAutoUpdate: true,
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpdateEnabled {
		t.Fatalf("AutoUpdateEnabled = true with --no-auto-update set; flag must win")
	}
}

// TestLoadConfig_AutoReload_DefaultsOnEvenForSelfHost is the review's first
// product decision, encoded: "don't pull new versions from GitHub" and "follow
// the binary I replaced myself" are separate concerns. Self-host defaults
// auto-update OFF (MUL-2381) because upgrading a fork from an upstream release
// would clobber it — an argument that says nothing about a binary the operator
// installed by hand.
func TestLoadConfig_AutoReload_DefaultsOnEvenForSelfHost(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "")
	t.Setenv("MULTICA_DAEMON_AUTO_RELOAD", "")
	cfg, err := LoadConfig(Overrides{
		ServerURL:      "http://localhost:8080",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AutoUpdateEnabled {
		t.Fatalf("AutoUpdateEnabled = true for self-host, want false (MUL-2381)")
	}
	if !cfg.AutoReloadEnabled {
		t.Fatalf("AutoReloadEnabled = false for self-host; the on-disk watcher must not ride on the auto-update default")
	}
}

// TestLoadConfig_AutoReload_NotGatedOnAutoUpdateEnv is the same decoupling at
// the env layer: turning GitHub polling off must not silently stop the daemon
// from following a hand-installed binary.
func TestLoadConfig_AutoReload_NotGatedOnAutoUpdateEnv(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("MULTICA_DAEMON_AUTO_UPDATE", "false")
	t.Setenv("MULTICA_DAEMON_AUTO_RELOAD", "")
	cfg, err := LoadConfig(Overrides{
		ServerURL:      "https://api.multica.ai",
		WorkspacesRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AutoReloadEnabled {
		t.Fatalf("MULTICA_DAEMON_AUTO_UPDATE=false disabled auto-reload; the two switches are independent")
	}
}

// TestLoadConfig_AutoReload_OffSwitches pins the escape hatch across both layers
// it can be turned off from. The config-file layer resolves to
// overrides.DisableAutoReload in cmd_daemon.go, so it is covered by the same
// assertion as the flag.
func TestLoadConfig_AutoReload_OffSwitches(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		overrides Overrides
	}{
		{name: "env false", env: "false"},
		{name: "env 0", env: "0"},
		{name: "env off", env: "off"},
		{name: "flag or config file", env: "", overrides: Overrides{DisableAutoReload: true}},
		{name: "flag beats a truthy env", env: "true", overrides: Overrides{DisableAutoReload: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stageFakeAgent(t)
			t.Setenv("MULTICA_DAEMON_AUTO_RELOAD", tc.env)
			overrides := tc.overrides
			overrides.ServerURL = "https://api.multica.ai"
			overrides.WorkspacesRoot = t.TempDir()
			cfg, err := LoadConfig(overrides)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.AutoReloadEnabled {
				t.Fatalf("AutoReloadEnabled = true, want false")
			}
		})
	}
}

func pinNonCodexAgentsToMissingPaths(t *testing.T) {
	t.Helper()
	missingDir := t.TempDir()
	for _, name := range []string{
		"MULTICA_CLAUDE_PATH",
		"MULTICA_OPENCODE_PATH",
		"MULTICA_OPENCLAW_PATH",
		"MULTICA_HERMES_PATH",
		"MULTICA_PI_PATH",
		"MULTICA_CURSOR_PATH",
		"MULTICA_COPILOT_PATH",
		"MULTICA_KIMI_PATH",
		"MULTICA_REASONIX_PATH",
		"MULTICA_DSH_PATH",
		"MULTICA_KIRO_PATH",
		"MULTICA_GROK_PATH",
	} {
		t.Setenv(name, filepath.Join(missingDir, strings.ToLower(name)))
	}
}

// =============================================================================
// CLI config Backends.OpenClaw overrides (issue #3875)
// =============================================================================

// writeCLIConfigForProfile is a minimal helper for the override tests:
// stages a HOME, writes a config.json under the given profile (empty profile
// = default), and returns the resolved path so tests can assert against it.
func writeCLIConfigForProfile(t *testing.T, profile string, cfg cli.CLIConfig) {
	t.Helper()
	tmp := t.TempDir()
	redirectTestHome(t, tmp)
	if err := cli.SaveCLIConfigForProfile(cfg, profile); err != nil {
		t.Fatalf("write cli config: %v", err)
	}
}

// agentKeys is a tiny helper to make agent-map missing-key error messages
// readable. Returns sorted keys.
func agentKeys(m map[string]AgentEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
