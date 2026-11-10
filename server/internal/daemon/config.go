package daemon

import (
	"fmt"

	"net/url"
	"os"

	"path/filepath"

	"strings"
	"time"
)

const (
	DefaultServerURL    = "ws://localhost:8080/ws"
	DefaultPollInterval = 30 * time.Second
	// DefaultWSClaimPollInterval is the upper bound for missed-event safety
	// polls while task availability and claims use a healthy WebSocket. The
	// poller applies downward-only jitter before each sleep.
	DefaultWSClaimPollInterval = 3 * time.Minute
	DefaultHeartbeatInterval   = 15 * time.Second
	// DefaultAgentTimeout is the optional absolute wall-clock cap on a single
	// agent run. 0 = no cap: a run is bounded only by the inactivity watchdog
	// (DefaultAgentIdleWatchdog), so a session that keeps emitting events is
	// never killed merely for running long (MUL-3064). Operators who want a
	// hard ceiling for cost/resource control can set MULTICA_AGENT_TIMEOUT.
	DefaultAgentTimeout = 0
	// DefaultAgentIdleWatchdog is the per-task safety net that force-stops a
	// run when the backend has emitted no message for this long AND its
	// message queue is empty. Backends like Claude Code can hang indefinitely
	// on a stuck child process (e.g. `docker ps` against a frozen dockerd),
	// in which case `cmd.Wait()` never returns. With no wall-clock cap
	// (DefaultAgentTimeout = 0) such a run would otherwise sit at "running"
	// forever, so this watchdog is its sole liveness net.
	//
	// The budget answers one question: how long may the LONGEST legitimate
	// silent step take? Not "how fast do we want to notice a hang". Those
	// two costs are wildly asymmetric — force-stopping a healthy run throws
	// away the work and flips the issue to blocked, while noticing a hang
	// late only holds a concurrency slot. So the value tracks the legitimate
	// end: a single RFC-length assistant message (MUL-2300, which moved this
	// from 5 min to 30 min), a subagent, a full test suite. 30 min still cut
	// real work short, so this is 2h.
	//
	// Set MULTICA_AGENT_IDLE_WATCHDOG=0 to disable the whole watchdog suite.
	DefaultAgentIdleWatchdog              = 2 * time.Hour
	DefaultRuntimeName                    = "Local Agent"
	DefaultWorkspaceBootstrapSyncInterval = 30 * time.Second
	DefaultWorkspaceLegacySyncInterval    = 5 * time.Minute
	DefaultWorkspaceSyncInterval          = 30 * time.Minute
	DefaultWorkspaceSyncMaxBackoff        = 30 * time.Minute
	DefaultHealthPort                     = 19514
	DefaultMaxConcurrentTasks             = 20
	DefaultGCInterval                     = 2 * time.Hour
	DefaultGCTTL                          = 24 * time.Hour      // 1 day — AI-coding issues rarely stay open long
	DefaultGCCompletedTaskTTLCloud        = 14 * 24 * time.Hour // 14 days — Multica Cloud bounds completed issue-task env retention by default; see defaultGCCompletedTaskTTL
	DefaultGCCompletedTaskTTLSelfHost     = 0                   // disabled — self-host keeps every completed env until its issue goes terminal, unless an operator opts in
	DefaultGCOrphanTTL                    = 72 * time.Hour      // 3 days — orphans with no meta (crashes, pre-GC leftovers)
	DefaultGCArtifactTTL                  = 12 * time.Hour      // 12h — drop regenerable artifacts once a task has been completed this long
	DefaultGCRepoTTL                      = 30 * 24 * time.Hour // 30 days — evict a bare repo cache no task has checked out this long
	// DefaultGCTaskTempLegacyTTL is 0 — disabled. Per-task temp dirs left by a
	// daemon predating the temp dir execution lock carry no liveness signal at
	// all, and age cannot supply one: a task may legitimately run for weeks
	// (DefaultAgentTimeout is 0), a pre-lock daemon on another profile can
	// still be running one, and every daemon on the machine shares the temp
	// base. Reclaiming those is therefore an operator's explicit call, made
	// when no pre-lock daemon is running here. Dirs that DO carry the lock are
	// reclaimed on liveness and never touch this knob.
	DefaultGCTaskTempLegacyTTL     = time.Duration(0)
	DefaultAutoUpdateCheckInterval = 6 * time.Hour // how often the daemon polls GitHub for a newer CLI release
)

