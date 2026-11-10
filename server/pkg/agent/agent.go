// Package agent provides a unified interface for executing prompts via
// coding agents (Claude Code, CodeBuddy, Codex, Copilot, OpenCode, DevEco Code,
// OpenClaw, Hermes, Pi, Oh-My-Pi, Cursor, Kimi, Reasonix, Kiro, Antigravity, Qoder,
// Trae, Grok, Qwen Code, QwenPaw, MiniMax Code). It
// mirrors the happy-cli AgentBackend pattern, translated to idiomatic Go.
package agent

import (
	"context"
	"encoding/json"

	"time"
)

// Backend is the unified interface for executing prompts via coding agents.
type Backend interface {
	// Execute runs a prompt and returns a Session for streaming results.
	// The caller should read from Session.Messages (optional) and wait on
	// Session.Result for the final outcome.
	Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
}

// ExecOptions configures a single execution.
type ExecOptions struct {
	// EnableTaskSupplement installs provider hooks only for runs whose daemon/server
	// capability handshake enabled additional messages.
	EnableTaskSupplement bool
	Cwd                  string
	Model                string
	// SystemPrompt carries the Multica runtime brief for the few providers
	// that cannot pick it up from disk. The daemon leaves it empty for every
	// other provider (see daemon.providerNeedsInlineSystemPrompt), because the
	// brief is already delivered as a per-task context file in the workdir —
	// CLAUDE.md, AGENTS.md, CODEBUDDY.md or QWEN.md depending on the runtime.
	//
	// A backend must therefore NOT assume this is populated, and adding a new
	// backend that only reads SystemPrompt will silently receive nothing.
	SystemPrompt              string
	ThreadName                string
	MaxTurns                  int
	Timeout                   time.Duration
	SemanticInactivityTimeout time.Duration
	// FirstTurnNoProgressTimeout optionally overrides the Codex first-turn
	// no-progress ceiling — the window a turn may stay completely silent after
	// the app-server reports turn/started before the watchdog fails it. Zero
	// keeps the provider default and the existing behaviour where
	// SemanticInactivityTimeout can only shrink that ceiling; a positive value
	// sets it explicitly, upward included. This answers a different question than
	// SemanticInactivityTimeout ("did the process ever start producing?" vs "has
	// a running turn gone quiet?"), so the two move independently. Currently
	// honoured by the codex backend (GH #3262).
	FirstTurnNoProgressTimeout time.Duration
	// IdleWatchdogTimeout optionally narrows the daemon's generic no-message
	// watchdog for this execution. Zero keeps the daemon-wide window, and a
	// value above that window cannot extend the global safety bound. The
	// daemon-wide zero still disables the watchdog entirely, and an in-flight
	// tool continues to use the separate tool watchdog budget.
	IdleWatchdogTimeout time.Duration
	// HandshakeTimeout bounds startup RPCs for providers with a long-lived
	// protocol transport, including Codex app-server and Claude SDK hooks;
	// zero uses the provider default rather than disabling the bound.
	HandshakeTimeout time.Duration
	// TurnInterruptTimeout bounds how long the Codex backend waits for the
	// app-server to acknowledge turn/interrupt and emit turn/completed after a
	// task is cancelled. Zero uses the provider default. A positive override is
	// useful on slower hosts without coupling cancellation cleanup to the much
	// longer execution or handshake budgets.
	TurnInterruptTimeout time.Duration
	// ThreadHandshakeTimeout optionally gives Codex's heavier thread/start and
	// thread/resume RPCs a wider budget than initialize and turn/start. Zero
	// preserves the legacy behavior for callers that explicitly set
	// HandshakeTimeout; when both are zero Codex uses separate built-in defaults.
	ThreadHandshakeTimeout time.Duration
	ResumeSessionID        string // if non-empty, resume a previous agent session
	// ResumeExpected records that this task intended to continue a prior
	// conversation, independent of ResumeSessionID (which a fallback retry may
	// clear). When it is true but the backend ends up on a fresh thread — the
	// live resume RPC was rejected, or a transport failure forced a fresh retry —
	// the backend surfaces a continuity notice instead of silently
	// restarting. Currently honoured by the codex backend (MUL-4424).
	ResumeExpected bool
	// ResumeContinuityNotice is the text to prepend to the first turn when
	// ResumeExpected holds but the backend lands on a fresh thread anyway. The
	// caller owns the wording because only it knows what the surface lost — an
	// issue's comments and a Slack channel's history survive and can be re-read,
	// a web chat's and a Feishu channel's cannot — and that difference decides
	// whether the agent should tell the user anything at all (MUL-5722).
	//
	// Empty means say nothing, and the caller MUST leave it empty when its own
	// prompt already carries the notice. That is what keeps a turn from paying
	// for the same paragraph twice: the daemon injects it whenever it already
	// knows the resume is gone, and the backend covers only the case the daemon
	// cannot see — a live resume RPC rejected mid-run.
	ResumeContinuityNotice string
	// ExtraArgs is honoured only by backends that opt in by reading it; the
	// rest ignore it. Deliberately not enumerated here — the previous list
	// went stale as backends were added, which is how MULTICA_QWENPAW_ARGS
	// shipped plumbed but dropped. Grep for ExtraArgs to see today's set.
	ExtraArgs        []string        // daemon-wide default CLI arguments appended before CustomArgs
	CustomArgs       []string        // per-agent CLI arguments appended after ExtraArgs
	QwenpawWorkspace string          // per-task QwenPaw workspace directory (passed as --workspace to qwenpaw acp); empty when not applicable
	McpConfig        json.RawMessage // if non-nil, MCP server config to pass via --mcp-config
	// ThinkingLevel is the runtime-native reasoning/effort value (e.g.
	// Claude's "low|medium|high|xhigh|max", Codex's "none|minimal|low|
	// medium|high|xhigh", OpenCode's model variant names). Empty means
	// "use the runtime/model default" —
	// every backend that consumes this skips its --effort / reasoning_effort
	// injection so the upstream CLI's own default applies. Currently honoured
	// by the claude, codex, opencode, codebuddy, dsh, and grok (ACP
	// `--effort` on `grok agent`) backends; other backends ignore
	// the field rather than fail (so MUL-2339 can grow runtime support
	// incrementally without breaking unrelated agents).
	ThinkingLevel string
	// ServiceTier is a runtime-native Codex execution tier (for example
	// "priority", displayed as Fast). "default" explicitly selects standard
	// routing; empty means inherit local Codex config.
	// Other providers ignore this field.
	ServiceTier string
	// OpenclawMode chooses between local (embedded) and gateway routing for
	// the openclaw backend. "" or "local" keeps the historical behaviour —
	// the daemon spawns `openclaw agent --local …` and the agent loop runs
	// in-process on the daemon host. "gateway" instructs the daemon to drop
	// the --local flag and let openclaw route the turn through a Gateway (the
	// user's globally-configured one, or an endpoint pinned in the per-task
	// config wrapper that the daemon writes from execenv.OpenclawGatewayPin —
	// see server/internal/daemon/execenv/openclaw_config.go). Other backends
	// ignore this field, mirroring ThinkingLevel's renderer-side fall-through
	// pattern. See issue #3260.
	OpenclawMode string
	// ClaudeSettingsPath is a daemon-owned, task-local settings file passed
	// through Claude Code's --settings flag. It currently carries restrictive
	// runtime-skill overrides only; other providers ignore it.
	ClaudeSettingsPath string
	// piMcpExtensionPath is set by piBackend.Execute after it writes the
	// temporary MCP extension. The daemon does not fill it.
	piMcpExtensionPath string
}

