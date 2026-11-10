package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"

	"path/filepath"
	"runtime"

	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/internal/selfexec"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/awakeclock"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
	"github.com/multica-ai/multica/server/pkg/skillbundle"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// ErrRepoNotConfigured is returned by ensureRepoReady when the requested repo
// URL is not present in the workspace's repo configuration after a fresh
// server refresh.
var ErrRepoNotConfigured = errors.New("repo is not configured for this workspace")

// errNoWorkspaceRuntimesRegistered reports failure to register the native runtime.
var errNoWorkspaceRuntimesRegistered = errors.New("failed to register runtimes")

// errTaskPrepareTimeout distinguishes the daemon's dispatched -> running
// startup deadline from provider execution timeouts. handleTask maps it to the
// platform-side timeout failure reason so the server's existing retry path can
// recover the task on a fresh attempt.
var errTaskPrepareTimeout = errors.New("task preparation timed out")

// errSkillBundleUnavailable marks a task that died in preparation because the
// daemon could not download one of the agent's skill bundles. Carrying it as a
// sentinel — rather than leaving handleTask to pattern-match the wrapped
// transport error — is what lets the failure land on the platform-side
// skill_bundle_unavailable reason instead of agent_error.unknown, which is not
// on the server's retry allowlist. (MUL-5370)
var errSkillBundleUnavailable = errors.New("skill bundle unavailable")

const (
	taskSlotWaitTimeout      = 2 * time.Second
	taskSlotCapacityBackoff  = 5 * time.Second
	repoCheckoutModeEnv      = "MULTICA_REPO_CHECKOUT_MODE"
	repoCheckoutModeIsolated = "isolated"
	// defaultTaskPrepareTimeout is a hard liveness bound for everything after
	// claim and before StartTask succeeds: runtime resolution, skill bundles,
	// execution-environment setup, and the StartTask request itself. It is
	// intentionally independent from AgentTimeout, which only governs the
	// provider process after the task reaches running.
	defaultTaskPrepareTimeout = 5 * time.Minute
	// pendingWorkHeartbeatTimeout bounds the out-of-band heartbeat a
	// server-pushed daemon:pending_work hint triggers (MUL-5444). Short on
	// purpose: the hint is only a latency optimisation, and the scheduled
	// heartbeat still picks the request up if this attempt fails.
	pendingWorkHeartbeatTimeout = 15 * time.Second
	// pendingWorkHintBookkeepingTTL is how long a runtime's last-hint timestamp
	// is retained before it is swept — purely to keep the map bounded.
	pendingWorkHintBookkeepingTTL = 10 * time.Minute
	// idleWatchdogMaxTick caps the idle watchdog's polling interval. At the
	// base rate of window/2 the overshoot scales with the budget: a 2h window
	// would only be checked hourly, so a genuinely stuck run could hold its
	// slot for 3h. The cap makes worst-case detection window + 5m no matter how
	// large an operator sets the budget.
	//
	// 5m is chosen as the largest overshoot worth tolerating on top of a budget
	// already measured in hours, not for its polling cost — a tick is an atomic
	// load and a channel length check, so it is free at any interval anyone
	// would pick.
	idleWatchdogMaxTick = 5 * time.Minute
)

// pendingWorkHintMinInterval is the floor between two hint-driven heartbeats
// for the same runtime. Keeps an interactive first open instant while stopping a
// caller-triggered hint from becoming a heartbeat amplifier. A var so tests can
// shrink it, same as the other timing knobs in this package.
var pendingWorkHintMinInterval = time.Second

const (
	taskPrepareLeaseRefresh = 15 * time.Second
	taskPrepareLeaseTimeout = 10 * time.Second
)

var errInvalidTaskIdentity = errors.New("invalid task identity")

func validateTaskIdentity(task Task) error {
	if strings.TrimSpace(task.AgentID) == "" {
		return fmt.Errorf("%w: task %s has no authoritative agent_id", errInvalidTaskIdentity, task.ID)
	}
	if task.Agent == nil {
		return fmt.Errorf("%w: task %s has no agent payload (agent_id=%s)", errInvalidTaskIdentity, task.ID, task.AgentID)
	}
	if task.Agent.ID != task.AgentID {
		return fmt.Errorf("%w: task %s agent_id=%s but agent.id=%s", errInvalidTaskIdentity, task.ID, task.AgentID, task.Agent.ID)
	}
	return nil
}

func taskScopedAuthToken(task Task) (string, error) {
	token := strings.TrimSpace(task.AuthToken)
	if token == "" {
		return "", errors.New("server did not provide task-scoped auth token")
	}
	if !strings.HasPrefix(token, "mat_") {
		return "", errors.New("server provided non-task-scoped auth token")
	}
	return token, nil
}

func taskMulticaEnvironment(task Task, provider, agentName, token, configRoot, workspacesRoot, serverURL string, healthPort, slot int, tempDir string) map[string]string {
	env := map[string]string{
		"MULTICA_TOKEN":        token,
		cli.TaskConfigRootEnv:  configRoot,
		TaskWorkspacesRootEnv:  workspacesRoot,
		"MULTICA_SERVER_URL":   serverURL,
		"MULTICA_DAEMON_PORT":  strconv.Itoa(healthPort),
		"MULTICA_WORKSPACE_ID": task.WorkspaceID,
		"MULTICA_AGENT_NAME":   agentName,
		"MULTICA_AGENT_ID":     task.AgentID,
		"MULTICA_TASK_ID":      task.ID,
		"MULTICA_TASK_SLOT":    strconv.Itoa(slot),
		"TMPDIR":               tempDir,
		"TMP":                  tempDir,
		"TEMP":                 tempDir,
	}
	if runtime.GOOS != "windows" {
		return env
	}
	// MSYS (Git Bash) resolves its `/tmp usertemp` mount from TMP/TEMP once,
	// in the first process, and every live MSYS process of this user shares
	// that mount table — whichever provider started it. A task-private TMP/TEMP
	// would pin /tmp to a directory this daemon deletes when that task ends,
	// breaking /tmp for every later task (#8824). So on Windows no task
	// overrides TMP/TEMP; they inherit the daemon user's host temp, and only
	// TMPDIR stays task-private.
	delete(env, "TMP")
	delete(env, "TEMP")

	return env
}

// taskRunner executes a single agent task and returns the result.
// Extracted as an interface so tests can inject a fake without spawning real
// agent processes, while keeping test scaffolding out of the production struct.
type taskRunner interface {
	run(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error)
}

// taskRunnerFunc adapts a plain function to the taskRunner interface.
type taskRunnerFunc func(context.Context, Task, string, int, *slog.Logger) (TaskResult, error)

func (f taskRunnerFunc) run(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
	return f(ctx, task, provider, slot, log)
}

type terminalTaskReportKind uint8

const (
	terminalTaskReportComplete terminalTaskReportKind = iota + 1
	terminalTaskReportFail

	// CompleteTask and FailTask can make six 30-second HTTP attempts around
	// the five backoffs in defaultTerminalRetrySchedule (124 seconds total).
	// Keep the detached callback's own deadline above that worst-case budget
	// so it does not silently shorten the client's existing retry contract.
	// During daemon restart pollLoop still imposes its separate 30-second
	// process drain boundary.
	terminalTaskReportTimeout = 6 * time.Minute
)

// terminalTaskReport is the single daemon-side representation of a terminal
// callback. Keeping every complete/fail path behind this value and
// reportTerminalTask gives the durable outbox one insertion point without
// revisiting every task exit when it is added.
type terminalTaskReport struct {
	kind           terminalTaskReportKind
	taskID         string
	output         string
	branchName     string
	errorMessage   string
	sessionID      string
	workDir        string
	durableWorkDir string
	failureReason  string
	// sessionRolloutMissing is true when the daemon withheld this task's Codex
	// session because its rollout was not in the store (MUL-5305). The server
	// clears the resume pointer and flags the continuity gap for the next claim.
	sessionRolloutMissing bool
	// retiredSessionID names a session this run was told to resume and then
	// abandoned as unresumable (GH #6066). The server records it so no later
	// run on the issue or chat can select it again, however many clean rows
	// still reference it.
	retiredSessionID string
}

type terminalReportSendFunc func(context.Context, terminalTaskReport, []time.Duration) error

type executionEnvironmentCommand func() ([]string, error)

func defaultExecutionEnvironmentCommand() ([]string, error) {
	executable, err := resolveSelfExecutable()
	if err != nil {
		return nil, fmt.Errorf("resolve execution-environment helper: %w", err)
	}
	return []string{executable, execenv.PreparationHelperArg}, nil
}

var (
	isBrewInstall         = cli.IsBrewInstall
	getBrewPrefix         = cli.GetBrewPrefix
	matchKnownBrewPrefix  = cli.MatchKnownBrewPrefix
	resolveSelfExecutable = selfexec.Resolve
)

// workspaceState tracks registered runtimes for a single workspace.
//
// allowedRepoURLs covers the workspace-level repo bindings; it gets rebuilt on
// every refresh from the server. taskRepoURLs covers repos that the server
// surfaced through a per-task claim (project github_repo resources today,
// possibly other typed sources later) — those don't show up in
// GetWorkspaceRepos, so they would be wiped on refresh if we shared one map.
// taskRepoRefs tracks optional checkout refs for the specific task that
// surfaced each project repo so two projects using the same URL don't leak refs
// into each other.
type workspaceState struct {
	workspaceID     string
	runtimeIDs      []string
	reposVersion    string // stored for future use: skip refresh when version unchanged
	allowedRepoURLs map[string]struct{}
	taskRepoURLs    map[string]struct{}
	taskRepoRefs    map[string]map[string]string // taskID -> repo URL -> checkout ref
	settings        json.RawMessage              // workspace settings (JSONB)
	lastRepoSyncErr string
	repoRefreshMu   contextLock
	// coAuthorPublishMu serializes publication of the Co-authored-by verdict
	// for this workspace. Unlike the fields above it is NOT guarded by
	// Daemon.mu: it exists precisely so the verdict can be read and written as
	// one step without holding the daemon's central lock across file I/O.
	coAuthorPublishMu sync.Mutex
}

// contextLock is a zero-value-ready mutex whose wait can be cancelled. Repo
// checkout requests use it for workspace refresh coalescing so disconnecting a
// client never leaves the handler stuck behind another cold-cache refresh.
type contextLock struct {
	once  sync.Once
	token chan struct{}
}

func (l *contextLock) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	l.once.Do(func() {
		l.token = make(chan struct{}, 1)
		l.token <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-l.token:
		if err := ctx.Err(); err != nil {
			l.token <- struct{}{}
			return context.Cause(ctx)
		}
		return nil
	}
}

func (l *contextLock) Unlock() {
	l.token <- struct{}{}
}

type repoCacheBackend interface {
	Lookup(workspaceID, url string) string
	BarePath(workspaceID, url string) string
	Sync(workspaceID string, repos []repocache.RepoInfo) error
	WithRepoLock(barePath string, fn func() error) error
	CreateWorktree(params repocache.WorktreeParams) (*repocache.WorktreeResult, error)
}

// Daemon is the local agent runtime that polls for and executes tasks.
type Daemon struct {
	cfg        Config
	client     *Client
	repoCache  repoCacheBackend
	skillCache *SkillBundleCache
	logger     *slog.Logger

	// terminalReports is the durable outbox for complete/fail callbacks. The
	// sender hook is production-wired through Client and overridable in focused
	// tests; terminalReportWakeup coalesces new-report and reconnect nudges.
	terminalReports      *terminalReportStore
	terminalReportSend   terminalReportSendFunc
	terminalReportWakeup chan struct{}
	terminalReportNow    func() time.Time
	terminalReportMu     sync.Mutex
	terminalReportFlight map[string]struct{}

	mu             sync.Mutex
	workspaces     map[string]*workspaceState
	runtimeIndex   map[string]Runtime // runtimeID -> Runtime for provider lookups
	reloading      sync.Mutex         // prevents concurrent workspace syncs
	runtimeSet     *runtimeSetWatcher
	registerSerial sync.Map // workspace ID -> registration mutex

	wsHBMu      sync.RWMutex         // guards wsHBLastAck
	wsHBLastAck map[string]time.Time // runtime_id -> last successful WS heartbeat ack timestamp

	// reconcile fans out a "re-check server state now" signal to subscribers
	// (watchTaskCancellation, workspaceSyncLoop) so the WS connect/reconnect
	// path can shrink coarse fallback reconciliation gaps to sub-second. See
	// reconcile.go and runTaskWakeupConnection.
	reconcile *reconcileBroadcaster
	// workspaceChanges is the account-scoped server hint for membership-set
	// changes. It stays separate from reconcile because a membership hint only
	// needs the minimal workspace list, while a WS reconnect also reconciles
	// runtime profiles that may have changed during the gap.
	workspaceChanges *workspaceChangeSignal

	// wsRPC carries generic request/response RPCs (e.g. tasks.claim, MUL-4257)
	// over the task-wakeup WS connection. It is attached to the live
	// connection in runTaskWakeupConnection and detached on disconnect; when
	// detached, callers fall back to HTTP.
	wsRPC *wsRPCClient

	// batchClaimUnsupported is set once a batch claim gets a 404 from the
	// server (no /api/daemon/tasks/claim route — an un-upgraded server), so
	// subsequent polls skip WS+batch and use the legacy per-runtime claim
	// directly. Reset when the WS (re)connects, so a server upgrade that
	// bounces the connection re-probes the batch route (MUL-4257).
	batchClaimUnsupported atomic.Bool
	// wsClaimHTTPFallbackAfter is set after an uncertain WS claim outcome. Once
	// the safety delay elapses, the next claim bypasses WS once and uses HTTP so
	// a flaky reconnecting WS cannot starve queued tasks indefinitely.
	wsClaimHTTPFallbackAfter atomic.Int64

	// runtimeGoneMu guards runtimeGoneInflight, reregisterNextAttempt, and
	// reregisterLastCompletedAt. The state lets heartbeat / poller / WS-ack
	// handlers converge on a single recovery path when they each detect that a
	// runtime row was deleted server-side without three of them stampeding
	// registerRuntimesForWorkspace.
	runtimeGoneMu             sync.Mutex
	runtimeGoneInflight       map[string]struct{}  // runtime_id -> currently recovering
	reregisterNextAttempt     map[string]time.Time // workspace_id -> earliest time the next re-register attempt may run
	reregisterLastCompletedAt map[string]time.Time // workspace_id -> wall-clock at which the last SUCCESSFUL re-register call returned (failures intentionally not stamped — see recordRegisterCompletion)

	// pendingWorkMu guards pendingWorkInflight and pendingWorkLastRun, which
	// coalesce and rate-limit server-pushed "heartbeat now" hints (MUL-5444).
	// Several UI surfaces can request the same runtime's model list within
	// milliseconds; without the guard each hint would fire its own out-of-band
	// heartbeat, and an authenticated caller looping the list-models endpoint
	// could turn that into a heartbeat amplifier.
	pendingWorkMu       sync.Mutex
	pendingWorkInflight map[string]struct{}  // runtime_id -> hint-driven heartbeat in flight
	pendingWorkLastRun  map[string]time.Time // runtime_id -> when the last hint-driven heartbeat started

	cancelFunc context.CancelFunc // set by Run(); called by triggerRestart
	rootCtx    context.Context    // set by Run(); used by long-running recoveries that must survive per-runtime ctx cancellation
	// restartMu guards restartBinary. Two goroutines can reach triggerRestart —
	// the server-triggered handleUpdate and the autoUpdateLoop — and
	// trySelfReload reads RestartBinary() from the latter to avoid racing the
	// former into a second handoff.
	restartMu     sync.Mutex
	restartBinary string // non-empty after a successful update; path to the new binary
	// brewTargetOnce caches the brew half of restartTargetBinary. The install
	// method and brew prefix cannot change for the lifetime of the process, and
	// trySelfReload now calls restartTargetBinary every check tick — without
	// the cache that is up to two uncached `brew --prefix` forks per tick.
	brewTargetOnce sync.Once
	brewInstall    bool        // resolved once: was this binary installed via brew?
	brewPrefix     string      // Homebrew prefix when brewInstall and it resolved; the stable path is derived per call by cli.StableExecutablePath
	updating       atomic.Bool // prevents concurrent update attempts
	// activeTasks is the ownership-safe count of tasks currently in handleTask.
	// It deliberately includes preparation and local-directory waiters because
	// restart/update barriers must not kill any claimed task.
	activeTasks atomic.Int64
	// runningTasks counts live provider execution sessions, beginning only after
	// backend.Execute returns. It can briefly lag the server-side running state,
	// which starts during preparation before provider launch. resourceWaitTasks
	// counts tasks blocked on a local_directory path mutex. Both are diagnostic
	// /health dimensions and must never replace activeTasks in safety barriers.
	runningTasks      atomic.Int64
	resourceWaitTasks atomic.Int64
	ready             atomic.Bool // false until preflight completes; gates /health status (starting -> running)
	// reloadPendingReason explains why a confirmed multica version change hasn't
	// restarted the daemon yet (a task was running at the barrier check). Set
	// and cleared by trySelfReload, read by /health. Diagnostic only.
	reloadPendingReason atomic.Pointer[string]

	// claimMu guards pauseClaims and claimsInFlight. It is held only for the
	// microseconds it takes to make a decision; ClaimTask itself runs without
	// the lock so a slow per-runtime claim cannot stall auto-update or any
	// other poller.
	//
	// The pair is the auto-update path's barrier against the issue's
	// requirement that "升级过程中如果有 task 进来，会延后升级而不是中断 task":
	// runRuntimePoller refuses to call ClaimTask while pauseClaims is set, and
	// tryAutoUpdate refuses to flip pauseClaims while any poller is mid-claim
	// or any task is in handleTask. Together that closes the fetch-then-claim
	// race where a new task slipping in during the release-metadata fetch
	// would be cancelled by triggerRestart's root-ctx cancel.
	claimMu        sync.Mutex
	pauseClaims    bool // when true, the batch poller skips claiming
	claimsInFlight int  // pollers that have decided to claim but haven't yet handed the task off to handleTask

	activeEnvRootsMu   sync.Mutex
	activeEnvRootsCond *sync.Cond      // signalled when an in-flight env-root GC mutation finishes
	activeEnvRoots     map[string]int  // env root path -> reference count (handles reuse paths marked twice)
	deletingEnvRoots   map[string]bool // env roots reserved by GC; new tasks wait until the mutation finishes

	activeStoresMu   sync.Mutex
	activeStoresCond *sync.Cond      // signalled when an in-flight store deletion finishes, so a blocked markActive can proceed
	activeStores     map[string]int  // persistent store path (per-conversation Codex sessions, per-agent Hermes memories) -> live-task refcount; guards the store from GC mid-task (MUL-4424)
	deletingStores   map[string]bool // store paths a GC delete has reserved; markActive waits these out so a task never mounts a store mid-removal

	// repoCheckoutTasks binds the localhost /repo/checkout endpoint to the
	// task-scoped bearer token of a currently running agent. The request body is
	// never an identity source: workspace, task, agent, and allowed workdir all
	// come from this registry.
	repoCheckoutTasksMu sync.RWMutex
	repoCheckoutTasks   map[string]activeRepoCheckoutTask

	// localPathLocks serialises agent tasks whose project resource is a
	// local_directory pinned to this daemon. Two tasks targeting the same
	// on-disk path run sequentially; the second blocks on the lock and is
	// surfaced via the server-side waiting_local_directory status while it
	// waits. See MUL-2663.
	localPathLocks *LocalPathLocker

	// bgSyncs tracks background goroutines started by registerTaskRepos so
	// callers (notably tests using t.TempDir-backed cache roots) can wait for
	// them to drain before tearing the daemon down. Without this the bg
	// goroutine can race against t.TempDir cleanup, leaving a partially
	// deleted bare clone and an unrelated `not empty` cleanup failure.
	bgSyncs sync.WaitGroup

	runner             taskRunner    // executes agent tasks; set to d.runTask by New(), overridable in tests
	cancelPollInterval time.Duration // how often handleTask polls for server-side cancellation; overridable in tests
	// taskSlotWait is the brief semaphore wait before the capacity backoff.
	// New sets the production default; tests shorten it to reach that branch.
	taskSlotWait time.Duration
	// taskSupplementSignals carries content-free server hints to the exact
	// negotiated task. The two intervals are production defaults in New and
	// independently overridable by focused tests.
	taskSupplementSignals       taskSupplementSignals
	taskSupplementPollInterval  time.Duration
	taskSupplementReadyInterval time.Duration
	// envRootBusyWait is how long a task that is entitled to a prior env root
	// waits for the previous run to let go of it before giving up and preparing
	// a fresh one. New() sets it; the zero value means "do not wait", which is
	// what focused unit tests want. See lockReusablePriorEnvRoot (MUL-6880).
	envRootBusyWait time.Duration
	// executionEnvironmentCommand resolves the killable helper used for
	// Prepare/Reuse. New always sets it; nil keeps focused unit tests in-process.
	executionEnvironmentCommand executionEnvironmentCommand
	// taskPrepareTimeout is the dispatched -> running hard deadline. New sets
	// the production default; zero-valued test daemons fall back to the same
	// default in effectiveTaskPrepareTimeout.
	taskPrepareTimeout time.Duration
	// prepareLeaseRefresh is how often a preparing task extends its prepare
	// lease. New sets the production default; zero-valued test daemons fall
	// back to the same default in startTaskPrepareLeaseExtender.
	prepareLeaseRefresh time.Duration
	// runUpdateFn executes the brew-or-download upgrade. Set to d.runUpdate by
	// New() and overridable in tests so the auto-update poller can be exercised
	// without touching the real network or the brew CLI.
	runUpdateFn func(targetVersion string) (string, error)
}

// New creates a new Daemon instance.
func New(cfg Config, logger *slog.Logger) *Daemon {
	cacheRoot := filepath.Join(cfg.WorkspacesRoot, ".repos")
	skillCacheRoot := filepath.Join(cfg.WorkspacesRoot, ".skill-cache", "v1")
	client := NewClient(cfg.ServerBaseURL)
	// Tag every daemon HTTP request with the daemon's CLI version so the
	// server can split logs/metrics by client version (parallel to the CLI).
	client.SetVersion(cfg.CLIVersion)
	d := &Daemon{
		cfg:                         cfg,
		client:                      client,
		repoCache:                   repocache.New(cacheRoot, logger),
		skillCache:                  NewSkillBundleCache(skillCacheRoot),
		logger:                      logger,
		terminalReports:             newTerminalReportStore(cfg),
		terminalReportWakeup:        make(chan struct{}, 1),
		terminalReportNow:           time.Now,
		terminalReportFlight:        make(map[string]struct{}),
		workspaces:                  make(map[string]*workspaceState),
		runtimeIndex:                make(map[string]Runtime),
		runtimeSet:                  newRuntimeSetWatcher(),
		wsHBLastAck:                 make(map[string]time.Time),
		activeEnvRoots:              make(map[string]int),
		deletingEnvRoots:            make(map[string]bool),
		activeStores:                make(map[string]int),
		deletingStores:              make(map[string]bool),
		localPathLocks:              NewLocalPathLocker(),
		runtimeGoneInflight:         make(map[string]struct{}),
		pendingWorkInflight:         make(map[string]struct{}),
		pendingWorkLastRun:          make(map[string]time.Time),
		reregisterNextAttempt:       make(map[string]time.Time),
		reregisterLastCompletedAt:   make(map[string]time.Time),
		cancelPollInterval:          5 * time.Second,
		taskSlotWait:                taskSlotWaitTimeout,
		taskSupplementPollInterval:  defaultTaskSupplementPollInterval,
		taskSupplementReadyInterval: defaultTaskSupplementReadyInterval,
		envRootBusyWait:             15 * time.Second,
		taskPrepareTimeout:          defaultTaskPrepareTimeout,
		prepareLeaseRefresh:         taskPrepareLeaseRefresh,
		reconcile:                   newReconcileBroadcaster(),
		workspaceChanges:            newWorkspaceChangeSignal(),
		wsRPC:                       newWSRPCClient(wsRPCResponseGrace),
	}
	d.activeEnvRootsCond = sync.NewCond(&d.activeEnvRootsMu)
	d.activeStoresCond = sync.NewCond(&d.activeStoresMu)
	d.executionEnvironmentCommand = defaultExecutionEnvironmentCommand
	d.runner = taskRunnerFunc(d.runTask)
	d.runUpdateFn = d.runUpdate
	return d
}

// workspaceRegisterLock serializes registration, publishing state and cleanup.
func (d *Daemon) workspaceRegisterLock(workspaceID string) *sync.Mutex {
	mu, _ := d.registerSerial.LoadOrStore(workspaceID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// withWorkspaceRegisterLock runs one workspace's registration sequence as a
// single ordered step. Everything that sends a Register for a workspace, folds
// the response into local state, or deregisters rows that response cost, must
// run inside fn — see workspaceRegisterLock for why the boundary sits after the
// cleanup rather than after the send.
//
// The lock is per workspace, so sequences for different workspaces still run
// concurrently and nothing here is ever held across two of them.
func (d *Daemon) withWorkspaceRegisterLock(workspaceID string, fn func() error) error {
	mu := d.workspaceRegisterLock(workspaceID)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func (d *Daemon) notifyRuntimeSetChanged() {
	d.runtimeSet.notify()
}

// reregisterCoalesceWindow caps how often the daemon re-registers a workspace
// after detecting a runtime_not_found response. Many stale runtime IDs may be
// reported within seconds of each other (one delete clears all of a daemon's
// runtimes), and a single re-register call replaces every runtime in the
// workspace, so concurrent recoveries must collapse to one API call.
const reregisterCoalesceWindow = 30 * time.Second

// reregisterFailureBackoff is the additional wait inserted before the next
// re-register attempt when the previous one failed. This prevents heartbeat
// ticks (~15s) from converting a server-side log flood into a re-register
// flood when re-registration itself is failing (workspace removed, server
// unreachable, ...).
const reregisterFailureBackoff = 60 * time.Second

// handleRuntimeGone is the single recovery entry point shared by the HTTP
// heartbeat path, the runtime poller, and the WebSocket runtime_gone ack
// handler. All three may notice the same stale runtime within a few ms of
// each other, so this function:
//
//   - keys an in-flight set on runtimeID to drop concurrent calls for the same
//     ID after the first one is already cleaning up;
//   - keys a per-workspace next-attempt timestamp on workspaceID so that
//     concurrent recoveries triggered by the SAME initial event coalesce to a
//     single registerRuntimesForWorkspace call. The slot is cleared on success
//     so a later distinct runtime deletion in the same workspace can trigger
//     its own recovery without waiting for the coalesce window to expire; and
//   - keys a per-workspace last-completed timestamp so that a straggler whose
//     removeStaleRuntime took long enough that a sibling fully ran AND cleared
//     the slot can still recognize itself as same-wave and bail. Without this,
//     the success-case slot clear opens a race where the late caller re-claims
//     an empty slot and double-registers.
//
// On failure of the underlying re-register, the next-attempt timestamp is
// extended by reregisterFailureBackoff so we don't replace a server-side log
// flood with a daemon-side register flood. workspaceSyncLoop will retry
// independently every DefaultWorkspaceSyncInterval as a safety net.
//
// The recovery HTTP call uses the daemon root context, not the caller's. The
// heartbeat path's per-runtime ctx is cancelled by notifyRuntimeSetChanged the
// moment we prune the dead UUID, and if we forwarded that ctx the in-flight
// register would self-cancel mid-flight.
func (d *Daemon) handleRuntimeGone(runtimeID string) {
	if runtimeID == "" {
		return
	}

	// entryAt anchors the same-wave-straggler check at the bottom of the
	// function. Captured at the very top so removeStaleRuntime mutex
	// contention can't push it past a sibling's register completion.
	entryAt := time.Now()

	// Stampede control per runtime ID.
	d.runtimeGoneMu.Lock()
	if _, inflight := d.runtimeGoneInflight[runtimeID]; inflight {
		d.runtimeGoneMu.Unlock()
		return
	}
	d.runtimeGoneInflight[runtimeID] = struct{}{}
	d.runtimeGoneMu.Unlock()
	defer func() {
		d.runtimeGoneMu.Lock()
		delete(d.runtimeGoneInflight, runtimeID)
		d.runtimeGoneMu.Unlock()
	}()

	workspaceID, removed := d.removeStaleRuntime(runtimeID)
	if !removed {
		// Already gone from local state — a parallel recovery already
		// cleaned this up, or workspaceSyncLoop pruned the whole workspace.
		return
	}

	d.logger.Info("runtime deleted server-side; pruned from local state",
		"runtime_id", runtimeID, "workspace_id", workspaceID)
	d.notifyRuntimeSetChanged()

	if !d.tryClaimRegisterSlot(workspaceID, entryAt, time.Now()) {
		d.logger.Debug("skip re-register: coalescing with recent attempt",
			"workspace_id", workspaceID)
		return
	}

	err := d.reregisterWorkspaceAfterRuntimeGone(d.recoveryContext(), workspaceID)
	d.recordRegisterCompletion(workspaceID, time.Now(), err)
	if err != nil {
		// Logged at Warn (not Error) because workspaceSyncLoop retries
		// independently every DefaultWorkspaceSyncInterval, so a transient
		// failure here is not a stuck state — just an extra wait.
		d.logger.Warn("re-register after runtime gone failed",
			"workspace_id", workspaceID, "error", err)
	}
}

// tryClaimRegisterSlot atomically decides whether the calling goroutine should
// run registerRuntimesForWorkspace. Returns true and claims the in-flight slot
// when the caller may proceed; returns false (without mutating state) when the
// call must be coalesced with a peer.
//
// Two gates are checked under runtimeGoneMu:
//
//  1. reregisterNextAttempt: a future timestamp means a peer holds the slot or
//     a previous attempt failed and we are inside the failure backoff window.
//  2. reregisterLastCompletedAt: a timestamp at or after our entryAt means a
//     peer's register SUCCEEDED after we entered handleRuntimeGone, so the
//     workspace state is already covered for our wave and we can bail.
//     Failures intentionally don't stamp this field (see
//     recordRegisterCompletion), so a same-wave straggler whose entryAt
//     predates a failed sibling can still retry once the failure backoff
//     expires — failures don't cover anything.
//
// entryAt is the wall-clock captured at the top of handleRuntimeGone. now is
// passed in (rather than read inside) so tests can drive the gate
// deterministically without sleeping.
func (d *Daemon) tryClaimRegisterSlot(workspaceID string, entryAt, now time.Time) bool {
	d.runtimeGoneMu.Lock()
	defer d.runtimeGoneMu.Unlock()
	if next, ok := d.reregisterNextAttempt[workspaceID]; ok && now.Before(next) {
		return false
	}
	if last, ok := d.reregisterLastCompletedAt[workspaceID]; ok && !last.Before(entryAt) {
		return false
	}
	d.reregisterNextAttempt[workspaceID] = now.Add(reregisterCoalesceWindow)
	return true
}

// recordRegisterCompletion records the outcome of a register call. On success
// it stamps lastCompletedAt (which suppresses same-wave stragglers via
// tryClaimRegisterSlot) and clears the in-flight slot so a genuinely later
// runtime deletion can claim immediately. On failure it extends
// reregisterNextAttempt by the failure backoff and intentionally does NOT
// stamp lastCompletedAt — a failed register did not cover any workspace
// state, so a same-wave straggler whose entryAt predates the failure must
// still be allowed to retry once the backoff expires. workspaceSyncLoop only
// retries when the workspace's runtimeIDs fully drain, so partial-deletion
// recovery has to come from the straggler path.
func (d *Daemon) recordRegisterCompletion(workspaceID string, completedAt time.Time, err error) {
	d.runtimeGoneMu.Lock()
	defer d.runtimeGoneMu.Unlock()
	if err != nil {
		d.reregisterNextAttempt[workspaceID] = completedAt.Add(reregisterFailureBackoff)
		return
	}
	d.reregisterLastCompletedAt[workspaceID] = completedAt
	delete(d.reregisterNextAttempt, workspaceID)
}

// recoveryContext returns the daemon root context for long-running recovery
// HTTP calls (re-register, recover-orphans) that must survive the heartbeat
// loop tearing down a per-runtime context. Falls back to Background when the
// daemon was not started via Run(), e.g. unit-test fixtures.
func (d *Daemon) recoveryContext() context.Context {
	if d.rootCtx != nil {
		return d.rootCtx
	}
	return context.Background()
}

// removeStaleRuntime drops a runtime ID from its owning workspace's runtimeIDs
// list, the daemon-level runtimeIndex, and the WS heartbeat freshness map.
// Returns the workspace ID and true if the runtime was tracked, "" and false
// otherwise.
//
// Callers must NOT replace workspaceState pointers — only mutate fields in
// place — because ensureRepoReady holds workspaceState.repoRefreshMu through
// long repo-sync calls. See syncWorkspacesFromAPI for the same invariant.
func (d *Daemon) removeStaleRuntime(runtimeID string) (string, bool) {
	d.mu.Lock()
	var workspaceID string
	for wsID, ws := range d.workspaces {
		found := false
		filtered := ws.runtimeIDs[:0:0]
		for _, rid := range ws.runtimeIDs {
			if rid == runtimeID {
				found = true
				continue
			}
			filtered = append(filtered, rid)
		}
		if found {
			ws.runtimeIDs = filtered
			workspaceID = wsID
			break
		}
	}
	if workspaceID == "" {
		d.mu.Unlock()
		return "", false
	}
	delete(d.runtimeIndex, runtimeID)
	d.mu.Unlock()

	d.wsHBMu.Lock()
	delete(d.wsHBLastAck, runtimeID)
	d.wsHBMu.Unlock()

	return workspaceID, true
}

// workspaceNeedsRuntimeRecovery reports whether a tracked workspace currently
// has zero runtime IDs — the state reached when handleRuntimeGone pruned every
// runtime and its inline re-register failed. workspaceSyncLoop calls this on
// each tick so the workspace can recover without waiting for an external
// trigger.
func (d *Daemon) workspaceNeedsRuntimeRecovery(workspaceID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, ok := d.workspaces[workspaceID]
	if !ok {
		return false
	}
	return len(ws.runtimeIDs) == 0
}

// reregisterWorkspaceAfterRuntimeGone calls registerRuntimesForWorkspace and
// updates the existing workspaceState in place. The register response is
// authoritative for this workspace's runtime set — every configured provider
// is included, with UpsertAgentRuntime returning the same row ID for surviving
// providers and a fresh ID for any that were deleted server-side. Replacing
// (rather than appending) is required: a partial recovery, where only one
// runtime in a multi-provider workspace was deleted, would otherwise produce
// duplicates for every provider that wasn't deleted.
//
// The workspaceState pointer is NEVER replaced (see syncWorkspacesFromAPI's
// invariant about repoRefreshMu). Only fields are mutated.
// applyRegisterResponseInPlace replaces runtime IDs while preserving workspace locks.
func (d *Daemon) applyRegisterResponseInPlace(workspaceID string, resp *RegisterResponse) (newIDs, droppedIDs []string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, exists := d.workspaces[workspaceID]
	if !exists {
		return nil, nil, false
	}
	current := make(map[string]bool, len(resp.Runtimes))
	for _, rt := range resp.Runtimes {
		newIDs = append(newIDs, rt.ID)
		current[rt.ID] = true
		d.runtimeIndex[rt.ID] = rt
	}
	for _, oldID := range ws.runtimeIDs {
		if !current[oldID] {
			delete(d.runtimeIndex, oldID)
			droppedIDs = append(droppedIDs, oldID)
		}
	}
	ws.runtimeIDs = newIDs
	if resp.ReposVersion != "" {
		ws.reposVersion = resp.ReposVersion
		ws.allowedRepoURLs = repoAllowlist(resp.Repos)
	}
	if len(resp.Settings) > 0 {
		ws.settings = resp.Settings
	}
	return newIDs, droppedIDs, true
}

// untrackedRuntimeIDs filters cleanup candidates under the workspace registration lock.
func (d *Daemon) untrackedRuntimeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, tracked := d.runtimeIndex[id]; tracked {
			continue
		}
		out = append(out, id)
	}
	return out
}

func (d *Daemon) reregisterWorkspaceAfterRuntimeGone(ctx context.Context, workspaceID string) error {
	var newIDs []string
	// Send, apply and clean up as one ordered step — see workspaceRegisterLock.
	err := d.withWorkspaceRegisterLock(workspaceID, func() error {
		resp, err := d.registerRuntimesForWorkspaceLocked(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("register runtimes: %w", err)
		}

		ids, droppedIDs, ok := d.applyRegisterResponseInPlace(workspaceID, resp)
		if !ok {
			return fmt.Errorf("workspace %s no longer tracked", workspaceID)
		}
		newIDs = ids

		for _, rid := range newIDs {
			d.logger.Info("re-registered runtime after server-side deletion",
				"workspace_id", workspaceID, "runtime_id", rid)
		}

		// A sibling runtime dropped by this recovery (a provider removed from the
		// daemon's config, a disabled profile) still has a live server row — the
		// runtime_gone trigger only deleted its own. Eagerly mark those offline,
		// matching the drift path, instead of leaving them claimable until the
		// stale-heartbeat sweep.
		d.deregisterDroppedRuntimes(ctx, workspaceID, droppedIDs, "runtime_gone recovery", nil)
		return nil
	})
	if err != nil {
		return err
	}
	d.notifyRuntimeSetChanged()

	// Tell the server about any tasks the previous (now-deleted) runtime
	// was working on, mirroring the registration path's recover-orphans call.
	// This is intentionally scoped to the runtime_gone recovery: the
	// runtimes were truly gone server-side, so anything still in
	// dispatched/running/waiting_local_directory on those rows is an orphan
	// that needs to be failed-and-retried. The drift-refresh path (which
	// also feeds applyRegisterResponseInPlace) deliberately skips this step
	// because its surviving runtime IDs may still be actively executing
	// tasks for the user (MUL-3332).
	for _, rid := range newIDs {
		if err := d.client.RecoverOrphans(ctx, rid); err != nil {
			d.logger.Warn("recover-orphans after re-register failed",
				"runtime_id", rid, "error", err)
		}
	}
	return nil
}

// runtimeSetWatcher is a tiny pub/sub for runtime-set changes. It exists
// because more than one supervisor (taskWakeupLoop, heartbeatLoop, pollLoop)
// needs to react to runtime-set changes; a single buffered channel would
// race so only the first listener would learn about each change.
//
// Each subscriber gets a 1-slot channel; missed nudges coalesce into a
// single signal — the subscriber is expected to re-derive the current
// runtime set via allRuntimeIDs() rather than relying on edge counts.
type runtimeSetWatcher struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func newRuntimeSetWatcher() *runtimeSetWatcher {
	return &runtimeSetWatcher{subscribers: make(map[chan struct{}]struct{})}
}

// Subscribe returns a channel that receives a non-blocking nudge whenever
// the runtime set changes, and an unsubscribe func the caller must invoke
// when done.
func (w *runtimeSetWatcher) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subscribers[ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.subscribers, ch)
		w.mu.Unlock()
	}
}