// DefaultGCArtifactPatterns lists basename matches that the GC loop treats as
// regenerable build artifacts. Kept conservative: only directories that are
// always cheap to recreate (`pnpm install`, `next build`, `turbo build`). Things
// like `dist/`, `build/`, `.cache/` or `.venv/` may legitimately hold source or
// release output in some repos and are NOT included by default — set
// MULTICA_GC_ARTIFACT_PATTERNS to extend the list per deployment.
var DefaultGCArtifactPatterns = []string{"node_modules", ".next", ".turbo"}

// Config holds all daemon configuration.
type Config struct {
	NativeRuntime            bool
	NativeAllowWrite         bool
	NativeAllowShell         bool
	NativeHTTPOrigins        []string
	ServerBaseURL            string
	DaemonID                 string
	LegacyDaemonIDs          []string // historical daemon_ids this machine may have registered under; reported at register time so the server can merge old runtime rows
	DeviceName               string
	RuntimeName              string
	CLIVersion               string                // multica CLI version (e.g. "0.1.13")
	CLICommit                string                // full build revision embedded in the running CLI; empty/unknown if unavailable
	LaunchedBy               string                // "desktop" when spawned by the Electron app, empty for standalone
	Profile                  string                // profile name (empty = default)
	Agents                   map[string]AgentEntry // contains the native runtime descriptor
	WorkspacesRoot           string                // base path for execution envs (default: ~/multica_workspaces)
	KeepEnvAfterTask         bool                  // preserve env after task for debugging
	HealthPort               int                   // local HTTP port for health checks (default: 19514)
	MaxConcurrentTasks       int                   // max tasks running in parallel (default: 20)
	GCEnabled                bool                  // enable periodic workspace garbage collection (default: true)
	GCInterval               time.Duration         // how often the GC loop runs (default: 2h)
	GCTTL                    time.Duration         // clean dirs whose issue is done/cancelled and updated_at < now()-TTL (default: 24h)
	GCCompletedTaskTTL       time.Duration         // fully clean inactive issue-task envs completed at least this long ago, regardless of parent issue status (default: 14d on Multica Cloud, 0/disabled elsewhere; local_directory envs are never fully removed)
	GCOrphanTTL              time.Duration         // clean orphan dirs with no meta, or dirs whose issue gc-check returns 404, once they exceed this age (default: 72h). The 404 path uses the same TTL — a scoped-down token can't instantly wipe live workspaces.
	GCArtifactTTL            time.Duration         // once a task has been completed for at least this long, drop regenerable artifacts: pattern-matched build outputs when the parent record keeps the directory (an open issue), and the exact daemon-managed Codex cache for every task kind (default: 12h, set 0 to disable both)
	GCArtifactPatterns       []string              // basename patterns whose subtrees are removed during artifact cleanup (default: node_modules, .next, .turbo)
	GCRepoTTL                time.Duration         // evict a cached bare repo under .repos once no task has created a worktree from it for this long, it has no worktrees left, and it is no longer attached to any watched workspace (default: 30d, set 0 to disable)
	GCRepoMaintenanceEnabled bool                  // run reflog expiry and git gc after stale agent refs are removed (default: true; disable independently as an operational kill switch)
	GCTaskTempLegacyTTL      time.Duration         // reclaim a per-task temp dir (<temp base>/multica-task-*) that carries no execution lock — i.e. left by a daemon predating the lock — once nothing inside it has been touched for this long. Dirs that DO carry the lock are reclaimed on liveness, never on age, so this knob does not apply to them. Neither does it reclaim a dir holding no task content — an old empty leftover, or a shell left by a daemon that died between creating the dir and publishing its lock — because holding no content is exactly what a dir currently being published looks like (default: 0, disabled — see DefaultGCTaskTempLegacyTTL)
	AutoUpdateEnabled        bool                  // periodically check for a newer CLI release and self-update when idle (default: true on Multica Cloud, false on self-host)
	AutoUpdateCheckInterval  time.Duration         // how often the auto-update loop polls for a new release (default: 6h)
	AutoReloadEnabled        bool                  // restart when the multica binary on disk no longer matches the running version (default: true for CLI-launched daemons)
	PollInterval             time.Duration
	WSClaimPollInterval      time.Duration // upper bound for healthy WS batch-claim safety polls; actual sleeps use downward-only jitter
	HeartbeatInterval        time.Duration
	AgentTimeout             time.Duration
	AgentIdleWatchdog        time.Duration // force-stop a run when the backend goes silent this long with an empty queue (0 = disabled)
	AgentToolWatchdog        time.Duration // force-stop a run when a single tool call stays in flight (silent) this long (0 = never force-stop during a tool call, which now also covers a live Cursor background shell); defaults to AgentIdleWatchdog, so operators tune one number unless they deliberately want a wider tool budget

}