// Session represents a running agent execution.
type Session struct {
	// Supplement delivers an additional human instruction to the currently
	// active provider turn. Nil means the backend cannot safely do so.
	// Adapters bound their own transport calls; a hook-based adapter may wait
	// for the next provider boundary until ctx or the execution is cancelled.
	Supplement func(context.Context, string) error
	// SupplementWithID durably accepts a server-owned input identity. Native
	// sessions use it to deduplicate redelivery after an acknowledgement loss.
	SupplementWithID func(context.Context, string, string) error
	// SupplementReady reports whether Supplement currently targets a live,
	// provider-confirmed turn. The daemon must not claim durable input before
	// this becomes true: claiming during provider initialization would convert
	// a transient startup window into a user-visible delivery failure. Nil is
	// fail-closed and means no run-scoped input may be claimed.
	SupplementReady func() bool
	// ToolActivity optionally reports backend-owned tool accounting and its last
	// transition time, independent of the best-effort transcript. Nil uses the
	// daemon's message-based accounting. The timestamp gives completed tools a
	// fresh idle budget even when their transcript message has not drained yet.
	// The timestamp is an awakeclock reading (see package awakeclock), the same
	// clock the idle watchdog measures, so host sleep never counts as silence.
	ToolActivity func() (int32, time.Duration)
	// InterruptBackgroundTools stops owned background tools at the daemon's
	// tool watchdog boundary without cancelling the agent. True means at least
	// one tool completed/was stopped and released from accounting; the watchdog
	// gives the agent a fresh budget to report its authoritative result.
	// Implementations must be concurrency-safe and return false after cleanup.
	//
	// This is reached only at the tool budget, so a zero tool budget never calls
	// it: work that is genuinely in flight is what that setting declines to
	// force-stop. The daemon's AgentToolWatchdog documentation states the
	// operator-facing consequence.
	InterruptBackgroundTools func() bool
	// TerminalObserved reports whether the backend has already read its
	// authoritative terminal result. Once true the run's outcome is decided and
	// no liveness policy may reclassify it, however long the backend then takes
	// to finish cleaning up. Backends must publish this before any cleanup that
	// can block or fail, otherwise a completed run can still be re-tagged as a
	// hang. Nil means the backend offers no such boundary.
	//
	// It must also be published before the backend sends on Result. The daemon
	// reads it only after a result is in hand, so that ordering is what makes
	// the read reliable instead of a race: delivering the result establishes
	// the happens-before, and no flag read has to win a timing window.
	TerminalObserved func() bool
	// Messages streams events as the agent works. The channel is closed
	// when the agent finishes (before Result is sent).
	Messages <-chan Message
	// Result receives exactly one value — the final outcome — then closes.
	Result <-chan Result
}