func (w *runtimeSetWatcher) notify() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// wsHeartbeatFreshness defines how long a WS heartbeat ack is considered
// "fresh enough" to suppress the HTTP heartbeat for that runtime. The window
// is 2× HeartbeatInterval so a single dropped WS ack still keeps HTTP
// suppressed, but two missed acks (~30s of WS silence) re-enable HTTP — well
// inside the server-side 45s offline threshold.
func (d *Daemon) wsHeartbeatFreshness() time.Duration {
	if d.cfg.HeartbeatInterval <= 0 {
		return 30 * time.Second
	}
	return 2 * d.cfg.HeartbeatInterval
}

// recordWSHeartbeatAck stamps the runtime as having received a fresh WS
// heartbeat ack from the server. Called by the WS read pump.
func (d *Daemon) recordWSHeartbeatAck(runtimeID string) {
	if runtimeID == "" {
		return
	}
	d.wsHBMu.Lock()
	d.wsHBLastAck[runtimeID] = time.Now()
	d.wsHBMu.Unlock()
}

// wsHeartbeatRecentlyAcked reports whether the runtime received a WS
// heartbeat ack inside the freshness window. The HTTP heartbeat loop uses
// this to skip duplicate work when WS is already keeping the runtime alive.
func (d *Daemon) wsHeartbeatRecentlyAcked(runtimeID string) bool {
	d.wsHBMu.RLock()
	last, ok := d.wsHBLastAck[runtimeID]
	d.wsHBMu.RUnlock()
	if !ok {
		return false
	}
	return time.Since(last) < d.wsHeartbeatFreshness()
}

// clearWSHeartbeatAcks drops all WS heartbeat freshness records. Called on
// WS disconnect so HTTP heartbeats resume on the next tick.
func (d *Daemon) clearWSHeartbeatAcks() {
	d.wsHBMu.Lock()
	for k := range d.wsHBLastAck {
		delete(d.wsHBLastAck, k)
	}
	d.wsHBMu.Unlock()
}

// Run starts the daemon: resolves auth, registers runtimes, then polls for tasks.
func (d *Daemon) Run(ctx context.Context) error {
	// Wrap context so handleUpdate can cancel the daemon for restart.
	ctx, cancel := context.WithCancel(ctx)
	d.cancelFunc = cancel
	d.rootCtx = ctx

	// Bind health port early to detect another running daemon.
	healthLn, err := d.listenHealth()
	if err != nil {
		return err
	}

	agentNames := make([]string, 0, len(d.agents()))
	for name := range d.agents() {
		agentNames = append(agentNames, name)
	}
	logFields := []any{"version", d.cfg.CLIVersion, "agents", agentNames, "server", d.cfg.ServerBaseURL}
	if d.cfg.Profile != "" {
		logFields = append(logFields, "profile", d.cfg.Profile)
	}
	d.logger.Info("starting daemon", logFields...)
	d.logger.Debug("daemon config resolved",
		"daemon_id", d.cfg.DaemonID,
		"device_name", d.cfg.DeviceName,
		"workspaces_root", d.cfg.WorkspacesRoot,
		"health_port", d.cfg.HealthPort,
		"poll_interval", d.cfg.PollInterval,
		"ws_claim_poll_interval", d.cfg.WSClaimPollInterval,
		"heartbeat_interval", d.cfg.HeartbeatInterval,
		"agent_timeout", d.cfg.AgentTimeout,
		"idle_watchdog", d.cfg.AgentIdleWatchdog,
		// Logged explicitly because it is normally derived from idle_watchdog:
		// without it an operator cannot read the tool budget actually in effect.
		"tool_watchdog", d.cfg.AgentToolWatchdog,
	)

	// Mark the daemon-owned workspaces tree before any task runs. A sandbox
	// fault can strip every MULTICA_* env var from an agent subprocess; the
	// per-workdir marker then only protects cwds inside the workdir, and a
	// subprocess that escaped to the workdir's parent would fall back to the
	// user's config PAT. The root marker makes the CLI fail closed anywhere
	// under the tree. Non-fatal: Prepare re-ensures it per task.
	if err := execenv.EnsureWorkspacesRootMarker(d.cfg.WorkspacesRoot); err != nil {
		d.logger.Warn("workspaces root marker not written; CLI fail-closed guard limited to task workdirs", "error", err)
	}

	// Load auth token from CLI config.
	if err := d.resolveAuth(); err != nil {
		return err
	}

	// Bind and serve the health port before the (potentially slow) preflight,
	// so `daemon start` and the desktop see a live "starting" daemon instead
	// of connection-refused while preflightAuth runs. preflightAuth's initial
	// workspace sync detects every configured agent's version by exec'ing it,
	// which on a cold cache with many agents takes ~20s. Liveness (port up) and
	// readiness (status:"running") are reported separately: /health stays
	// "starting" until d.ready is set after preflight, so a slow or *failing*
	// preflight is never misreported as a started daemon. resolveAuth has
	// already run, so a missing token still fails fast before we begin serving.
	go d.serveHealth(ctx, healthLn, time.Now())

	// Renew the PAT before the first API call, then do the initial
	// workspace sync. Both steps live in preflightAuth so the ordering
	// invariant (renew first) is enforced at one site instead of
	// scattered into Run, and tests can exercise the failure paths
	// without the full Run setup.
	if err := d.preflightAuth(ctx); err != nil {
		return err
	}

	// Deregister runtimes on shutdown (uses a fresh context since ctx will be cancelled).
	defer d.deregisterRuntimes()

	// Start workspace sync loop to discover newly created workspaces.
	go d.workspaceSyncLoop(ctx)
	go d.terminalReportReplayLoop(ctx)
	go d.nativeJobWatchLoop(ctx)

	taskWakeups := make(chan taskWakeup, 256)
	go d.taskWakeupLoop(ctx, taskWakeups)
	go d.heartbeatLoop(ctx)
	go d.gcLoop(ctx)
	go d.autoUpdateLoop(ctx)
	go d.tokenRenewalLoop(ctx)

	// Preflight succeeded and the background loops are up: the daemon has
	// registered its runtimes and can now claim and run tasks. Flip /health
	// from "starting" to "running" — this is the signal `daemon start`'s
	// readiness wait blocks on, so success is reported only after startup
	// actually completed, not merely because the health port came up.
	d.ready.Store(true)
	d.logger.Debug("background loops launched (workspace-sync, terminal-report-replay, task-wakeup, heartbeat, gc, auto-update, token-renewal); health now reporting ready")
	err = d.pollLoop(ctx, taskWakeups)
	d.logger.Debug("daemon main loop returning", "error", err)
	return err
}

// RestartBinary returns the path to the new binary if the daemon needs to restart
// after a successful update, or empty string if no restart is needed.
func (d *Daemon) RestartBinary() string {
	d.restartMu.Lock()
	defer d.restartMu.Unlock()
	return d.restartBinary
}

// deregisterRuntimes notifies the server that all runtimes are going offline.
func (d *Daemon) deregisterRuntimes() {
	runtimeIDs := d.allRuntimeIDs()
	if len(runtimeIDs) == 0 {
		d.logger.Debug("deregister: no runtimes to deregister")
		return
	}

	d.logger.Debug("deregistering runtimes on shutdown", "count", len(runtimeIDs), "runtime_ids", runtimeIDs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := d.client.Deregister(ctx, runtimeIDs, nil); err != nil {
		d.logger.Warn("failed to deregister runtimes on shutdown", "error", err)
	} else {
		d.logger.Info("deregistered runtimes", "count", len(runtimeIDs))
	}
}

// resolveAuth loads the auth token from the CLI config for the active profile.
func (d *Daemon) resolveAuth() error {
	cfg, err := cli.LoadCLIConfigForProfile(d.cfg.Profile)
	if err != nil {
		return fmt.Errorf("load CLI config: %w", err)
	}
	if cfg.Token == "" {
		loginHint := "'multica login'"
		if d.cfg.Profile != "" {
			loginHint = fmt.Sprintf("'multica login --profile %s'", d.cfg.Profile)
		}
		d.logger.Warn("not authenticated — run " + loginHint + " to authenticate, then restart the daemon")
		return fmt.Errorf("not authenticated: run %s first", loginHint)
	}
	d.client.SetToken(cfg.Token)
	d.logger.Info("authenticated")
	d.logger.Debug("auth token loaded", "profile", d.cfg.Profile, "token_len", len(cfg.Token))
	return nil
}

// allRuntimeIDs returns all runtime IDs across all watched workspaces.
func (d *Daemon) allRuntimeIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, ws := range d.workspaces {
		ids = append(ids, ws.runtimeIDs...)
	}
	return ids
}

// findRuntime looks up a Runtime by its ID.
func (d *Daemon) findRuntime(id string) *Runtime {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rt, ok := d.runtimeIndex[id]; ok {
		return &rt
	}
	return nil
}

// nativeRuntimes returns the sole runtime hosted by this daemon.
func (d *Daemon) nativeRuntimes() []map[string]string {
	return []map[string]string{{"name": "Multica Native", "type": "native", "version": d.cfg.CLIVersion, "status": "online"}}
}

func cloneRuntimeEntries(in []map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, entry := range in {
		cp := make(map[string]string, len(entry))
		for k, v := range entry {
			cp[k] = v
		}
		out = append(out, cp)
	}
	return out
}

// registerRuntimesForWorkspaceLocked registers the native runtime. The caller
// holds the workspace registration lock through state publication and cleanup.
func (d *Daemon) registerRuntimesForWorkspaceLocked(ctx context.Context, workspaceID string) (*RegisterResponse, error) {
	resp, err := d.client.Register(ctx, map[string]any{
		"workspace_id": workspaceID, "daemon_id": d.cfg.DaemonID,
		"legacy_daemon_ids": d.cfg.LegacyDaemonIDs, "device_name": d.cfg.DeviceName,
		"cli_version": d.cfg.CLIVersion, "launched_by": d.cfg.LaunchedBy,
		"runtimes":      d.nativeRuntimes(),
		"native_limits": d.nativeLimits(),
	})
	if err != nil {
		return nil, fmt.Errorf("register native runtime: %w", err)
	}
	if len(resp.Runtimes) == 0 {
		return nil, fmt.Errorf("register native runtime: empty response")
	}
	return resp, nil
}

func newWorkspaceState(workspaceID string, runtimeIDs []string, reposVersion string, repos []RepoData, settings json.RawMessage) *workspaceState {
	return &workspaceState{
		workspaceID:     workspaceID,
		runtimeIDs:      runtimeIDs,
		reposVersion:    reposVersion,
		allowedRepoURLs: repoAllowlist(repos),
		settings:        settings,
	}
}

func repoAllowlist(repos []RepoData) map[string]struct{} {
	allowed := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo.URL == "" {
			continue
		}
		allowed[repo.URL] = struct{}{}
	}
	return allowed
}

func (d *Daemon) setWorkspaceRepoSyncError(workspaceID, syncErr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ws, ok := d.workspaces[workspaceID]; ok {
		ws.lastRepoSyncErr = syncErr
	}
}

func (d *Daemon) workspaceRepoAllowed(workspaceID, repoURL string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, ok := d.workspaces[workspaceID]
	if !ok {
		return false
	}
	if _, allowed := ws.allowedRepoURLs[repoURL]; allowed {
		return true
	}
	if _, allowed := ws.taskRepoURLs[repoURL]; allowed {
		return true
	}
	return false
}

// repoBarePathIsLive reports whether some watched workspace still claims the
// repo cached at barePath, so the GC can refuse to evict it.
//
// Answering per-path rather than materializing the whole set lets the GC ask
// again immediately before it deletes. A snapshot taken once per cycle goes
// stale while the caller runs git and filesystem work on each repo in turn,
// which is exactly the window in which a workspace can re-attach one.
//
// It mirrors workspaceRepoAllowed by unioning both sources: allowedRepoURLs
// (workspace-level bindings) and taskRepoURLs (project repos the server
// surfaced through a task claim, which never appear in GetWorkspaceRepos).
// Missing the second set would make the GC evict repos that tasks actively
// check out.
//
// Read from in-memory state on purpose. The alternative — asking the server
// for each workspace's repo list during GC — would make a transient API
// failure look like "nothing is attached", and this set is what protects
// caches from deletion.
func (d *Daemon) repoBarePathIsLive(barePath string) bool {
	if d.repoCache == nil || barePath == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for workspaceID, ws := range d.workspaces {
		for url := range ws.allowedRepoURLs {
			if d.repoCache.BarePath(workspaceID, url) == barePath {
				return true
			}
		}
		for url := range ws.taskRepoURLs {
			if d.repoCache.BarePath(workspaceID, url) == barePath {
				return true
			}
		}
	}
	return false
}

func (d *Daemon) workspaceLastRepoSyncErr(workspaceID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, ok := d.workspaces[workspaceID]
	if !ok {
		return ""
	}
	return ws.lastRepoSyncErr
}

// workspaceCoAuthoredByEnabled returns whether the Co-authored-by hook should
// be installed for the given workspace. Defaults to true when either setting
// is absent (new workspaces, older servers that don't send settings).
//
// The hook is gated by BOTH the GitHub master switch (`github_enabled`) and
// the dedicated co-author switch (`co_authored_by_enabled`) so flipping the
// workspace's master GitHub toggle off also stops new trailers from landing
// in commits, matching the contract documented in RFC MUL-2414 §4.8.
func (d *Daemon) workspaceCoAuthoredByEnabled(workspaceID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, ok := d.workspaces[workspaceID]
	if !ok || len(ws.settings) == 0 {
		return true // default: enabled
	}
	var s struct {
		GitHubEnabled       *bool `json:"github_enabled"`
		CoAuthoredByEnabled *bool `json:"co_authored_by_enabled"`
	}
	if err := json.Unmarshal(ws.settings, &s); err != nil {
		return true // default: enabled when payload is malformed
	}
	if s.GitHubEnabled != nil && !*s.GitHubEnabled {
		return false
	}
	if s.CoAuthoredByEnabled == nil {
		return true // default: enabled
	}
	return *s.CoAuthoredByEnabled
}

// registerTaskRepos merges task-scoped repos (e.g. project github_repo
// resources lifted into resp.Repos by the claim handler) into the workspace's
// allowlist and kicks off a cache sync for any URLs that aren't yet cached.
//
// It's safe to call with the workspace's own repos — duplicates are
// idempotent. Called from runTask before the agent spawns so
// `multica repo checkout` accepts project-only URLs without an extra round
// trip back to GetWorkspaceRepos (which doesn't carry project resources).
func (d *Daemon) registerTaskRepos(workspaceID, taskID string, repos []RepoData) {
	if len(repos) == 0 {
		return
	}

	type repoCandidate struct {
		url     string
		tracked bool
	}

	d.mu.Lock()
	ws, ok := d.workspaces[workspaceID]
	if !ok {
		d.mu.Unlock()
		return
	}
	if ws.taskRepoURLs == nil {
		ws.taskRepoURLs = make(map[string]struct{}, len(repos))
	}
	if taskID != "" && ws.taskRepoRefs == nil {
		ws.taskRepoRefs = make(map[string]map[string]string)
	}
	candidates := make([]repoCandidate, 0, len(repos))
	for _, repo := range repos {
		url := strings.TrimSpace(repo.URL)
		if url == "" {
			continue
		}
		// Don't re-sync if the URL is already tracked (workspace or task-scoped)
		// AND the cache already has it.
		_, inWorkspace := ws.allowedRepoURLs[url]
		_, inTask := ws.taskRepoURLs[url]
		ws.taskRepoURLs[url] = struct{}{}
		if taskID != "" {
			if ws.taskRepoRefs[taskID] == nil {
				ws.taskRepoRefs[taskID] = make(map[string]string, len(repos))
			}
			if _, exists := ws.taskRepoRefs[taskID][url]; !exists {
				ws.taskRepoRefs[taskID][url] = strings.TrimSpace(repo.Ref)
			}
		}
		candidates = append(candidates, repoCandidate{
			url:     url,
			tracked: inWorkspace || inTask,
		})
	}
	d.mu.Unlock()

	toSync := make([]RepoData, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.tracked && d.repoCache != nil && d.repoCache.Lookup(workspaceID, candidate.url) != "" {
			continue
		}
		toSync = append(toSync, RepoData{URL: candidate.url})
	}

	if d.repoCache != nil && len(toSync) > 0 {
		// Sync in the background — same shape used at workspace registration.
		// `ensureRepoReady` reports a meaningful error if the cache isn't ready
		// yet, so the agent's first checkout will surface a sync failure
		// without silently treating it as a config bug.
		d.bgSyncs.Add(1)
		go func() {
			defer d.bgSyncs.Done()
			d.syncWorkspaceRepos(workspaceID, toSync)
		}()
	}
}

func (d *Daemon) taskRepoDefaultRef(workspaceID, taskID, repoURL string) string {
	taskID = strings.TrimSpace(taskID)
	repoURL = strings.TrimSpace(repoURL)
	if taskID == "" || repoURL == "" {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ws, ok := d.workspaces[workspaceID]
	if !ok || ws.taskRepoRefs == nil {
		return ""
	}
	return strings.TrimSpace(ws.taskRepoRefs[taskID][repoURL])
}

func (d *Daemon) clearTaskRepoRefs(workspaceID, taskID string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ws, ok := d.workspaces[workspaceID]; ok && ws.taskRepoRefs != nil {
		delete(ws.taskRepoRefs, taskID)
	}
}

// waitBackgroundSyncs blocks until every background sync started by
// registerTaskRepos has finished. Intended for test teardown: tests that
// hand the daemon a t.TempDir-backed repo cache must call this before
// returning, otherwise an in-flight clone/fetch can race against TempDir
// cleanup and surface as an unrelated "directory not empty" failure.
func (d *Daemon) waitBackgroundSyncs() {
	d.bgSyncs.Wait()
}

func (d *Daemon) syncWorkspaceRepos(workspaceID string, repos []RepoData) {
	d.syncWorkspaceReposContext(context.Background(), workspaceID, repos)
}

func (d *Daemon) syncWorkspaceReposContext(ctx context.Context, workspaceID string, repos []RepoData) {
	if d.repoCache == nil {
		return
	}
	var err error
	if cache, ok := d.repoCache.(interface {
		SyncContext(context.Context, string, []repocache.RepoInfo) error
	}); ok {
		err = cache.SyncContext(ctx, workspaceID, repoDataToInfo(repos))
	} else {
		err = d.repoCache.Sync(workspaceID, repoDataToInfo(repos))
	}
	if err != nil {
		d.setWorkspaceRepoSyncError(workspaceID, err.Error())
		d.logger.Warn("repo cache sync failed", "workspace_id", workspaceID, "error", err)
		return
	}
	d.setWorkspaceRepoSyncError(workspaceID, "")
}

func (d *Daemon) refreshWorkspaceRepos(ctx context.Context, workspaceID string) (*WorkspaceReposResponse, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := d.client.GetWorkspaceRepos(refreshCtx, workspaceID)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	if ws, ok := d.workspaces[workspaceID]; ok {
		ws.reposVersion = resp.ReposVersion
		ws.allowedRepoURLs = repoAllowlist(resp.Repos)
		// Keep the cached settings in sync with the server. The daemon's
		// feature gates (e.g. workspaceCoAuthoredByEnabled) read directly from
		// this field, so toggling a Setting in the web UI must update it here
		// without requiring a daemon restart. An empty payload from the server
		// clears the override and falls back to defaults.
		ws.settings = resp.Settings
	}
	d.mu.Unlock()

	// Publish the refreshed Co-authored-by verdict to the repo cache. Hooks
	// installed by earlier checkouts read that file at commit time, so this is
	// what makes a toggled-off setting apply to checkouts that already exist
	// instead of only to the next one (MUL-6921).
	d.persistCoAuthoredByState(workspaceID)

	return resp, nil
}

// coAuthoredByPublisher is the repo cache's half of the Co-authored-by wiring:
// the state file every installed hook reads at commit time, and the hooks
// themselves in the workspace's bare caches. Optional so test daemons can run
// with a minimal repoCacheBackend.
type coAuthoredByPublisher interface {
	WriteCoAuthoredByState(workspaceID string, enabled bool) error
	ReconcileCoAuthoredByHooks(workspaceID string, enabled bool) error
	ReconcileCoAuthoredByHookInCheckout(checkoutPath, workspaceID string, enabled bool) error
}

// persistCoAuthoredByState records the workspace's current Co-authored-by
// verdict where the installed prepare-commit-msg hooks can see it, and brings
// hooks written by earlier daemon releases — which read no state at all — in
// line with it. Called after every settings refresh and on every workspace
// sync; best-effort, since a failed write only leaves the previous value in
// place.
//
// The daemon is the only writer of this state, and publishes under a
// per-workspace lock with the verdict read INSIDE that lock. Two publishers
// racing (a settings refresh and a sync tick, say) therefore serialize, and the
// one that writes last is the one that read last — a publisher that started
// before a settings update can never overwrite the value that update produced.
func (d *Daemon) persistCoAuthoredByState(workspaceID string) {
	d.publishCoAuthoredByState(workspaceID, d.workspaceCoAuthoredByEnabled)
}

// publishCoAuthoredByState is persistCoAuthoredByState with the verdict read
// injected. Production always passes workspaceCoAuthoredByEnabled; tests pass a
// wrapper that parks a publisher between taking the lock and reading, which is
// the one ordering the lock exists to guarantee and cannot be observed
// otherwise.
func (d *Daemon) publishCoAuthoredByState(workspaceID string, verdict func(string) bool) {
	if d.repoCache == nil || workspaceID == "" {
		return
	}
	cache, ok := d.repoCache.(coAuthoredByPublisher)
	if !ok {
		return
	}

	d.mu.Lock()
	ws := d.workspaces[workspaceID]
	d.mu.Unlock()
	if ws == nil {
		return
	}

	ws.coAuthorPublishMu.Lock()
	defer ws.coAuthorPublishMu.Unlock()

	enabled := verdict(workspaceID)
	if err := cache.WriteCoAuthoredByState(workspaceID, enabled); err != nil {
		d.logger.Warn("record co-authored-by state failed", "workspace_id", workspaceID, "error", err)
	}
	if err := cache.ReconcileCoAuthoredByHooks(workspaceID, enabled); err != nil {
		d.logger.Warn("reconcile co-authored-by hooks failed", "workspace_id", workspaceID, "error", err)
	}
	d.reconcileIsolatedCoAuthoredByHooks(cache, workspaceID, enabled)
}

// isolatedCheckoutScanDepth bounds how far below an env root the sweep looks
// for a checkout. Repos land at <env root>/<workdir>/<repo>, so two levels
// covers the layout with one to spare and keeps the walk from wandering into
// the checked-out source tree.
const isolatedCheckoutScanDepth = 2

// reconcileIsolatedCoAuthoredByHooks applies the workspace setting to hooks
// that live inside task workdirs rather than in the shared bare cache.
//
// Codex on Linux and the Windows sandbox check out with isolated git metadata,
// so their hook sits in <checkout>/.git/hooks and the repo cache cannot see it.
// Left alone, a workdir prepared by an earlier release keeps its unconditional
// hook — and its next commit keeps adding the trailer — until that repo happens
// to be checked out again. Env roots are attributed by their owner record, so a
// workspace never rewrites hooks belonging to another one.
func (d *Daemon) reconcileIsolatedCoAuthoredByHooks(cache coAuthoredByPublisher, workspaceID string, enabled bool) {
	root := d.cfg.WorkspacesRoot
	if root == "" || workspaceID == "" {
		return
	}
	wsEntries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			d.logger.Debug("co-authored-by sweep: read workspaces root failed", "error", err)
		}
		return
	}

	for _, wsEntry := range wsEntries {
		// Dot directories are daemon-internal caches (.repos, .skill-cache),
		// never workspace directories — the same rule runGC walks by.
		if !wsEntry.IsDir() || strings.HasPrefix(wsEntry.Name(), ".") {
			continue
		}
		wsDir := filepath.Join(root, wsEntry.Name())
		envRoots, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, envEntry := range envRoots {
			if !envEntry.IsDir() || strings.HasPrefix(envEntry.Name(), ".") {
				continue
			}
			envRoot := filepath.Join(wsDir, envEntry.Name())
			if !d.envRootBelongsToWorkspace(wsEntry.Name(), envRoot, workspaceID) {
				continue
			}
			for _, checkout := range isolatedCheckoutCandidates(envRoot, isolatedCheckoutScanDepth) {
				if err := cache.ReconcileCoAuthoredByHookInCheckout(checkout, workspaceID, enabled); err != nil {
					d.logger.Warn("reconcile co-authored-by hook in checkout failed",
						"workspace_id", workspaceID, "path", checkout, "error", err)
				}
			}
		}
	}
}