// Overrides allows CLI flags to override environment variables and defaults.
// Zero values are ignored and the env/default value is used instead.
type Overrides struct {
	ServerURL           string
	WorkspacesRoot      string
	PollInterval        time.Duration
	WSClaimPollInterval time.Duration
	HeartbeatInterval   time.Duration
	// AgentTimeout is a pointer so an explicit `--agent-timeout 0` (no cap) is
	// distinguishable from "flag not passed". nil = use env/default.
	AgentTimeout       *time.Duration
	MaxConcurrentTasks int
	DaemonID           string
	DeviceName         string
	RuntimeName        string
	Profile            string // profile name (empty = default)
	HealthPort         int    // health check port (0 = use default)
	// AllowNoAgents is reserved for read-only local configuration probes. Daemon
	// startup keeps the default false and still refuses to run with no agent CLI.
	AllowNoAgents bool
	// DisableAutoUpdate, when true, forces the auto-update poller off. There
	// is no symmetric "force on" override because the env/default already
	// resolves to enabled; the flag exists so users can opt out from the CLI.
	DisableAutoUpdate       bool
	AutoUpdateCheckInterval time.Duration // 0 = use env/default
	// DisableAutoReload, when true, forces the on-disk version watcher off.
	// Single-direction for the same reason as DisableAutoUpdate: the
	// env/default already resolves to enabled.
	DisableAutoReload bool
}