// MessageType identifies the kind of Message.
type MessageType string

const (
	MessageText       MessageType = "text"
	MessageThinking   MessageType = "thinking"
	MessageToolUse    MessageType = "tool-use"
	MessageToolResult MessageType = "tool-result"
	MessageStatus     MessageType = "status"
	MessageError      MessageType = "error"
	MessageLog        MessageType = "log"
)

// Message is a unified event emitted by an agent during execution.
type Message struct {
	Type      MessageType
	Content   string         // text content (Text, Error, Log)
	Tool      string         // tool name (ToolUse, ToolResult)
	CallID    string         // tool call ID (ToolUse, ToolResult)
	Input     map[string]any // tool input (ToolUse)
	Output    string         // tool output (ToolResult)
	Status    string         // agent status string (Status)
	Level     string         // log level (Log)
	SessionID string         // backend session id (Status), for early resume-pointer pinning
}

// TokenUsage tracks token consumption for a single model.
// Its four token counts are mutually exclusive: InputTokens excludes cache
// reads and writes, and OutputTokens includes reasoning/thinking.
// Breakdowns already included in a total must not be added to that total again.
// Zero counters alone do not establish that the provider reported complete usage.
type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CostUSDTicks is the provider's own statement of what this usage cost,
	// in ticks of 1e-10 USD. Zero means "not reported" — only a few agents
	// return it (xAI Grok Build does, via `_meta.usage.costUsdTicks`).
	//
	// It matters because a token-times-rate estimate cannot reproduce
	// request-level pricing rules. xAI bills a request at 2x once its prompt
	// reaches 200K tokens, and a usage record aggregates every model call in
	// a turn — so the stored token counts cannot say which tier any single
	// request hit. The provider's own figure already has that priced in.
	CostUSDTicks int64
}