// envRootBelongsToWorkspace reports whether an env root is this workspace's, by
// the strongest evidence the release that created it left behind.
//
// Since v0.4.35 the owner record carries the workspace ID, and an exact match
// is required. Env roots prepared before that recorded only a task ID — or, on
// older releases still, no marker at all — and they live under the layout that
// release used: <workspaces root>/<workspace ID>/<task key>. So for an owner
// record that names no workspace, the directory name is the evidence, and it
// has to BE the workspace ID. The readable layout that replaced it always
// appends a suffix (`<slug>-<id tail>`), so a bare workspace UUID can only be
// one of those older roots and this can never alias a modern one.
//
// An unreadable marker attributes nothing: skipping costs a stale hook in one
// workdir, guessing would rewrite hooks in another workspace's.
func (d *Daemon) envRootBelongsToWorkspace(wsDirName, envRoot, workspaceID string) bool {
	owner, err := execenv.ReadEnvRootOwner(envRoot)
	if err != nil || owner == nil {
		return false
	}
	if owner.WorkspaceID != "" {
		return owner.WorkspaceID == workspaceID
	}
	return wsDirName == workspaceID
}

// isolatedCheckoutCandidates returns directories at or below dir that hold
// their own git metadata. A linked worktree's .git is a file, so only isolated
// checkouts match, and matching stops descending: nothing nested inside a
// checkout is one.
func isolatedCheckoutCandidates(dir string, depth int) []string {
	if depth <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		child := filepath.Join(dir, entry.Name())
		if info, err := os.Stat(filepath.Join(child, ".git")); err == nil && info.IsDir() {
			found = append(found, child)
			continue
		}
		found = append(found, isolatedCheckoutCandidates(child, depth-1)...)
	}
	return found
}

// publishTrackedCoAuthoredByState writes the current verdict for every tracked
// workspace and reconciles the hooks in their bare caches. Purely local — no
// request — and both halves skip the write when what is on disk already
// matches, so a sync tick on a converged host costs a handful of small reads.
func (d *Daemon) publishTrackedCoAuthoredByState() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.workspaces))
	for id := range d.workspaces {
		ids = append(ids, id)
	}
	d.mu.Unlock()

	for _, id := range ids {
		d.persistCoAuthoredByState(id)
	}
}

// trackedSettingsRefreshTimeout bounds one refreshTrackedWorkspaceSettings
// pass. A workspace it does not reach keeps its cached settings and picks the
// change up on its next repo checkout.
const trackedSettingsRefreshTimeout = 30 * time.Second

// refreshTrackedWorkspaceSettings re-reads repos and settings for workspaces
// this daemon already tracks. Only the server's workspaces-changed hint calls
// it: the periodic sync deliberately makes no repos request for a tracked
// workspace (see syncWorkspacesFromAPI), so without this a settings edit —
// the GitHub master switch, the Co-authored-by toggle — would sit unseen
// until the workspace's next repo checkout.
func (d *Daemon) refreshTrackedWorkspaceSettings(ctx context.Context) {
	// The sync loop is single-threaded and still owes the hint its membership
	// reconcile, so bound the whole pass rather than letting one slow
	// workspace hold every other signal behind it.
	ctx, cancel := context.WithTimeout(ctx, trackedSettingsRefreshTimeout)
	defer cancel()

	d.mu.Lock()
	tracked := make(map[string]*workspaceState, len(d.workspaces))
	for id, ws := range d.workspaces {
		tracked[id] = ws
	}
	d.mu.Unlock()

	for id, ws := range tracked {
		if ctx.Err() != nil {
			return
		}
		// Share ensureRepoReady's lock so a checkout in flight and this
		// refresh cannot interleave their writes to the same workspace.
		if err := ws.repoRefreshMu.Lock(ctx); err != nil {
			return
		}
		_, err := d.refreshWorkspaceRepos(ctx, id)
		ws.repoRefreshMu.Unlock()
		if err != nil {
			d.logger.Debug("workspace settings refresh failed", "workspace_id", id, "error", err)
		}
	}
}

func (d *Daemon) ensureRepoReady(ctx context.Context, workspaceID, repoURL string) error {
	if d.repoCache == nil {
		return fmt.Errorf("repo cache not initialized")
	}

	repoURL = strings.TrimSpace(repoURL)

	d.mu.Lock()
	ws, ok := d.workspaces[workspaceID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace is not watched by this daemon: %s", workspaceID)
	}

	// Record whether the cache already had this repo before we took the
	// per-workspace mutex. The two states behave differently below:
	//
	//   - cacheHitOnEntry=true: the repo is already cloned; we still must
	//     refresh `workspaceState.settings` because the /repo/checkout
	//     handler reads workspaceCoAuthoredByEnabled right after this. The
	//     periodic workspace sync deliberately does not refresh repos or
	//     settings, so this is the point at which a freshly-flipped GitHub
	//     master switch / `co_authored_by_enabled` toggle becomes live.
	//
	//   - cacheHitOnEntry=false but cache hit *after* we acquire the mutex:
	//     a sibling goroutine on a concurrent cold-miss already refreshed
	//     and populated the cache. We can skip the duplicate refresh — the
	//     sibling's refresh is fresh enough for our gate read.
	cacheHitOnEntry := d.workspaceRepoAllowed(workspaceID, repoURL) && d.repoCache.Lookup(workspaceID, repoURL) != ""

	if err := ws.repoRefreshMu.Lock(ctx); err != nil {
		return err
	}
	defer ws.repoRefreshMu.Unlock()

	if !cacheHitOnEntry && d.workspaceRepoAllowed(workspaceID, repoURL) && d.repoCache.Lookup(workspaceID, repoURL) != "" {
		return nil
	}

	resp, err := d.refreshWorkspaceRepos(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("refresh workspace repos: %w", err)
	}

	if !d.workspaceRepoAllowed(workspaceID, repoURL) {
		return ErrRepoNotConfigured
	}

	if d.repoCache.Lookup(workspaceID, repoURL) != "" {
		return nil
	}

	d.syncWorkspaceReposContext(ctx, workspaceID, resp.Repos)
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}

	if d.repoCache.Lookup(workspaceID, repoURL) != "" {
		return nil
	}

	if syncErr := d.workspaceLastRepoSyncErr(workspaceID); syncErr != "" {
		return fmt.Errorf("repo is configured but not synced: %s", syncErr)
	}

	return fmt.Errorf("repo is configured but not synced")
}

// DefaultTokenRenewalInterval is how often the daemon asks the server to
// extend its PAT. The server-side threshold is 7 days of remaining lifetime;
// polling every ~3 days gives at least two chances to renew before the
// window closes, so a single failed call (network blip, server restart) does
// not push the token out of the renewal window.
const DefaultTokenRenewalInterval = 3 * 24 * time.Hour

// preflightAuth runs the two auth-sensitive startup steps in their
// required order: a synchronous PAT renewal first, then the initial
// workspace sync. The order matters — running tryRenewToken before any
// other API call is what surfaces a user-actionable "run multica login"
// WARN when the PAT is already revoked or expired. If we let the
// workspace sync go first, its 401 would short-circuit Run before the
// renewal loop's first tick ever fires, and the operator would see only
// a generic auth failure in the workspace-sync log with no hint that
// re-login is the fix.
//
// The renewal is best-effort: tryRenewToken logs and returns, never
// propagating errors. preflightAuth's exit status is driven entirely by
// the workspace sync — so a transient renewal failure (network blip,
// 500) does not by itself block startup. A successful sync with zero
// workspaces is fine: a newly-signed-up user may start the daemon
// before creating their first workspace, and workspaceSyncLoop will
// register runtimes once one appears.
func (d *Daemon) preflightAuth(ctx context.Context) error {
	d.tryRenewToken(ctx)
	return d.syncWorkspacesFromAPI(ctx)
}

// tokenRenewalLoop keeps the daemon's PAT alive by periodically asking the
// server to extend its expires_at in-place. The startup renewal happens
// synchronously in preflightAuth so a daemon coming back online after a
// week of downtime gets a fresh expiry before its next heartbeat could
// 401; this loop owns the long-running ~3-day cadence after that.
//
// The server is authoritative on the renewal threshold (it sees expires_at;
// we don't), so this loop is intentionally dumb: call, log, sleep, repeat.
// On 401 we surface a clear "re-login required" warning because the daemon
// has no way to recover automatically — but we keep the loop running so the
// user sees the same warning on every cycle until they fix it, rather than
// silently exiting and forcing them to read scrollback to find the cause.
func (d *Daemon) tokenRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(DefaultTokenRenewalInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.tryRenewToken(ctx)
		}
	}
}

// tryRenewToken performs one renewal round-trip with a short, isolated
// timeout. Errors are logged but never propagated — there is no caller to
// handle them. Failures are debug-level except for 401, which gets a
// user-actionable warning.
func (d *Daemon) tryRenewToken(ctx context.Context) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := d.client.RenewToken(reqCtx)
	if err != nil {
		if isUnauthorizedError(err) {
			loginHint := "'multica login'"
			if d.cfg.Profile != "" {
				loginHint = fmt.Sprintf("'multica login --profile %s'", d.cfg.Profile)
			}
			d.logger.Warn("auth token rejected by server — run "+loginHint+" to re-authenticate, then restart the daemon", "error", err)
			return
		}
		d.logger.Debug("token renewal failed; will retry on next cycle", "error", err)
		return
	}
	if resp.Renewed {
		d.logger.Info("auth token renewed", "expires_at", resp.ExpiresAt)
	} else {
		d.logger.Debug("auth token not yet eligible for renewal", "expires_at", resp.ExpiresAt)
	}
}

// workspaceSyncLoop reconciles the user's workspace membership set. Daemons
// with runtimes and account-scoped WS support use a thirty-minute jittered
// consistency check; daemons talking to older servers retain a five-minute
// fallback. Bootstrap daemons without runtimes keep the shorter interval needed
// to discover their first workspace. Account-scoped WS hints trigger an
// immediate minimal sync, while a WS reconnect refreshes settings
// changed during the connection gap.
func (d *Daemon) workspaceSyncLoop(ctx context.Context) {
	timer := time.NewTimer(jitterDuration(d.workspaceSyncBaseInterval()))
	defer timer.Stop()

	var reconcileCh <-chan struct{}
	if d.reconcile != nil {
		reconcileCh = d.reconcile.notify()
	}
	var workspaceChangesCh <-chan struct{}
	if d.workspaceChanges != nil {
		workspaceChangesCh = d.workspaceChanges.notify()
	}

	var consecutiveFailures int
	resetTimer := func() {
		interval := workspaceSyncBackoff(d.workspaceSyncBaseInterval(), consecutiveFailures)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(jitterDuration(interval))
	}

	syncNow := func() {
		if err := d.syncWorkspacesFromAPI(ctx); err != nil {
			consecutiveFailures++
			d.logger.Debug("workspace sync failed", "error", err)
		} else {
			consecutiveFailures = 0
		}
		resetTimer()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcileCh:
			if d.reconcile != nil {
				reconcileCh = d.reconcile.notify()
			}
			// A settings edit made while the websocket was down produced a
			// workspaces-changed hint nobody received. Reconnecting is the
			// daemon's chance to catch up on it: without this the stale
			// verdict would just get republished by the sync below.
			d.refreshTrackedWorkspaceSettings(ctx)
			syncNow()
		case <-workspaceChangesCh:
			// The hint fires for membership changes AND for workspace edits,
			// including settings. Refresh cached settings for the workspaces
			// we already track before reconciling the membership set.
			d.refreshTrackedWorkspaceSettings(ctx)
			syncNow()
		case <-timer.C:
			syncNow()
		}
	}
}

func (d *Daemon) workspaceSyncBaseInterval() time.Duration {
	if len(d.allRuntimeIDs()) == 0 {
		return DefaultWorkspaceBootstrapSyncInterval
	}
	if d.client.usesLegacyWorkspaceEndpoint() {
		return DefaultWorkspaceLegacySyncInterval
	}
	return DefaultWorkspaceSyncInterval
}

func workspaceSyncBackoff(base time.Duration, consecutiveFailures int) time.Duration {
	maxInterval := DefaultWorkspaceSyncMaxBackoff
	if base == DefaultWorkspaceBootstrapSyncInterval {
		maxInterval = DefaultWorkspaceLegacySyncInterval
	}
	interval := base
	for i := 0; i < consecutiveFailures; i++ {
		if interval >= maxInterval/2 {
			return maxInterval
		}
		interval *= 2
	}
	if interval > maxInterval {
		return maxInterval
	}
	return interval
}

// syncWorkspacesFromAPI reconciles membership and registers the native runtime
// for newly joined workspaces. Existing workspace state is preserved.
func (d *Daemon) syncWorkspacesFromAPI(ctx context.Context) error {
	d.reloading.Lock()
	defer d.reloading.Unlock()

	apiCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	workspaces, err := d.client.ListWorkspaces(apiCtx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	d.logger.Debug("workspace sync: fetched workspaces", "count", len(workspaces))

	apiIDs := make(map[string]string, len(workspaces)) // id -> name
	for _, ws := range workspaces {
		apiIDs[ws.ID] = ws.Name
	}

	d.mu.Lock()
	currentIDs := make(map[string]bool, len(d.workspaces))
	for id := range d.workspaces {
		currentIDs[id] = true
	}
	d.mu.Unlock()

	var registered int
	var removed int
	for id, name := range apiIDs {
		if currentIDs[id] {
			// Only intervene further if the workspace lost all of its
			// runtimes (most commonly because handleRuntimeGone pruned them
			// and its inline re-register failed). The pointer is not replaced
			// here either — ensureRepoReady holds repoRefreshMu from the
			// original pointer.
			if !d.workspaceNeedsRuntimeRecovery(id) {
				continue
			}
			d.logger.Info("workspace has no runtimes; retrying registration", "workspace_id", id, "name", name)
			if err := d.reregisterWorkspaceAfterRuntimeGone(ctx, id); err != nil {
				d.logger.Warn("retry register failed", "workspace_id", id, "error", err)
				continue
			}
			registered++
			continue
		}
		var (
			resp       *RegisterResponse
			runtimeIDs []string
		)
		// Send, publish and clean up as one ordered step — see
		// workspaceRegisterLock.
		err := d.withWorkspaceRegisterLock(id, func() error {
			var err error
			resp, err = d.registerRuntimesForWorkspaceLocked(ctx, id)
			if err != nil {
				return err
			}
			d.mu.Lock()
			runtimeIDs = make([]string, 0, len(resp.Runtimes))
			for _, rt := range resp.Runtimes {
				runtimeIDs = append(runtimeIDs, rt.ID)
				d.runtimeIndex[rt.ID] = rt
				d.logger.Info("registered native runtime", "workspace_id", id, "runtime_id", rt.ID)
			}
			d.workspaces[id] = newWorkspaceState(id, runtimeIDs, resp.ReposVersion, resp.Repos, resp.Settings)
			d.mu.Unlock()
			return nil
		})
		if err != nil {
			d.logger.Error("failed to register runtimes", "workspace_id", id, "name", name, "error", err)
			continue
		}

		if d.repoCache != nil && len(resp.Repos) > 0 {
			go d.syncWorkspaceRepos(id, resp.Repos)
		}

		// Tell the server about any tasks the previous daemon process was
		// running on these runtimes. Without this, an issue can stay stuck
		// at in_progress until the slow heartbeat sweeper or the in-flight
		// task timeout (2.5h) kicks in.
		for _, rid := range runtimeIDs {
			if err := d.client.RecoverOrphans(ctx, rid); err != nil {
				d.logger.Warn("recover-orphans failed", "runtime_id", rid, "error", err)
			}
		}

		d.logger.Info("watching workspace", "workspace_id", id, "name", name, "runtimes", len(runtimeIDs), "repos", len(resp.Repos))
		registered++
	}

	// Remove workspaces the user no longer belongs to.
	for id := range currentIDs {
		if _, ok := apiIDs[id]; !ok {
			d.mu.Lock()
			if ws, exists := d.workspaces[id]; exists {
				for _, rid := range ws.runtimeIDs {
					delete(d.runtimeIndex, rid)
				}
			}
			delete(d.workspaces, id)
			d.mu.Unlock()
			d.logger.Info("stopped watching workspace", "workspace_id", id)
			removed++
		}
	}
	if registered > 0 || removed > 0 {
		d.notifyRuntimeSetChanged()
	}

	// Republish each tracked workspace's Co-authored-by verdict. This costs no
	// request — it writes the daemon's cached verdict where prepare-commit-msg
	// hooks read it, and migrates hooks left by earlier releases — and is what
	// covers the cases no refresh reaches: a toggle flipped while the daemon
	// was down (settings arrive with the register response above), a host that
	// just upgraded, and a state file removed by repo cache GC.
	d.publishTrackedCoAuthoredByState()

	if len(d.allRuntimeIDs()) == 0 && registered == 0 && len(workspaces) > 0 {
		return fmt.Errorf("%w for any of the %d workspace(s)", errNoWorkspaceRuntimesRegistered, len(workspaces))
	}
	if registered > 0 || removed > 0 {
		d.logger.Debug("workspace sync done", "registered", registered, "removed", removed, "tracked", len(apiIDs))
	}
	return nil
}

// heartbeatLoop supervises per-runtime HTTP heartbeat goroutines. Each runtime
// gets an independent ticker so a slow heartbeat for one runtime cannot block
// heartbeats for any other runtime — this matters when a single daemon serves
// multiple workspaces, because the previous shared loop would serialize an
// up-to-30s HTTP timeout across every runtime in the set.
func (d *Daemon) heartbeatLoop(ctx context.Context) {
	runtimeSetCh, unsub := d.runtimeSet.Subscribe()
	defer unsub()

	cancels := make(map[string]context.CancelFunc)
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	sync := func() {
		want := make(map[string]struct{})
		for _, rid := range d.allRuntimeIDs() {
			want[rid] = struct{}{}
		}
		for rid, cancel := range cancels {
			if _, ok := want[rid]; !ok {
				cancel()
				delete(cancels, rid)
			}
		}
		for rid := range want {
			if _, ok := cancels[rid]; ok {
				continue
			}
			rctx, rcancel := context.WithCancel(ctx)
			cancels[rid] = rcancel
			go d.runRuntimeHeartbeat(rctx, rid)
		}
	}

	sync()
	for {
		select {
		case <-ctx.Done():
			return
		case <-runtimeSetCh:
			sync()
		}
	}
}

// runRuntimeHeartbeat owns the HTTP heartbeat schedule for a single runtime.
// The first tick fires after a small jittered delay (up to one full interval)
// to avoid a thundering herd when the daemon registers many runtimes at once.
func (d *Daemon) runRuntimeHeartbeat(ctx context.Context, rid string) {
	interval := d.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	// Jittered initial delay; cap at the interval so the first beat still
	// happens within one period.
	if jitter := time.Duration(rand.Int63n(int64(interval))); jitter > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}
	}

	consecutiveTransientFailures := 0
	tick := func() {
		if d.runHeartbeatTick(ctx, rid) {
			consecutiveTransientFailures++
			if consecutiveTransientFailures == 2 {
				d.client.CloseIdleConnections()
			}
			return
		}
		consecutiveTransientFailures = 0
	}

	tick()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// runHeartbeatTick returns true when the HTTP heartbeat hit a transient
// failure that should count toward stale idle-connection cleanup.
func (d *Daemon) runHeartbeatTick(ctx context.Context, rid string) bool {
	// Skip HTTP heartbeat for runtimes that successfully acked a recent
	// WebSocket heartbeat. The WS path keeps last_seen_at fresh and delivers
	// actions, so the HTTP write would be a duplicate DB update. If the WS
	// heartbeat goes silent the freshness window expires and HTTP resumes
	// automatically on the next tick — that is the fallback the WS path
	// relies on.
	if d.wsHeartbeatRecentlyAcked(rid) {
		d.logger.Debug("heartbeat: skipping HTTP tick, WS recently acked", "runtime_id", rid)
		return false
	}
	d.logger.Debug("heartbeat: HTTP tick", "runtime_id", rid)
	resp, err := d.client.SendHeartbeat(ctx, rid)
	if err != nil {
		if ctx.Err() == nil {
			if isRuntimeNotFoundError(err) {
				// Server says this runtime is gone — recover instead of
				// looping on the dead UUID. handleRuntimeGone coalesces
				// concurrent callers and runs the recovery HTTP call under
				// the daemon root context so notifyRuntimeSetChanged
				// tearing down this heartbeat goroutine cannot abort it.
				go d.handleRuntimeGone(rid)
				return false
			}
			d.logger.Warn("heartbeat failed", "runtime_id", rid, "error", err)
		}
		return ctx.Err() == nil && isTransientError(err)
	}
	if resp != nil && resp.RuntimeGone {
		// The WS path returns a successful ack with RuntimeGone=true for the
		// same scenario; treat it the same way here in case HTTP starts
		// surfacing this signal too.
		go d.handleRuntimeGone(rid)
		return false
	}
	d.handleHeartbeatActions(ctx, rid, resp)
	return false
}

// handleHeartbeatActions dispatches the pending-action set returned by either
// transport (HTTP POST /api/daemon/heartbeat or WS daemon:heartbeat_ack).
// Each action is dispatched in its own goroutine so a slow handler cannot
// block subsequent heartbeats.
func (d *Daemon) handleHeartbeatActions(ctx context.Context, runtimeID string, resp *HeartbeatResponse) {
	if resp == nil {
		return
	}
	if resp.PendingUpdate != nil || resp.PendingModelList != nil || resp.PendingLocalSkills != nil || resp.PendingLocalSkillImport != nil {
		d.logger.Debug("heartbeat: pending actions",
			"runtime_id", runtimeID,
			"update", resp.PendingUpdate != nil,
			"model_list", resp.PendingModelList != nil,
			"local_skills", resp.PendingLocalSkills != nil,
			"local_skill_import", resp.PendingLocalSkillImport != nil,
		)
	}
	if resp.PendingUpdate != nil {
		go d.handleUpdate(ctx, runtimeID, resp.PendingUpdate)
	}
	if resp.PendingModelList != nil {
		if rt := d.findRuntime(runtimeID); rt != nil {
			go d.handleModelList(ctx, *rt, resp.PendingModelList.ID)
		}
	}
	if resp.PendingLocalSkills != nil {
		if rt := d.findRuntime(runtimeID); rt != nil {
			go d.handleLocalSkillList(ctx, *rt, resp.PendingLocalSkills.ID)
		}
	}
	// Prefer the batch field (new backend); fall back to singular (old backend).
	if len(resp.PendingLocalSkillImports) > 0 {
		if rt := d.findRuntime(runtimeID); rt != nil {
			for _, imp := range resp.PendingLocalSkillImports {
				go d.handleLocalSkillImport(ctx, *rt, imp)
			}
		}
	} else if resp.PendingLocalSkillImport != nil {
		if rt := d.findRuntime(runtimeID); rt != nil {
			go d.handleLocalSkillImport(ctx, *rt, *resp.PendingLocalSkillImport)
		}
	}
}

// handlePendingWorkHint reacts to a server-pushed daemon:pending_work frame by
// sending ONE immediate heartbeat for the runtime, then dispatching whatever
// that heartbeat claimed (MUL-5444).
//
// Why a heartbeat and not the work itself: the hint deliberately carries no
// request payload, so the server never has to un-claim anything when delivery
// fails, and a duplicated or replayed hint cannot duplicate work — the claim
// stays atomic inside the store's PopPending. A hint that never arrives (daemon
// offline, WS gap, relay drop) costs nothing but the pre-existing wait for the
// next scheduled heartbeat.
//
// The HTTP heartbeat is used on purpose rather than queueing a WS frame: the
// hint arrives on the read pump, the WS write path may be backed up or tearing
// down, and this is a human-interactive, low-frequency path where one extra
// request is cheaper than a missed wakeup. Note it intentionally bypasses the
// wsHeartbeatRecentlyAcked suppression that the scheduled HTTP tick honours —
// that suppression exists to avoid duplicate periodic writes, not to block an
// explicitly requested pull.
func (d *Daemon) handlePendingWorkHint(runtimeID, kind string) {
	if runtimeID == "" {
		return
	}
	if d.findRuntime(runtimeID) == nil {
		// Not one of ours (stale relay fanout, or the runtime was just pruned).
		return
	}

	d.pendingWorkMu.Lock()
	if d.pendingWorkInflight == nil {
		d.pendingWorkInflight = make(map[string]struct{})
	}
	if d.pendingWorkLastRun == nil {
		d.pendingWorkLastRun = make(map[string]time.Time)
	}
	// Drop long-idle bookkeeping so the map can't grow with every runtime this
	// process has ever seen.
	for id, at := range d.pendingWorkLastRun {
		if time.Since(at) > pendingWorkHintBookkeepingTTL {
			delete(d.pendingWorkLastRun, id)
		}
	}
	if _, inflight := d.pendingWorkInflight[runtimeID]; inflight {
		d.pendingWorkMu.Unlock()
		return
	}
	// Rate limit per runtime. The hint is caller-triggered by interactive model
	// or capability endpoints, so without a floor a request loop would become a
	// heartbeat amplifier. A suppressed hint costs nothing but the pre-existing
	// wait for the scheduled heartbeat.
	if last, ok := d.pendingWorkLastRun[runtimeID]; ok && time.Since(last) < pendingWorkHintMinInterval {
		d.pendingWorkMu.Unlock()
		d.logger.Debug("pending work hint throttled", "runtime_id", runtimeID, "kind", kind)
		return
	}
	d.pendingWorkInflight[runtimeID] = struct{}{}
	d.pendingWorkLastRun[runtimeID] = time.Now()
	d.pendingWorkMu.Unlock()
	defer func() {
		d.pendingWorkMu.Lock()
		delete(d.pendingWorkInflight, runtimeID)
		d.pendingWorkMu.Unlock()
	}()

	// Root context: handleHeartbeatActions hands this ctx to the actual work, so
	// it must outlive this function. Capability discovery and local-skill imports
	// are normally quick filesystem operations, while model discovery may shell
	// out to a CLI for up to ~40s on the slowest provider (see
	// agent.hermesDiscoveryTimeout). Only the heartbeat request itself is
	// time-bounded.
	ctx := d.recoveryContext()
	if ctx.Err() != nil {
		return
	}
	hbCtx, cancel := context.WithTimeout(ctx, pendingWorkHeartbeatTimeout)
	resp, err := d.client.SendHeartbeat(hbCtx, runtimeID)
	cancel()
	if err != nil {
		if isRuntimeNotFoundError(err) {
			go d.handleRuntimeGone(runtimeID)
			return
		}
		d.logger.Debug("pending work hint heartbeat failed", "runtime_id", runtimeID, "kind", kind, "error", err)
		return
	}
	if resp == nil {
		return
	}
	if resp.RuntimeGone {
		go d.handleRuntimeGone(runtimeID)
		return
	}
	d.logger.Debug("pending work hint served", "runtime_id", runtimeID, "kind", kind)
	d.handleHeartbeatActions(ctx, runtimeID, resp)
}

// handleModelList reports the single deployment model.
func (d *Daemon) handleModelList(ctx context.Context, rt Runtime, requestID string) {
	if rt.Provider != "native" {
		d.reportModelListResult(ctx, rt, requestID, map[string]any{"status": "failed", "error": "External AI runtimes have been retired; migrate to Multica Native"})
		return
	}
	d.handleNativeModelList(ctx, rt, requestID)
}

func (d *Daemon) handleLocalSkillList(ctx context.Context, rt Runtime, requestID string) {
	d.logger.Info("runtime local skills requested", "runtime_id", rt.ID, "request_id", requestID, "provider", rt.Provider)

	skills, supported, err := listRuntimeLocalSkills(rt.Provider)
	if err != nil {
		d.reportLocalSkillListResult(ctx, rt, requestID, map[string]any{
			"status": "failed",
			"error":  err.Error(),
		})
		return
	}

	d.reportLocalSkillListResult(ctx, rt, requestID, map[string]any{
		"status":        "completed",
		"skills":        skills,
		"supported":     supported,
		"mcp_servers":   []any{},
		"mcp_supported": false,
	})
}

func (d *Daemon) handleLocalSkillImport(ctx context.Context, rt Runtime, pending PendingLocalSkillImport) {
	d.logger.Info("runtime local skill import requested", "runtime_id", rt.ID, "request_id", pending.ID, "provider", rt.Provider, "skill_key", pending.SkillKey)

	skill, supported, err := loadRuntimeLocalSkillBundle(rt.Provider, pending.SkillKey)
	if err != nil {
		d.reportLocalSkillImportResult(ctx, rt, pending.ID, map[string]any{
			"status": "failed",
			"error":  err.Error(),
		})
		return
	}
	if !supported {
		d.reportLocalSkillImportResult(ctx, rt, pending.ID, map[string]any{
			"status": "failed",
			"error":  fmt.Sprintf("provider %q does not expose runtime local skills", rt.Provider),
		})
		return
	}

	d.reportLocalSkillImportResult(ctx, rt, pending.ID, map[string]any{
		"status": "completed",
		"skill":  skill,
	})
}

// runtimeReportBackoffs defines the retry schedule for delivering any
// daemon→server async result (model list, local-skill list, local-skill
// import). First attempt runs immediately, then we back off. The sum
// (≈6.5s) leaves most of the server-side running timeout (60s) available for
// discovery and report attempts. Each HTTP attempt has its own client timeout,
// so this schedule alone is not an end-to-end delivery deadline.
//
// Overridable for tests to avoid real sleeps.
var runtimeReportBackoffs = []time.Duration{0, 500 * time.Millisecond, 2 * time.Second, 4 * time.Second}

// reportLocalSkillListResult delivers a list-report to the server with retry
// on transient failures. See reportRuntimeResultWithRetry for semantics.
func (d *Daemon) reportLocalSkillListResult(ctx context.Context, rt Runtime, requestID string, payload map[string]any) {
	d.reportRuntimeResultWithRetry(ctx, "local_skill_list", rt.ID, requestID, func(ctx context.Context) error {
		return d.client.ReportLocalSkillListResult(ctx, rt.ID, requestID, payload)
	})
}

// reportLocalSkillImportResult delivers an import-report to the server with
// retry on transient failures.
func (d *Daemon) reportLocalSkillImportResult(ctx context.Context, rt Runtime, requestID string, payload map[string]any) {
	d.reportRuntimeResultWithRetry(ctx, "local_skill_import", rt.ID, requestID, func(ctx context.Context) error {
		return d.client.ReportLocalSkillImportResult(ctx, rt.ID, requestID, payload)
	})
}

// reportModelListResult delivers a model-list report to the server with retry
// on transient failures. Without this the daemon used to fire once and
// swallow any 5xx, leaving the request stranded in "running" on the server
// until its 60s timeout — defeating the multi-node store fix.
func (d *Daemon) reportModelListResult(ctx context.Context, rt Runtime, requestID string, payload map[string]any) {
	d.reportRuntimeResultWithRetry(ctx, "model_list", rt.ID, requestID, func(ctx context.Context) error {
		return d.client.ReportModelListResult(ctx, rt.ID, requestID, payload)
	})
}