// LoadConfig builds the daemon configuration from environment variables
// and optional CLI flag overrides.
func LoadConfig(overrides Overrides) (Config, error) {
	// Server URL: override > env > default
	rawServerURL := envOrDefault("MULTICA_SERVER_URL", DefaultServerURL)
	if overrides.ServerURL != "" {
		rawServerURL = overrides.ServerURL
	}
	serverBaseURL, err := NormalizeServerBaseURL(rawServerURL)
	if err != nil {
		return Config{}, err
	}

	for _, key := range []string{"MULTICA_NATIVE_RUNTIME", "MULTICA_NATIVE_ALLOW_WRITE", "MULTICA_NATIVE_ALLOW_SHELL"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
		case "", "false", "0", "no", "off", "true", "1", "yes", "on":
		default:
			return Config{}, fmt.Errorf("%s must be a boolean", key)
		}
	}
	if !boolFromEnv("MULTICA_NATIVE_RUNTIME", true) {
		return Config{}, fmt.Errorf("external AI runtimes have been retired; remove MULTICA_NATIVE_RUNTIME=false and migrate bindings")
	}
	agents := map[string]AgentEntry{"native": {}}
	// Host info
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "local-machine"
	}

	// Durations: override > env > default
	pollInterval, err := durationFromEnv("MULTICA_DAEMON_POLL_INTERVAL", DefaultPollInterval)
	if err != nil {
		return Config{}, err
	}
	if overrides.PollInterval > 0 {
		pollInterval = overrides.PollInterval
	}
	wsClaimPollInterval, err := durationFromEnv("MULTICA_DAEMON_WS_CLAIM_POLL_INTERVAL", DefaultWSClaimPollInterval)
	if err != nil {
		return Config{}, err
	}
	if overrides.WSClaimPollInterval > 0 {
		wsClaimPollInterval = overrides.WSClaimPollInterval
	}
	if wsClaimPollInterval <= 0 {
		return Config{}, fmt.Errorf("MULTICA_DAEMON_WS_CLAIM_POLL_INTERVAL must be positive (got %s)", wsClaimPollInterval)
	}

	heartbeatInterval, err := durationFromEnv("MULTICA_DAEMON_HEARTBEAT_INTERVAL", DefaultHeartbeatInterval)
	if err != nil {
		return Config{}, err
	}
	if overrides.HeartbeatInterval > 0 {
		heartbeatInterval = overrides.HeartbeatInterval
	}

	agentTimeout, err := durationFromEnv("MULTICA_AGENT_TIMEOUT", DefaultAgentTimeout)
	if err != nil {
		return Config{}, err
	}
	if overrides.AgentTimeout != nil {
		agentTimeout = *overrides.AgentTimeout
	}

	// MULTICA_AGENT_IDLE_WATCHDOG=0 disables the per-task idle watchdog. We
	// route 0 through durationFromEnv so the operator can opt out without
	// patching the binary; any positive duration overrides DefaultAgentIdleWatchdog.
	agentIdleWatchdog, err := durationFromEnv("MULTICA_AGENT_IDLE_WATCHDOG", DefaultAgentIdleWatchdog)
	if err != nil {
		return Config{}, err
	}
	// The in-flight-tool budget defaults to the idle budget: the tool window
	// only ever existed because 30 min was too short for a real build/install/
	// test, and now that the idle budget is sized for the longest legitimate
	// silent step it already covers those.
	//
	// The derivation tracks in BOTH directions, which is the point of collapsing
	// this to one number. Raising MULTICA_AGENT_IDLE_WATCHDOG no longer leaves
	// tool calls silently pinned to the old ceiling — and lowering it now also
	// lowers the tool budget, where previously a shortened idle window left
	// tools at a separate, larger 2h. That second direction is a real behaviour
	// change for anyone who had deliberately shortened the idle window; the
	// override below is how they keep the two apart.
	//
	// MULTICA_AGENT_TOOL_WATCHDOG still overrides for the deliberate "tools may
	// run longer than the model may think" case, and 0 keeps its meaning: never
	// force-stop while a tool is in flight.
	//
	// A Cursor background shell counts as in flight for as long as the launched
	// process lives, not just until Cursor reports the launch complete — that is
	// what keeps a legitimate long background job on the tool budget instead of
	// the shorter idle one. The consequence at 0 is the same one a foreground
	// tool that never returns already has: such a run is bounded only by
	// MULTICA_AGENT_TIMEOUT, which is itself 0 by default. Operators who want a
	// stalled background shell bounded must leave this non-zero.
	agentToolWatchdog, err := durationFromEnv("MULTICA_AGENT_TOOL_WATCHDOG", agentIdleWatchdog)
	if err != nil {
		return Config{}, err
	}

	maxConcurrentTasks, err := intFromEnv("MULTICA_DAEMON_MAX_CONCURRENT_TASKS", DefaultMaxConcurrentTasks)
	if err != nil {
		return Config{}, err
	}
	if overrides.MaxConcurrentTasks > 0 {
		maxConcurrentTasks = overrides.MaxConcurrentTasks
	}

	// Profile
	profile := overrides.Profile

	// daemon_id resolution: override > env > persistent UUID on disk.
	// The persistent UUID is written once to `<profile-dir>/daemon.id` and
	// then reused forever so hostname drift (.local suffix, system rename,
	// mDNS state, profile switch) no longer mints a new runtime identity.
	// Callers may still pin a specific id via MULTICA_DAEMON_ID or the
	// override field (e.g. for tests or embedded environments).
	daemonID := strings.TrimSpace(os.Getenv("MULTICA_DAEMON_ID"))
	if overrides.DaemonID != "" {
		daemonID = overrides.DaemonID
	}
	if daemonID == "" {
		persisted, err := EnsureDaemonID(profile)
		if err != nil {
			return Config{}, fmt.Errorf("ensure daemon id: %w", err)
		}
		daemonID = persisted
	}
	// Historical daemon_ids derived from the current hostname/profile. The
	// server uses these at register time to merge any pre-UUID runtime rows
	// for this machine into the new UUID-keyed row and delete the stale ones.
	legacyDaemonIDs := LegacyDaemonIDs(host, profile)
	// Pre-change (#1220) daemon identity was stored per profile, which means
	// the same machine could end up with multiple leftover daemon.id files
	// — e.g. ~/.multica/daemon.id (default) plus ~/.multica/profiles/<x>/
	// daemon.id. Surface those UUIDs so the server can merge their runtime
	// rows into the canonical machine UUID. Fatal-free: a broken profiles
	// dir shouldn't block startup.
	if uuids, err := LegacyDaemonUUIDs(); err == nil {
		legacyDaemonIDs = append(legacyDaemonIDs, uuids...)
	}
	// Strip anything that collides with the resolved daemon_id (e.g. when
	// the user explicitly pins MULTICA_DAEMON_ID=<hostname>, or when the
	// canonical id was itself promoted from a pre-change profile file).
	legacyDaemonIDs = filterLegacyIDs(legacyDaemonIDs, daemonID)

	deviceName := envOrDefault("MULTICA_DAEMON_DEVICE_NAME", host)
	if overrides.DeviceName != "" {
		deviceName = overrides.DeviceName
	}

	runtimeName := envOrDefault("MULTICA_AGENT_RUNTIME_NAME", DefaultRuntimeName)
	if overrides.RuntimeName != "" {
		runtimeName = overrides.RuntimeName
	}

	// Workspaces root: override > env > default (~/multica_workspaces or ~/multica_workspaces_<profile>)
	workspacesRoot, err := ResolveWorkspacesRoot(profile, overrides.WorkspacesRoot)
	if err != nil {
		return Config{}, err
	}

	// Health port: override > default
	healthPort := DefaultHealthPort
	if overrides.HealthPort > 0 {
		healthPort = overrides.HealthPort
	}

	// Keep env after task: env > default (false)
	keepEnv := os.Getenv("MULTICA_KEEP_ENV_AFTER_TASK") == "true" || os.Getenv("MULTICA_KEEP_ENV_AFTER_TASK") == "1"

	// GC config: env > defaults
	gcEnabled := true
	if v := os.Getenv("MULTICA_GC_ENABLED"); v == "false" || v == "0" {
		gcEnabled = false
	}
	gcInterval, err := durationFromEnv("MULTICA_GC_INTERVAL", DefaultGCInterval)
	if err != nil {
		return Config{}, err
	}
	gcTTL, err := durationFromEnv("MULTICA_GC_TTL", DefaultGCTTL)
	if err != nil {
		return Config{}, err
	}
	gcCompletedTaskTTL, err := durationFromEnv("MULTICA_GC_COMPLETED_TASK_TTL", defaultGCCompletedTaskTTL(serverBaseURL))
	if err != nil {
		return Config{}, err
	}
	gcOrphanTTL, err := durationFromEnv("MULTICA_GC_ORPHAN_TTL", DefaultGCOrphanTTL)
	if err != nil {
		return Config{}, err
	}
	gcArtifactTTL, err := durationFromEnv("MULTICA_GC_ARTIFACT_TTL", DefaultGCArtifactTTL)
	if err != nil {
		return Config{}, err
	}
	gcTaskTempLegacyTTL, err := durationFromEnv("MULTICA_GC_TASK_TEMP_LEGACY_TTL", DefaultGCTaskTempLegacyTTL)
	if err != nil {
		return Config{}, err
	}
	gcRepoTTL, err := durationFromEnv("MULTICA_GC_REPO_TTL", DefaultGCRepoTTL)
	if err != nil {
		return Config{}, err
	}
	gcRepoMaintenanceEnabled := boolFromEnv("MULTICA_GC_REPO_MAINTENANCE_ENABLED", true)
	gcArtifactPatterns := patternsFromEnv("MULTICA_GC_ARTIFACT_PATTERNS", DefaultGCArtifactPatterns)

	// Auto-update config: default -> env override -> CLI override.
	//
	// Default is opt-in on Multica Cloud (api.multica.ai) and opt-out for
	// self-hosted instances. Self-host operators frequently run a fork with
	// their own patches, and silently upgrading their daemon to an upstream
	// GitHub release would clobber that work; they also commonly stay on an
	// older server build, which a fresh CLI may no longer talk to. Keeping
	// auto-update off by default for self-host avoids both footguns (MUL-2381).
	// Operators on either side can flip the default with MULTICA_DAEMON_AUTO_UPDATE.
	autoUpdateEnabled := boolFromEnv("MULTICA_DAEMON_AUTO_UPDATE", isOfficialCloudServer(serverBaseURL))
	if overrides.DisableAutoUpdate {
		autoUpdateEnabled = false
	}
	autoUpdateInterval, err := durationFromEnv("MULTICA_DAEMON_AUTO_UPDATE_INTERVAL", DefaultAutoUpdateCheckInterval)
	if err != nil {
		return Config{}, err
	}
	if overrides.AutoUpdateCheckInterval > 0 {
		autoUpdateInterval = overrides.AutoUpdateCheckInterval
	}

	// Auto-reload is deliberately NOT gated on autoUpdateEnabled. "Don't pull
	// new versions from GitHub" and "follow the binary I replaced myself" are
	// different concerns, and the self-host rationale for defaulting the former
	// off (don't clobber my fork) argues the opposite way for the latter: an
	// operator who installed a build by hand wants the daemon to run it.
	// Default on for every CLI-launched daemon; Desktop opts out at the loop.
	autoReloadEnabled := boolFromEnv("MULTICA_DAEMON_AUTO_RELOAD", true)
	if overrides.DisableAutoReload {
		autoReloadEnabled = false
	}

	return Config{
		ServerBaseURL:            serverBaseURL,
		DaemonID:                 daemonID,
		LegacyDaemonIDs:          legacyDaemonIDs,
		DeviceName:               deviceName,
		RuntimeName:              runtimeName,
		Profile:                  profile,
		Agents:                   agents,
		NativeRuntime:            true,
		NativeAllowWrite:         boolFromEnv("MULTICA_NATIVE_ALLOW_WRITE", false),
		NativeAllowShell:         boolFromEnv("MULTICA_NATIVE_ALLOW_SHELL", false),
		NativeHTTPOrigins:        strings.FieldsFunc(os.Getenv("MULTICA_NATIVE_HTTP_ORIGINS"), func(r rune) bool { return r == ',' }),
		WorkspacesRoot:           workspacesRoot,
		KeepEnvAfterTask:         keepEnv,
		GCEnabled:                gcEnabled,
		GCInterval:               gcInterval,
		GCTTL:                    gcTTL,
		GCCompletedTaskTTL:       gcCompletedTaskTTL,
		GCOrphanTTL:              gcOrphanTTL,
		GCArtifactTTL:            gcArtifactTTL,
		GCArtifactPatterns:       gcArtifactPatterns,
		GCRepoTTL:                gcRepoTTL,
		GCRepoMaintenanceEnabled: gcRepoMaintenanceEnabled,
		GCTaskTempLegacyTTL:      gcTaskTempLegacyTTL,
		AutoUpdateEnabled:        autoUpdateEnabled,
		AutoUpdateCheckInterval:  autoUpdateInterval,
		AutoReloadEnabled:        autoReloadEnabled,
		HealthPort:               healthPort,
		MaxConcurrentTasks:       maxConcurrentTasks,
		PollInterval:             pollInterval,
		WSClaimPollInterval:      wsClaimPollInterval,
		HeartbeatInterval:        heartbeatInterval,
		AgentTimeout:             agentTimeout,
		AgentIdleWatchdog:        agentIdleWatchdog,
		AgentToolWatchdog:        agentToolWatchdog,
	}, nil
}