// Result is the final outcome after an agent session completes.
type Result struct {
	Status     string // "completed", "failed", "aborted", "timeout", "cancelled"
	Output     string // final user-facing output selected by the backend
	Error      string // error message if failed
	DurationMs int64
	SessionID  string
	Usage      map[string]TokenUsage // keyed by model name
	// ResumeRejected is positive evidence that this run's requested resume
	// was permanently refused — the transcript is gone, the session belongs to
	// another provider account, OR the session still exists but its history
	// can no longer be replayed to the provider (e.g. GH #5975: a stored
	// image now exceeds the provider's max dimensions, so every resumed
	// session/prompt is rejected before the turn runs). What unites these is
	// that the resume CANNOT continue and only starting over can cure it, so
	// it is what the daemon's fresh-session fallback looks for first. Note the
	// last case keeps a non-empty SessionID (the id is real, only its history
	// is unusable) — the daemon gates on this boolean, not an empty id.
	//
	// false is NOT evidence of the opposite. For a backend listed in
	// ResumeRejectionUndetectable it means "could not tell"; for every other
	// backend it means "checked, and this was not a rejection". The daemon
	// needs the provider name to tell those apart — see
	// shouldRetryWithFreshSession in internal/daemon.
	//
	// Backends must NOT set it for failures a new session cannot cure:
	// network drops, rate limits, quota, provider 5xx, or auth errors. Those
	// keep the session pointer so the platform's own retry can resume the
	// truncated conversation (see retryableReasons in internal/service/task.go).
	//
	// The auth exclusion above stands even for the one auth error a fresh
	// session DOES cure — a resumed session whose persisted provider identity
	// can no longer resolve its credentials (GH #6777). An adapter cannot tell
	// that apart from a genuinely bad credential by looking at the error, so
	// the judgement is made once, provider-agnostically, in
	// shouldRetryWithFreshSession, where "was this run a resume?" is already
	// known. Do not encode it here.
	ResumeRejected bool
	// ResumeRejectedTransient is positive evidence that this run's requested
	// resume cannot proceed right now, but the session itself remains healthy.
	// The daemon may use a fresh session for this turn, but must not retire the
	// requested session from future lookups. Backends must not set this together
	// with ResumeRejected; the latter means the session is permanently unusable.
	ResumeRejectedTransient bool
	// codexInitializeRetrySafe is provider-internal evidence that an
	// initialize timeout happened before semantic activity and after the
	// process tree was reaped. It is intentionally not part of the public
	// result contract.
	codexInitializeRetrySafe bool
	// codexStartupRefreshRetrySafe is provider-internal evidence that the
	// first turn produced no semantic progress because Codex could not load
	// its model catalog, and that the process tree was reaped afterwards.
	// Like codexInitializeRetrySafe it is not part of the public contract.
	codexStartupRefreshRetrySafe bool
}

// New creates a Backend for the given agent type.
// Supported types: "claude", "codebuddy", "codex", "copilot", "opencode", "codearts", "deveco", "openclaw", "hermes", "pi", "cursor", "kimi", "reasonix", "dsh", "kiro", "antigravity", "qoder", "qoderclicn", "traecli", "grok", "qwen", "qwenpaw", "mcode".
//
// SupportedTypes is the canonical whitelist of agent types eligible to back a
// custom runtime profile. It MUST stay in lockstep with the
// runtime_profile.protocol_family CHECK constraint (migration 120, widened by
// migration 134 to add qoder, migration 136 to add traecli, migration 175 to
// add deveco, migration 179 to add grok, migration 202 to add qwen,
// migration 242 to add qoderclicn, migration 253 to add qwenpaw,
// migration 254 to add reasonix, migration 313 to add dsh, migration 342 to
// add mcode, migration 370 to add dim, migration 403 to add zeroclaw, and
// migration 441 to add codearts): a custom runtime profile may
// only be based on a backend Multica officially supports.
// qoder and qoderclicn share the same ACP backend; keeping both provider keys
// lets the daemon auto-detect and register the international and China-region
// binaries independently. traecli (Trae) has a New backend, launch
// header and provider branding but was previously missing from this whitelist,
// so the family picker rejected it (#4945). grok is the xAI Grok Build CLI
// ACP backend (`grok agent --always-approve stdio`). qwen is Qwen Code's
// native `qwen -p <prompt> --output-format stream-json` backend.
var SupportedTypes = []string{
	"claude",
	"codebuddy",
	"codex",
	"copilot",
	"opencode",
	"codearts",
	"deveco",
	"openclaw",
	"hermes",
	"pi",
	"cursor",
	"kimi",
	"reasonix",
	"dsh",
	"kiro",
	"antigravity",
	"qoder",
	"qoderclicn",
	"traecli",
	"grok",
	"qwen",
	"qwenpaw",
	"mcode",
	"dim",
	"zeroclaw",
}