// reportRuntimeResultWithRetry retries `fn` on 5xx / network errors and
// stops on success, 4xx, or after exhausting runtimeReportBackoffs.
//
// Why this exists: the server persists the report through a Redis / DB
// write; on a transient store failure it correctly returns 500. Without a
// client-side retry the daemon would fire once, swallow the error, and the
// pending request stays in "running" on the server until its timeout — which
// is exactly the "daemon did not respond" failure mode the multi-node store
// fix was meant to eliminate. 4xx is treated as permanent (request-not-found,
// cross-workspace token rejected, bad body) — retrying those just wastes
// heartbeat cycles.
func (d *Daemon) reportRuntimeResultWithRetry(ctx context.Context, kind, runtimeID, requestID string, fn func(context.Context) error) {
	var lastErr error
	for attempt, wait := range runtimeReportBackoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				d.logger.Error("runtime async report cancelled",
					"kind", kind, "runtime_id", runtimeID, "request_id", requestID,
					"attempt", attempt, "error", ctx.Err())
				return
			case <-time.After(wait):
			}
		}
		err := fn(ctx)
		if err == nil {
			if attempt > 0 {
				d.logger.Info("runtime async report succeeded after retry",
					"kind", kind, "runtime_id", runtimeID, "request_id", requestID,
					"attempt", attempt+1)
			}
			return
		}
		lastErr = err

		// 4xx is permanent (request expired, workspace mismatch, malformed
		// body). No amount of retrying will make it succeed.
		var reqErr *requestError
		if errors.As(err, &reqErr) && reqErr.StatusCode >= 400 && reqErr.StatusCode < 500 {
			d.logger.Error("runtime async report rejected — not retrying",
				"kind", kind, "runtime_id", runtimeID, "request_id", requestID,
				"status", reqErr.StatusCode, "error", err)
			return
		}

		d.logger.Warn("runtime async report failed — will retry",
			"kind", kind, "runtime_id", runtimeID, "request_id", requestID,
			"attempt", attempt+1, "error", err)
	}
	d.logger.Error("runtime async report exhausted retries",
		"kind", kind, "runtime_id", runtimeID, "request_id", requestID, "error", lastErr)
}

// handleUpdate performs the CLI update when triggered by the server via heartbeat.
func (d *Daemon) handleUpdate(ctx context.Context, runtimeID string, update *PendingUpdate) {
	// Desktop-managed daemons share their CLI binary with the Electron app,
	// which is responsible for shipping and replacing it. Letting the daemon
	// self-update would just get overwritten on the next Desktop launch and
	// could brick the embedded binary mid-update. Refuse cleanly.
	if d.cfg.LaunchedBy == "desktop" {
		d.logger.Info("refusing CLI self-update: daemon is managed by Desktop", "runtime_id", runtimeID, "update_id", update.ID)
		d.reportUpdateResult(ctx, runtimeID, update.ID, map[string]any{
			"status": "failed",
			"error":  "CLI is managed by Multica Desktop — update the Desktop app to upgrade the CLI",
		})
		return
	}

	switch d.tryBeginServerUpdate(ctx) {
	case serverUpdateAlreadyRunning:
		d.logger.Warn("update deferred: another update is already in progress", "runtime_id", runtimeID, "update_id", update.ID)
		d.reportUpdateResult(ctx, runtimeID, update.ID, map[string]any{
			"status": "failed",
			"error":  "another runtime update is already in progress on this machine",
		})
		return
	case serverUpdateRuntimeBusy:
		d.logger.Info("update deferred: task or claim in progress", "runtime_id", runtimeID, "update_id", update.ID)
		d.reportUpdateResult(ctx, runtimeID, update.ID, map[string]any{
			"status": "failed",
			"error":  "runtime update deferred because agent work is starting or still active; retry when the machine is idle",
		})
		return
	}
	restarting := false
	defer func() {
		if !restarting {
			d.releaseClaimBarrier()
			d.updating.Store(false)
		}
	}()

	d.logger.Info("CLI update requested", "runtime_id", runtimeID, "update_id", update.ID, "target_version", update.TargetVersion)

	// Report running status.
	d.reportUpdateResult(ctx, runtimeID, update.ID, map[string]any{
		"status": "running",
	})

	output, err := d.runUpdateFn(update.TargetVersion)
	if err != nil {
		d.logger.Error("CLI update failed", "error", err, "output", output)
		d.reportUpdateResult(ctx, runtimeID, update.ID, map[string]any{
			"status": "failed",
			"error":  err.Error(),
		})
		return
	}

	d.logger.Info("CLI update completed successfully", "output", output)
	d.reportUpdateResult(ctx, runtimeID, update.ID, map[string]any{
		"status": "completed",
		"output": fmt.Sprintf("Updated to %s", update.TargetVersion),
	})

	// Trigger daemon restart with the new binary.
	d.triggerRestart()
	restarting = d.RestartBinary() != ""
}

type serverUpdateAcquireResult uint8

const (
	serverUpdateAcquired serverUpdateAcquireResult = iota
	serverUpdateAlreadyRunning
	serverUpdateRuntimeBusy
)

// tryBeginServerUpdate atomically claims update ownership, pauses new claims,
// and lets any claim already in flight finish its handoff. An empty claim can
// therefore never starve an update; a claim that returns work increments
// activeTasks before exitClaim, so the final check still defers safely.
func (d *Daemon) tryBeginServerUpdate(ctx context.Context) serverUpdateAcquireResult {
	if !d.updating.CompareAndSwap(false, true) {
		return serverUpdateAlreadyRunning
	}

	d.claimMu.Lock()
	if d.pauseClaims || d.activeTasks.Load() > 0 {
		d.claimMu.Unlock()
		d.updating.Store(false)
		return serverUpdateRuntimeBusy
	}
	d.pauseClaims = true
	d.claimMu.Unlock()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		d.claimMu.Lock()
		if d.claimsInFlight == 0 {
			if d.activeTasks.Load() == 0 {
				d.claimMu.Unlock()
				return serverUpdateAcquired
			}
			d.pauseClaims = false
			d.claimMu.Unlock()
			d.updating.Store(false)
			return serverUpdateRuntimeBusy
		}
		d.claimMu.Unlock()

		select {
		case <-ctx.Done():
			d.releaseClaimBarrier()
			d.updating.Store(false)
			return serverUpdateRuntimeBusy
		case <-ticker.C:
		}
	}
}

// runUpdate executes the brew-or-download upgrade against targetVersion and
// returns the human-readable output (always populated, even on failure when
// brew gives us a useful diagnostic). The caller is responsible for the
// `updating` CAS guard and for reporting status back to the server / triggering
// the restart — extracted so the server-triggered path (handleUpdate) and the
// auto-update poller (autoUpdateLoop) share the exact same execution body.
func (d *Daemon) runUpdate(targetVersion string) (string, error) {
	if cli.IsBrewInstall() {
		d.logger.Info("updating CLI via Homebrew...")
		out, err := cli.UpdateViaBrew()
		if err != nil {
			return out, fmt.Errorf("brew upgrade failed: %w", err)
		}
		return out, nil
	}
	d.logger.Info("updating CLI via direct download...", "target_version", targetVersion)
	out, err := cli.UpdateViaDownload(targetVersion)
	if err != nil {
		return out, fmt.Errorf("download update failed: %w", err)
	}
	return out, nil
}

// updateReportBackoffs defines the retry schedule for delivering CLI update
// status back to the server. This mirrors localSkillReportBackoffs because
// both features have the same user-visible failure mode: the daemon completed
// work locally, but a transient report failure leaves the UI waiting until the
// server-side request times out.
//
// Overridable for tests to avoid real sleeps.
var updateReportBackoffs = []time.Duration{0, 500 * time.Millisecond, 2 * time.Second, 4 * time.Second}

func (d *Daemon) reportUpdateResult(ctx context.Context, runtimeID, updateID string, payload map[string]any) {
	d.reportUpdateResultWithRetry(ctx, runtimeID, updateID, func(ctx context.Context) error {
		return d.client.ReportUpdateResult(ctx, runtimeID, updateID, payload)
	})
}

func (d *Daemon) reportUpdateResultWithRetry(ctx context.Context, runtimeID, updateID string, fn func(context.Context) error) {
	var lastErr error
	for attempt, wait := range updateReportBackoffs {
		if wait > 0 {
			select {
			case <-ctx.Done():
				d.logger.Error("CLI update report cancelled",
					"runtime_id", runtimeID, "update_id", updateID,
					"attempt", attempt, "error", ctx.Err())
				return
			case <-time.After(wait):
			}
		}

		err := fn(ctx)
		if err == nil {
			if attempt > 0 {
				d.logger.Info("CLI update report succeeded after retry",
					"runtime_id", runtimeID, "update_id", updateID,
					"attempt", attempt+1)
			}
			return
		}
		lastErr = err

		var reqErr *requestError
		if errors.As(err, &reqErr) && reqErr.StatusCode >= 400 && reqErr.StatusCode < 500 {
			d.logger.Error("CLI update report rejected — not retrying",
				"runtime_id", runtimeID, "update_id", updateID,
				"status", reqErr.StatusCode, "error", err)
			return
		}

		d.logger.Warn("CLI update report failed — will retry",
			"runtime_id", runtimeID, "update_id", updateID,
			"attempt", attempt+1, "error", err)
	}
	d.logger.Error("CLI update report exhausted retries",
		"runtime_id", runtimeID, "update_id", updateID, "error", lastErr)
}

// tryEnterClaim records the intent to call ClaimTask. Returns true if the
// caller may proceed, false if the auto-update barrier is in effect. Every
// successful call MUST be paired with an exitClaim() on every exit path —
// either right after a failed/empty claim, or via the handleTask goroutine's
// defer once the task is handed off.
func (d *Daemon) tryEnterClaim() bool {
	d.claimMu.Lock()
	defer d.claimMu.Unlock()
	if d.pauseClaims {
		return false
	}
	d.claimsInFlight++
	return true
}

// exitClaim releases the in-flight claim recorded by tryEnterClaim.
func (d *Daemon) exitClaim() {
	d.claimMu.Lock()
	defer d.claimMu.Unlock()
	d.claimsInFlight--
}

// trySetClaimBarrier atomically pauses new ClaimTask calls if the daemon is
// fully idle (no claims in flight, no tasks running). Returns true if the
// caller now holds the barrier and must release it with releaseClaimBarrier
// on every non-restart exit path; false if the daemon is busy and the caller
// should defer to the next tick. Used by tryAutoUpdate to close the race
// where a task slips in between the cheap pre-fetch idle check and the
// actual upgrade kick-off.
func (d *Daemon) trySetClaimBarrier() bool {
	d.claimMu.Lock()
	defer d.claimMu.Unlock()
	// Refuse when the barrier is already held. Without this the function silently
	// double-acquires: two holders both believe they own it, and whichever
	// finishes first releases it out from under the other. tryBeginServerUpdate
	// makes the same check for the same reason.
	if d.pauseClaims || d.claimsInFlight > 0 || d.activeTasks.Load() > 0 {
		return false
	}
	d.pauseClaims = true
	return true
}

// releaseClaimBarrier clears the auto-update claim barrier so pollers may
// resume claiming. Called on failure paths only — a successful upgrade leaves
// the barrier set because triggerRestart is about to take the process down
// and clearing it would open a window for new claims during shutdown.
func (d *Daemon) releaseClaimBarrier() {
	d.claimMu.Lock()
	defer d.claimMu.Unlock()
	d.pauseClaims = false
}

// triggerRestart initiates a graceful daemon restart into the binary at
// restartTargetBinary(). The caller (cmd_daemon.go) checks RestartBinary() and
// launches the new process.
//
// Returns false when the target path could not be resolved, so a caller holding
// the claim barrier can release it instead of leaving the daemon paused for a
// restart that will never happen. A restart already scheduled counts as success:
// the process is going down either way.
func (d *Daemon) triggerRestart() bool {
	d.restartMu.Lock()
	defer d.restartMu.Unlock()

	if d.restartBinary != "" {
		d.logger.Debug("daemon restart already scheduled", "new_binary", d.restartBinary)
		return true
	}

	newBin, err := d.restartTargetBinary()
	if err != nil {
		d.logger.Error("could not resolve executable path for restart", "error", err)
		return false
	}

	d.logger.Info("scheduling daemon restart", "new_binary", newBin)
	d.restartBinary = newBin

	// Cancel the main context to trigger graceful shutdown.
	if d.cancelFunc != nil {
		d.cancelFunc()
	}
	return true
}

// restartTargetBinary resolves the path a restart would re-exec.
//
// For brew installs it keeps the stable symlink path (e.g.
// /opt/homebrew/bin/multica) so the restarted daemon picks up the new Cellar
// version automatically: on Linux os.Executable() reads /proc/self/exe, which
// the kernel resolves to the Cellar path, and brew cleanup deletes that path
// after an upgrade. For non-brew installs it resolves to the absolute path of
// the replaced binary.
//
// The mapping itself is cli.StableExecutablePath, shared with boot-autostart
// registration: a restart target and a recorded ExecStart must never disagree
// about which path survives brew cleanup, or the systemd handoff would
// restart a binary `brew upgrade` already deleted.
//
// Shared with trySelfReload, which must version-probe the same binary the
// restart would run — probing os.Executable() directly would read the old
// Cellar path under brew and miss the upgrade entirely.
func (d *Daemon) restartTargetBinary() (string, error) {
	newBin, err := resolveSelfExecutable()
	if err != nil {
		return "", err
	}
	// The install method and brew prefix are fixed for the process lifetime;
	// resolve them once so the per-tick reload probe doesn't fork
	// `brew --prefix` every 5 minutes. Detection order matches
	// cli.StableSelfExecutable — the path's own Cellar shape first, then
	// `brew --prefix` — so the restart target and a recorded autostart
	// ExecStart always agree.
	d.brewTargetOnce.Do(func() {
		d.brewInstall = isBrewInstall()
		if !d.brewInstall {
			return
		}
		if prefix := matchKnownBrewPrefix(newBin); prefix != "" {
			d.brewPrefix = prefix
		} else {
			d.brewPrefix = getBrewPrefix()
		}
	})
	if d.brewInstall && d.brewPrefix == "" {
		d.logger.Warn("brew install detected but prefix could not be resolved; restart may fail",
			"executable", newBin)
	}
	return cli.StableExecutablePath(newBin, d.brewPrefix, d.brewInstall), nil
}

// pollLoop runs the machine-level batch claim poller (MUL-4257): a single
// goroutine claims across ALL of the daemon's runtimes per cycle via
// ClaimTasksWSFirst (WS-first, HTTP fallback), replacing the previous
// one-HTTP-poller-per-runtime model. Wake-up signals — a WS task_available /
// catch-up nudge or a runtime-set change — all collapse to one nudge because a
// batch claim already covers every runtime. On shutdown it stops the poller,
// then drains in-flight tasks.
//
// This trades the per-runtime isolation the old model gave (MUL-1744) for a
// single request; the head-of-line risk is bounded by ClaimTasksWSFirst's short
// per-request timeout (WS) / the client's timeout (HTTP fallback), and the
// server-side batch claim is index-backed + short.
func (d *Daemon) pollLoop(ctx context.Context, taskWakeups <-chan taskWakeup) error {
	sem := newTaskSlotSemaphore(d.cfg.MaxConcurrentTasks)
	var taskWG sync.WaitGroup // tracks in-flight handleTask goroutines

	runtimeSetCh, unsub := d.runtimeSet.Subscribe()
	defer unsub()

	wakeup := make(chan struct{}, 1)
	nudge := func() {
		signalPollerWakeup(wakeup)
	}

	pollerCtx, pollerCancel := context.WithCancel(ctx)
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		d.runBatchPoller(pollerCtx, ctx, sem, wakeup, &taskWG)
	}()

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("poll loop stopping, waiting for in-flight tasks", "max_wait", "30s")
			pollerCancel()
			// Wait for the poller to fully return before waiting on taskWG so a
			// poller between ClaimTasksWSFirst and taskWG.Add(1) cannot race
			// taskWG.Wait at a zero counter.
			<-pollerDone
			waitDone := make(chan struct{})
			go func() { taskWG.Wait(); close(waitDone) }()
			select {
			case <-waitDone:
			case <-time.After(30 * time.Second):
				d.logger.Warn("timed out waiting for in-flight tasks")
			}
			return ctx.Err()
		case <-runtimeSetCh:
			// The batch poller re-derives allRuntimeIDs() each cycle; nudge it to
			// pick up a registered/removed runtime promptly.
			nudge()
		case <-taskWakeups:
			// Targeted-runtime and catch-up wakeups both trigger one batch claim
			// across the whole runtime set.
			nudge()
		}
	}
}

// runBatchPoller is the single machine-level claim+dispatch loop. Each cycle it
// acquires whatever execution slots are free (slot-before-claim, so a claimed
// task never sits server-side `dispatched` without local capacity to run it and
// race the dispatch-timeout sweeper), asks the server for up to that many tasks
// across all of the daemon's runtimes in one call, and dispatches each returned
// task — routed to its runtime by handleTask — into a slot.
//
// pollerCtx is cancelled on shutdown. parentCtx is the daemon root ctx passed to
// handleTask so an in-flight task is not killed just because the poll loop is
// stopping mid-flight.
func (d *Daemon) runBatchPoller(pollerCtx, parentCtx context.Context, sem chan int, wakeup chan struct{}, taskWG *sync.WaitGroup) {
	releaseSlots := func(slots []int) {
		for _, sl := range slots {
			sem <- sl
		}
	}

	for {
		if pollerCtx.Err() != nil {
			return
		}

		runtimeIDs := d.allRuntimeIDs()
		if len(runtimeIDs) == 0 {
			if err := sleepWithContextOrWakeup(pollerCtx, d.cfg.PollInterval, wakeup); err != nil {
				return
			}
			continue
		}

		// Acquire at least one slot (blocking briefly), then grab any other free
		// slots so a single batch claim can fill them all.
		slotWait := d.taskSlotWait
		if slotWait <= 0 {
			slotWait = taskSlotWaitTimeout
		}
		slot, acquired, woke, err := waitForTaskSlot(pollerCtx, sem, wakeup, slotWait)
		if err != nil {
			return
		}
		if !acquired {
			if woke {
				continue
			}
			if err := sleepWithContextOrWakeup(pollerCtx, capacityBackoff(d.cfg.PollInterval), wakeup); err != nil {
				return
			}
			continue
		}
		slots := append([]int{slot}, drainAvailableSlots(sem, d.cfg.MaxConcurrentTasks-1)...)

		// Auto-update barrier: refuse to claim while an update prepares to roll
		// the process (paired with the re-check in tryAutoUpdate).
		if !d.tryEnterClaim() {
			releaseSlots(slots)
			if err := sleepWithContextOrWakeup(pollerCtx, d.cfg.PollInterval, wakeup); err != nil {
				return
			}
			continue
		}

		claimResult, err := d.claimTasksWSFirst(pollerCtx, d.cfg.DaemonID, runtimeIDs, len(slots))
		if err != nil {
			d.exitClaim()
			releaseSlots(slots)
			if pollerCtx.Err() == nil {
				d.logger.Warn("batch claim failed", "error", err)
			}
			if err := sleepWithContextOrWakeup(pollerCtx, d.cfg.PollInterval, wakeup); err != nil {
				return
			}
			continue
		}
		tasks := claimResult.Tasks

		// Dispatch each claimed task into a slot. activeTasks is incremented for
		// every dispatched task BEFORE exitClaim so the auto-update barrier never
		// sees a zero-claims / zero-active window between claim and dispatch.
		dispatched := 0
		for i := range tasks {
			if i >= len(slots) || tasks[i] == nil {
				break
			}
			t := *tasks[i]
			slot := slots[i]
			taskTarget := t.IssueID
			if taskTarget == "" && t.ChatSessionID != "" {
				taskTarget = "chat:" + t.ChatSessionID
			}
			d.logger.Info("task received", "task", t.ID, "target", taskTarget)
			taskWG.Add(1)
			d.activeTasks.Add(1)
			if cache, ok := d.repoCache.(interface{ CancelMaintenance() }); ok {
				// A task can reuse an existing worktree and never enter the
				// checkout path that normally preempts repository maintenance.
				// Cancel all low-priority maintenance before the agent starts so
				// direct Git operations cannot overlap it.
				cache.CancelMaintenance()
			}
			go func(t Task, slot int) {
				defer taskWG.Done()
				defer d.activeTasks.Add(-1)
				defer func() {
					// Release local capacity before waking the poller. The task's
					// terminal callback and local cleanup have both finished at this
					// point, so a successor that was previously blocked by agent
					// capacity or per-(issue, agent) serialization can be claimed
					// immediately instead of waiting for PollInterval.
					sem <- slot
					signalPollerWakeup(wakeup)
				}()
				d.handleTask(parentCtx, t, slot)
			}(t, slot)
			dispatched++
		}
		d.exitClaim()
		if dispatched < len(slots) {
			releaseSlots(slots[dispatched:])
		}

		// If we filled every slot, more work may be queued — loop immediately.
		// Otherwise wait for the next wakeup / poll interval. A connection that
		// negotiated WS RPC receives task-available pushes, so this poll is only
		// a missed-event safety net and can run less often. Claim errors retain
		// the configured fallback cadence above.
		if dispatched > 0 && dispatched == len(slots) {
			continue
		}
		if err := sleepWithContextOrWakeup(pollerCtx, d.taskClaimPollInterval(claimResult), wakeup); err != nil {
			return
		}
	}
}

// taskClaimPollInterval returns the next missed-event safety poll. The longer
// cadence is only safe when this exact claim response came from a server that
// opted into scheduling hints; a missing hint covers old servers and uncertain
// WS claims, both of which retain the normal fallback cadence. Downward-only
// jitter keeps the default below the server's 3-minute empty-claim cache TTL
// while preventing an idle fleet from polling in lockstep.
func (d *Daemon) taskClaimPollInterval(result claimTasksResult) time.Duration {
	if !d.wsRPC.supportsRPCV1() || !result.ClaimedOverWS || !result.ClaimPollHintSupported {
		if d.cfg.PollInterval > 0 {
			return d.cfg.PollInterval
		}
		return DefaultPollInterval
	}
	upperBound := d.cfg.WSClaimPollInterval
	if upperBound <= 0 {
		upperBound = DefaultWSClaimPollInterval
	}
	interval := downwardJitterDuration(upperBound)
	if result.NextDeferredTaskAfterMillis > 0 {
		untilDeferred := time.Duration(result.NextDeferredTaskAfterMillis) * time.Millisecond
		if untilDeferred < interval {
			interval = untilDeferred
		}
	}
	return interval
}

func downwardJitterDuration(interval time.Duration) time.Duration {
	minReduction := interval / 12
	maxReduction := interval / 6
	if minReduction <= 0 || maxReduction <= minReduction {
		return interval
	}
	reduction := minReduction + time.Duration(rand.Int63n(int64(maxReduction-minReduction)+1))
	return interval - reduction
}

func signalPollerWakeup(wakeup chan<- struct{}) {
	select {
	case wakeup <- struct{}{}:
	default:
	}
}

// drainAvailableSlots non-blockingly pulls up to max additional slots from the
// semaphore, returning immediately when none are free.
func drainAvailableSlots(sem chan int, max int) []int {
	if max <= 0 {
		return nil
	}
	var slots []int
	for len(slots) < max {
		select {
		case s := <-sem:
			slots = append(slots, s)
		default:
			return slots
		}
	}
	return slots
}

func capacityBackoff(pollInterval time.Duration) time.Duration {
	if pollInterval <= 0 || pollInterval > taskSlotCapacityBackoff {
		return taskSlotCapacityBackoff
	}
	return pollInterval
}

func waitForTaskSlot(ctx context.Context, sem chan int, wakeup <-chan struct{}, wait time.Duration) (slot int, acquired, woke bool, err error) {
	select {
	case slot = <-sem:
		return slot, true, false, nil
	case <-ctx.Done():
		return 0, false, false, ctx.Err()
	default:
	}

	if wait <= 0 {
		return 0, false, false, nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case slot = <-sem:
		return slot, true, false, nil
	case <-wakeup:
		return 0, false, true, nil
	case <-ctx.Done():
		return 0, false, false, ctx.Err()
	case <-timer.C:
		return 0, false, false, nil
	}
}

// newTaskSlotSemaphore returns a buffered channel pre-populated with stable
// slot indices [0, n). Receive to acquire a slot, send the same slot back to
// release. Used by pollLoop to expose MULTICA_TASK_SLOT to spawned tasks.
func newTaskSlotSemaphore(maxConcurrentTasks int) chan int {
	sem := make(chan int, maxConcurrentTasks)
	for i := 0; i < maxConcurrentTasks; i++ {
		sem <- i
	}
	return sem
}

// shouldInterruptAgent decides whether the running agent should be cancelled
// based on the latest GetTaskStatus call. Pure function so the decision is
// trivially testable; the polling goroutine in watchTaskCancellation is just
// I/O around it.
//
// Two conditions trigger cancellation:
//
//  1. status is a terminal state — "completed", "failed", or "cancelled"
//     (isAgentTaskTerminal). The server has already finalized the task: user
//     cancel, issue reassignment, the runtime offline sweeper flipping
//     running → failed during a disconnect, or a duplicate execution that
//     already completed it. Letting the local agent run on is pure waste —
//     CompleteAgentTask only accepts status == "running", so its eventual
//     CompleteTask/FailTask callback is guaranteed to fail and just adds log
//     noise. Reusing isAgentTaskTerminal keeps this set in lockstep with the
//     GC's notion of a terminal task.
//  2. err is a 404 with "task not found" — the task row was deleted while
//     the agent was running. Without this we'd let the local agent keep
//     emitting tool calls against a dead task for its full timeout window.
//
// All other errors (transient network, 5xx, ...) intentionally do NOT
// trigger cancellation — the next tick will retry and we don't want a
// flaky link to kill an in-flight agent.
func shouldInterruptAgent(status string, err error) bool {
	if err != nil {
		return isTaskNotFoundError(err)
	}
	return isAgentTaskTerminal(status)
}

// watchTaskCancellation polls the server for the task's status on the given
// interval and returns a channel that is closed when the running agent
// should be interrupted. The polling goroutine stops when ctx is cancelled,
// so callers should pass the runCtx that was set up around the agent run.
func (d *Daemon) watchTaskCancellation(ctx context.Context, taskID string, pollInterval time.Duration, taskLog *slog.Logger) <-chan struct{} {
	cancelled := make(chan struct{})
	// Subscribe to the reconcile broadcaster before launching the inner
	// goroutine. A WS reconnect that fires between the goroutine starting
	// and its first notify() call would otherwise be dropped; the ticker
	// still bounds the worst case, but the whole point of the broadcast is
	// to avoid waiting on that ticker.
	var reconcileCh <-chan struct{}
	if d.reconcile != nil {
		reconcileCh = d.reconcile.notify()
	}
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		check := func() bool {
			status, err := d.client.GetTaskStatus(ctx, taskID)
			if !shouldInterruptAgent(status, err) {
				return false
			}
			if err != nil {
				taskLog.Info("task gone server-side, interrupting agent", "error", err)
			} else {
				taskLog.Info("task reached terminal state server-side, interrupting agent", "status", status)
			}
			close(cancelled)
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-reconcileCh:
				// Refresh the subscription before issuing the request so a
				// second broadcast that overlaps GetTaskStatus is not lost.
				if d.reconcile != nil {
					reconcileCh = d.reconcile.notify()
				}
				if check() {
					return
				}
			case <-ticker.C:
				if check() {
					return
				}
			}
		}
	}()
	return cancelled
}