// officialCloudHost is the hostname of Multica's hosted cloud. It's the only
// origin we treat as "official" for the auto-update default — staging,
// preview, and any future *.multica.ai subdomains are deliberately excluded
// so they inherit the safer self-host default until explicitly opted in.
const officialCloudHost = "api.multica.ai"

// isOfficialCloudServer reports whether the resolved server base URL points
// at Multica's hosted cloud. Used to pick defaults that are safe on
// infrastructure we operate but not on someone else's: auto-update (cloud
// users run a server that publishes the matching CLI release, so opt-in
// self-update is safe, while self-host users may run a fork or pin to an
// older server) and the completed-task retention TTL (see
// defaultGCCompletedTaskTTL). Matching is host-only and case-insensitive —
// port and path are ignored.
func isOfficialCloudServer(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), officialCloudHost)
}

// defaultGCCompletedTaskTTL picks the completed-task retention default from the
// deployment kind, the same way the auto-update default is picked.
//
// Full removal of a completed task environment is irreversible: it takes the
// checkout, .git (including work an agent left uncommitted), output/ and logs/
// with it. On Multica Cloud that trade is ours to make — we operate the nodes,
// a full disk is our incident rather than a user's, and unbounded retention has
// no operator watching it. On self-host the same default would turn a routine
// daemon upgrade into a silent deletion of data the operator never agreed to
// give up, so it stays disabled until they set MULTICA_GC_COMPLETED_TASK_TTL
// themselves. Either side can override in either direction; cloud disables it
// again with an explicit 0.
//
// Non-production cloud origins (staging, previews) fall to the self-host value
// along with everything else officialCloudHost excludes, and opt in explicitly
// if they want the bound.
func defaultGCCompletedTaskTTL(serverBaseURL string) time.Duration {
	if isOfficialCloudServer(serverBaseURL) {
		return DefaultGCCompletedTaskTTLCloud
	}
	return DefaultGCCompletedTaskTTLSelfHost
}