// IsSupportedType reports whether agentType is in the SupportedTypes whitelist.
// Used to validate a custom runtime profile's protocol_family before it is
// persisted or registered.
func IsSupportedType(agentType string) bool {
	for _, t := range SupportedTypes {
		if t == agentType {
			return true
		}
	}
	return false
}

// resumeRejectionUndetectable lists the backends that cannot produce
// Result.ResumeRejected at all. They scrape SessionID out of stream output and
// have no rejection detection: no phrase match, no structured error code, no
// internal restart. copilot's own comment documents the hole (a session.error
// arriving before session.start leaves SessionID empty), and antigravity's
// conversation-id reader returns "" whenever the CLI exits before dispatching.
//
// Membership is deliberately opt-in. A backend absent from this map is treated
// as capable, so a new backend fails closed — it reports no rejection and gets
// no fallback — rather than silently inheriting a guess-based retry. Remove an
// entry as soon as its backend learns to report rejections.
var resumeRejectionUndetectable = map[string]bool{
	"antigravity": true,
	"copilot":     true,
	"cursor":      true,
	"deveco":      true,
	"opencode":    true,
	"codearts":    true,
}

// ResumeRejectionUndetectable reports whether agentType is a backend that
// cannot tell a refused resume from any other startup failure. Callers use it
// to read a false Result.ResumeRejected correctly: "could not tell" for these,
// "checked, not a rejection" for everything else.
func ResumeRejectionUndetectable(agentType string) bool {
	return resumeRejectionUndetectable[agentType]
}

// launchHeaders maps each supported agent type to the user-visible skeleton
// that the daemon spawns before any custom_args are appended. This is
// intentionally minimal — only the command + subcommand (or a short mode
// label when there is no subcommand). Internal flags, transport values, and
// environment variables are deliberately omitted so the string is a hint
// about *what* users are extending, not a dump of the full command line.
var launchHeaders = map[string]string{
	"antigravity": "agy -p (non-interactive)",
	"claude":      "claude (stream-json)",
	"codebuddy":   "codebuddy (stream-json)",
	"codex":       "codex app-server",
	"copilot":     "copilot (json)",
	"cursor":      "cursor-agent (stream-json)",
	"codearts":    "codearts run (json)",
	"deveco":      "deveco run (json)",
	"hermes":      "hermes acp",
	"kimi":        "kimi acp",
	"reasonix":    "reasonix acp",
	"dsh":         "dsh --profile multica (stdio)",
	"kiro":        "kiro-cli acp",
	"openclaw":    "openclaw agent (json)",
	"opencode":    "opencode run (json)",
	"pi":          "pi (json mode)",
	"qoder":       "qodercli --acp",
	"qoderclicn":  "qoderclicn --acp",
	"traecli":     "traecli acp serve",
	"grok":        "grok agent stdio",
	"qwen":        "qwen -p (stream-json)",
	"qwenpaw":     "qwenpaw acp",
	"dim":         "dim acp",
	"mcode":       "mcode acp",
	"zeroclaw":    "zeroclaw acp",
}

// LaunchHeader returns the user-visible launch skeleton for agentType, or an
// empty string if the type is unknown. Callers render this as a preview so
// users understand which command their custom_args get appended to.
func LaunchHeader(agentType string) string {
	if h := launchHeaders[agentType]; h != "" {
		return h
	}
	// Built-in runtime identities derive their launch header from the
	// descriptor, not the protocol-family map.
	if desc, ok := BuiltinRuntimeByID(agentType); ok {
		return desc.LaunchHeader
	}
	return ""
}