func (d *Daemon) handleTask(ctx context.Context, task Task, slot int) {
	d.mu.Lock()
	rt, tracked := d.runtimeIndex[task.RuntimeID]
	d.mu.Unlock()
	// The runtime can go offline between the batch claim leaving with its ID and
	// the claimed task arriving here — a below-minimum demotion or a drift
	// convergence to zero both drop rows while a claim is in flight. Reporting it
	// as runtime_offline is what the server already retries on; without this the
	// zero-value Runtime carries an empty provider and the task dies several
	// hundred lines later as `no agent configured for provider ""`, which reads
	// like a misconfigured host and is not retried.
	if !tracked {
		d.logger.Warn("claimed task targets a runtime this daemon no longer hosts; failing it back for retry",
			"task", task.ID, "runtime_id", task.RuntimeID)
		if err := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:          terminalTaskReportFail,
			taskID:        task.ID,
			errorMessage:  "runtime went offline before the run started",
			failureReason: taskfailure.ReasonRuntimeOffline.String(),
		}); err != nil {
			d.logger.Error("fail task callback failed", "task", task.ID, "error", err)
		}
		return
	}
	provider := rt.Provider

	// Task-scoped logger. The task id goes in whole: it is the key every
	// other surface prints (task JSON, env-root ownership manifest, server
	// logs), so a full id is what lets a log line be joined to them. An
	// 8-char prefix cannot — task ids are UUIDv7, whose leading 32 bits are
	// a millisecond timestamp that only advances every ~65s, so concurrent
	// tasks routinely share one (#7326).
	taskLog := d.logger.With("task", task.ID)
	phaseRecorder := newTaskPhaseRecorder(taskLog.With("task_id", task.ID, "runtime_id", task.RuntimeID), time.Now)
	ctx = withTaskPhaseRecorder(ctx, phaseRecorder)
	phaseRecorder.Mark(taskPhaseClaimed)
	defer phaseRecorder.Mark(taskPhaseFinished)
	agentName := "agent"
	if task.Agent != nil {
		agentName = task.Agent.Name
	}
	if task.ChatSessionID != "" {
		taskLog.Info("picked chat task", "chat_session", task.ChatSessionID, "agent", agentName, "provider", provider)
	} else {
		taskLog.Info("picked task", "issue", task.IssueID, "agent", agentName, "provider", provider)
	}
	taskLog.Debug("task context",
		"workspace_id", task.WorkspaceID,
		"runtime_id", task.RuntimeID,
		"agent_id", task.AgentID,
		"repos", len(task.Repos),
		"project_id", task.ProjectID,
		"autopilot_run_id", task.AutopilotRunID,
		"trigger_comment_id", task.TriggerCommentID,
		"resume_session", task.PriorSessionID != "",
		"reuse_workdir", task.PriorWorkDir != "",
	)

	// If the task targets a project_resource of type local_directory that
	// is pinned to this daemon, acquire the path mutex before runner.run
	// so the server-side state machine is dispatched →
	// waiting_local_directory → running rather than backwards-transitioning
	// from running into the wait state. The release is deferred so a panic
	// or early return always frees the lock for the next waiter.
	//
	// StartTask itself now lives in runTask (see issue #3999 race A) and
	// fires only after execenv.Prepare/Reuse has put env.WorkDir on disk,
	// so consumers that read status==running can resolve the workdir path
	// without racing the daemon's os.MkdirAll.
	localRelease, abort := d.acquireLocalDirectoryLockIfNeeded(ctx, task, taskLog)
	if abort {
		return
	}
	if localRelease != nil {
		defer localRelease()
	}

	// Hold a process-wide active-root guard for the rest of this task so
	// the GC loop never sees a window where the env root has neither the
	// in-process guard nor .gc_meta.json (issue #3999 race B). runTask
	// installs its own ref-counted mark/unmark internally; without this
	// outer guard the inner unmark fires when runTask returns, leaving
	// the directory protected only by the 72h orphan TTL through
	// reportTaskResult and execenv.WriteGCMeta below. markActiveEnvRoot
	// is reference-counted, so the duplicate marks runTask installs are
	// correctly nested within these.
	resolvedEnvRoot, resolveRootErr := execenv.ResolveRootDir(taskRootDirParams(d.cfg.WorkspacesRoot, task))
	if resolveRootErr != nil {
		taskLog.Error("resolve stable task env root", "error", resolveRootErr)
	}
	if resolvedEnvRoot != "" {
		d.markActiveEnvRoot(resolvedEnvRoot)
		defer d.unmarkActiveEnvRoot(resolvedEnvRoot)
	}
	if task.PriorWorkDir != "" {
		if priorRoot := filepath.Dir(task.PriorWorkDir); priorRoot != "" && priorRoot != resolvedEnvRoot {
			d.markActiveEnvRoot(priorRoot)
			defer d.unmarkActiveEnvRoot(priorRoot)
		}
	}

	// Create a cancellable context so we can interrupt the running agent
	// when the server signals the task should stop — either the task reached
	// a terminal state (completed/failed/cancelled) or the task row is
	// deleted (404).
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Poll interval is d.cancelPollInterval (5s in production, reduced in tests
	// via direct field override). Guard against zero so a misconfigured daemon
	// doesn't panic time.NewTicker.
	pollInterval := d.cancelPollInterval
	if pollInterval == 0 {
		pollInterval = 5 * time.Second
	}
	cancelledByPoll := d.watchTaskCancellation(runCtx, task.ID, pollInterval, taskLog)
	go func() {
		select {
		case <-cancelledByPoll:
			runCancel()
		case <-runCtx.Done():
		}
	}()

	result, err := d.runner.run(runCtx, task, provider, slot, taskLog)
	if errors.Is(err, errStartClaimRejected) {
		// The row belongs to another claim (or is terminal). A task-id-only
		// failure callback from this stale delivery could kill its new owner.
		taskLog.Info("discarding rejected start claim", "error", err)
		return
	}

	// Report usage before any early return — the agent accumulates tokens
	// whether the task completes, errors, or is cancelled mid-run by the poll
	// goroutine. Both claude.go and codex.go populate result.Usage even when
	// runCtx is cancelled, so dropping this on the cancelled path silently
	// under-reports billing.
	if len(result.Usage) > 0 {
		if usageErr := d.client.ReportTaskUsage(ctx, task.ID, result.Usage); usageErr != nil {
			taskLog.Warn("report task usage failed", "error", usageErr)
		}
	}

	// Check if we were cancelled by the polling goroutine.
	select {
	case <-cancelledByPoll:
		taskLog.Info("task cancelled during execution, discarding result",
			"branch_name", result.BranchName, "error", err)
		// runner.run has returned, so the transcript flush is complete —
		// tell the server it can settle its deferred chat finalization
		// (#5219). The sweeper grace period covers a lost ack's chat settle,
		// but NOT the payload: the branch rides along because the worktree was
		// already finalized before this check, and when Finalize instead
		// ABORTED, the preserved-worktree error is the only pointer left to
		// the agent's work — everything else on this path is discarded. Other
		// run errors stay discarded: on a cancelled run they are expected
		// noise (context canceled, killed process), and persisting them would
		// stamp a bogus reason on every ordinary mid-run cancel.
		ack := TaskCancelAck{BranchName: result.BranchName, DurableWorkDir: result.DurableWorkDir}
		var preserved *worktreePreservedError
		if errors.As(err, &preserved) {
			ack.ErrorMessage = preserved.Error()
			ack.FailureReason = "local_directory_error"
		}
		if ackErr := d.client.AckTaskCancelled(ctx, task.ID, ack); ackErr != nil {
			taskLog.Warn("cancel ack failed; server sweeper will finalize", "error", ackErr)
		}
		return
	default:
	}

	if err != nil {
		taskLog.Error("task failed", "error", err)
		// runTask may have reached worktree finalization before returning the
		// error. Preserve any delivery metadata that defer attached to the named
		// result, especially the actual/preserved workdir and delivered branch.
		// MUL-2946: route the bare error string through the canonical
		// classifier so the failure_reason column reflects the actual
		// shape of the failure (provider 5xx, network, process crash,
		// …) rather than the coarse legacy "agent_error" bucket.
		if failErr := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:           terminalTaskReportFail,
			taskID:         task.ID,
			errorMessage:   err.Error(),
			branchName:     result.BranchName,
			workDir:        result.WorkDir,
			durableWorkDir: result.DurableWorkDir,
			failureReason:  taskRunFailureReason(err),
		}); failErr != nil {
			taskLog.Error("fail task callback failed", "error", failErr)
		}
		return
	}

	_ = d.client.ReportProgress(ctx, task.ID, "Finishing task", 2, 2)

	// Final pre-completion check: if the server already moved the task to a
	// terminal state (completed/failed/cancelled) or deleted the row
	// outright, skip reporting — the complete/fail callbacks would fail
	// anyway. Reuse shouldInterruptAgent so this guard honors the same
	// signals as the in-flight watcher.
	if status, err := d.client.GetTaskStatus(ctx, task.ID); shouldInterruptAgent(status, err) {
		taskLog.Info("task cancelled during execution, discarding result",
			"status", status, "error", err, "branch_name", result.BranchName)
		// Same contract as the poll-cancelled path above: the transcript is
		// flushed, so let the server settle its deferred chat finalization, and
		// carry the finalized branch so cancelled work stays discoverable. No
		// error to carry here — this branch is only reached with err == nil.
		// This fires for ANY observed terminal status; the server applies the
		// payload only to a cancelled row (status CAS), because for
		// completed/failed rows the complete/fail callback is the
		// authoritative channel and a stale run's late ack must not touch
		// them.
		if ackErr := d.client.AckTaskCancelled(ctx, task.ID, TaskCancelAck{BranchName: result.BranchName, DurableWorkDir: result.DurableWorkDir}); ackErr != nil {
			taskLog.Warn("cancel ack failed; server sweeper will finalize", "error", ackErr)
		}
		return
	}

	d.reportTaskResult(ctx, task.ID, result, taskLog)

	// Write GC metadata after the task finishes so the periodic GC loop
	// can look up the parent record (issue / chat session / autopilot run /
	// task itself for quick-create) later. Written last so that a mid-task
	// crash leaves the directory as an orphan (cleaned up by GCOrphanTTL).
	if result.EnvRoot != "" {
		if meta, ok := gcMetaForTask(task); ok {
			// A local_directory project_resource matched this daemon
			// means the agent ran in the user's own tree. Stamp the
			// meta so the GC loop never tries to RemoveAll envRoot's
			// sibling workdir (which is the user's path) or the envRoot
			// itself (we want output/ and logs/ to linger for forensic
			// access).
			//
			// Worktree mode is excluded: its workdir is a disposable
			// worktree INSIDE envRoot, already removed by Finalize, and
			// the deliverable lives on as a branch in the user's repo.
			// Stamping it would hand every worktree task a permanently
			// exempt env root, so the directory would accumulate one env
			// root per task forever — the exact cost the exemption was
			// meant to trade away for a user's own files.
			if assignment, _ := localDirectoryAssignmentForTask(task, d.cfg.DaemonID); assignment != nil && !assignment.UsesWorktree() {
				meta.LocalDirectory = true
			}
			if err := execenv.WriteGCMeta(result.EnvRoot, meta, taskLog); err != nil {
				taskLog.Warn("write gc meta failed (non-fatal)", "error", err)
			}
		}
	}
}

// worktreePreservedError marks a task error that must survive the cancel path:
// its message names the preserved worktree holding the agent's uncommitted
// work. Every other error on a cancelled run is expected noise (context
// canceled, killed process) and stays discarded; this one is the only pointer
// to real work and rides the cancel ack to the server.
type worktreePreservedError struct{ err error }

func (e *worktreePreservedError) Error() string { return e.err.Error() }
func (e *worktreePreservedError) Unwrap() error { return e.err }

// environmentSetupError marks a failure to build or re-open the task's
// execution environment: everything execenv.Prepare / execenv.Reuse does on
// this host before the agent process exists — the workspace directory and its
// overlay homes, plus the per-provider local config written or validated
// inside it. Its causes are local, and not only filesystem ones: a full
// volume, a read-only or permission-denied workspaces root, a directory
// another process still holds open, an I/O error on the disk underneath, or a
// local runtime config the daemon refuses to boot past.
//
// It carries the wrapped error's message unchanged and exists purely so
// taskRunFailureReason can recognise the phase structurally. Without it the
// OS error text falls through to taskfailure.Classify, a classifier written
// to read agent and provider output, and lands in agent_error.* — today the
// catchall, historically provider_server_error, which pointed one bug report
// at an LLM vendor for hours (#7913). The task never reached an agent, so no
// reason in that namespace can be right.
type environmentSetupError struct{ err error }

func (e *environmentSetupError) Error() string { return e.err.Error() }
func (e *environmentSetupError) Unwrap() error { return e.err }

// asEnvironmentSetupFailure tags err as an execution-environment setup
// failure. The message is untouched, so the wrapper is invisible to everything
// except taskRunFailureReason.
func asEnvironmentSetupFailure(err error) error { return &environmentSetupError{err: err} }

func taskRunFailureReason(err error) string {
	if errors.Is(err, errInvalidTaskIdentity) {
		return taskfailure.ReasonInvalidTaskIdentity.String()
	}
	if errors.Is(err, errTaskPrepareTimeout) {
		return taskfailure.ReasonTimeout.String()
	}
	// Checked after the prepare deadline: when the whole prepare budget ran
	// out, runTask has already collapsed the error into errTaskPrepareTimeout
	// and that classification is the more accurate one. This branch is for the
	// per-skill download deadline firing inside a prepare budget that still had
	// room (MUL-5370).
	if errors.Is(err, errSkillBundleUnavailable) {
		return taskfailure.ReasonSkillBundleUnavailable.String()
	}
	// Structural, not textual: the message ends in "context deadline exceeded",
	// which Classify routes to agent_error.provider_network — "the connection to
	// the model provider dropped, check your network" for a stall that is purely
	// local, plus an auto-retry of a failure that is deterministic on the host.
	// The sentinel survives the preparation helper boundary via
	// preparationErrorKind (#7112).
	// Everything else that failed while building or re-opening the execution
	// environment. Last of the structural branches: the four sentinels above
	// each name a cause the daemon already recognises on its own (task
	// identity, the prepare budget, skill downloads, the local runtime CLI)
	// and stay the better label. What is left is the rest of preparation on
	// this host — filesystem faults, and the per-provider local config
	// Prepare writes or validates — whose text Classify can only read as
	// agent output (#7913).
	var envSetupErr *environmentSetupError
	if errors.As(err, &envSetupErr) {
		return taskfailure.ReasonEnvironmentPrepareFailed.String()
	}
	return taskfailure.Classify(err.Error()).String()
}

// acquireLocalDirectoryLockIfNeeded inspects the task's project resources for
// a local_directory pinned to this daemon, validates the path, and takes the
// path mutex. Returns a release callback (nil when no local_directory
// resource applies) and abort=true when the caller must bail without
// starting the task (the helper has already reported the failure to the
// server).
//
// The helper covers four distinct failure modes:
//
//  1. The project_resource JSON is structurally broken — fail the task fast.
//  2. The path fails validation (missing, not a directory, no R/W, system
//     blacklist) — fail the task fast with a user-facing reason.
//  3. The mutex is held by another task — call MarkTaskWaitingLocalDirectory
//     so the row flips to waiting_local_directory while we block on the
//     lock, then return the release callback once we win.
//  4. The blocking wait is cancelled (daemon shutdown, server-side cancel)
//     — fail the task with the ctx error.
func (d *Daemon) acquireLocalDirectoryLockIfNeeded(ctx context.Context, task Task, taskLog *slog.Logger) (release func(), abort bool) {
	if len(task.ProjectResources) == 0 || d.cfg.DaemonID == "" {
		return nil, false
	}
	assignment, err := localDirectoryAssignmentForTask(task, d.cfg.DaemonID)
	if err != nil {
		taskLog.Error("local_directory: resolve resource failed", "error", err)
		if failErr := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:          terminalTaskReportFail,
			taskID:        task.ID,
			errorMessage:  err.Error(),
			failureReason: "local_directory_error",
		}); failErr != nil {
			taskLog.Error("fail task after local_directory resolve error", "error", failErr)
		}
		return nil, true
	}
	if assignment == nil {
		return nil, false
	}
	taskLog = taskLog.With("local_directory", assignment.AbsPath)
	// Check the mode before the path: a mode this daemon can't honour is a
	// version-skew problem the user fixes by upgrading, and reporting a path
	// complaint first would send them looking in the wrong place.
	if err := assignment.ValidateExecutionMode(); err != nil {
		taskLog.Error("local_directory: unsupported execution mode", "error", err)
		if failErr := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:          terminalTaskReportFail,
			taskID:        task.ID,
			errorMessage:  err.Error(),
			failureReason: "local_directory_error",
		}); failErr != nil {
			taskLog.Error("fail task after local_directory mode check", "error", failErr)
		}
		return nil, true
	}
	if err := validateLocalPath(assignment.AbsPath); err != nil {
		taskLog.Error("local_directory: path validation failed", "error", err)
		if failErr := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:          terminalTaskReportFail,
			taskID:        task.ID,
			errorMessage:  err.Error(),
			failureReason: "local_directory_error",
		}); failErr != nil {
			taskLog.Error("fail task after local_directory validation error", "error", failErr)
		}
		return nil, true
	}

	// Worktree mode is the whole point of not serialising: each task gets its
	// own checkout of the repo inside its env root, so there is no shared
	// mutable state on the user's path to protect. Skipping the mutex here is
	// what lets sibling tasks on one directory run concurrently. Path
	// validation above still applies — git needs to write worktree
	// registrations into the user's repo.
	if assignment.UsesWorktree() {
		taskLog.Info("local_directory: worktree mode, skipping path mutex")
		return nil, false
	}

	// A conversation is not a second writer. Everything above still applied —
	// the mode was checked, the path was validated, and the assignment stands,
	// so this task keeps the user's directory as its working directory. Only
	// the queueing is skipped, which is what stops a chat turn from sitting
	// behind a 20-minute build with nothing to contribute to it (issue #7344).
	// See localDirectoryLockExempt for why the mutex does not owe this task a
	// slot.
	if localDirectoryLockExempt(task) {
		taskLog.Info("local_directory: chat task, skipping path mutex")
		return nil, false
	}

	// While the lock is contended the daemon would otherwise sit blocked on
	// the path mutex with no signal back from the server — the main
	// per-task watcher only starts after the lock is acquired. If the user
	// cancels the issue or it gets reassigned during the wait, we need to
	// notice promptly so the daemon slot isn't pinned by a phantom waiter.
	// We spin up the cancellation watcher lazily inside onWait so the
	// no-contention fast path still costs nothing.
	waitCtx, waitCancel := context.WithCancel(ctx)
	defer waitCancel()
	pollInterval := d.cancelPollInterval
	if pollInterval == 0 {
		pollInterval = 5 * time.Second
	}
	var (
		watcherOnce      sync.Once
		prepareLeaseOnce sync.Once
		cancelledByPoll  <-chan struct{}
		stopPrepareLease func()
		waitCounted      bool
	)
	defer func() {
		if waitCounted {
			d.resourceWaitTasks.Add(-1)
		}
	}()
	defer func() {
		if stopPrepareLease != nil {
			stopPrepareLease()
		}
	}()

	onWait := func(holder string) {
		// LocalPathLocker invokes onWait synchronously and at most once for an
		// Acquire call. Count the actual mutex wait even if the best-effort
		// server status update below fails.
		d.resourceWaitTasks.Add(1)
		waitCounted = true
		// Rendered to the user, so it names the directory rather than its path
		// (see localDirectoryAssignment.DisplayName). The absolute path stays in
		// the daemon's own logs, which is where an operator debugging a wedged
		// lock looks for it.
		reason := assignment.DisplayName()
		if holder != "" {
			// Known rough edge: this clause is English and the client renders it
			// inside a localized "Waiting for {reason}" label, so a zh/ja/ko user
			// sees mixed script. Fixing it properly means sending the directory
			// and the holder as separate fields and localizing the join on the
			// client — worth doing if this hint grows, not for one parenthetical.
			reason = fmt.Sprintf("%s (held by task %s)", reason, shortID(holder))
		}
		taskLog.Info("local_directory: waiting on path mutex", "holder", holder)
		if waitErr := d.client.MarkTaskWaitingLocalDirectory(ctx, task.ID, reason); waitErr != nil {
			// Non-fatal: even if the server-side flag fails to update,
			// we still want to block on the lock and proceed when free.
			// The UI just won't see the explicit "waiting" badge.
			taskLog.Warn("local_directory: mark waiting status failed", "error", waitErr)
		}
		prepareLeaseOnce.Do(func() {
			stopPrepareLease = d.startTaskPrepareLeaseExtender(waitCtx, task, taskLog)
		})
		// Start polling once we actually park. shouldInterruptAgent inside
		// watchTaskCancellation already handles both server-side terminal
		// states (completed/failed/cancelled) and the row-deleted
		// reassignment case (404), which is the full set of "this task
		// shouldn't run anymore" signals we need to react to during the wait.
		watcherOnce.Do(func() {
			cancelledByPoll = d.watchTaskCancellation(waitCtx, task.ID, pollInterval, taskLog)
			go func() {
				select {
				case <-cancelledByPoll:
					waitCancel()
				case <-waitCtx.Done():
				}
			}()
		})
	}
	release, err = d.localPathLocks.Acquire(waitCtx, assignment.RealPath, task.ID, onWait)
	if err != nil {
		// If the wait was cut short because the server finalized the task
		// (terminal state) or deleted the row, the row is already in a
		// terminal state — return silently the same way the run-phase poller
		// does at lines ~2104. Issuing FailTask here would be a no-op at best
		// and a confusing redundant log line at worst.
		if cancelledByPoll != nil {
			select {
			case <-cancelledByPoll:
				taskLog.Info("local_directory: wait aborted by server-side terminal state")
				return nil, true
			default:
			}
		}
		taskLog.Error("local_directory: lock acquire failed", "error", err)
		failureReason := "local_directory_error"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failureReason = "cancelled"
		}
		if failErr := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:          terminalTaskReportFail,
			taskID:        task.ID,
			errorMessage:  fmt.Sprintf("local_directory wait cancelled: %s", err.Error()),
			failureReason: failureReason,
		}); failErr != nil {
			taskLog.Error("fail task after local_directory lock cancel", "error", failErr)
		}
		return nil, true
	}
	taskLog.Info("local_directory: lock acquired")
	return release, false
}

// reportTaskResult writes the final task disposition back to the server.
//
// Fail closed: only an explicit "completed" status is reported as success.
// Anything else — "blocked", "cancelled", or any future status we forget to
// enumerate — must go through FailTask, so a run that never produced a real
// result can never be displayed as "Completed" in the UI (e.g. provider 429 /
// out-of-credit / runtime crash). Forward SessionID/WorkDir on every path:
// the agent may have built a real session before getting stuck, and we want
// the next chat turn to resume there rather than start over and "forget"
// the conversation.
func (d *Daemon) reportTaskResult(ctx context.Context, taskID string, result TaskResult, taskLog *slog.Logger) {
	switch result.Status {
	case "completed":
		taskLog.Info("task completed", "status", result.Status)
		err := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:                  terminalTaskReportComplete,
			taskID:                taskID,
			output:                result.Comment,
			branchName:            result.BranchName,
			sessionID:             result.SessionID,
			workDir:               result.WorkDir,
			durableWorkDir:        result.DurableWorkDir,
			sessionRolloutMissing: result.SessionRolloutMissing,
			retiredSessionID:      result.RetiredSessionID,
		})
		if err == nil {
			return
		}
		// The original completion is already durable. Never overwrite it with a
		// synthetic failure: a temporary auth/config skew can make a 4xx recover
		// after restart just as a transport outage can make a 5xx recover, and the
		// user's successful output must remain authoritative in both cases.
		taskLog.Error("complete task callback not acknowledged; durable report remains queued", "error", err)
	default:
		failureReason := result.FailureReason
		if failureReason == "" {
			if result.Status == "cancelled" {
				// "cancelled" is a deliberate non-failure terminal
				// state masquerading as a failure_reason — preserved
				// outside the canonical taxonomy so the UI can render
				// it differently from a real failure.
				failureReason = "cancelled"
			} else {
				// MUL-2946: classify the agent's comment text so the
				// failure_reason lands in the refined taxonomy
				// (provider_auth_or_access, context_overflow,
				// process_failure, …) instead of the legacy coarse
				// "agent_error" bucket. Empty comment lands in
				// ReasonAgentUnknown.
				failureReason = taskfailure.Classify(result.Comment).String()
			}
		}
		taskLog.Info("task did not complete, reporting failure", "status", result.Status, "failure_reason", failureReason)
		if err := d.reportTerminalTask(ctx, terminalTaskReport{
			kind:           terminalTaskReportFail,
			taskID:         taskID,
			errorMessage:   result.Comment,
			sessionID:      result.SessionID,
			workDir:        result.WorkDir,
			durableWorkDir: result.DurableWorkDir,
			// Worktree mode commits the agent's leftovers before tearing the
			// worktree down, so a failed run routinely still has a branch. This
			// is the case where the user most needs it: the task went wrong and
			// they want to see how far it got.
			branchName:            result.BranchName,
			failureReason:         failureReason,
			sessionRolloutMissing: result.SessionRolloutMissing,
			retiredSessionID:      result.RetiredSessionID,
		}); err != nil {
			taskLog.Error("report failed task failed", "error", err)
		}
	}
}

// reportTerminalTask is the only path that sends complete/fail callbacks. It
// attempts to persist the exact report before the first network request and
// removes a persisted copy only after a successful response. A crash after the
// server commit but before local acknowledgement merely replays the same
// idempotent terminal request. If persistence itself fails, the direct request
// still runs so a healthy server is not held hostage by the local disk.
//
// It deliberately preserves context values while discarding cancellation and
// parent deadlines: daemon shutdown cancels the root context before pollLoop's
// 30-second drain, but terminal callbacks must still use that remaining window.
// The explicit timeout keeps this detached work bounded during normal runs.
func (d *Daemon) reportTerminalTask(parentCtx context.Context, report terminalTaskReport) error {
	if _, err := persistedTerminalReport(report, time.Now()); err != nil {
		return err
	}
	release, ok := d.beginTerminalReportDelivery(report.taskID)
	if !ok {
		return fmt.Errorf("terminal task report for %s is already being delivered", report.taskID)
	}
	defer release()

	persisted := false
	if d.terminalReports != nil {
		if err := d.terminalReports.enqueue(report); err != nil {
			// Durability is an availability improvement, not a prerequisite for
			// the online callback. A read-only/full disk must not turn a request
			// that the server could accept right now into a stuck task.
			d.logger.Error("persist terminal task report; continuing with direct delivery",
				"task", report.taskID,
				"kind", report.kind,
				"error", err,
			)
		} else {
			persisted = true
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), terminalTaskReportTimeout)
	defer cancel()
	err := d.sendTerminalTaskReport(ctx, report, defaultTerminalRetrySchedule)
	if err != nil {
		quarantined := false
		if persisted {
			item := pendingTerminalTaskReport{fileName: terminalReportFileName(report.taskID), report: report}
			quarantined = d.handleTerminalReportDeliveryError(ctx, item, err)
		}
		if persisted && !quarantined {
			d.signalTerminalReportReplay()
		}
		return err
	}
	if !persisted {
		return nil
	}
	item := pendingTerminalTaskReport{fileName: terminalReportFileName(report.taskID), report: report}
	if err := d.terminalReports.acknowledge(item); err != nil {
		d.signalTerminalReportReplay()
		return fmt.Errorf("acknowledge terminal task report: %w", err)
	}
	return nil
}

func (d *Daemon) beginTerminalReportDelivery(taskID string) (func(), bool) {
	d.terminalReportMu.Lock()
	if d.terminalReportFlight == nil {
		d.terminalReportFlight = make(map[string]struct{})
	}
	if _, exists := d.terminalReportFlight[taskID]; exists {
		d.terminalReportMu.Unlock()
		return nil, false
	}
	d.terminalReportFlight[taskID] = struct{}{}
	d.terminalReportMu.Unlock()
	return func() {
		d.terminalReportMu.Lock()
		delete(d.terminalReportFlight, taskID)
		d.terminalReportMu.Unlock()
	}, true
}

func (d *Daemon) terminalReportClock() time.Time {
	if d.terminalReportNow != nil {
		return d.terminalReportNow()
	}
	return time.Now()
}

func (d *Daemon) sendTerminalTaskReport(ctx context.Context, report terminalTaskReport, schedule []time.Duration) error {
	if d.terminalReportSend != nil {
		return d.terminalReportSend(ctx, report, schedule)
	}
	switch report.kind {
	case terminalTaskReportComplete:
		return d.client.completeTaskWithRetrySchedule(ctx, report.taskID, report.output, report.branchName, report.sessionID, report.workDir, report.sessionRolloutMissing, report.retiredSessionID, report.durableWorkDir, schedule)
	case terminalTaskReportFail:
		return d.client.failTaskWithRetrySchedule(ctx, report.taskID, report.errorMessage, report.sessionID, report.workDir, report.branchName, report.failureReason, report.sessionRolloutMissing, report.retiredSessionID, report.durableWorkDir, schedule)
	default:
		return fmt.Errorf("unsupported terminal task report kind %d", report.kind)
	}
}

// gcMetaForTask classifies a finished task and produces a GCMeta of the right
// kind. The discriminator order matters: a task carrying both an issue_id
// and a chat_session_id (theoretical, not produced today) should be treated
// as a chat task because the chat session is the longer-lived parent record.
//
// Returns ok=false when the task has no recognizable parent (e.g. an
// internal task with no IDs at all). The caller skips writing a meta file
// in that case so the directory falls back to mtime-based orphan cleanup.
func gcMetaForTask(task Task) (execenv.GCMeta, bool) {
	meta := execenv.GCMeta{WorkspaceID: task.WorkspaceID, TaskID: task.ID}
	switch {
	case task.ChatSessionID != "":
		meta.Kind = execenv.GCKindChat
		meta.ChatSessionID = task.ChatSessionID
	case task.AutopilotRunID != "":
		meta.Kind = execenv.GCKindAutopilotRun
		meta.AutopilotRunID = task.AutopilotRunID
	case task.IssueID != "":
		meta.Kind = execenv.GCKindIssue
		meta.IssueID = task.IssueID
	case task.QuickCreatePrompt != "":
		// Quick-create tasks reach WriteGCMeta before the server runs
		// LinkTaskToIssue, so IssueID is always empty here. Persist the
		// task ID instead and let the GC loop ask the server for terminal
		// state via the task gc-check endpoint.
		meta.Kind = execenv.GCKindQuickCreate
		meta.TaskID = task.ID
	default:
		return execenv.GCMeta{}, false
	}
	return meta, true
}

func taskRootDirParams(workspacesRoot string, task Task) execenv.RootDirParams {
	return execenv.RootDirParams{
		WorkspacesRoot:  workspacesRoot,
		WorkspaceID:     task.WorkspaceID,
		WorkspaceSlug:   task.WorkspaceSlug,
		TaskID:          task.ID,
		IssueIdentifier: task.IssueIdentifier,
	}
}

// shouldReusePriorWorkdir requires a managed task environment for the same conversation.
func shouldReusePriorWorkdir(task Task, localAssignment *localDirectoryAssignment, workspacesRoot string) (string, bool) {
	if task.PriorWorkDir == "" || localAssignment != nil {
		return "", false
	}

	// util.ResolveSymlinks, not filepath.EvalSymlinks: on Windows the latter
	// cannot pass through a directory junction, so a junctioned workspaces
	// root silently declined every reuse and each follow-up lost its session
	// (#8946).
	root, err := util.ResolveSymlinks(workspacesRoot)
	if err != nil {
		return "", false
	}
	workdir, err := util.ResolveSymlinks(task.PriorWorkDir)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(workdir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	rel, err := filepath.Rel(root, workdir)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] != "workdir" {
		return "", false
	}
	if task.AgentID == "" || (task.IssueID == "" && task.ChatSessionID == "") {
		return "", false
	}
	// Managed-env provenance is written only for non-local resumable envs, so
	// its presence (plus the workspace/scope/agent match) proves this is a
	// safe daemon-managed reuse target and not a residual local_directory path.
	prov, err := execenv.ReadManagedEnvProvenance(filepath.Dir(workdir))
	if err != nil || prov.ManagedBy != execenv.ManagedEnvProvenanceManagedBy ||
		prov.WorkspaceID != task.WorkspaceID ||
		prov.AgentID != task.AgentID {
		return "", false
	}

	data, err := os.ReadFile(filepath.Join(workdir, execenv.TaskContextMarkerRelPath))
	if err != nil {
		return "", false
	}
	var marker struct {
		ManagedBy     string `json:"managed_by"`
		AgentID       string `json:"agent_id"`
		IssueID       string `json:"issue_id"`
		ChatSessionID string `json:"chat_session_id"`
	}
	if json.Unmarshal(data, &marker) != nil {
		return "", false
	}
	if marker.ManagedBy != execenv.TaskContextMarkerManagedBy || marker.AgentID != task.AgentID {
		return "", false
	}
	if task.IssueID != "" {
		if prov.IssueID != task.IssueID || marker.IssueID != task.IssueID {
			return "", false
		}
		return workdir, true
	}
	if prov.ChatSessionID != task.ChatSessionID || marker.ChatSessionID != task.ChatSessionID {
		return "", false
	}
	return workdir, true
}

func (d *Daemon) ensureTaskSkillBundles(ctx context.Context, task *Task) error {
	if task == nil || task.Agent == nil || len(task.Agent.SkillRefs) == 0 {
		return nil
	}
	resolved := make(map[string]SkillData, len(task.Agent.SkillRefs))
	misses := make([]SkillRefData, 0)
	for _, ref := range task.Agent.SkillRefs {
		ref := ref
		var bundle SkillData
		if err := d.skillCache.WithRefLock(task.WorkspaceID, ref, func() error {
			if cached, ok := d.skillCache.Load(task.WorkspaceID, ref); ok {
				bundle = cached
				return nil
			}
			misses = append(misses, ref)
			return nil
		}); err != nil {
			return fmt.Errorf("load skill bundle cache: %w", err)
		}
		if bundle.ID != "" {
			resolved[skillRefKey(ref.Source, ref.ID)] = bundle
		}
	}

	// Resolve each missing bundle in its own request, caching it the moment it
	// arrives. The download is the slow part on jittery links, so fetching the
	// whole set in one atomic body read meant a single timeout discarded all
	// progress and the cache never converged — every dispatch re-downloaded
	// everything and timed out again. Per-skill, each download fits its own
	// size-scaled deadline and is persisted independently, so even a dispatch
	// that ultimately fails leaves the skills it did fetch cached for the next
	// one. (GitHub #4505 / MUL-3650)
	for _, ref := range misses {
		started := time.Now()
		bundle, stats, err := d.resolveSkillBundle(ctx, task, ref)
		if err != nil {
			if isSkillBundleTransferFailure(err) {
				// Only transport failures and incomplete 2xx bodies get the
				// network diagnosis. HTTP error responses and invalid complete
				// payloads retain their server semantics instead of being
				// relabelled as connectivity problems (GitHub #7386).
				return fmt.Errorf("%w: %s: %w",
					errSkillBundleUnavailable,
					describeSkillBundleFailure(ref, stats, time.Since(started)),
					err)
			}
			return fmt.Errorf("%w: %w", errSkillBundleUnavailable, err)
		}
		resolved[skillRefKey(bundle.Source, bundle.ID)] = bundle
	}

	skills := make([]SkillData, 0, len(task.Agent.SkillRefs))
	for _, ref := range task.Agent.SkillRefs {
		bundle, ok := resolved[skillRefKey(ref.Source, ref.ID)]
		if !ok {
			return fmt.Errorf("skill bundle missing after resolve: skill_id=%s source=%s hash=%s", ref.ID, ref.Source, ref.Hash)
		}
		skills = append(skills, bundle)
	}
	task.Agent.Skills = skills
	return nil
}

// resolveSkillBundle downloads one skill bundle and writes it to the on-disk
// cache before returning. The request runs under its own deadline, scaled to
// the bundle's declared size rather than the daemon's fixed 30s control-plane
// timeout, so a large bundle on a slow link is given room to finish instead of
// being cut off mid-body. Caching on success is what lets the resolve converge
// across dispatches. (GitHub #4505 / MUL-3650)
func (d *Daemon) resolveSkillBundle(ctx context.Context, task *Task, ref SkillRefData) (SkillData, TransferStats, error) {
	reqCtx, cancel := context.WithTimeout(ctx, skillBundleResolveTimeout(ref.SizeBytes))
	defer cancel()

	bundle, stats, err := d.client.ResolveSkillBundle(reqCtx, task.RuntimeID, task.ID, ref)
	if err != nil {
		return SkillData{}, stats, err
	}
	// The resolve endpoint serves the agent's *current* bundle and hash, which
	// may differ from the claim-time ref when the skill was edited between
	// claim and prepare (see ResolveTaskSkillBundles). So confirm only that the
	// server returned the skill we asked for (source/id), then validate the
	// bundle for self-consistency against a ref derived from itself — pinning
	// it to the possibly-stale requested hash would reject a legitimate update.
	if bundle.Source != ref.Source || bundle.ID != ref.ID {
		return SkillData{}, stats, fmt.Errorf("resolve skill bundle returned wrong skill: requested source=%s id=%s, got source=%s id=%s", ref.Source, ref.ID, bundle.Source, bundle.ID)
	}
	bundleRef := skillRefFromBundle(bundle)
	validationRef := bundleRef
	if ref.Source == skillbundle.SourcePlugin {
		validationRef = ref
	}
	if !validateSkillBundle(validationRef, bundle) {
		return SkillData{}, stats, fmt.Errorf("resolve skill bundle returned invalid bundle: skill_id=%s source=%s hash=%s", bundle.ID, bundle.Source, bundle.Hash)
	}
	if err := d.skillCache.WithRefLock(task.WorkspaceID, validationRef, func() error {
		return d.skillCache.Store(task.WorkspaceID, bundle)
	}); err != nil {
		if d.logger != nil {
			d.logger.Warn("skill bundle cache store failed; continuing with downloaded bundle",
				"workspace_id", task.WorkspaceID,
				"skill_id", bundle.ID,
				"source", bundle.Source,
				"hash", bundle.Hash,
				"error", err,
			)
		}
	}
	return bundle, stats, nil
}