// NormalizeServerBaseURL converts a WebSocket or HTTP URL to a base HTTP URL.
func NormalizeServerBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid MULTICA_SERVER_URL: %w", err)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("MULTICA_SERVER_URL must use ws, wss, http, or https")
	}
	if u.Path == "/ws" {
		u.Path = ""
	}
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// TaskWorkspacesRootEnv carries the workspaces root of the daemon that owns a
// managed task into that task's environment. Task-mode `daemon disk-usage`
// reads this and nothing else: ResolveWorkspacesRoot derives its default from
// $HOME and the --profile name, so a task hosted by a named-profile daemon
// would otherwise scan the default root and silently report the wrong tree.
const TaskWorkspacesRootEnv = "MULTICA_TASK_WORKSPACES_ROOT"

// ResolveWorkspacesRoot returns the absolute path that the daemon and CLI
// should treat as the workspaces root. Resolution order: explicit override >
// MULTICA_WORKSPACES_ROOT env > default ($HOME/multica_workspaces, or
// $HOME/multica_workspaces_<profile> for a named profile). Read-only callers
// (e.g. `multica daemon disk-usage`) use this directly so they pick the same
// directory the running daemon would have picked. Inside a managed task use
// TaskWorkspacesRootEnv instead — see resolveDiskUsageRoot.
func ResolveWorkspacesRoot(profile, override string) (string, error) {
	root := strings.TrimSpace(os.Getenv("MULTICA_WORKSPACES_ROOT"))
	if override != "" {
		root = override
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w (set MULTICA_WORKSPACES_ROOT to override)", err)
		}
		if profile != "" {
			root = filepath.Join(home, "multica_workspaces_"+profile)
		} else {
			root = filepath.Join(home, "multica_workspaces")
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve absolute workspaces root: %w", err)
	}
	return abs, nil
}

// ArtifactPatternsFromEnv returns the configured artifact patternSet — the
// same list the GC loop consults when it runs the artifact-only cleanup. The
// disk-usage CLI uses this to make sure the "artifact size" it reports
// matches what the GC would actually reclaim.
func ArtifactPatternsFromEnv() []string {
	return patternsFromEnv("MULTICA_GC_ARTIFACT_PATTERNS", DefaultGCArtifactPatterns)
}

// patternsFromEnv reads a comma-separated list from env. Patterns containing
// path separators are silently dropped — the GC artifact cleanup only matches
// directory basenames, never paths, so a pattern like "foo/bar" is meaningless
// and accepting it would just be a footgun.
func patternsFromEnv(name string, defaults []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		out := make([]string, len(defaults))
		copy(out, defaults)
		return out
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || strings.ContainsAny(p, "/\\") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// canonicalExecutablePath resolves the daemon binary identity for self-reload.
func canonicalExecutablePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}