// isSkillBundleTransferFailure identifies errors for which byte-level network
// diagnostics are meaningful. A requestError is an explicit server response;
// malformed but complete JSON and bundle-validation errors are server payload
// problems. Neither should be presented as a slow or dead network link. In
// particular, json.Decoder can synthesize io.ErrUnexpectedEOF after its source
// ended with a clean EOF, so that decoder error alone is not transport proof.
func isSkillBundleTransferFailure(err error) bool {
	var reqErr *requestError
	if errors.As(err, &reqErr) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// describeSkillBundleFailure renders the diagnostic half of a failed bundle
// download: what was being fetched, how many HTTP response-body bytes arrived,
// the separately declared decoded content size, and whether this host has a
// proxy at all.
//
// The three facts answer the three wrong turns the old text invited. "network
// error downloading skill X" instead of "skill X unavailable" stops the reader
// blaming the skill. The response byte count distinguishes a dead link from a
// partial response without pretending JSON wire bytes and decoded skill-content
// bytes form one progress ratio. The proxy note catches the case behind GitHub
// #7386, where a daemon that inherited no proxy on a cross-border link never got
// a byte through while the same host succeeded through a local proxy.
func describeSkillBundleFailure(ref SkillRefData, stats TransferStats, elapsed time.Duration) string {
	elapsed = elapsed.Round(time.Millisecond)
	var transfer string
	switch {
	case !stats.ResponseStarted:
		transfer = fmt.Sprintf("no successful response from server after %s overall", elapsed)
	case stats.BytesRead == 0:
		transfer = fmt.Sprintf("a successful response started but delivered no body bytes before failing after %s overall", elapsed)
	default:
		// BytesRead is a single-attempt high-water mark, while elapsed covers
		// the logical call including retries and backoff. State both scopes
		// rather than deriving a rate from mismatched measurements.
		transfer = fmt.Sprintf("received up to %s of response body data in one attempt; failed after %s overall",
			formatBytes(stats.BytesRead), elapsed)
	}
	return fmt.Sprintf("network error downloading skill %q (id=%s): %s; declared skill content size %s; %s; the skill content is not at fault",
		ref.Name, ref.ID, transfer, formatBytes(ref.SizeBytes), proxyEnvSummary())
}

// proxyEnvSummary reports whether the daemon inherited any proxy setting. It
// names the variable but never its value: proxy URLs routinely embed
// credentials, and this string lands in task failure text the whole workspace
// can read.
//
// Go's transport only consults these variables — it does not read the Windows
// system proxy — and it reads them at process start, so a daemon that was
// already running when they were set still shows none (GitHub #7386).
func proxyEnvSummary() string {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return fmt.Sprintf("proxy configured (%s)", key)
		}
	}
	return "no proxy configured (HTTPS_PROXY unset)"
}

// formatBytes renders a byte count for humans reading a failure message.
func formatBytes(n int64) string {
	switch {
	case n < 0:
		return "unknown size"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2f MB", float64(n)/(1024*1024))
	}
}

const (
	// skillBundleResolveMinTimeout floors the per-skill resolve deadline so a
	// tiny bundle still tolerates connection setup and round-trip latency.
	skillBundleResolveMinTimeout = 30 * time.Second
	// skillBundleResolveMaxTimeout caps it so a wedged download cannot pin a
	// task in prepare indefinitely.
	skillBundleResolveMaxTimeout = 5 * time.Minute
	// skillBundleResolveMinThroughput is the pessimistic floor throughput
	// (bytes/sec) used to scale the deadline to bundle size — deliberately low
	// to cover slow, jittery links rather than ideal bandwidth.
	skillBundleResolveMinThroughput = 50 * 1024
)

// skillBundleResolveTimeout returns the deadline budget for downloading a
// bundle of the given size: at least skillBundleResolveMinTimeout, scaled up at
// skillBundleResolveMinThroughput, and capped at skillBundleResolveMaxTimeout.
func skillBundleResolveTimeout(sizeBytes int64) time.Duration {
	if sizeBytes <= 0 {
		return skillBundleResolveMinTimeout
	}
	scaled := time.Duration(sizeBytes/skillBundleResolveMinThroughput) * time.Second
	if scaled < skillBundleResolveMinTimeout {
		return skillBundleResolveMinTimeout
	}
	if scaled > skillBundleResolveMaxTimeout {
		return skillBundleResolveMaxTimeout
	}
	return scaled
}

func (d *Daemon) startTaskPrepareLeaseExtender(ctx context.Context, task Task, taskLog *slog.Logger) func() {
	refresh := d.prepareLeaseRefresh
	if refresh <= 0 {
		refresh = taskPrepareLeaseRefresh
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(refresh)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				reqCtx, reqCancel := context.WithTimeout(leaseCtx, taskPrepareLeaseTimeout)
				err := d.client.ExtendTaskPrepareLease(reqCtx, task.RuntimeID, task.ID)
				reqCancel()
				if err != nil {
					taskLog.Warn("extend task prepare lease failed", "error", err)
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// lockReusablePriorEnvRoot decides whether this task may continue in a prior
// task's env root and, if so, takes the exclusion lock on it.
//
// Order matters and is the point of this function. The lock WRITES a file into
// the directory, so eligibility has to be proven first:
// shouldReusePriorWorkdir resolves symlinks, checks the
// {workspace}/{task}/workdir shape and verifies managed provenance, and only
// the canonical path it returns is ever opened. Locking on
// filepath.Dir(task.PriorWorkDir) before that would drop .task_lock into
// whatever the path happened to point at — a symlink target outside the
// workspaces root, or a user's own local_directory.
//
// The re-check under the lock closes the gap between proving eligibility and
// acting on it. Declining is always safe: the caller falls back to a fresh
// Prepare, which costs session continuity, not correctness.
//
// The error return is the one outcome that is NOT "decline and carry on": it is
// non-nil only when the run was cancelled while waiting for the prior env root,
// and it carries the context's cause so the caller can end the task instead of
// preparing an environment for work that no longer exists.
func (d *Daemon) lockReusablePriorEnvRoot(ctx context.Context, task Task, localAssignment *localDirectoryAssignment, heldRoot string) (*execenv.EnvRootClaim, string, os.FileInfo, bool, error) {
	// Pin the workspaces root BEFORE validating anything. Opening it after,
	// from a name validation just approved, would re-resolve that name: rename
	// the root aside, leave a symlink to a look-alike tree, and os.Root
	// faithfully pins the replacement. Root promises you cannot escape the tree
	// it opened — not that it opened the tree you meant. Holding the handle
	// first makes that ordering impossible.
	wsRoot, err := os.OpenRoot(d.cfg.WorkspacesRoot)
	if err != nil {
		return nil, "", nil, false, nil
	}
	defer wsRoot.Close()

	workDir, ok := shouldReusePriorWorkdir(task, localAssignment, d.cfg.WorkspacesRoot)
	if !ok {
		return nil, "", nil, false, nil
	}
	priorRoot := filepath.Dir(workDir)
	// workDir came back through util.ResolveSymlinks, so the root it is
	// measured against has to be resolved the same way — otherwise a symlinked
	// workspaces root (macOS /tmp -> /private/tmp, a home on a linked volume,
	// a Windows junction to another drive) makes the two look unrelated and
	// every reuse is refused.
	canonicalWorkspacesRoot, err := util.ResolveSymlinks(d.cfg.WorkspacesRoot)
	if err != nil {
		return nil, "", nil, false, nil
	}
	rel, err := filepath.Rel(canonicalWorkspacesRoot, priorRoot)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, "", nil, false, nil
	}
	// Already covered by this run's own claim (a task re-dispatched onto its
	// own directory); taking a second lock on it would only deadlock against
	// ourselves.
	if priorRoot == heldRoot {
		return nil, workDir, nil, true, nil
	}

	// Pin the identity of the directory that just passed validation. Every
	// later step is checked against THIS, not against whatever the name
	// resolves to next: a path string cannot tell "the directory I validated"
	// apart from "a different directory now answering to that name".
	validatedInfo, err := os.Stat(priorRoot)
	if err != nil {
		return nil, "", nil, false, nil
	}

	// Deterministic seam for the TOCTOU regressions: tests swap the validated
	// directory here, between proving eligibility and taking the lock.
	if reuseLockTestHook != nil {
		reuseLockTestHook()
	}

	lockStartedAt := time.Now()
	claim, lockedInfo, err := d.lockEnvRootForReuseWaitingOutTheBusyWindow(ctx, wsRoot, rel, priorRoot, task)
	switch {
	case errors.Is(err, errPriorEnvRootWaitAborted):
		// The run is over. Declining reuse would hand the caller on to a fresh
		// Prepare, which is the one thing that must not happen here.
		return nil, "", nil, false, context.Cause(ctx)
	case errors.Is(err, execenv.ErrEnvRootBusy):
		d.logger.Info("prior workdir is still in use after waiting for it; starting a fresh environment",
			"task", task.ID, "prior_root", filepath.Base(priorRoot),
			"waited", time.Since(lockStartedAt).Round(time.Millisecond), "budget", d.envRootBusyWait)
		return nil, "", nil, false, nil
	case err != nil:
		d.logger.Warn("could not lock prior workdir; starting a fresh environment",
			"task", task.ID, "error", err)
		return nil, "", nil, false, nil
	case claim == nil:
		return nil, "", nil, false, nil
	}
	// The lock has to have landed on the directory validation approved.
	if !os.SameFile(validatedInfo, lockedInfo) {
		d.logger.Info("prior workdir changed identity before it could be claimed; starting a fresh environment",
			"task", task.ID)
		claim.Release()
		return nil, "", nil, false, nil
	}

	// Re-validate while holding the lock, and require the answer to be the SAME
	// directory. Equality of the canonical path is not enough on its own: the
	// tree can be rearranged so a different-but-equally-valid env root now
	// answers to that name, which would leave us holding a lock on one
	// directory while handing another to Reuse. os.SameFile compares identity,
	// not spelling.
	recheckedWorkDir, stillOK := shouldReusePriorWorkdir(task, localAssignment, d.cfg.WorkspacesRoot)
	if !stillOK || recheckedWorkDir != workDir {
		claim.Release()
		return nil, "", nil, false, nil
	}
	// ...and the directory we are about to hand to Reuse has to be that same
	// one, so the object locked and the object used cannot diverge.
	currentInfo, err := os.Stat(filepath.Dir(recheckedWorkDir))
	if err != nil || !os.SameFile(lockedInfo, currentInfo) {
		d.logger.Info("prior workdir changed identity while being claimed; starting a fresh environment",
			"task", task.ID)
		claim.Release()
		return nil, "", nil, false, nil
	}
	return claim, workDir, lockedInfo, true, nil
}

// envRootBusyRetryInterval is how often the wait below re-tries the lock. The
// window it is covering is seconds long, so this only has to be small relative
// to that, not tight.
const envRootBusyRetryInterval = 250 * time.Millisecond

// errPriorEnvRootWaitAborted marks the wait as ended by the run itself rather
// than by the lock. It wraps the context's cause so the task can be failed with
// the real reason.
var errPriorEnvRootWaitAborted = errors.New("waiting for the prior env root was aborted")

// lockEnvRootForReuseWaitingOutTheBusyWindow takes the reuse lock, waiting out
// a lock the PREVIOUS run of this (issue, agent) has not let go of yet.
//
// A busy lock here has one cause in a healthy system: the server hands a task
// to a daemon only when no other task for the same (issue, agent) is dispatched
// or running (ClaimAgentTask's serialization), so by the time this task exists
// its predecessor is already finished as far as the server is concerned. The
// process is not: it learns of its cancellation from its own poll tick and
// takes a few seconds to exit, and it holds .task_lock until it does. That is
// the whole of the gap — a machine-local fact, visible right here.
//
// Giving up immediately costs the workdir, and with it the provider session
// living in it: the agent restarts with no memory of the conversation the user
// was in the middle of editing (MUL-6880). Waiting costs a few seconds of one
// task slot, which is far less than the fresh Prepare — including a repo
// checkout — that declining forces instead.
//
// The wait is bounded and gives up into exactly the old behaviour, so a
// predecessor that is genuinely wedged, or a lock held by something else on a
// shared workspaces root, still ends in a fresh environment rather than a
// stuck task.
func (d *Daemon) lockEnvRootForReuseWaitingOutTheBusyWindow(
	ctx context.Context,
	wsRoot *os.Root,
	rel, priorRoot string,
	task Task,
) (*execenv.EnvRootClaim, os.FileInfo, error) {
	start := time.Now()
	deadline := start.Add(d.envRootBusyWait)
	waited := false
	for {
		claim, info, err := execenv.LockEnvRootForReuse(wsRoot, rel, priorRoot)
		if !errors.Is(err, execenv.ErrEnvRootBusy) || !time.Now().Before(deadline) {
			// The wait's real duration is logged, not the budget: 15s is a
			// reasoned guess (one cancel-poll interval plus the agent's exit),
			// and these lines are how it gets checked against production
			// instead of staying a guess.
			if waited && err == nil {
				d.logger.Info("prior workdir freed while waiting for the previous run to exit",
					"task", task.ID, "prior_root", filepath.Base(priorRoot),
					"waited", time.Since(start).Round(time.Millisecond))
			}
			return claim, info, err
		}
		if !waited {
			waited = true
			d.logger.Info("prior workdir is still held by the previous run; waiting for it",
				"task", task.ID, "prior_root", filepath.Base(priorRoot),
				"budget", d.envRootBusyWait)
		}
		select {
		case <-ctx.Done():
			d.logger.Info("stopped waiting for the prior workdir: the run was cancelled",
				"task", task.ID, "prior_root", filepath.Base(priorRoot),
				"waited", time.Since(start).Round(time.Millisecond))
			// NOT the busy error the loop was carrying. A cancelled run is a
			// third outcome, and collapsing it into "still busy" would send the
			// caller on to a fresh Prepare for work nobody is waiting for, and
			// would file one cancellation under both "cancelled" and "budget
			// exhausted" in the very logs the budget is meant to be judged by.
			return nil, nil, fmt.Errorf("%w: %w", errPriorEnvRootWaitAborted, context.Cause(ctx))
		case <-time.After(envRootBusyRetryInterval):
		}
	}
}

// reuseLockTestHook runs between reuse eligibility validation and taking the
// prior-root lock. Nil outside tests; the TOCTOU regressions use it to make the
// swap deterministic instead of racing it.
var reuseLockTestHook func()

// reuseBeforeUseTestHook runs after the prior-root claim is settled and before
// Reuse resolves that path by name. Nil outside tests.
var reuseBeforeUseTestHook func()

func (d *Daemon) prepareExecutionEnvironment(ctx context.Context, params execenv.PrepareParams) (*execenv.Environment, error) {
	if d.executionEnvironmentCommand == nil {
		// Focused runTask tests construct a zero-valued Daemon and keep setup
		// in-process. Production Daemons created by New always use isolation.
		return execenv.Prepare(params, d.logger)
	}
	command, err := d.executionEnvironmentCommand()
	if err != nil {
		return nil, err
	}
	return execenv.PrepareIsolated(ctx, command, params, d.logger)
}

func (d *Daemon) reuseExecutionEnvironment(ctx context.Context, params execenv.ReuseParams) (*execenv.Environment, error) {
	if d.executionEnvironmentCommand == nil {
		return execenv.Reuse(params, d.logger), nil
	}
	command, err := d.executionEnvironmentCommand()
	if err != nil {
		return nil, err
	}
	return execenv.ReuseIsolated(ctx, command, params, d.logger)
}

func (d *Daemon) effectiveTaskPrepareTimeout() time.Duration {
	if d.taskPrepareTimeout > 0 {
		return d.taskPrepareTimeout
	}
	return defaultTaskPrepareTimeout
}

func skillRefKey(source, id string) string {
	return source + "\x00" + id
}

func skillRefFromBundle(bundle SkillData) SkillRefData {
	files := make([]skillbundle.File, 0, len(bundle.Files))
	for _, file := range bundle.Files {
		files = append(files, skillbundle.File{Path: file.Path, Content: file.Content})
	}
	manifest := skillbundle.BuildManifest(skillbundle.Skill{
		ID:          bundle.ID,
		Source:      bundle.Source,
		Name:        bundle.Name,
		Description: bundle.Description,
		Content:     bundle.Content,
		Files:       files,
	})
	fileRefs := make([]SkillFileRefData, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		fileRefs = append(fileRefs, SkillFileRefData{Path: file.Path, SHA256: file.SHA256, SizeBytes: file.SizeBytes})
	}
	return SkillRefData{
		ID:        bundle.ID,
		Source:    bundle.Source,
		Hash:      manifest.Hash,
		SizeBytes: manifest.SizeBytes,
		FileCount: manifest.FileCount,
		Files:     fileRefs,
	}
}

// taskModelSelection is what a task actually launches with: the model
// selector plus the capability overrides that survived validation.
type taskModelSelection struct {
	Model         string
	ThinkingLevel string
	ServiceTier   string
}

// resolveTaskModelSelection settles the model selector and its capability
// overrides against the runtime's own model catalog, reading that catalog at
// most once per task — and not at all when nothing needs it.
func (d *Daemon) runTask(ctx context.Context, task Task, provider string, slot int, taskLog *slog.Logger) (taskResult TaskResult, returnErr error) {
	if provider != "native" {
		return TaskResult{}, fmt.Errorf("native daemon refuses external runtime %q", provider)
	}
	phaseRecorder := taskPhaseRecorderFromContext(ctx)
	phaseRecorder.Mark(taskPhasePrepareStarted)
	// A claim carries the task-row agent id both at the top level and inside
	// the expanded agent configuration. The top-level id is authoritative
	// because it is also bound into the task-scoped token. Never prepare or
	// reuse a workdir when the two identities disagree.
	if err := validateTaskIdentity(task); err != nil {
		return TaskResult{}, err
	}

	// Refuse to spawn an agent without a workspace. An empty workspace_id
	// here would make MULTICA_WORKSPACE_ID empty in the agent env, and the
	// CLI would otherwise silently fall back to the user-global config — a
	// path that can leak operations into an unrelated workspace when
	// multiple workspaces share a host.
	if task.WorkspaceID == "" {
		return TaskResult{}, fmt.Errorf("refusing to spawn agent: task has no workspace_id (task_id=%s)", task.ID)
	}

	prepareTimeout := d.effectiveTaskPrepareTimeout()
	prepareCtx, cancelPrepare := context.WithTimeoutCause(ctx, prepareTimeout, errTaskPrepareTimeout)
	prepareComplete := false
	defer func() {
		cancelPrepare()
		if prepareComplete || returnErr == nil || errors.Is(returnErr, errStartClaimRejected) || !errors.Is(context.Cause(prepareCtx), errTaskPrepareTimeout) {
			return
		}
		// Collapse every deadline shape (context deadline, HTTP cancellation,
		// or the explicit waitForExecutionEnvironment cause) into one sentinel
		// that handleTask can classify as a retryable platform timeout.
		taskResult = TaskResult{}
		returnErr = fmt.Errorf("%w after %s", errTaskPrepareTimeout, prepareTimeout)
	}()

	// task.Repos is the authoritative repo list for this task — when the
	// claimed task belongs to a project with github_repo resources the server
	// has already narrowed it to project repos only. Make sure those URLs are
	// in the per-workspace allowlist and the local cache, otherwise
	// `multica repo checkout` would reject project-only URLs that aren't also
	// bound at the workspace level.
	d.registerTaskRepos(task.WorkspaceID, task.ID, task.Repos)
	defer d.clearTaskRepoRefs(task.WorkspaceID, task.ID)

	stopPrepareLease := d.startTaskPrepareLeaseExtender(prepareCtx, task, taskLog)
	defer stopPrepareLease()

	if err := d.ensureTaskSkillBundles(prepareCtx, &task); err != nil {
		return TaskResult{}, err
	}
	phaseRecorder.Mark(taskPhaseSkillsReady)

	agentName := "agent"
	var skills []SkillData
	var instructions string
	agentName = task.Agent.Name
	skills = task.Agent.Skills
	instructions = task.Agent.Instructions

	// Prepare isolated execution environment.
	// Repos are passed as metadata only — the agent checks them out on demand
	// via `multica repo checkout <url>`.
	taskCtx := execenv.TaskContextForEnv{
		IssueID:             task.IssueID,
		TriggerCommentID:    task.TriggerCommentID,
		TriggerThreadID:     task.TriggerThreadID,
		CommentReplyTargets: commentReplyThreads(task),
		NewCommentCount:     task.NewCommentCount,
		NewCommentsSince:    task.NewCommentsSince,
		PriorSessionResumed: task.PriorSessionID != "",
		// MUL-5305: the server sets this when a more recent Codex session was
		// withheld (rollout missing) and PriorSessionID is an older fallback (or
		// absent). Seed the brief's continuity disclosure from it; the local
		// resume gates below only ever OR it to true, so the signal is monotonic.
		PriorSessionResumeUnavailable:    task.PriorSessionResumeUnavailable,
		AgentID:                          task.AgentID,
		AgentName:                        agentName,
		AgentInstructions:                instructions,
		AgentSkills:                      convertSkillsForEnv(skills),
		DisabledRuntimeSkills:            convertDisabledRuntimeSkillsForEnv(task.Agent, task.RuntimeID, provider),
		Repos:                            convertReposForEnv(task.Repos),
		ProjectID:                        task.ProjectID,
		ProjectTitle:                     task.ProjectTitle,
		ProjectDescription:               task.ProjectDescription,
		ProjectResources:                 convertProjectResourcesForEnv(task.ProjectResources),
		ChatSessionID:                    task.ChatSessionID,
		ChatChannelType:                  task.ChatChannelType,
		ChatChannelDeliversFiles:         task.ChatChannelDeliversFiles,
		AutopilotRunID:                   task.AutopilotRunID,
		AutopilotID:                      task.AutopilotID,
		AutopilotTitle:                   task.AutopilotTitle,
		AutopilotDescription:             task.AutopilotDescription,
		AutopilotSource:                  task.AutopilotSource,
		AutopilotTriggerPayload:          strings.TrimSpace(string(task.AutopilotTriggerPayload)),
		QuickCreatePrompt:                task.QuickCreatePrompt,
		IsSquadLeader:                    taskIsSquadLeader(task),
		RequestingUserName:               task.RequestingUserName,
		RequestingUserProfileDescription: task.RequestingUserProfileDescription,
		InitiatorType:                    task.InitiatorType,
		InitiatorID:                      task.InitiatorID,
		InitiatorName:                    task.InitiatorName,
		InitiatorEmail:                   task.InitiatorEmail,
		WorkspaceContext:                 task.WorkspaceContext,
		IssueStatuses:                    convertIssueStatusesForEnv(task.IssueStatuses),
		IssueStatusesOmitted:             task.IssueStatusesOmitted,
		ConnectedApps:                    task.ConnectedApps,
	}

	// Mark candidate env roots as active before any env work so the GC loop
	// can't reclaim artifacts inside them mid-execution. We mark both the
	// stable root for a fresh Prepare and the prior root for Reuse — they
	// usually differ (Reuse keeps the original task's directory).
	resolvedRoot, err := execenv.ResolveRootDir(taskRootDirParams(d.cfg.WorkspacesRoot, task))
	if err != nil {
		return TaskResult{}, fmt.Errorf("resolve stable task env root: %w", err)
	}
	d.markActiveEnvRoot(resolvedRoot)
	defer d.unmarkActiveEnvRoot(resolvedRoot)
	if task.PriorWorkDir != "" {
		priorRoot := filepath.Dir(task.PriorWorkDir)
		if priorRoot != resolvedRoot {
			d.markActiveEnvRoot(priorRoot)
			defer d.unmarkActiveEnvRoot(priorRoot)
		}
	}

	// Claim the env root HERE, in the daemon parent, and hold it for the whole
	// task run — the same lifetime as unmarkActiveEnvRoot above.
	//
	// It cannot be claimed inside preparation: production preparation runs in a
	// short-lived helper process (prepareExecutionEnvironment ->
	// PrepareIsolated), so a lock taken there dies with the helper and the
	// *os.File cannot cross its JSON response back to us. Claiming there would
	// leave the agent running with no protection at all — which is exactly the
	// re-dispatch window this guards.
	envClaim, err := execenv.ClaimEnvRoot(taskRootDirParams(d.cfg.WorkspacesRoot, task))
	if err != nil {
		return TaskResult{}, fmt.Errorf("claim execution environment: %w", err)
	}
	defer envClaim.Release()

	// Try to reuse the workdir from a previous task on the same (agent, issue) pair.
	var env *execenv.Environment
	// Resolve any local_directory assignment again here so runTask can plumb
	// LocalWorkDir into execenv. handleTask already validated + locked the
	// path for worker tasks; leader tasks intentionally skip the assignment.
	localAssignment, _ := localDirectoryAssignmentForTask(task, d.cfg.DaemonID)
	// Reuse intentionally skipped for local_directory tasks: the prior
	// WorkDir is the user's own path (always present) but the reuse path
	// loses the envRoot association the GC loop needs, and re-running
	// Prepare against a stable user path is cheap (no clone, no copy).
	// Leader tasks have no localAssignment; shouldReusePriorWorkdir separately
	// requires Prepare-time managed-env provenance and a daemon-owned marker
	// before allowing reuse, so a pre-fix leader session recorded against
	// local_directory still fails closed.
	var effectiveMcpConfig json.RawMessage
	remoteMCPConfig, remoteMCPDiagnostics, remoteMCPBrokers, remoteMCPErr := startTaskRemoteMCPBrokers(
		prepareCtx, ctx, task.ID, provider, task.RemoteMCPConnections,
		func(resolveCtx context.Context, contributionID string) (http.Header, error) {
			return d.client.ResolveRemoteMCPCredential(resolveCtx, task.RemoteMCPDaemonToken, task.ID, contributionID)
		},
		taskLog,
	)
	if remoteMCPErr != nil {
		return TaskResult{}, fmt.Errorf("prepare Remote MCP broker: %w", remoteMCPErr)
	}
	if remoteMCPBrokers != nil {
		defer remoteMCPBrokers.Close()
	}
	for _, diagnostic := range remoteMCPDiagnostics {
		taskLog.Warn("Remote MCP degraded", "reason", diagnostic)
	}

	// Agent-trigger plugin hooks, as a second local MCP server beside the
	// broker. A failure to start it degrades to no plugin tools rather than
	// failing the task: an agent that cannot reach a plugin should still work
	// on the issue, which is the same rule that makes a failing tool call a
	// tool error rather than a task error.
	pluginHookConfig, pluginHookServer, pluginHookErr := startTaskPluginHookMCP(
		ctx, task.ID, task.PluginHookTools,
		func(callCtx context.Context, taskID, installationID, hookKey string, input json.RawMessage) (json.RawMessage, error) {
			return d.client.InvokeAgentPluginHook(callCtx, task.RemoteMCPDaemonToken, taskID, installationID, hookKey, input)
		},
		taskLog,
	)
	if pluginHookErr != nil {
		taskLog.Warn("plugin hook tools unavailable", "error", pluginHookErr)
	}
	if pluginHookServer != nil {
		defer pluginHookServer.Close()
	}
	if len(pluginHookConfig) > 0 {
		merged, mergeErr := mergeTaskRemoteMCPConfig(remoteMCPConfig, pluginHookConfig)
		if mergeErr != nil {
			taskLog.Warn("could not merge plugin hook MCP config", "error", mergeErr)
		} else {
			remoteMCPConfig = merged
		}
	}
	if task.Agent != nil {
		effectiveMcpConfig = task.Agent.McpConfig
	}
	if len(remoteMCPConfig) > 0 {
		merged, mergeErr := mergeTaskRemoteMCPConfig(effectiveMcpConfig, remoteMCPConfig)
		if mergeErr != nil {
			return TaskResult{}, fmt.Errorf("merge task MCP broker configuration: %w", mergeErr)
		}
		effectiveMcpConfig = merged
	}
	priorClaim, priorWorkDir, lockedPriorInfo, reusable, reuseErr := d.lockReusablePriorEnvRoot(ctx, task, localAssignment, envClaim.RootDir())
	if reuseErr != nil {
		// Cancelled while waiting for the previous run to let go of its
		// directory. Ending here IS the behaviour: falling through would
		// prepare a whole environment — repo checkout included — for a task
		// nobody is waiting for any more.
		return TaskResult{}, reuseErr
	}
	if reusable {
		defer priorClaim.Release()
		// Deterministic seam for the last-window regression: tests swap the
		// directory here, after the claim is settled and before Reuse resolves
		// the path by name.
		if reuseBeforeUseTestHook != nil {
			reuseBeforeUseTestHook()
		}
		var err error
		env, err = d.reuseExecutionEnvironment(prepareCtx, execenv.ReuseParams{
			WorkspacesRoot: d.cfg.WorkspacesRoot,
			Profile:        d.cfg.Profile,
			// The canonical path the lock was taken on. Handing Reuse the raw
			// PriorWorkDir instead would re-resolve it, so the directory we
			// locked and the directory we use could differ.
			WorkDir:         priorWorkDir,
			Provider:        provider,
			ResumeSessionID: task.PriorSessionID,
			McpConfig:       effectiveMcpConfig,
			Task:            taskCtx,
		})
		if err != nil {
			return TaskResult{}, asEnvironmentSetupFailure(fmt.Errorf("reuse execution environment: %w", err))
		}
		// Reuse resolves priorWorkDir by name, so confirm what it actually
		// opened is still the directory we hold the lock on. An fd cannot cross
		// into the preparation helper process, so the name is the only thing
		// that can be handed over; this turns "silently ran somewhere else"
		// into "declined and started clean". See lockReusablePriorEnvRoot for
		// what remains uncovered.
		if env != nil && lockedPriorInfo != nil {
			usedInfo, statErr := os.Stat(filepath.Dir(env.WorkDir))
			if statErr != nil || !os.SameFile(lockedPriorInfo, usedInfo) {
				// No "task" field here: taskLog already carries the full id.
				taskLog.Info("reused workdir is not the directory that was claimed; starting a fresh environment")
				env = nil
			}
		}
		// Reuse can decline (nil) and fall through to a fresh Prepare below.
		// Whether it did decides whether an env-root-scoped session store — the
		// Hermes overlay's task-local state.db — carried over from the prior task.
	}
	if env == nil {
		var err error
		prepParams := execenv.PrepareParams{
			WorkspacesRoot:  d.cfg.WorkspacesRoot,
			Profile:         d.cfg.Profile,
			WorkspaceID:     task.WorkspaceID,
			WorkspaceSlug:   task.WorkspaceSlug,
			TaskID:          task.ID,
			IssueIdentifier: task.IssueIdentifier,
			AgentName:       agentName,
			// This run already holds the claim (envClaim above) and the reset
			// it implies; preparation must not try to take it again.
			EnvRootPreclaimed: true,
			Provider:          provider,
			McpConfig:         effectiveMcpConfig,
			Task:              taskCtx,
		}
		if localAssignment.UsesWorktree() {
			prepParams.LocalWorktree = &execenv.LocalWorktreeParams{LocalPath: localAssignment.AbsPath}
			// Take the per-path mutex for the snapshot alone, then hand it
			// straight back — long enough to read a consistent tree, short
			// enough that worktree tasks still overlap for the run itself.
			//
			// A worktree task skips this lock for its execution, but the
			// snapshot is the one moment it READS the user's directory, and the
			// same real path can be attached to another project as an in_place
			// resource (each project may attach it once, so several can).
			// Snapshotting underneath a running in_place task would capture a
			// half-written tree plus that task's in-flight sidecars.
			//
			// The wait gets the same visibility plumbing as the in-place
			// acquire in acquireLocalDirectoryLockIfNeeded, because the holder
			// can be an in-place task that runs for hours: without the status
			// update the user sees a bare "preparing" with no hint the task is
			// queued behind the directory, and without the poller a task the
			// user cancels keeps its daemon slot pinned until the prepare
			// timeout — the run-phase cancellation watcher only starts after
			// launch. The prepare-lease extender is already running for this
			// whole phase, so only status, accounting, and cancellation are
			// mirrored here.
			waitCtx, waitCancel := context.WithCancel(prepareCtx)
			defer waitCancel()
			pollInterval := d.cancelPollInterval
			if pollInterval == 0 {
				pollInterval = 5 * time.Second
			}
			// LocalPathLocker invokes onWait synchronously, in this goroutine,
			// at most once per Acquire — see the in-place call site.
			waitCounted := false
			release, lockErr := d.localPathLocks.Acquire(waitCtx, localAssignment.RealPath, task.ID, func(holder string) {
				d.resourceWaitTasks.Add(1)
				waitCounted = true
				reason := fmt.Sprintf("local_directory %s", localAssignment.AbsPath)
				if holder != "" {
					reason = fmt.Sprintf("%s (held by task %s)", reason, shortID(holder))
				}
				taskLog.Info("local_directory: worktree snapshot waiting for holder",
					"holder", holder)
				if waitErr := d.client.MarkTaskWaitingLocalDirectory(waitCtx, task.ID, reason); waitErr != nil {
					// Non-fatal: the wait still happens, the UI just won't
					// show the explicit "waiting" badge.
					taskLog.Warn("local_directory: mark waiting status failed", "error", waitErr)
				}
				cancelled := d.watchTaskCancellation(waitCtx, task.ID, pollInterval, taskLog)
				go func() {
					select {
					case <-cancelled:
						waitCancel()
					case <-waitCtx.Done():
					}
				}()
			})
			if waitCounted {
				d.resourceWaitTasks.Add(-1)
			}
			if lockErr != nil {
				return TaskResult{}, fmt.Errorf("local_directory worktree: wait for a consistent snapshot of %s: %w",
					localAssignment.AbsPath, lockErr)
			}
			env, err = d.prepareExecutionEnvironment(prepareCtx, prepParams)
			release()
			if err != nil {
				return TaskResult{}, asEnvironmentSetupFailure(fmt.Errorf("prepare execution environment: %w", err))
			}
		} else {
			if localAssignment != nil {
				prepParams.LocalWorkDir = localAssignment.AbsPath
			}
			env, err = d.prepareExecutionEnvironment(prepareCtx, prepParams)
			if err != nil {
				return TaskResult{}, asEnvironmentSetupFailure(fmt.Errorf("prepare execution environment: %w", err))
			}
		}
	}
	phaseRecorder.Mark(taskPhaseEnvironmentReady)
	// Belt-and-suspenders: also mark whatever root we ended up with, in case
	// future changes diverge from ResolveRootDir.
	if env.RootDir != resolvedRoot && env.RootDir != "" {
		d.markActiveEnvRoot(env.RootDir)
		defer d.unmarkActiveEnvRoot(env.RootDir)
	}
	// Finalize the worktree on EVERY exit path, success or failure: commit
	// whatever the agent left uncommitted, then unregister the worktree from
	// the user's repo. Deferred against the named return so a task that fails
	// mid-run still hands back the branch holding its partial work instead of
	// letting `git worktree remove --force` delete it. A failing task is
	// exactly when the user most wants to see how far the agent got.
	//
	// In-place local_directory runs never enter this block: their WorkDir is
	// already durable, so DurableWorkDir deliberately stays absent instead of
	// duplicating the same path under two lifecycle meanings.
	if env.LocalWorktree != nil {
		defer func() {
			if taskResult.WorkDir == "" {
				taskResult.WorkDir = env.WorkDir
			}
			if taskResult.EnvRoot == "" {
				taskResult.EnvRoot = env.RootDir
			}
			outcome, finalizeErr := env.LocalWorktree.Finalize(taskLog)
			if outcome.Branch != "" {
				taskResult.BranchName = outcome.Branch
			}
			if finalizeErr == nil {
				// The configured local_directory becomes authoritative only after
				// Finalize confirms the disposable task worktree is actually gone.
				if localAssignment != nil {
					taskResult.DurableWorkDir = localAssignment.AbsPath
				}
				return
			}
			// Finalize could not complete its delivery contract, so the task
			// worktree remains authoritative. This covers both an uncommitted
			// change set and a committed branch whose worktree removal could not
			// be confirmed. Fail the task: reporting success or a durable project
			// directory here would hide the path that still needs attention.
			//
			// Wrapped in worktreePreservedError so the cancel path can
			// recognise it: a cancelled task discards its result and error, but
			// THIS error names the preserved worktree holding the agent's work
			// and must ride the cancel ack instead of vanishing into a log.
			// Joined rather than replacing an earlier failure — that one is
			// usually the more useful primary cause, but the preserved path
			// must not be displaced by it.
			taskLog.Error("local_directory: worktree finalize incomplete; keeping the task worktree authoritative",
				"error", finalizeErr, "preserved_path", outcome.PreservedPath)
			wrapped := &worktreePreservedError{err: fmt.Errorf("local_directory worktree: %w", finalizeErr)}
			if returnErr == nil {
				returnErr = wrapped
			} else {
				returnErr = errors.Join(returnErr, wrapped)
			}
		}()
	}
	// Workdir is preserved for reuse by future tasks on the same (agent,
	// issue) pair in cloud mode; the work_dir path is stored in DB on task
	// completion and passed back via PriorWorkDir on the next claim, so
	// rewriting the marker block in place is the right behavior.
	//
	// In local_directory mode the workdir is the user's own repo, reuse is
	// already disabled above (see localAssignment == nil), and the brief
	// would otherwise live on inside the user's repository — a subsequent
	// manual `claude` / `codex` run in that directory would pick
	// up stale Multica instructions (issue id, trigger comment id, reply
	// rules) and start acting on the previous task's context. Excise the
	// marker block on the way out instead.
	//
	// Worktree mode runs the same pass for a different reason: the worktree is
	// disposable, but its branch is the deliverable, and Finalize commits
	// whatever is still on disk. Without this the sidecars would land in every
	// task's diff. The .git/info/exclude trick repocache uses for github_repo
	// worktrees is not available here — a linked worktree resolves info/exclude
	// to the user's own common git dir, so using it would silently change what
	// `git status` hides in the user's checkout. Removing the files we wrote is
	// both narrower and exact; it also leaves a genuine agent edit to a tracked
	// CLAUDE.md intact, since CleanupRuntimeConfig only excises our marker block.
	//
	// Ordering: registered immediately after the Finalize defer above, so LIFO
	// runs cleanup first and Finalize commits an already-clean worktree. It must
	// also precede every early return between here and provider launch
	// (temp-dir setup, StartTask): those paths still run Finalize, and without
	// this pass Finalize would auto-commit the sidecars Prepare just wrote and
	// deliver a branch whose only content is Multica's own runtime files — or,
	// in place, leave them behind in the user's tree.
	if env.LocalDirectory || env.LocalWorktree != nil {
		defer func() {
			var cleanupErr error
			if cerr := execenv.CleanupRuntimeConfig(env.WorkDir, provider); cerr != nil {
				cleanupErr = cerr
				d.logger.Warn("execenv: cleanup runtime config failed", "error", cerr)
			}
			// Excise the sidecar tree (.agent_context/, .multica/,
			// provider-specific .claude/skills/ etc.) that Prepare wrote
			// into the user's repo. Without this pass the user's tree
			// accumulates one directory layer per task — see MUL-2784.
			// CleanupRuntimeConfig handles the runtime brief inside
			// CLAUDE.md / AGENTS.md; CleanupSidecars handles
			// every other file Prepare placed under WorkDir. Together
			// they round-trip the workdir to its exact pre-task bytes.
			if cerr := execenv.CleanupSidecars(env.RootDir); cerr != nil {
				if cleanupErr == nil {
					cleanupErr = cerr
				}
				d.logger.Warn("execenv: cleanup sidecars failed", "error", cerr)
			}
			// In worktree mode a failed cleanup is NOT survivable: Finalize is
			// about to `git add -A`, so whatever the cleanup could not remove
			// gets committed and delivered as the task's branch — a diff whose
			// content is Multica's own runtime files, which is precisely what
			// this mode promises never to produce. Tell Finalize to abort
			// instead, so nothing is committed and the worktree is kept for
			// inspection. (In place there is no commit and no branch, so a
			// cleanup failure stays a warning: the leftover files are visible
			// in the user's own tree and removable by hand.)
			if cleanupErr != nil && env.LocalWorktree != nil {
				env.LocalWorktree.AbortWithReason(fmt.Errorf(
					"could not remove the runtime's own files from the worktree before committing: %w", cleanupErr))
			}
		}()
	}
	taskTempDir, taskTempLock, err := ensureTaskTempDir(env.RootDir, task.WorkspaceID, task.ID)
	if err != nil {
		return TaskResult{}, fmt.Errorf("prepare task temp dir: %w", err)
	}
	defer func() {
		// Drop the execution lock before removing the directory: while it is
		// held the GC sweep correctly refuses to touch this directory, so a
		// removal that fails here (a file inside still open — the Windows case
		// in #7364) would otherwise leave the directory pinned until the daemon
		// exits. Released first, the next GC cycle acquires the lock, sees the
		// owner is gone and reclaims it. Nothing waits on the task path.
		//
		// RemoveTaskTempDir rather than os.RemoveAll so that a cleanup which
		// fails here leaves the .task_lock marker intact — without it the next
		// sweep could not tell this directory from a pre-lock leftover.
		execenv.ReleaseTaskTempLock(taskTempLock)
		if cerr := execenv.RemoveTaskTempDir(taskTempDir); cerr != nil {
			taskLog.Warn("task temp dir cleanup failed", "path", taskTempDir, "error", cerr)
		}
	}()

	// Issue #3999 race A: now that env.WorkDir is on disk, transition the
	// server-side state machine dispatched (or waiting_local_directory) →
	// running. Calling StartTask before Prepare/Reuse let any consumer
	// that read status==running and resolved
	// /multica_workspaces/{ws}/{short-id}/workdir hit FileNotFoundError in
	// the microsecond window before os.MkdirAll ran.
	//
	// On error we return early. A rejected claim is discarded by handleTask;
	// other errors use its existing FailTask + taskfailure.Classify path with the same
	// "start task failed: <…>" string and the same failure_reason
	// taxonomy as before — see MUL-2946 for the classifier contract.
	var taskCapabilities []string
	if task.IssueID != "" {
		taskCapabilities = append(taskCapabilities, protocol.DaemonCapabilityTaskSupplementV1)
	}
	taskSupplementNegotiated, err := d.client.StartTask(prepareCtx, task, taskCapabilities...)
	if err != nil {
		stopPrepareLease()
		return TaskResult{}, fmt.Errorf("start task failed: %w", err)
	}
	if taskSupplementNegotiated {
		// Register before provider launch so a hint cannot arrive in the gap
		// between the committed server transition and turn/started. The row is
		// durable, so a coalesced hint is sufficient; the five-second fallback
		// covers a notification sent before this start response arrived.
		_, unsubscribeSupplements := d.taskSupplementSignals.subscribe(task.ID)
		defer unsubscribeSupplements()
	}
	stopPrepareLease()
	prepareComplete = true
	cancelPrepare()
	_ = d.client.ReportProgress(ctx, task.ID, fmt.Sprintf("Launching %s", provider), 1, 2)

	// Deliver workspace and agent instructions directly to the native model.
	runtimeBrief, err := execenv.InjectRuntimeConfig(env.WorkDir, provider, taskCtx)
	if err != nil {
		return TaskResult{}, fmt.Errorf("prepare native runtime instructions: %w", err)
	}
	// An exempt turn runs in the user's directory without having queued for it,
	// so a sibling coding task may be writing to the same tree right now. That
	// is the one thing it cannot work out from its own context — tell it.
	// Worktree mode is excluded: there the tree is this task's private checkout.
	var promptOptions []PromptOption
	if localAssignment != nil && !localAssignment.UsesWorktree() && localDirectoryLockExempt(task) {
		promptOptions = append(promptOptions, WithSharedLocalDirectory())
	}
	// Worktree mode hands this turn a tree that is mid-merge when the user's
	// edits since the previous turn collided with the branch's own work. The
	// conflict is deliberately left in place for the agent to resolve, so the
	// prompt has to be the thing that tells it (MUL-6881).
	if env.LocalWorktree != nil && len(env.LocalWorktree.ReplayConflicts) > 0 {
		promptOptions = append(promptOptions, WithWorktreeReplayConflicts(env.LocalWorktree.ReplayConflicts))
	}
	prompt := BuildPrompt(task, provider, promptOptions...)

	// Pass task-scoped auth credentials and context so the spawned agent CLI
	// can call the Multica API and the local daemon (e.g. `multica repo checkout`).
	// MULTICA_TASK_SLOT is allocated from the daemon-wide concurrency pool, not
	// per-agent. When one daemon hosts multiple agents, slots index shared
	// daemon-level resources such as GPUs.
	// MULTICA_TOKEN is bound to (agent, task) by the server. Never fall back
	// to the daemon's own credential here: doing so lets agent CLI writes land
	// as the runtime owner's member actor and can retrigger the same agent.
	agentToken, err := taskScopedAuthToken(task)
	if err != nil {
		taskLog.Error("task auth token invalid; refusing to start agent", "error", err)
		return TaskResult{}, err
	}
	agentEnv := taskMulticaEnvironment(task, provider, agentName, agentToken, env.MulticaConfigRoot, d.cfg.WorkspacesRoot, d.cfg.ServerBaseURL, d.cfg.HealthPort, slot, taskTempDir)

	if task.AutopilotRunID != "" {
		agentEnv["MULTICA_AUTOPILOT_RUN_ID"] = task.AutopilotRunID
	}
	if task.AutopilotID != "" {
		agentEnv["MULTICA_AUTOPILOT_ID"] = task.AutopilotID
	}
	// Quick-create marker — when set, the multica CLI's `issue create`
	// command stamps the new issue with origin_type=quick_create +
	// origin_id=<task_id> so the completion handler can find it
	// deterministically (see GetIssueByOrigin).
	if task.QuickCreatePrompt != "" {
		agentEnv["MULTICA_QUICK_CREATE_TASK_ID"] = task.ID
		if len(task.QuickCreateAttachmentIDs) > 0 {
			if raw, err := json.Marshal(task.QuickCreateAttachmentIDs); err == nil {
				agentEnv["MULTICA_QUICK_CREATE_ATTACHMENT_IDS"] = string(raw)
			} else {
				taskLog.Warn("quick-create attachment ids: marshal failed; skipping env injection", "error", err)
			}
		}
	}
	// Ensure the multica CLI is on PATH inside the agent's environment.
	// Prepend this Multica binary so explicitly requested platform commands resolve.
	if selfBin, err := resolveSelfExecutable(); err == nil {
		binDir := filepath.Dir(selfBin)
		agentEnv["PATH"] = binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	// Inject user-configured custom environment variables (e.g. ANTHROPIC_API_KEY,
	// ANTHROPIC_BASE_URL for router/proxy mode, or CLAUDE_CODE_USE_BEDROCK for
	// Bedrock). These are set per-agent via the agent settings UI.
	// Critical internal variables are blocklisted to prevent accidental or
	// malicious override of daemon-set values.
	var agentCustomEnv map[string]string
	if task.Agent != nil {
		agentCustomEnv = task.Agent.CustomEnv
	}
	for key, value := range sanitizeAgentEnv(agentCustomEnv) {
		agentEnv[key] = value
	}
	backend, closeNative, err := d.nativeBackend(ctx, task, env.WorkDir, agentEnv, effectiveMcpConfig, nativeTrustedBrokerEndpoints(remoteMCPConfig))
	if err != nil {
		return TaskResult{}, err
	}
	defer closeNative()
	taskStart := time.Now()
	execOpts := agent.ExecOptions{SystemPrompt: runtimeBrief, EnableTaskSupplement: taskSupplementNegotiated, Cwd: env.WorkDir, Timeout: d.cfg.AgentTimeout}
	// A quick-actions refresh task from a server that predates server-side
	// generation (MUL-5573). This daemon no longer has a suggestion pass to run
	// it with, and it must NOT fall through to the ordinary chat path below:
	// the task carries no user message, so the agent would answer a prompt
	// nobody wrote and that server would persist the result as a real assistant
	// reply. Complete it empty instead — the same shape the retired pass
	// produced on this task, which that server writes no row for. The user's
	// refresh spinner resolves via the client's own timeout.
	if task.RegenerateQuickActionsFor != "" {
		taskLog.Warn("refusing quick-actions refresh task from an older server; complete the daemon upgrade by updating the server",
			"target_task", task.RegenerateQuickActionsFor,
		)
		return TaskResult{Status: "completed", Comment: "", WorkDir: env.WorkDir, EnvRoot: env.RootDir}, nil
	}

	// Authenticate the localhost repo-checkout endpoint with the same
	// task-scoped token the child receives. The endpoint derives identity and
	// branch ownership from this in-memory record instead of trusting request
	// fields or ambient process environment. Register only for the provider
	// execution window and always remove the credential afterwards.
	d.registerActiveRepoCheckoutTask(agentToken, activeRepoCheckoutTask{
		WorkspaceID: task.WorkspaceID,
		TaskID:      task.ID,
		AgentID:     task.AgentID,
		AgentName:   task.Agent.Name,
		WorkDir:     env.WorkDir,
	})
	defer d.clearActiveRepoCheckoutTask(agentToken)

	var msgSeq atomic.Int32
	result, tools, err := d.executeAndDrain(ctx, backend, prompt, execOpts, taskLog, task.ID, &msgSeq)
	if err != nil {
		return TaskResult{}, err
	}
	phaseRecorder.Mark(taskPhaseTurnCompleted)

	elapsed := time.Since(taskStart).Round(time.Second)
	taskLog.Info("agent finished",
		"status", result.Status,
		"duration", elapsed.String(),
		"tools", tools,
	)
	taskLog.Debug("agent result detail",
		"status", result.Status,
		"output_bytes", len(result.Output),
		"session_id", result.SessionID,
		"models_with_usage", len(result.Usage),
		"agent_error", result.Error,
	)

	// Convert agent usage map to task usage entries.
	var usageEntries []TaskUsageEntry
	for model, u := range result.Usage {
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 && u.CostUSDTicks <= 0 {
			continue
		}
		usageEntries = append(usageEntries, TaskUsageEntry{
			Provider:         provider,
			Model:            model,
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
			CostUSDTicks:     u.CostUSDTicks,
		})
	}

	switch result.Status {
	case "completed":
		if result.Output == "" {
			// The agent completed successfully but produced no text output.
			// This is valid — the agent may have done all its work via tool
			// calls (e.g. posting comments via CLI, pushing code). Treat as
			// a normal completion so the task is not incorrectly marked as
			// blocked.
			return TaskResult{
				Status:    "completed",
				Comment:   "",
				SessionID: result.SessionID,
				WorkDir:   env.WorkDir,
				EnvRoot:   env.RootDir,
				Usage:     usageEntries,
			}, nil
		}
		// Detect "poisoned" terminal output: the agent didn't reach a real
		// conclusion but emitted a known fallback marker (iteration limit,
		// fallback meta message). Route through the blocked path with a
		// specific failure_reason so the server can exclude this session
		// from the (agent_id, issue_id) resume lookup — otherwise a manual
		// rerun would inherit the same poisoned session and reproduce the
		// same bad output.
		if reason, ok := classifyPoisonedOutput(result.Output); ok {
			taskLog.Warn("agent finished with poisoned fallback output, classifying as blocked",
				"failure_reason", reason,
			)
			return TaskResult{
				Status:        "blocked",
				Comment:       result.Output,
				SessionID:     result.SessionID,
				WorkDir:       env.WorkDir,
				EnvRoot:       env.RootDir,
				Usage:         usageEntries,
				FailureReason: reason,
			}, nil
		}
		taskResult = TaskResult{
			Status:    "completed",
			Comment:   result.Output,
			SessionID: result.SessionID,
			WorkDir:   env.WorkDir,
			EnvRoot:   env.RootDir,
			Usage:     usageEntries,
		}
		return taskResult, nil
	case "timeout":
		// Surface session_id/work_dir so the chat resume pointer is kept
		// in sync even when the agent times out after building a session.
		// We mark as "blocked" (not a hard error return) so handleTask
		// goes through the FailTask path that forwards session info.
		comment := result.Error
		if comment == "" {
			comment = fmt.Sprintf("%s timed out after %s", provider, d.cfg.AgentTimeout)
		}
		failureReason := "timeout"
		if reason, ok := classifyResumeUnsafeTimeout(provider, comment); ok {
			taskLog.Warn("agent timed out with resume-unsafe session, classifying as blocked",
				"failure_reason", reason,
			)
			failureReason = reason
		}
		return TaskResult{
			Status:        "blocked",
			Comment:       comment,
			SessionID:     result.SessionID,
			WorkDir:       env.WorkDir,
			EnvRoot:       env.RootDir,
			FailureReason: failureReason,
			Usage:         usageEntries,
		}, nil
	case "idle_watchdog":
		// The idle watchdog force-stopped the run because the backend
		// went silent (e.g. claude blocked on a tool call against a
		// frozen child process). Route through the blocked path with a
		// dedicated failure_reason so the run leaves "running" state and
		// operators can tell idle-stop apart from a real timeout.
		comment := result.Error
		if comment == "" {
			comment = idleWatchdogReason(d.cfg.AgentIdleWatchdog)
		}
		return TaskResult{
			Status:        "blocked",
			Comment:       comment,
			SessionID:     result.SessionID,
			WorkDir:       env.WorkDir,
			EnvRoot:       env.RootDir,
			FailureReason: "idle_watchdog",
			Usage:         usageEntries,
		}, nil
	case "cancelled":
		// Server cancelled the task (e.g. issue reassignment, user cancel).
		// handleTask's cancelledByPoll branch already discards this result,
		// so this case is mainly defensive — and preserves the "cancelled"
		// status string for the "agent finished" log line so operators can
		// distinguish "task cancelled by server" from a real timeout.
		return TaskResult{
			Status:    "cancelled",
			Comment:   "task cancelled by server",
			SessionID: result.SessionID,
			WorkDir:   env.WorkDir,
			EnvRoot:   env.RootDir,
			Usage:     usageEntries,
		}, nil
	default:
		errMsg := result.Error
		if errMsg == "" {
			errMsg = fmt.Sprintf("%s execution %s", provider, result.Status)
		}
		if provider == "native" {
			// Native recovery is driven by its journal and SQL checkpoint.
			// Text classifiers must not spawn a fresh run after a partial stream
			// or reinterpret an unknown write as a retryable network failure.
			return TaskResult{Status: "blocked", Comment: errMsg, SessionID: result.SessionID,
				WorkDir: env.WorkDir, EnvRoot: env.RootDir, Usage: usageEntries,
				FailureReason: taskfailure.ReasonAgentUnknown.String()}, nil
		}
		// Forward SessionID/WorkDir on the blocked path: backends commonly
		// emit a real session_id before failing (rate-limit, tool error,
		// model reject, …). Without this the chat_session resume pointer
		// would either be left stale or overwritten with NULL on the
		// server, causing the next chat turn to lose context.
		//
		// Classify upstream API 400 invalid_request_error failures with a
		// dedicated failure_reason so GetLastTaskSession excludes the
		// task from the (agent_id, issue_id) resume lookup. Without this
		// classifier a corrupt image or oversized payload baked into the
		// conversation permanently blocks the issue: every follow-up
		// task resumes the same poisoned session and hits the same 400.
		failureReason, _ := classifyPoisonedError(errMsg)

		if failureReason != "" {
			taskLog.Warn("agent failed with a resume-unsafe error, retiring the session",
				"failure_reason", failureReason,
			)
		} else {
			// MUL-2946: classifyPoisonedError only matches the
			// session-poisoning Anthropic 400 shape. Everything else
			// falls through to taskfailure.Classify, which maps the
			// raw error string to one of the 14 agent_error.*
			// sub-reasons (provider auth, capacity, context overflow,
			// runner crash, …) or to ReasonAgentUnknown. This keeps
			// the failure_reason column in the canonical refined
			// taxonomy at write time instead of waiting on the
			// MUL-1949 offline backfill to re-classify after the
			// fact.
			failureReason = taskfailure.Classify(errMsg).String()
		}
		// After the classifiers above have read errMsg. Each hint is fixed
		// prose chosen to match none of the resume guards (see its const), so
		// ordering is not what makes it safe — but it keeps the machine
		// decisions reading exactly what the runtime reported, and leaves the
		// annotations on the outside where a future edit is visibly a change
		// to human-facing text rather than to classifier input. They are
		// mutually exclusive by provider, so they cannot stack.
		return TaskResult{
			Status:        "blocked",
			Comment:       errMsg,
			SessionID:     result.SessionID,
			WorkDir:       env.WorkDir,
			EnvRoot:       env.RootDir,
			Usage:         usageEntries,
			FailureReason: failureReason,
		}, nil
	}
}

// executeAndDrain runs a backend, drains its message stream (forwarding to the
// server), and waits for the final result. msgSeq numbers the reported task
// messages and is owned by the caller so a same-task retry continues the
// sequence instead of restarting at 1 — the server orders the transcript by
// seq alone, and duplicate seqs would interleave the two attempts' rows.
func (d *Daemon) executeAndDrain(ctx context.Context, backend agent.Backend, prompt string, opts agent.ExecOptions, taskLog *slog.Logger, taskID string, msgSeq *atomic.Int32) (agent.Result, int32, error) {
	phaseRecorder := taskPhaseRecorderFromContext(ctx)
	// Wrap the caller's ctx so the idle watchdog (below) can interrupt both
	// the agent subprocess (via the ctx passed to backend.Execute) AND the
	// drain loop with a single cancel. Without this layer the backend would
	// stay tied to the parent ctx and our cancellation could only abort
	// drain, leaving the subprocess running.
	agentCtx, agentCancel := context.WithCancel(ctx)
	defer agentCancel()

	session, err := backend.Execute(agentCtx, prompt, opts)
	if err != nil {
		// One provider-agnostic boundary for launches: every backend's
		// cmd.Start() failure arrives here, so diagnosing ENOEXEC at this point
		// covers claude, opencode and any CLI added later without a wrap in
		// each backend (MUL-6164).
		err = agent.ExplainExecError(err)
		taskLog.Debug("backend execute returned error", "error", err)
		return agent.Result{}, 0, err
	}
	// This counter intentionally starts at the narrower provider-session
	// boundary, not at the earlier server-side StartTask transition.
	d.runningTasks.Add(1)
	defer d.runningTasks.Add(-1)
	phaseRecorder.Mark(taskPhaseRuntimeStarted)
	taskLog.Debug("backend started, draining messages")

	// Only negotiated sessions may claim additions. Stop and join delivery
	// before the caller reports the task's terminal state to the server.
	if opts.EnableTaskSupplement && (session.Supplement != nil || session.SupplementWithID != nil) && session.SupplementReady != nil {
		supplementCtx, cancelSupplements := context.WithCancel(agentCtx)
		supplementsDone := make(chan struct{})
		wakeup, unsubscribe := d.taskSupplementSignals.subscribe(taskID)
		go func() {
			defer unsubscribe()
			defer close(supplementsDone)
			d.runTaskSupplementLoop(supplementCtx, session, taskID, wakeup, taskLog)
		}()
		defer func() {
			cancelSupplements()
			<-supplementsDone
		}()
	}

	// Bound the drain loop only when there is a wall-clock cap. With a positive
	// opts.Timeout, give the drain a slightly longer deadline than the backend
	// so it can still collect the backend's own timeout Result if the scanner
	// is stuck on a hung stdout pipe (the extra 30 s covers cleanup after the
	// backend's own deadline fires). With no cap (opts.Timeout <= 0) the
	// inactivity watchdog is the only liveness net, so the drain must NOT
	// impose its own deadline either — otherwise an actively streaming long run
	// would be cut off here regardless of progress (MUL-3064).
	var drainCtx context.Context
	var drainCancel context.CancelFunc
	if opts.Timeout > 0 {
		drainCtx, drainCancel = context.WithTimeout(agentCtx, opts.Timeout+30*time.Second)
	} else {
		drainCtx, drainCancel = context.WithCancel(agentCtx)
	}
	defer drainCancel()

	var toolCount atomic.Int32
	// lastActivityAt records (as awakeclock nanos) when the drain loop most
	// recently received a message from the backend. The idle watchdog
	// reads this to decide whether the agent has gone silent for too long.
	// It is an awake-time reading, not wall time, so a host suspend is never
	// charged to the agent as silence (GitHub #9024).
	// Initialise to the start so a backend that never emits a single
	// message also trips the watchdog.
	var lastActivityAt atomic.Int64
	lastActivityAt.Store(int64(awakeclock.Now()))
	// inFlightTools counts tool_use messages that haven't yet been paired
	// with a matching tool_result. A non-zero count means the agent is
	// legitimately waiting on a tool (e.g. `npm install`, `docker build`)
	// that may run far longer than the idle window without emitting any
	// message — so while a tool is in flight the watchdog applies the larger
	// AgentToolWatchdog budget instead of treating that silence as a hang.
	var inFlightTools atomic.Int32
	var idleWatchdogFired atomic.Bool
	// idleWatchdogThreshold records (as nanos) which silence budget actually
	// tripped the watchdog — the idle window or the larger in-flight-tool
	// window — so the failure message reports the real duration.
	idleWindow := d.cfg.AgentIdleWatchdog
	// A provider may opt into a shorter per-run no-message budget. The global
	// zero remains authoritative so MULTICA_AGENT_IDLE_WATCHDOG=0 still disables
	// the entire watchdog suite. Tool calls continue to use AgentToolWatchdog.
	if idleWindow > 0 && opts.IdleWatchdogTimeout > 0 && opts.IdleWatchdogTimeout < idleWindow {
		idleWindow = opts.IdleWatchdogTimeout
	}
	var idleWatchdogThreshold atomic.Int64
	idleWatchdogThreshold.Store(int64(idleWindow))
	watchdogToolCount := inFlightTools.Load
	if session.ToolActivity != nil {
		watchdogToolCount = func() int32 {
			count, at := session.ToolActivity()
			for {
				previous := lastActivityAt.Load()
				if int64(at) <= previous || lastActivityAt.CompareAndSwap(previous, int64(at)) {
					break
				}
			}
			return count
		}
	}
	// A backend that can prove its outcome is already decided outranks every
	// liveness policy below: a run whose terminal result has been read is not a
	// hang, no matter how long its cleanup then takes.
	// Nil is meaningful and kept distinguishable: a backend that offers no
	// terminal boundary is one whose result cannot outrank a force stop, so it
	// must not be given a hand-off window it can never use. Every such backend
	// keeps the previous behaviour, including a wedged one, which is force
	// stopped and classified without waiting for anything.
	handsOverTerminal := session.TerminalObserved != nil
	terminalObserved := session.TerminalObserved
	if terminalObserved == nil {
		terminalObserved = func() bool { return false }
	}
	watchdogCtx, stopWatchdog := context.WithCancel(agentCtx)
	defer stopWatchdog()
	if idleWindow > 0 {
		go d.runIdleWatchdog(watchdogCtx, idleWindow, d.cfg.AgentToolWatchdog, &lastActivityAt, watchdogToolCount, &idleWatchdogFired, &idleWatchdogThreshold, agentCancel, session.Messages, session.InterruptBackgroundTools, terminalObserved, taskLog)
	}

	// drainFinished closes after the drain goroutine has flushed the last
	// message batch, so the result hand-off below can wait for the transcript
	// tail to be persisted.
	drainFinished := make(chan struct{})
	go func() {
		defer close(drainFinished)
		var mu sync.Mutex
		var pendingContent strings.Builder
		var pendingType string
		var pendingAt time.Time
		var batch []TaskMessageData
		callIDToTool := map[string]string{}
		// Provider IDs can restart on a same-task retry (for example item_0).
		// Allocate opaque transcript IDs per execution, including orphan results,
		// so neither a retry nor a missing call can steal another call's result.
		transcriptCallIDs := map[string]string{}
		transcriptCallID := func(providerID string) string {
			if providerID == "" {
				return ""
			}
			if id, ok := transcriptCallIDs[providerID]; ok {
				return id
			}
			id := uuid.NewString()
			transcriptCallIDs[providerID] = id
			return id
		}

		// sealPendingLocked turns the current contiguous text/thinking frame
		// into a sequenced row. Callers hold mu so a ticker flush cannot assign
		// a later seq between sealing the frame and appending the event that
		// followed it.
		sealPendingLocked := func() {
			if pendingContent.Len() == 0 {
				return
			}
			s := msgSeq.Add(1)
			batch = append(batch, TaskMessageData{
				Seq:       int(s),
				Type:      pendingType,
				Content:   pendingContent.String(),
				CreatedAt: pendingAt,
			})
			pendingContent.Reset()
			pendingType = ""
			pendingAt = time.Time{}
		}

		appendPending := func(messageType, content string, observedAt time.Time) {
			if content == "" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if pendingType != "" && pendingType != messageType {
				sealPendingLocked()
			}
			if pendingContent.Len() == 0 {
				pendingType = messageType
				pendingAt = observedAt
			}
			pendingContent.WriteString(content)
		}

		flush := func() {
			mu.Lock()
			sealPendingLocked()
			toSend := batch
			batch = nil
			mu.Unlock()

			if len(toSend) > 0 {
				sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := d.client.ReportTaskMessages(sendCtx, taskID, toSend); err != nil {
					taskLog.Debug("failed to report task messages", "error", err)
				} else {
					taskLog.Debug("reported task messages", "count", len(toSend), "last_seq", toSend[len(toSend)-1].Seq)
				}
				cancel()
			}
		}

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		done := make(chan struct{})
		tickerDone := make(chan struct{})
		firstVisible := make(chan struct{}, 1)
		go func() {
			defer close(tickerDone)
			for {
				select {
				case <-ticker.C:
					flush()
				case <-firstVisible:
					flush()
				case <-done:
					return
				}
			}
		}()
		// The periodic flush bounds request rate for the rest of the transcript,
		// but making the first visible event wait for its next 500 ms edge adds
		// pure presentation latency. Signal at most once per execution; a buffered
		// channel keeps the drain loop non-blocking while the reporter is busy.
		var firstVisibleOnce sync.Once
		flushFirstVisible := func() {
			firstVisibleOnce.Do(func() {
				firstVisible <- struct{}{}
			})
		}

		var sessionPinned atomic.Bool
		for {
			select {
			case msg, ok := <-session.Messages:
				if !ok {
					goto drainDone
				}
				if isTaskOutputReceived(msg) {
					phaseRecorder.Mark(taskPhaseFirstOutputReceived)
				}
				if isTaskToolUse(msg) {
					phaseRecorder.Mark(taskPhaseFirstToolUse)
				}
				// Stamp activity as soon as a message lands. The idle
				// watchdog reads this to decide whether the backend has
				// gone silent — stamping before processing makes sure a
				// slow downstream call (mu.Lock contention, batch resize)
				// can't be misattributed to backend silence.
				observedAt := time.Now().UTC()
				lastActivityAt.Store(int64(awakeclock.Now()))
				switch msg.Type {
				case agent.MessageStatus:
					// Native emits the session ID after the durable checkpoint
					// exists. Pin it once for crash recovery.

					if msg.SessionID != "" && !sessionPinned.Swap(true) {
						sid := msg.SessionID
						wd := opts.Cwd
						go func() {
							pinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							if err := d.client.PinTaskSession(pinCtx, taskID, sid, wd); err != nil {
								taskLog.Debug("pin session failed", "error", err)
							}
						}()
					}
				case agent.MessageToolUse:
					n := toolCount.Add(1)
					inFlightTools.Add(1)
					taskLog.Info(fmt.Sprintf("tool #%d: %s", n, msg.Tool))
					mu.Lock()
					sealPendingLocked()
					if msg.CallID != "" {
						callIDToTool[msg.CallID] = msg.Tool
					}
					s := msgSeq.Add(1)
					batch = append(batch, TaskMessageData{
						Seq:       int(s),
						Type:      "tool_use",
						CallID:    transcriptCallID(msg.CallID),
						Tool:      msg.Tool,
						CreatedAt: observedAt,
						// Redact before the payload leaves this process, not
						// only on arrival. The server redacts again in its
						// ingest handler, but that is the *remote* side: a
						// daemon that self-updated ahead of the server — or one
						// talking to a server mid-rollout — would otherwise ship
						// whole-file edit contents (a deleted .env, a patched
						// credential) to a peer that does not scrub nested
						// values yet. Deployment order is not a control we
						// have, so this side has to be safe on its own.
						Input: redact.InputMap(msg.Input),
					})
					mu.Unlock()
					flushFirstVisible()
				case agent.MessageToolResult:
					// Decrement only when the count would stay >= 0. A stray
					// tool_result with no matching tool_use (backend bug or
					// reconnect mid-stream) shouldn't push the counter
					// negative — that would re-arm the watchdog one tool_use
					// too early on the next call.
					for {
						cur := inFlightTools.Load()
						if cur <= 0 {
							break
						}
						if inFlightTools.CompareAndSwap(cur, cur-1) {
							break
						}
					}
					output, outputTruncated := toolOutputPreview(msg.Output)
					mu.Lock()
					sealPendingLocked()
					toolName := msg.Tool
					if toolName == "" && msg.CallID != "" {
						toolName = callIDToTool[msg.CallID]
					}
					s := msgSeq.Add(1)
					taskLog.Info("tool_result observed", "seq", s, "tool", toolName, "call_id", msg.CallID)
					batch = append(batch, TaskMessageData{
						Seq:       int(s),
						Type:      "tool_result",
						CallID:    transcriptCallID(msg.CallID),
						Tool:      toolName,
						Output:    output,
						CreatedAt: observedAt,
						// Always sent, including false: the reader has to be
						// able to tell "this record is complete" from "this
						// record predates the flag", and only a daemon that
						// measured the output can say the former.
						OutputTruncated: &outputTruncated,
					})
					mu.Unlock()
					flushFirstVisible()
				case agent.MessageThinking:
					appendPending("thinking", msg.Content, observedAt)
					if msg.Content != "" {
						flushFirstVisible()
					}
				case agent.MessageText:
					if msg.Content != "" {
						taskLog.Debug("agent", "text", truncateLog(msg.Content, 200))
					}
					appendPending("text", msg.Content, observedAt)
					if msg.Content != "" {
						flushFirstVisible()
					}
				case agent.MessageError:
					taskLog.Error("agent error", "content", msg.Content)
					mu.Lock()
					sealPendingLocked()
					s := msgSeq.Add(1)
					batch = append(batch, TaskMessageData{
						Seq:       int(s),
						Type:      "error",
						Content:   msg.Content,
						CreatedAt: observedAt,
					})
					mu.Unlock()
					flushFirstVisible()
				}
			case <-drainCtx.Done():
				goto drainDone
			}
		}
	drainDone:
		close(done)
		// Let any tick-driven flush finish before the final one: a flush still
		// in flight would otherwise keep posting batches after this goroutine
		// signalled that the transcript tail was persisted.
		<-tickerDone
		flush()
	}()

	// waitForDrain blocks until the drain goroutine has flushed the transcript
	// tail, so every terminal return below hands control back only after the
	// task's reported messages are persisted — a consumer reading them at the
	// terminal transition would otherwise see a transcript that is non-empty
	// but truncated, indistinguishable from a complete one. Bounded so a
	// backend that never closes its message channel cannot stall the terminal
	// transition: after 10s the drain loop is cancelled and given a window
	// wide enough for its worst-case exit — an in-flight tick flush plus the
	// final one, each capped by the 5s ReportTaskMessages timeout and neither
	// interruptible by the cancel (they post on context.Background()).
	waitForDrain := func() {
		select {
		case <-drainFinished:
		case <-time.After(10 * time.Second):
			drainCancel()
			select {
			case <-drainFinished:
			case <-time.After(12 * time.Second):
				taskLog.Warn("transcript drain did not stop after cancel; completing anyway")
			}
		}
	}
	// awaitTerminalResult gives a backend that advertises an authoritative
	// terminal boundary one bounded chance to hand over its result after a
	// cancellation won the outer select. Result delivery is the linearization
	// point: TerminalObserved must be published before that send, so checking it
	// afterwards preserves a provider outcome without racing a flag read. A
	// delivered non-authoritative result is still returned to the idle-watchdog
	// caller for re-tagging; ordinary upstream cancellation deliberately ignores
	// it and keeps the existing generic cancelled disposition.
	awaitTerminalResult := func(trigger string) (result agent.Result, delivered, authoritative bool) {
		if !handsOverTerminal {
			return agent.Result{}, false, false
		}
		if trigger == "idle_watchdog" {
			// Keep this event stable: besides operator diagnostics, the terminal
			// race regression uses it as the hand-off linearization probe.
			taskLog.Info("idle watchdog fired; waiting for the backend to hand over its result",
				"budget", terminalResultHandoffBudget.String())
		} else {
			taskLog.Info("waiting for the backend to hand over its result after cancellation",
				"trigger", trigger,
				"budget", terminalResultHandoffBudget.String())
		}
		timer := time.NewTimer(terminalResultHandoffBudget)
		defer timer.Stop()
		select {
		case result, ok := <-session.Result:
			if !ok {
				return agent.Result{}, false, false
			}
			return result, true, terminalObserved()
		case <-timer.C:
			if trigger == "idle_watchdog" {
				taskLog.Warn("backend did not hand over a result within the budget; classifying by liveness",
					"budget", terminalResultHandoffBudget.String())
			} else {
				taskLog.Warn("backend did not hand over a result within the budget; classifying by cancellation trigger",
					"trigger", trigger,
					"budget", terminalResultHandoffBudget.String())
			}
			return agent.Result{}, false, false
		}
	}

	select {
	case result := <-session.Result:
		stopWatchdog()
		waitForDrain()
		// terminalObserved outranks a watchdog that fired anyway: if the backend
		// had already read its authoritative result, this is the real outcome and
		// re-tagging it would report a completed run as a hang.
		if idleWatchdogFired.Load() && !terminalObserved() {
			// The backend translates the
			// SIGKILL we delivered via agentCancel into Status="aborted".
			// Re-tag it as "idle_watchdog" so runTask routes the
			// disposition through a dedicated failure_reason, not the
			// generic "agent_error" bucket the aborted path falls into.
			result.Status = "idle_watchdog"
			if result.Error == "" {
				result.Error = idleWatchdogReason(time.Duration(idleWatchdogThreshold.Load()))
			}
		}
		return result, toolCount.Load(), nil
	case <-drainCtx.Done():
		// The drain loop is exiting on this same Done signal; wait for its
		// final flush so the timeout/watchdog/cancel terminals below cannot
		// hand back (and let runTask fail-and-broadcast) a still-flushing
		// transcript either.
		waitForDrain()
		// Idle watchdog cancels via agentCancel(), which propagates here as
		// context.Canceled. Check this BEFORE the generic cancelled/timeout
		// classifiers so a watchdog-induced stop isn't misreported as
		// "task cancelled by server".
		if idleWatchdogFired.Load() {
			// For a backend that publishes a terminal boundary, enter the
			// hand-off without asking terminalObserved first. Reading a flag and
			// then acting on it is exactly the window this branch kept losing:
			// the backend can publish between the read and the classifier below.
			// Waiting for the result instead makes its delivery the
			// linearization point, and the backend contract — publish the
			// observation before sending Result — is what makes the check after
			// delivery reliable rather than lucky.
			//
			// Such a backend always closes Result, so a wedged one still ends
			// this wait promptly through the closed channel rather than the
			// budget.
			if result, delivered, authoritative := awaitTerminalResult("idle_watchdog"); authoritative {
				// The backend had already read its authoritative result, so
				// this is the real outcome, not a hang.
				return result, toolCount.Load(), nil
			} else if delivered {
				// The backend translates the
				// SIGKILL we delivered via agentCancel into Status="aborted".
				// Re-tag it so runTask routes the disposition through the
				// dedicated liveness failure_reason.
				result.Status = "idle_watchdog"
				if result.Error == "" {
					result.Error = idleWatchdogReason(time.Duration(idleWatchdogThreshold.Load()))
				}
				return result, toolCount.Load(), nil
			}
			return agent.Result{
				Status: "idle_watchdog",
				Error:  idleWatchdogReason(time.Duration(idleWatchdogThreshold.Load())),
			}, toolCount.Load(), nil
		}
		// Distinguish external cancellation (e.g. server-initiated cancel
		// because the issue was reassigned, or the user invoked CancelTask)
		// from genuine drain-deadline timeouts. context.Canceled means the
		// upstream runCtx fired runCancel(); context.DeadlineExceeded is the
		// drain deadline expiring on its own.
		if errors.Is(drainCtx.Err(), context.Canceled) {
			if result, _, authoritative := awaitTerminalResult("upstream_context"); authoritative {
				return result, toolCount.Load(), nil
			}
			return agent.Result{
				Status: "cancelled",
				Error:  "task cancelled by upstream context (server cancel or daemon shutdown)",
			}, toolCount.Load(), nil
		}
		return agent.Result{
			Status: "timeout",
			Error:  "agent did not produce result within drain timeout",
		}, toolCount.Load(), nil
	}
}

// terminalResultHandoffBudget is how long executeAndDrain waits, after force-
// stopping a run, for the backend to hand over whatever result it has. It only
// decides how long we believe a backend before falling back to the liveness
// verdict; the backend caps its own finalization, so in practice the wait ends
// far sooner.
//
// The value is derived from the slowest finalization this daemon drives today,
// Cursor's, rather than picked: a concurrent background-cleanup pass we may have
// to wait behind (cursorCloseBudget, 10s), the closing pass itself (another
// 10s), one already-started process termination per pass overshooting its
// budget by that termination's own bound (~1s each), and the process WaitDelay
// after cancellation (0.5s) — about 22.5s. 30s leaves margin without letting a
// wedged backend hold a runtime slot indefinitely.
//
// Deliberately not a term: the background reaper's tick. Closing its stop
// channel wakes it immediately rather than at the next tick, so it adds
// nothing to this ceiling.
const terminalResultHandoffBudget = 30 * time.Second

// idleWatchdogReason formats the human-facing explanation surfaced on
// idle_watchdog dispositions. Centralised so the result-arrival branch and the
// drain-timeout branch in executeAndDrain emit identical wording.
func idleWatchdogReason(window time.Duration) string {
	return fmt.Sprintf("agent produced no new messages for %s and message queue was empty; force-stopped by idle watchdog", window)
}

// idleWatchdogTickInterval picks how often the idle watchdog re-checks the
// silence budget. Half the window is the base rate, capped at
// idleWatchdogMaxTick so a run is force-stopped within window + tick rather
// than window * 1.5. Sub-nanosecond halves fall back to the window itself so
// tests can pass tiny budgets and still get a valid ticker.
//
// There used to be a `window >= time.Minute && interval < 30*time.Second` floor
// here, meant to keep production polling no faster than 30 s. It was
// unreachable — window >= 1 min implies window/2 >= 30 s — and its only effect
// was to make the tests around it read as if a floor were being exercised.
// Production windows are minutes or hours, so window/2 already clears 30 s.
func idleWatchdogTickInterval(window time.Duration) time.Duration {
	interval := window / 2
	if interval > idleWatchdogMaxTick {
		interval = idleWatchdogMaxTick
	}
	if interval <= 0 {
		interval = window
	}
	return interval
}

// runIdleWatchdog ticks until either agentCtx is cancelled or the backend has
// been silent past the applicable budget. On firing, it records the tripped
// threshold, sets fired, and calls cancel, which propagates to the agent
// subprocess (via the ctx passed to backend.Execute) and to drainCtx. The
// silence budget depends on whether a tool call is in flight:
//
//  1. No tool in flight — a silent backend is a hang after `window`.
//  2. A tool in flight (tool_use with no matching tool_result yet) —
//     `toolWindow` applies instead. It defaults to `window`, so the two are
//     normally identical and this branch only changes which duration the
//     failure message reports; an operator who deliberately sets
//     MULTICA_AGENT_TOOL_WATCHDOG higher buys long tools extra room, and
//     toolWindow <= 0 keeps the historical behavior of never force-stopping
//     while a tool is in flight. Without this in-flight budget a backend that
//     emits tool_use and never the matching tool_result would run forever now
//     that there is no wall-clock cap (MUL-3064).
//
// In both cases the watchdog also requires the session.Messages buffer to be
// empty — a buffered-but-undrained message means the drain loop is behind, not
// the backend.
//
// Polling rate comes from idleWatchdogTickInterval, so a run is force-stopped
// somewhere between its budget and budget + tick, never earlier.
//
// Budgets are measured on awakeclock, so they count only time the host was
// awake. lastActivityAt and every native tool timestamp are awakeclock
// readings. A laptop that sleeps through a run resumes with its silence
// exactly where it was before the lid closed (GitHub #9024).
func (d *Daemon) runIdleWatchdog(agentCtx context.Context, window, toolWindow time.Duration, lastActivityAt *atomic.Int64, inFlightTools func() int32, fired *atomic.Bool, firedThreshold *atomic.Int64, cancel context.CancelFunc, messages <-chan agent.Message, interruptBackground func() bool, terminalObserved func() bool, taskLog *slog.Logger) {
	d.runIdleWatchdogWithClock(agentCtx, window, toolWindow, lastActivityAt, inFlightTools, fired, firedThreshold, cancel, messages, interruptBackground, terminalObserved, taskLog, awakeclock.Now)
}

// runIdleWatchdogWithClock is runIdleWatchdog with the awake clock injected,
// so tests can simulate a host suspend: wall time that passes while the
// awake reading stands still.
func (d *Daemon) runIdleWatchdogWithClock(agentCtx context.Context, window, toolWindow time.Duration, lastActivityAt *atomic.Int64, inFlightTools func() int32, fired *atomic.Bool, firedThreshold *atomic.Int64, cancel context.CancelFunc, messages <-chan agent.Message, interruptBackground func() bool, terminalObserved func() bool, taskLog *slog.Logger, awakeNow func() time.Duration) {
	tickWindow := window
	if toolWindow > 0 && toolWindow < tickWindow {
		tickWindow = toolWindow
	}
	ticker := time.NewTicker(idleWatchdogTickInterval(tickWindow))
	defer ticker.Stop()
	for {
		select {
		case <-agentCtx.Done():
			return
		case <-ticker.C:
			// Pick the silence budget. A tool in flight is expected to be
			// silent (a long build/install/test emits nothing between
			// tool_use and tool_result), so it gets the larger toolWindow;
			// toolWindow <= 0 disables the in-flight bound entirely.
			threshold := window
			toolInFlight := inFlightTools() > 0
			if toolInFlight {
				if toolWindow <= 0 {
					continue
				}
				threshold = toolWindow
			}
			last := lastActivityAt.Load()
			idleFor := awakeNow() - time.Duration(last)
			if idleFor < threshold {
				continue
			}
			// A buffered-but-undrained message means the drain loop is
			// behind, not the backend. Wait one more tick rather than
			// killing a backend that is still producing output.
			if len(messages) > 0 {
				continue
			}
			// Cursor can stop its owned background tools without aborting the
			// agent. The same watchdog owns both the budget and this recovery,
			// so no competing timer can cancel Cursor while it reports a result.
			if agentCtx.Err() != nil {
				return
			}
			if terminalObserved != nil && terminalObserved() {
				return
			}
			if toolInFlight && interruptBackground != nil && interruptBackground() {
				lastActivityAt.Store(int64(awakeNow()))
				taskLog.Info("tool watchdog stopped background tools; waiting for agent result")
				continue
			}
			// A natural tool completion may race the callback or the tick.
			if agentCtx.Err() != nil {
				return
			}
			// Refresh native tool activity BEFORE reading its timestamp. The
			// callback may publish newer activity without changing the count.
			currentToolInFlight := inFlightTools() > 0
			currentActivity := lastActivityAt.Load()
			if currentActivity != last ||
				currentToolInFlight != toolInFlight || len(messages) > 0 {
				continue
			}
			if agentCtx.Err() != nil {
				return
			}
			// Last gate before force-stopping. The terminal result can land while
			// this tick is deciding — including while it is blocked inside the
			// interrupt callback above — and a decided outcome is never a hang.
			if terminalObserved != nil && terminalObserved() {
				return
			}
			// No "task" field here: taskLog already carries the full id.
			taskLog.Warn("idle watchdog firing: no agent activity, force-stopping run",
				"idle_for", idleFor.Round(time.Second).String(),
				"threshold", threshold.String(),
				"tool_in_flight", toolInFlight,
			)
			firedThreshold.Store(int64(threshold))
			fired.Store(true)
			cancel()
			return
		}
	}
}

// repoDataToInfo converts daemon RepoData to repocache RepoInfo.
func repoDataToInfo(repos []RepoData) []repocache.RepoInfo {
	info := make([]repocache.RepoInfo, len(repos))
	for i, r := range repos {
		info[i] = repocache.RepoInfo{URL: r.URL}
	}
	return info
}

func convertIssueStatusesForEnv(statuses []IssueStatusData) []execenv.IssueStatusForEnv {
	if len(statuses) == 0 {
		return nil
	}
	result := make([]execenv.IssueStatusForEnv, len(statuses))
	for i, s := range statuses {
		result[i] = execenv.IssueStatusForEnv{Key: s.Key, Name: s.Name, Category: s.Category, Description: s.Description}
	}
	return result
}

func convertReposForEnv(repos []RepoData) []execenv.RepoContextForEnv {
	if len(repos) == 0 {
		return nil
	}
	result := make([]execenv.RepoContextForEnv, len(repos))
	for i, r := range repos {
		result[i] = execenv.RepoContextForEnv{URL: r.URL, Description: r.Description, Ref: r.Ref}
	}
	return result
}

func convertProjectResourcesForEnv(resources []ProjectResourceData) []execenv.ProjectResourceForEnv {
	if len(resources) == 0 {
		return nil
	}
	result := make([]execenv.ProjectResourceForEnv, len(resources))
	for i, r := range resources {
		result[i] = execenv.ProjectResourceForEnv{
			ID:           r.ID,
			ResourceType: r.ResourceType,
			ResourceRef:  r.ResourceRef,
			Label:        r.Label,
		}
	}
	return result
}

// markActiveEnvRoot records that a task is currently using the given env root,
// so the GC loop won't reclaim its artifacts mid-execution. Calls are
// reference-counted so a reuse path marked twice (predicted + prior) only
// becomes inactive after both unmark calls.
func (d *Daemon) markActiveEnvRoot(envRoot string) {
	if envRoot == "" {
		return
	}
	d.activeEnvRootsMu.Lock()
	defer d.activeEnvRootsMu.Unlock()
	d.ensureActiveEnvRootStateLocked()
	for d.deletingEnvRoots[envRoot] {
		d.activeEnvRootsCond.Wait()
	}
	d.activeEnvRoots[envRoot]++
}

func (d *Daemon) unmarkActiveEnvRoot(envRoot string) {
	if envRoot == "" {
		return
	}
	d.activeEnvRootsMu.Lock()
	defer d.activeEnvRootsMu.Unlock()
	d.ensureActiveEnvRootStateLocked()
	if d.activeEnvRoots[envRoot] <= 1 {
		delete(d.activeEnvRoots, envRoot)
		return
	}
	d.activeEnvRoots[envRoot]--
}

func (d *Daemon) isActiveEnvRoot(envRoot string) bool {
	d.activeEnvRootsMu.Lock()
	defer d.activeEnvRootsMu.Unlock()
	d.ensureActiveEnvRootStateLocked()
	return d.activeEnvRoots[envRoot] > 0
}

func (d *Daemon) ensureActiveEnvRootStateLocked() {
	if d.activeEnvRoots == nil {
		d.activeEnvRoots = make(map[string]int)
	}
	if d.deletingEnvRoots == nil {
		d.deletingEnvRoots = make(map[string]bool)
	}
	if d.activeEnvRootsCond == nil {
		d.activeEnvRootsCond = sync.NewCond(&d.activeEnvRootsMu)
	}
}

// reserveEnvRootForGC atomically confirms that no live task is using envRoot
// and prevents a new task from entering until release runs. This closes the
// check-then-remove race between the GC loop and task startup: either GC sees
// the active task and skips, or task startup waits for the mutation to finish
// and recreates/uses the post-GC environment.
func (d *Daemon) reserveEnvRootForGC(envRoot string) (release func(), ok bool) {
	if envRoot == "" {
		return nil, false
	}
	d.activeEnvRootsMu.Lock()
	defer d.activeEnvRootsMu.Unlock()
	d.ensureActiveEnvRootStateLocked()
	if d.activeEnvRoots[envRoot] > 0 || d.deletingEnvRoots[envRoot] {
		return nil, false
	}
	d.deletingEnvRoots[envRoot] = true
	return func() {
		d.activeEnvRootsMu.Lock()
		delete(d.deletingEnvRoots, envRoot)
		d.activeEnvRootsCond.Broadcast()
		d.activeEnvRootsMu.Unlock()
	}, true
}

// shortID returns the first 8 characters of an ID for a human-facing label.
//
// Display only, and only where the label is read rather than joined: the
// waiting-reason string the UI renders as "Waiting for {reason}" is the one
// caller left. Structured log fields must carry the FULL id — see the
// task-scoped logger in handleTask for why a prefix cannot identify a task.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// truncateLog truncates a string to maxLen, appending "…" if truncated.
// Also collapses newlines to spaces for single-line log output.
func truncateLog(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

func convertSkillsForEnv(skills []SkillData) []execenv.SkillContextForEnv {
	if len(skills) == 0 {
		return nil
	}
	result := make([]execenv.SkillContextForEnv, len(skills))
	for i, s := range skills {
		result[i] = execenv.SkillContextForEnv{
			Name:        s.Name,
			Description: s.Description,
			Content:     s.Content,
		}
		for _, f := range s.Files {
			result[i].Files = append(result[i].Files, execenv.SkillFileContextForEnv{
				Path:    f.Path,
				Content: f.Content,
			})
		}
	}
	return result
}

func convertDisabledRuntimeSkillsForEnv(agentData *AgentData, runtimeID, provider string) []execenv.RuntimeSkillRefForEnv {
	if agentData == nil || len(agentData.DisabledRuntimeSkills) == 0 {
		return nil
	}
	result := make([]execenv.RuntimeSkillRefForEnv, 0, len(agentData.DisabledRuntimeSkills))
	for _, skill := range agentData.DisabledRuntimeSkills {
		if skill.RuntimeID != runtimeID || skill.Provider != provider {
			continue
		}
		result = append(result, execenv.RuntimeSkillRefForEnv{
			Root:   skill.Root,
			Key:    skill.Key,
			Name:   skill.Name,
			Plugin: skill.Plugin,
		})
	}
	return result
}

// ensureTaskTempDir creates this task's private temp directory and returns it
// with its execution lock held. The caller owns the lock for the lifetime of
// the run and must release it before removing the directory — see the cleanup
// defer in runTask, and execenv.PruneTaskTempDirs for what the lock buys.
func ensureTaskTempDir(envRoot string, workspaceID string, taskID string) (string, *os.File, error) {
	envRoot = strings.TrimSpace(envRoot)
	if envRoot == "" {
		return "", nil, errors.New("env root is empty")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return "", nil, errors.New("workspace id is empty")
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", nil, errors.New("task id is empty")
	}
	base, overrideConfigured, err := taskTempBaseDir()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, execenv.TaskTempDirPrefix)
	if err != nil {
		if overrideConfigured {
			return "", nil, fmt.Errorf("MULTICA_AGENT_TEMP_BASE: create task temp dir: %w", err)
		}
		return "", nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	lock, err := execenv.LockTaskTempDir(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, lock, nil
}

// taskTempBaseDir resolves the parent directory for private per-task temp
// dirs on Linux and macOS. The daemon operator can relocate it with
// MULTICA_AGENT_TEMP_BASE, which must be an absolute path to an existing,
// writable directory; an invalid value fails task startup instead of silently
// falling back. Windows ignores the variable. Unset keeps the platform default
// exactly as before, down to the syscalls made.
// Operators should pick a short path: child tools may bind AF_UNIX sockets
// under $TMPDIR (sun_path is 108 bytes on Linux, 104 on macOS).
func taskTempBaseDir() (string, bool, error) {
	if runtime.GOOS == "windows" {
		return socketSafeTempBaseDir(), false, nil
	}
	base := strings.TrimSpace(os.Getenv("MULTICA_AGENT_TEMP_BASE"))
	if base == "" {
		return socketSafeTempBaseDir(), false, nil
	}
	if !filepath.IsAbs(base) {
		return "", true, fmt.Errorf("MULTICA_AGENT_TEMP_BASE must be an absolute path, got %q", base)
	}
	return base, true, nil
}

func socketSafeTempBaseDir() string {
	if os.PathSeparator == '/' {
		if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
			return "/tmp"
		}
	}
	return os.TempDir()
}

// isBlockedEnvKey returns true if the key must not be overridden by user-
// configured custom_env. This prevents accidental or malicious override of
// daemon-internal variables and critical system paths.
func isBlockedEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "MULTICA_") {
		return true
	}
	switch upper {
	case "HOME", "PATH", "USER", "SHELL", "TERM", "TMPDIR", "TMP", "TEMP", "CODEX_HOME", "REASONIX_STATE_HOME", "CURSOR_DATA_DIR", "OPENCLAW_CONFIG_PATH", "OPENCLAW_INCLUDE_ROOTS":
		return true
	}
	return false
}

// layerCustomEnvAndHermesHome applies the agent's custom_env onto the child env
// (skipping daemon-internal blocklisted keys), then overrides HERMES_HOME with
// the per-task overlay when one was built. HERMES_HOME is intentionally NOT
// blocklisted: with skills bound the overlay is built FROM the user's
// HERMES_HOME and must win here; with no skills bound (overlayHome empty) the
// user's own HERMES_HOME passes through unchanged, so a skill-less Hermes task
// keeps its original behavior.
// sanitizeAgentEnv returns the agent custom_env with daemon-blocklisted keys
// removed — the effective env the Hermes child actually sees, used to expand
// ${VAR} in external_dirs consistently (blocked keys like HOME resolve to the
// daemon process value, not the dropped custom one). Uses the same
// isBlockedEnvKey rule as layerCustomEnvAndHermesHome so the two agree.
func sanitizeAgentEnv(customEnv map[string]string) map[string]string {
	if len(customEnv) == 0 {
		return nil
	}
	out := make(map[string]string, len(customEnv))
	for k, v := range customEnv {
		if isBlockedEnvKey(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// hermesProviderUnconfiguredHint is appended verbatim to a "no LLM provider
// configured" failure. It is a CONSTANT, and that is a correctness property,
