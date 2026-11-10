package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/pkg/agent"
)

func createDaemonTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup failed: %s: %v", out, err)
		}
	}
	return dir
}

func TestNormalizeServerBaseURL(t *testing.T) {
	t.Parallel()

	got, err := NormalizeServerBaseURL("ws://localhost:8080/ws")
	if err != nil {
		t.Fatalf("NormalizeServerBaseURL returned error: %v", err)
	}
	if got != "http://localhost:8080" {
		t.Fatalf("expected http://localhost:8080, got %s", got)
	}
}

func TestTriggerRestart_BrewLinuxCellarDeleted(t *testing.T) {
	originalIsBrewInstall := isBrewInstall
	originalGetBrewPrefix := getBrewPrefix
	t.Cleanup(func() {
		isBrewInstall = originalIsBrewInstall
		getBrewPrefix = originalGetBrewPrefix
	})

	prefix := filepath.Join(t.TempDir(), "home", "linuxbrew", ".linuxbrew")
	deletedCellarPath := filepath.Join(prefix, "Cellar", "multica", "0.2.9", "bin", "multica")
	isBrewInstall = func() bool { return true }
	getBrewPrefix = func() string { return prefix }

	d := &Daemon{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.triggerRestart()

	want := filepath.Join(prefix, "bin", "multica")
	if got := d.RestartBinary(); got != want {
		t.Fatalf("restart binary = %q, want %q", got, want)
	}
	if got := d.RestartBinary(); got == deletedCellarPath {
		t.Fatalf("restart binary used deleted Cellar path %q", got)
	}
}

func TestTriggerRestart_UsesResolvedFallback(t *testing.T) {
	originalResolveSelfExecutable := resolveSelfExecutable
	originalIsBrewInstall := isBrewInstall
	t.Cleanup(func() {
		resolveSelfExecutable = originalResolveSelfExecutable
		isBrewInstall = originalIsBrewInstall
	})

	want := filepath.Join(t.TempDir(), "multica")
	if err := os.WriteFile(want, []byte("test executable"), 0o755); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolve executable fixture: %v", err)
	}
	resolveSelfExecutable = func() (string, error) { return want, nil }
	isBrewInstall = func() bool { return false }

	canceled := false
	d := &Daemon{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cancelFunc: func() { canceled = true },
	}
	d.triggerRestart()

	if got := d.RestartBinary(); got != wantResolved {
		t.Fatalf("restart binary = %q, want resolved fallback executable %q", got, wantResolved)
	}
	if !canceled {
		t.Fatal("triggerRestart did not initiate graceful shutdown")
	}
}

func TestTriggerRestart_ResolveFailureLeavesDaemonRunning(t *testing.T) {
	originalResolveSelfExecutable := resolveSelfExecutable
	t.Cleanup(func() { resolveSelfExecutable = originalResolveSelfExecutable })
	resolveSelfExecutable = func() (string, error) {
		return "", errors.New("cannot resolve executable")
	}

	canceled := false
	d := &Daemon{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cancelFunc: func() { canceled = true },
	}
	d.triggerRestart()

	if got := d.RestartBinary(); got != "" {
		t.Fatalf("restart binary = %q, want empty", got)
	}
	if canceled {
		t.Fatal("triggerRestart initiated shutdown without a restart binary")
	}
}

func TestIsBlockedEnvKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		want bool
	}{
		{key: "MULTICA_TOKEN", want: true},
		{key: "multica_runtime_id", want: true},
		{key: "HOME", want: true},
		{key: "PATH", want: true},
		{key: "TMPDIR", want: true},
		{key: "tmp", want: true},
		{key: "TEMP", want: true},
		{key: "CODEX_HOME", want: true},
		{key: "REASONIX_STATE_HOME", want: true},
		{key: "CURSOR_DATA_DIR", want: true},
		{key: "cursor_data_dir", want: true},
		{key: "CURSOR_MCP_AUTH_SOURCE", want: false},
		{key: "OPENCLAW_CONFIG_PATH", want: true},
		{key: "OPENCLAW_INCLUDE_ROOTS", want: true},
		{key: "ANTHROPIC_API_KEY", want: false},
		{key: "CURSOR_AGENT", want: false},
		// HERMES_HOME is intentionally NOT blocked: a skill-less Hermes task
		// must be able to honor a user-set profile/home, and when skills are
		// bound the per-task overlay overrides it after custom_env is layered.
		{key: "HERMES_HOME", want: false},
		{key: "hermes_home", want: false},
		// Reasonix credentials/config remain tool-owned and may use a custom
		// home; only the daemon-owned state overlay is blocked above.
		{key: "REASONIX_HOME", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			if got := isBlockedEnvKey(tt.key); got != tt.want {
				t.Fatalf("isBlockedEnvKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestTaskScopedAuthToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		want    string
		wantErr string
	}{
		{
			name:    "missing token fails closed",
			wantErr: "server did not provide task-scoped auth token",
		},
		{
			name:    "member token fails closed",
			token:   "mul_member_token",
			wantErr: "server provided non-task-scoped auth token",
		},
		{
			name:  "task token accepted",
			token: " mat_task_token ",
			want:  "mat_task_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := taskScopedAuthToken(Task{AuthToken: tt.token})
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("taskScopedAuthToken() error = nil, want %q", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("taskScopedAuthToken() error = %q, want %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("taskScopedAuthToken(): %v", err)
			}
			if got != tt.want {
				t.Fatalf("taskScopedAuthToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTaskMulticaEnvironmentTempPolicy(t *testing.T) {
	t.Setenv("CLAUDE_CODE_TMPDIR", "")
	for _, provider := range []string{"native"} {
		t.Run(provider, func(t *testing.T) {
			env := taskMulticaEnvironment(Task{}, provider, "", "", "", "", "", 0, 0, "/task/tmp")
			if env["TMPDIR"] != "/task/tmp" {
				t.Fatalf("TMPDIR = %q, want private task temp", env["TMPDIR"])
			}
			if runtime.GOOS == "windows" {
				// Every provider can start Git Bash, whose /tmp mount is shared.
				for _, key := range []string{"TMP", "TEMP"} {
					if _, overridden := env[key]; overridden {
						t.Errorf("%s must inherit the host path for Git Bash's shared /tmp", key)
					}
				}
			} else {
				for _, key := range []string{"TMP", "TEMP"} {
					if env[key] != "/task/tmp" {
						t.Errorf("%s = %q, want existing private task temp", key, env[key])
					}
				}
			}
            if _, ok := env["CLAUDE_CODE_TMPDIR"]; ok { t.Error("native tools must not receive external CLI configuration") }

		})
	}
}

// When `brew --prefix` is unavailable but the executable path is under a
// known Cellar root, triggerRestart must recover the prefix from the
// known-prefix list and target <prefix>/bin/multica.
func TestTriggerRestart_BrewPrefixUnavailable_FallsBackToKnownPrefix(t *testing.T) {
	originalIsBrewInstall := isBrewInstall
	originalGetBrewPrefix := getBrewPrefix
	originalMatchKnownBrewPrefix := matchKnownBrewPrefix
	originalResolveSelfExecutable := resolveSelfExecutable
	t.Cleanup(func() {
		isBrewInstall = originalIsBrewInstall
		getBrewPrefix = originalGetBrewPrefix
		matchKnownBrewPrefix = originalMatchKnownBrewPrefix
		resolveSelfExecutable = originalResolveSelfExecutable
	})

	const knownPrefix = "/home/linuxbrew/.linuxbrew"
	cellarPath := filepath.Join(knownPrefix, "Cellar", "multica", "0.2.9", "bin", "multica")
	isBrewInstall = func() bool { return true }
	getBrewPrefix = func() string { return "" }
	resolveSelfExecutable = func() (string, error) { return cellarPath, nil }
	matchKnownBrewPrefix = func(path string) string {
		if path != cellarPath {
			t.Fatalf("MatchKnownBrewPrefix path = %q, want resolver result %q", path, cellarPath)
		}
		return knownPrefix
	}

	d := &Daemon{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.triggerRestart()

	want := filepath.Join(knownPrefix, "bin", "multica")
	if got := d.RestartBinary(); got != want {
		t.Fatalf("restart binary = %q, want %q", got, want)
	}
}

// When `brew --prefix` is unavailable AND the executable is not under any
// known Cellar root, triggerRestart logs a warning and keeps the executable
// path (no fabricated <prefix>/bin/multica path).
func TestTriggerRestart_BrewPrefixUnavailable_NoKnownPrefix_KeepsExecutable(t *testing.T) {
	originalIsBrewInstall := isBrewInstall
	originalGetBrewPrefix := getBrewPrefix
	originalMatchKnownBrewPrefix := matchKnownBrewPrefix
	t.Cleanup(func() {
		isBrewInstall = originalIsBrewInstall
		getBrewPrefix = originalGetBrewPrefix
		matchKnownBrewPrefix = originalMatchKnownBrewPrefix
	})

	isBrewInstall = func() bool { return true }
	getBrewPrefix = func() string { return "" }
	matchKnownBrewPrefix = func(string) string { return "" }

	d := &Daemon{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.triggerRestart()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if got := d.RestartBinary(); got != exe {
		t.Fatalf("restart binary = %q, want unchanged executable %q", got, exe)
	}
}

func TestNewTaskSlotSemaphoreReturnsStableSlotIndexes(t *testing.T) {
	t.Parallel()

	sem := newTaskSlotSemaphore(4)
	seen := make(map[int]bool)
	for i := 0; i < 4; i++ {
		select {
		case slot := <-sem:
			if slot < 0 || slot > 3 {
				t.Fatalf("slot out of range: %d", slot)
			}
			if seen[slot] {
				t.Fatalf("duplicate slot: %d", slot)
			}
			seen[slot] = true
		default:
			t.Fatalf("expected slot %d to be available", i)
		}
	}

	select {
	case slot := <-sem:
		t.Fatalf("expected semaphore to be empty, got slot %d", slot)
	default:
	}

	sem <- 2
	select {
	case slot := <-sem:
		if slot != 2 {
			t.Fatalf("expected released slot 2, got %d", slot)
		}
	default:
		t.Fatal("expected released slot to be available")
	}
}

func TestBuildPromptContainsIssueID(t *testing.T) {
	t.Parallel()

	issueID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	prompt := BuildPrompt(Task{
		IssueID: issueID,
		Agent: &AgentData{
			Name: "Local Codex",
			Skills: []SkillData{
				{Name: "Concise", Content: "Be concise."},
			},
		},
	}, "claude")

	// Prompt should contain the issue ID and CLI hint.
	for _, want := range []string{
		issueID,
		"multica issue get",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}

	// Skills should NOT be inlined in the prompt (they're in runtime config).
	for _, absent := range []string{"## Agent Skills", "Be concise."} {
		if strings.Contains(prompt, absent) {
			t.Fatalf("prompt should NOT contain %q (skills are in runtime config)", absent)
		}
	}
}

// TestSessionContinuityNoticeMatchesSurface locks the MUL-5722 split. The same
// event costs each surface something different, so it cannot be reported with
// one sentence. The dividing question is whether the conversation can still be
// READ, not whether it is a chat: an issue's comments, a Slack channel's
// history, and a web chat's / Feishu's / WeCom's / DingTalk's stored
// chat_message transcript all can; only a surface Multica stores no transcript
// for cannot. Announcing a loss on the readable ones describes something that
// did not happen — the user hears "the discussion is gone" when every word
// survives.
func TestSessionContinuityNoticeMatchesSurface(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		task         Task
		tellUser     bool
		wantMentions string
	}{
		{
			name:         "issue rebuilds from comments",
			task:         Task{IssueID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
			tellUser:     false,
			wantMentions: "comment history",
		},
		{
			// Slack has a history reader, so the conversation is recoverable —
			// just from the channel rather than from Multica. Telling the user it
			// was lost contradicts the commands the same prompt hands the agent.
			name:         "slack rebuilds from the channel",
			task:         Task{ChatSessionID: "chat-1", ChatChannelType: execenv.ChannelTypeSlack},
			tellUser:     false,
			wantMentions: "multica chat history",
		},
		{
			// Web chat history is persisted in chat_message, which `multica chat
			// history` reads back — recoverable, just from Multica's store.
			name:         "web chat rebuilds from the stored transcript",
			task:         Task{ChatSessionID: "chat-1"},
			tellUser:     false,
			wantMentions: "multica chat history",
		},
		{
			// Feishu's conversation is persisted to chat_message too, and the
			// handler's non-Slack fallback reads it back via `multica chat history`.
			name:         "feishu rebuilds from the stored transcript",
			task:         Task{ChatSessionID: "chat-1", ChatChannelType: execenv.ChannelTypeFeishu},
			tellUser:     false,
			wantMentions: "multica chat history",
		},
		{
			// WeCom is fully wired on main (persists chat_message, stamps
			// ChatChannelType="wecom"), so its transcript is readable too — the
			// handler's non-Slack fallback serves it just like Feishu's.
			name:         "wecom rebuilds from the stored transcript",
			task:         Task{ChatSessionID: "chat-1", ChatChannelType: execenv.ChannelTypeWecom},
			tellUser:     false,
			wantMentions: "multica chat history",
		},
		{
			// DingTalk persists to chat_message through the same AppendUserMessage
			// path as Feishu/WeCom, so its transcript is readable the same way.
			name:         "dingtalk rebuilds from the stored transcript",
			task:         Task{ChatSessionID: "chat-1", ChatChannelType: execenv.ChannelTypeDingtalk},
			tellUser:     false,
			wantMentions: "multica chat history",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notice := sessionContinuityNoticeFor(tc.task)
			if got := strings.Contains(notice, "tell the user up front"); got != tc.tellUser {
				t.Errorf("tells the user = %v, want %v:\n%s", got, tc.tellUser, notice)
			}
			if !strings.Contains(notice, tc.wantMentions) {
				t.Errorf("notice missing %q, so the agent is not told where to rebuild from:\n%s", tc.wantMentions, notice)
			}
			// Where the conversation survives, the ONLY loss is the agent's
			// unrecorded working memory, and it has to be told so or it assumes
			// it still remembers work it no longer has. Where nothing survives
			// that distinction is meaningless — everything is gone — so the
			// requirement applies to the recoverable surfaces only.
			if !tc.tellUser && !strings.Contains(notice, "your own working memory") {
				t.Errorf("recoverable surface must name the real loss:\n%s", notice)
			}
		})
	}

	// The notice only renders when the resume actually failed.
	if blocks := perTurnContextBlocks(Task{IssueID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890"}, promptOpts{}); strings.Contains(blocks, "Session Continuity Notice") {
		t.Errorf("continuity notice leaked into a run that resumed fine:\n%s", blocks)
	}
	lost := perTurnContextBlocks(Task{
		IssueID:                       "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		PriorSessionResumeUnavailable: true,
	}, promptOpts{})
	if !strings.Contains(lost, "Session Continuity Notice") {
		t.Errorf("continuity notice missing when the resume was unavailable:\n%s", lost)
	}
}

// TestBackendResumeContinuityNoticeSuppressedWhenPromptAlreadyHasIt is the
// count guard Elon asked for, on the combination that actually occurs: the
// codex overflow fresh retry. The daemon appends the notice to the prompt AND
// hands the backend a notice to prepend, so before MUL-5722 one turn carried
// the same paragraph twice — at full token price, from two hand-written
// strings. Suppression is keyed on the prompt already carrying it, so the two
// injectors cannot both fire.
func TestBackendResumeContinuityNoticeSuppressedWhenPromptAlreadyHasIt(t *testing.T) {
	t.Parallel()

	const heading = "## Session Continuity Notice"

	// Live resume rejection the daemon cannot see pre-launch: the prompt says
	// nothing, so the backend must.
	fresh := Task{IssueID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890", TriggerCommentID: "c0ffee00-0000-0000-0000-000000000000"}
	if got := backendResumeContinuityNotice(fresh); !strings.Contains(got, heading) {
		t.Fatal("backend must disclose when the prompt does not")
	}
	if strings.Contains(BuildPrompt(fresh, "codex"), heading) {
		t.Fatal("prompt should not carry the notice when the resume has not failed yet")
	}

	// The daemon's fresh-session retry sets this, which is what makes
	// BuildPrompt append the notice — so the backend must now stay silent.
	retry := fresh
	retry.PriorSessionResumeUnavailable = true
	prompt := BuildPrompt(retry, "codex")
	if n := strings.Count(prompt, heading); n != 1 {
		t.Fatalf("prompt must carry exactly one notice, got %d", n)
	}
	backendNotice := backendResumeContinuityNotice(retry)
	if backendNotice != "" {
		t.Fatalf("backend must not add a second copy, got:\n%s", backendNotice)
	}
	// The whole turn, as codex would assemble it.
	turn := backendNotice + prompt
	if n := strings.Count(turn, heading); n != 1 {
		t.Fatalf("assembled turn carries %d notices, want exactly 1:\n%s", n, turn)
	}
}

func TestBuildPromptNoIssueDetails(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Task{
		IssueID: "test-id",
		Agent:   &AgentData{Name: "Test"},
	}, "claude")

	// Prompt should not contain issue title/description (agent fetches via CLI).
	for _, absent := range []string{"**Issue:**", "**Summary:**"} {
		if strings.Contains(prompt, absent) {
			t.Fatalf("prompt should NOT contain %q — agent fetches details via CLI", absent)
		}
	}
}

func TestBuildPromptAutopilotRunOnly(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Task{
		AutopilotRunID:          "run-1",
		AutopilotID:             "autopilot-1",
		AutopilotTitle:          "Daily dependency check",
		AutopilotDescription:    "Check dependencies and report outdated packages.",
		AutopilotSource:         "manual",
		AutopilotTriggerPayload: []byte(`{"action":"opened","number":7}`),
	}, "claude")

	// Every run-scoped autopilot value must appear HERE, and only here. The
	// runtime brief used to render the same set, which both broke its own
	// no-per-run-values contract and left two hand-maintained copies to drift
	// (MUL-6984); execenv.TestAutopilotBriefByteIdenticalAcrossRunScopedFields
	// pins the brief's side of that split. The payload is rendered whole:
	// ingress already caps a webhook body at 256 KiB, and truncating at the
	// only rendering would drop input no CLI can fetch back.
	for _, want := range []string{
		"run-only mode",
		"Autopilot run ID: run-1",
		"Daily dependency check",
		"Trigger source: manual",
		`{"action":"opened","number":7}`,
		"Check dependencies and report outdated packages.",
		"multica autopilot get autopilot-1 --output json",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("autopilot prompt missing %q\n---\n%s", want, prompt)
		}
	}

	// The issue-command boundary is emitted ONCE, by the brief's autopilot
	// workflow section (execenv.AutopilotIssueCommandsGuard). MUL-5696 found
	// that a second hand-maintained per-turn copy drifts, so the per-turn
	// prompt must not restate it in any form.
	if strings.Contains(prompt, "Do not run `multica issue get`") {
		t.Fatalf("autopilot prompt restates the issue-command boundary the brief owns (MUL-5696)\n---\n%s", prompt)
	}
	if strings.Contains(prompt, "Your assigned issue ID is:") {
		t.Fatalf("autopilot prompt should not use issue assignment template\n---\n%s", prompt)
	}
}

func TestBuildPromptCommentTriggered(t *testing.T) {
	t.Parallel()

	issueID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	commentID := "c1c2c3c4-d5d6-7890-abcd-ef1234567890"
	commentContent := "请把报告翻译成英文"

	prompt := BuildPrompt(Task{
		IssueID:               issueID,
		TriggerCommentID:      commentID,
		TriggerCommentContent: commentContent,
		Agent:                 &AgentData{Name: "Test"},
	}, "claude")

	// Prompt should contain the comment content, the trigger comment id, and
	// the full reply command with --parent. Re-emitting --parent on every turn
	// is what prevents resumed sessions from reusing the previous turn's
	// --parent UUID.
	for _, want := range []string{
		issueID,
		commentContent,
		"Focus on THIS comment",
		commentID,
		"multica issue comment add " + issueID + " --parent " + commentID,
		"do NOT reuse --parent values from previous turns",
		// MUL-5442 (2026-08-06): with the generic no-reply rule retired,
		// the reply command is framed as a plain imperative again — the
		// squad leader's `no_action` block states its own exception.
		"Post your reply as a comment",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n---\n%s", want, prompt)
		}
	}

	// Should still contain CLI hint for fetching issue context.
	if !strings.Contains(prompt, "multica issue get") {
		t.Fatal("prompt missing CLI hint for issue context")
	}
}

// TestBuildPromptCommentTriggeredByAgent covers the trigger-attribution
// line for agent-authored triggers, and guards the RETIREMENT of the
// generic no-reply warning block that used to follow it (MUL-1323 /
// GH#1576). Retired by MUL-5442 owner decision (2026-08-06): an agent
// comment cannot wake an ordinary agent without an explicit @mention
// (computeCommentAgentTriggers routes agent-authored comments only via
// mentions, plus the squad-leader wake), so loop prevention belongs to the
// mention discipline in the brief's `## Mentions`, and recorded silence
// stays squad-leader-only (`no_action`). Retired pins, now negative
// guards: "do not @mention the other agent as a sign-off", "Silence is
// the preferred way".
func TestBuildPromptCommentTriggeredByAgent(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Task{
		IssueID:               "issue-1",
		TriggerCommentID:      "comment-1",
		TriggerCommentContent: "thanks, looks good!",
		TriggerAuthorType:     "agent",
		TriggerAuthorName:     "Atlas",
		Agent:                 &AgentData{Name: "Test"},
	}, "claude")

	if !strings.Contains(prompt, "Another agent (Atlas)") {
		t.Fatalf("prompt missing trigger attribution\n---\n%s", prompt)
	}
	for _, banned := range []string{
		"Silence is the preferred way",
		"do not @mention the other agent as a sign-off",
		"reply is warranted",
		"exit with no output",
		// The conditional reply framing was the door the retired rule
		// walked through — guard it shut (MUL-5442 #6493 review).
		"If you decide to reply",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("per-turn prompt still carries retired no-reply warning %q\n---\n%s", banned, prompt)
		}
	}
}

// TestBuildPromptCommentTriggeredByMember guards against the agent-loop warning
// leaking into human-authored triggers — a human asking a question should not
// be pre-discouraged from getting a reply.
func TestBuildPromptCommentTriggeredByMember(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Task{
		IssueID:               "issue-1",
		TriggerCommentID:      "comment-1",
		TriggerCommentContent: "can you translate this?",
		TriggerAuthorType:     "member",
		TriggerAuthorName:     "Alice",
		Agent:                 &AgentData{Name: "Test"},
	}, "claude")

	if !strings.Contains(prompt, "A user just left a new comment") {
		t.Fatalf("member-triggered prompt should label the author as a user\n---\n%s", prompt)
	}
	if strings.Contains(prompt, "Another agent") {
		t.Fatalf("member-triggered prompt should not claim the author was another agent")
	}
	// Must NOT use the old "You MUST respond" language: the reply command is
	// shared across turn types, and a squad leader's `no_action` exit — the
	// one silent path left after MUL-5442 retired the generic no-reply rule —
	// must not be shouted over by the per-turn channel. The unconditional
	// one-comment contract for ordinary agents lives in the brief.
	if strings.Contains(prompt, "MUST respond") {
		t.Fatalf("prompt should not contain unconditional \"MUST respond\" language\n---\n%s", prompt)
	}
	if !strings.Contains(prompt, "Post your reply as a comment") {
		t.Fatalf("prompt should carry the imperative reply command\n---\n%s", prompt)
	}
}

func TestBuildPromptCommentTriggeredNoContent(t *testing.T) {
	t.Parallel()

	// When TriggerCommentID is set but content is empty (e.g. fetch failed),
	// it should still use the comment prompt path.
	prompt := BuildPrompt(Task{
		IssueID:          "test-id",
		TriggerCommentID: "comment-id",
		Agent:            &AgentData{Name: "Test"},
	}, "claude")

	if !strings.Contains(prompt, "multica issue get") {
		t.Fatal("prompt missing CLI hint")
	}
}

func TestIsWorkspaceNotFoundError(t *testing.T) {
	t.Parallel()

	err := &requestError{
		Method:     http.MethodPost,
		Path:       "/api/daemon/register",
		StatusCode: http.StatusNotFound,
		Body:       `{"error":"workspace not found"}`,
	}
	if !isWorkspaceNotFoundError(err) {
		t.Fatal("expected workspace not found error to be recognized")
	}

	if isWorkspaceNotFoundError(&requestError{StatusCode: http.StatusInternalServerError, Body: `{"error":"workspace not found"}`}) {
		t.Fatal("did not expect 500 to be treated as workspace not found")
	}
}

func TestIsTaskNotFoundError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "404 with task not found body",
			err: &requestError{
				Method:     http.MethodPost,
				Path:       "/api/daemon/tasks/abc/messages",
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"task not found"}`,
			},
			want: true,
		},
		{
			name: "404 with mixed-case body still matches",
			err: &requestError{
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"Task Not Found"}`,
			},
			want: true,
		},
		{
			name: "500 with same body is not task-not-found",
			err: &requestError{
				StatusCode: http.StatusInternalServerError,
				Body:       `{"error":"task not found"}`,
			},
			want: false,
		},
		{
			name: "404 with workspace-not-found body is not task-not-found",
			err: &requestError{
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"workspace not found"}`,
			},
			want: false,
		},
		{
			name: "non-requestError",
			err:  errors.New("network down"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTaskNotFoundError(tc.err); got != tc.want {
				t.Fatalf("isTaskNotFoundError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRuntimeNotFoundError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "404 with runtime not found body from heartbeat",
			err: &requestError{
				Method:     http.MethodPost,
				Path:       "/api/daemon/heartbeat",
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"runtime not found"}`,
			},
			want: true,
		},
		{
			name: "404 with runtime not found body from claim",
			err: &requestError{
				Method:     http.MethodPost,
				Path:       "/api/daemon/runtimes/abc/tasks/claim",
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"runtime not found"}`,
			},
			want: true,
		},
		{
			name: "mixed-case body still matches",
			err: &requestError{
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"Runtime Not Found"}`,
			},
			want: true,
		},
		{
			name: "500 with same body must NOT be treated as runtime-not-found",
			err: &requestError{
				StatusCode: http.StatusInternalServerError,
				Body:       `{"error":"runtime not found"}`,
			},
			want: false,
		},
		{
			name: "404 with task-not-found body is not runtime-not-found",
			err: &requestError{
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"task not found"}`,
			},
			want: false,
		},
		{
			name: "404 with workspace-not-found body is not runtime-not-found",
			err: &requestError{
				StatusCode: http.StatusNotFound,
				Body:       `{"error":"workspace not found"}`,
			},
			want: false,
		},
		{
			name: "non-requestError",
			err:  errors.New("network down"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isRuntimeNotFoundError(tc.err); got != tc.want {
				t.Fatalf("isRuntimeNotFoundError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestShouldInterruptAgent(t *testing.T) {
	t.Parallel()

	notFound := &requestError{
		StatusCode: http.StatusNotFound,
		Body:       `{"error":"task not found"}`,
	}
	transient := &requestError{
		StatusCode: http.StatusBadGateway,
		Body:       `<html>...</html>`,
	}

	cases := []struct {
		name   string
		status string
		err    error
		want   bool
	}{
		{name: "status cancelled", status: "cancelled", err: nil, want: true},
		{name: "status failed (offline sweeper)", status: "failed", err: nil, want: true},
		{name: "status completed (finished elsewhere)", status: "completed", err: nil, want: true},
		{name: "task deleted (404)", status: "", err: notFound, want: true},
		{name: "running normally", status: "running", err: nil, want: false},
		{name: "waiting_local_directory keeps running", status: "waiting_local_directory", err: nil, want: false},
		{name: "dispatched keeps running", status: "dispatched", err: nil, want: false},
		{name: "transient 5xx is not a cancel signal", status: "", err: transient, want: false},
		{name: "no information yet", status: "", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldInterruptAgent(tc.status, tc.err); got != tc.want {
				t.Fatalf("shouldInterruptAgent(%q, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// TestWatchTaskCancellation_TaskDeleted reproduces the zombie-task bug:
// when the server deletes a task while it is running (issue removed,
// agent reassigned, etc.), GetTaskStatus starts returning 404. Before the
// fix the daemon kept polling and never interrupted the running agent —
// codex would keep emitting tool calls for minutes against a dead task.
//
// After the fix, watchTaskCancellation must close its channel within a
// few poll intervals so the caller can cancel the agent context.
func TestWatchTaskCancellation_TaskDeleted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"task not found"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cancelled := d.watchTaskCancellation(ctx, "task-deleted", 10*time.Millisecond, slog.Default())

	select {
	case <-cancelled:
		// Expected: the watcher detected the 404 and signalled cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("watchTaskCancellation did not signal cancellation when task was deleted (404)")
	}
}

// TestWatchTaskCancellation_StatusCancelled keeps the existing behaviour
// (server transitions task status to "cancelled") working alongside the
// new 404 path.
func TestWatchTaskCancellation_StatusCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"cancelled"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cancelled := d.watchTaskCancellation(ctx, "task-cancelled", 10*time.Millisecond, slog.Default())

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("watchTaskCancellation did not signal cancellation when status=cancelled")
	}
}

// TestWatchTaskCancellation_RunningTaskNotInterrupted ensures the watcher
// does NOT trigger on transient errors or while the task is still running.
func TestWatchTaskCancellation_RunningTaskNotInterrupted(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cancelled := d.watchTaskCancellation(ctx, "task-running", 10*time.Millisecond, slog.Default())

	select {
	case <-cancelled:
		t.Fatal("watchTaskCancellation should not signal cancellation while task is running")
	case <-time.After(150 * time.Millisecond):
	}
	if calls.Load() < 5 {
		t.Fatalf("expected the watcher to poll at least 5 times in 150ms, got %d", calls.Load())
	}
}

// fakeBackend is a test double for agent.Backend that returns preconfigured
// results. Each call to Execute pops the next entry from the results slice.
type fakeBackend struct {
	calls   []agent.ExecOptions
	results []agent.Result
	errors  []error
	idx     atomic.Int32
}

func (b *fakeBackend) Execute(_ context.Context, _ string, opts agent.ExecOptions) (*agent.Session, error) {
	i := int(b.idx.Add(1)) - 1
	b.calls = append(b.calls, opts)
	if i < len(b.errors) && b.errors[i] != nil {
		return nil, b.errors[i]
	}
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	close(msgCh)
	resCh <- b.results[i]
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return &Daemon{
		client: NewClient(srv.URL),
		logger: slog.Default(),
	}
}

func newRepoReadyTestDaemon(t *testing.T, handler http.HandlerFunc) *Daemon {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := &Daemon{
		client:       NewClient(srv.URL),
		repoCache:    repocache.New(t.TempDir(), slog.Default()),
		logger:       slog.Default(),
		workspaces:   make(map[string]*workspaceState),
		runtimeIndex: make(map[string]Runtime),
	}
	// Drain background syncs (started by registerTaskRepos) before the
	// t.TempDir cache root is cleaned up, otherwise an in-flight clone/fetch
	// races against the deletion and the test fails with a misleading
	// "directory not empty" cleanup error.
	t.Cleanup(d.waitBackgroundSyncs)
	return d
}

// statusOnlyBackend emits a single status message carrying a session id (the
// point at which the daemon pins the resume pointer) and then completes.
type statusOnlyBackend struct {
	sessionID string
	result    agent.Result
}

func (b *statusOnlyBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message, 1)
	msgCh <- agent.Message{Type: agent.MessageStatus, SessionID: b.sessionID}
	close(msgCh)
	resCh := make(chan agent.Result, 1)
	resCh <- b.result
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// statusThenHoldBackend emits the session-id status, then keeps the run alive
// for `hold` before completing — so a rollout that only appears AFTER the
// status (mid-run) still has a live run for the pin waiter to observe.
type statusThenHoldBackend struct {
	sessionID string
	hold      time.Duration
	result    agent.Result
}

func (b *statusThenHoldBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		msgCh <- agent.Message{Type: agent.MessageStatus, SessionID: b.sessionID}
		time.Sleep(b.hold)
		close(msgCh)
		resCh <- b.result
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// pinRecorder captures the session ids the daemon pins mid-flight.
type pinRecorder struct {
	mu       sync.Mutex
	sessions []string
}

func (r *pinRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sessions)
}

func newPinRecorder(t *testing.T) (*Daemon, *pinRecorder) {
	t.Helper()
	rec := &pinRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/session") {
			var body struct {
				SessionID string `json:"session_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.SessionID != "" {
				rec.mu.Lock()
				rec.sessions = append(rec.sessions, body.SessionID)
				rec.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return &Daemon{client: NewClient(srv.URL), logger: slog.Default()}, rec
}

// TestExecuteAndDrain_PinsNativeSession persists the durable checkpoint identity.
func TestExecuteAndDrain_PinsNativeSession(t *testing.T) {
	t.Parallel()

	d, rec := newPinRecorder(t)
	backend := &statusOnlyBackend{sessionID: "sess-x", result: agent.Result{Status: "completed", Output: "done", SessionID: "sess-x"}}

	if _, _, err := d.executeAndDrain(context.Background(), backend, "p", agent.ExecOptions{}, slog.Default(), "task-pin", new(atomic.Int32)); err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}

	// The pin fires in a goroutine; wait briefly for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := rec.snapshot(); len(got) == 1 && got[0] == "sess-x" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected the native session to be pinned, got %+v", rec.snapshot())
}

// newCodexStoreGuardDaemon builds a Daemon with only the per-issue Codex session
// store guard initialised — enough to exercise the reserve-vs-mark protocol.
func newCodexStoreGuardDaemon() *Daemon {
	d := &Daemon{
		activeStores:   map[string]int{},
		deletingStores: map[string]bool{},
	}
	d.activeStoresCond = sync.NewCond(&d.activeStoresMu)
	return d
}

// transcriptBackend emits a tool_use and a text message before returning its
// result — the first call fails like a broken resume, the second completes.
type transcriptBackend struct {
	calls atomic.Int32
}

func (b *transcriptBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	n := b.calls.Add(1)
	msgCh := make(chan agent.Message, 2)
	msgCh <- agent.Message{Type: agent.MessageToolUse, Tool: "bash"}
	msgCh <- agent.Message{Type: agent.MessageText, Content: fmt.Sprintf("attempt %d", n)}
	close(msgCh)
	resCh := make(chan agent.Result, 1)
	if n == 1 {
		resCh <- agent.Result{Status: "failed", Error: "session not found"}
	} else {
		resCh <- agent.Result{Status: "completed", Output: "done", SessionID: "sess-2"}
	}
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// transcriptRecorder collects the task messages a daemon reports to its
// message endpoint.
type transcriptRecorder struct {
	mu       sync.Mutex
	messages []TaskMessageData
}

func (r *transcriptRecorder) snapshot() []TaskMessageData {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.messages)
}

// newTranscriptRecorder returns a daemon whose message endpoint appends every
// reported batch to the returned recorder.
func newTranscriptRecorder(t *testing.T) (*Daemon, *transcriptRecorder) {
	t.Helper()
	rec := &transcriptRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			var body struct {
				Messages []TaskMessageData `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				rec.mu.Lock()
				rec.messages = append(rec.messages, body.Messages...)
				rec.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return &Daemon{client: NewClient(srv.URL), logger: slog.Default()}, rec
}

// TestExecuteAndDrain_FlushesTranscriptBeforeReturningResult pins the
// completion contract: once the agent result is handed back, the task's
// messages have been reported. Without the wait the drain goroutine is still
// in flight, so a consumer reading the transcript sees it truncated.
func TestExecuteAndDrain_FlushesTranscriptBeforeReturningResult(t *testing.T) {
	t.Parallel()

	d, rec := newTranscriptRecorder(t)

	result, _, err := d.executeAndDrain(context.Background(), &transcriptBackend{}, "p", agent.ExecOptions{}, slog.Default(), "task-flush", new(atomic.Int32))
	if err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("expected the backend's result, got %+v", result)
	}

	if got := rec.snapshot(); len(got) != 2 {
		t.Fatalf("expected the transcript flushed before the result hand-off, got %d messages: %+v", len(got), got)
	}
}

type firstVisibleTranscriptBackend struct {
	emitted chan time.Time
	release chan struct{}
}

func (b firstVisibleTranscriptBackend) Execute(ctx context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		defer close(msgCh)
		msgCh <- agent.Message{Type: agent.MessageText, Content: "first visible text"}
		b.emitted <- time.Now()
		select {
		case <-b.release:
			resCh <- agent.Result{Status: "completed", Output: "done"}
		case <-ctx.Done():
			resCh <- agent.Result{Status: "cancelled", Error: ctx.Err().Error()}
		}
		close(resCh)
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// TestExecuteAndDrain_ReportsFirstVisibleMessageWithoutTickerDelay pins the
// leading edge of the daemon-to-server path. Later chunks remain batched, but
// the first user-visible content must not sit behind the 500 ms periodic flush.
func TestExecuteAndDrain_ReportsFirstVisibleMessageWithoutTickerDelay(t *testing.T) {
	emitted := make(chan time.Time, 1)
	release := make(chan struct{})
	reported := make(chan time.Time, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			var body struct {
				Messages []TaskMessageData `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode messages: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(body.Messages) > 0 {
				select {
				case reported <- time.Now():
				default:
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	done := make(chan error, 1)
	go func() {
		_, _, err := d.executeAndDrain(context.Background(), firstVisibleTranscriptBackend{
			emitted: emitted,
			release: release,
		}, "p", agent.ExecOptions{}, slog.Default(), "task-first-visible", new(atomic.Int32))
		done <- err
	}()

	emittedAt := <-emitted
	select {
	case reportedAt := <-reported:
		delay := reportedAt.Sub(emittedAt)
		t.Logf("first visible message reached the server in %s", delay.Round(time.Millisecond))
		if delay >= 300*time.Millisecond {
			t.Fatalf("first visible message report delay = %s, want <300ms", delay.Round(time.Millisecond))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first visible message report")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}
}

// timedTranscriptBackend keeps a tool open long enough to prove the daemon
// records event occurrence time rather than giving a whole flush batch one
// server insertion time.
type timedTranscriptBackend struct{}

func (timedTranscriptBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		msgCh <- agent.Message{Type: agent.MessageToolUse, Tool: "bash", CallID: "timed"}
		time.Sleep(10 * time.Millisecond)
		msgCh <- agent.Message{Type: agent.MessageToolResult, Tool: "bash", CallID: "timed", Output: "ok"}
		close(msgCh)
		resCh <- agent.Result{Status: "completed", Output: "done"}
		close(resCh)
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrain_ReportsPerEventTimestamps(t *testing.T) {
	t.Parallel()

	d, rec := newTranscriptRecorder(t)
	if _, _, err := d.executeAndDrain(context.Background(), timedTranscriptBackend{}, "p", agent.ExecOptions{}, slog.Default(), "task-timing", new(atomic.Int32)); err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("reported %d messages, want tool use and result: %+v", len(got), got)
	}
	if got[0].CreatedAt.IsZero() || !got[1].CreatedAt.After(got[0].CreatedAt) {
		t.Fatalf("event timestamps = [%s, %s], want distinct ordered times", got[0].CreatedAt, got[1].CreatedAt)
	}
}

type chunkedTranscriptBackend struct {
	thinkingSecondStartedAt chan time.Time
	textSecondStartedAt     chan time.Time
}

func (b *chunkedTranscriptBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		msgCh <- agent.Message{Type: agent.MessageThinking, Content: "think one "}
		time.Sleep(20 * time.Millisecond)
		b.thinkingSecondStartedAt <- time.Now()
		msgCh <- agent.Message{Type: agent.MessageThinking, Content: "think two"}

		msgCh <- agent.Message{Type: agent.MessageText, Content: "text one "}
		time.Sleep(20 * time.Millisecond)
		b.textSecondStartedAt <- time.Now()
		msgCh <- agent.Message{Type: agent.MessageText, Content: "text two"}
		close(msgCh)
		resCh <- agent.Result{Status: "completed", Output: "done"}
		close(resCh)
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrain_ReportsFirstBufferedChunkTimestamp(t *testing.T) {
	t.Parallel()

	backend := &chunkedTranscriptBackend{
		thinkingSecondStartedAt: make(chan time.Time, 1),
		textSecondStartedAt:     make(chan time.Time, 1),
	}
	d, rec := newTranscriptRecorder(t)
	if _, _, err := d.executeAndDrain(context.Background(), backend, "p", agent.ExecOptions{}, slog.Default(), "task-chunks", new(atomic.Int32)); err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}

	got := rec.snapshot()
	var thinking, text strings.Builder
	var thinkingAt, textAt time.Time
	for _, message := range got {
		switch message.Type {
		case "thinking":
			thinking.WriteString(message.Content)
			if thinkingAt.IsZero() {
				thinkingAt = message.CreatedAt
			}
		case "text":
			text.WriteString(message.Content)
			if textAt.IsZero() {
				textAt = message.CreatedAt
			}
		}
	}
	if thinking.String() != "think one think two" {
		t.Fatalf("thinking content = %q, want complete ordered chunks; messages=%+v", thinking.String(), got)
	}
	if boundary := <-backend.thinkingSecondStartedAt; !thinkingAt.Before(boundary) {
		t.Fatalf("first thinking created_at = %s, want before second chunk started at %s", thinkingAt, boundary)
	}
	if text.String() != "text one text two" {
		t.Fatalf("text content = %q, want complete ordered chunks; messages=%+v", text.String(), got)
	}
	if boundary := <-backend.textSecondStartedAt; !textAt.Before(boundary) {
		t.Fatalf("first text created_at = %s, want before second chunk started at %s", textAt, boundary)
	}
}

type orderedTranscriptBackend struct{}

func (orderedTranscriptBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		msgCh <- agent.Message{Type: agent.MessageText, Content: "preface"}
		msgCh <- agent.Message{Type: agent.MessageThinking, Content: "reasoning"}
		msgCh <- agent.Message{Type: agent.MessageText, Content: "answer one"}
		msgCh <- agent.Message{Type: agent.MessageToolUse, Tool: "read", CallID: "ordered"}
		msgCh <- agent.Message{Type: agent.MessageToolResult, Tool: "read", CallID: "ordered", Output: "ok"}
		msgCh <- agent.Message{Type: agent.MessageText, Content: "answer two"}
		msgCh <- agent.Message{Type: agent.MessageError, Content: "warning"}
		msgCh <- agent.Message{Type: agent.MessageText, Content: "answer three"}
		close(msgCh)
		resCh <- agent.Result{Status: "completed", Output: "done"}
		close(resCh)
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrain_PreservesTranscriptArrivalOrderAcrossMessageTypes(t *testing.T) {
	t.Parallel()

	d, rec := newTranscriptRecorder(t)
	if _, _, err := d.executeAndDrain(context.Background(), orderedTranscriptBackend{}, "p", agent.ExecOptions{}, slog.Default(), "task-order", new(atomic.Int32)); err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}

	got := rec.snapshot()
	want := []struct {
		typ     string
		content string
	}{
		{typ: "text", content: "preface"},
		{typ: "thinking", content: "reasoning"},
		{typ: "text", content: "answer one"},
		{typ: "tool_use"},
		{typ: "tool_result"},
		{typ: "text", content: "answer two"},
		{typ: "error", content: "warning"},
		{typ: "text", content: "answer three"},
	}
	if len(got) != len(want) {
		t.Fatalf("reported %d messages, want %d in arrival order: %+v", len(got), len(want), got)
	}
	for i, expected := range want {
		if got[i].Seq != i+1 || got[i].Type != expected.typ || got[i].Content != expected.content {
			t.Fatalf("message %d = %+v, want seq=%d type=%q content=%q", i, got[i], i+1, expected.typ, expected.content)
		}
	}
}

// TestExecuteAndDrain_SeqContinuesAcrossRetry pins the transcript's ordering
// key: the server sorts a task's messages by seq alone, so a same-task resume
// retry must keep numbering upwards instead of restarting at 1 and
// interleaving its rows with the failed attempt's.
func TestExecuteAndDrain_SeqContinuesAcrossRetry(t *testing.T) {
	t.Parallel()

	d, rec := newTranscriptRecorder(t)
	fb := &transcriptBackend{}
	var msgSeq atomic.Int32

	result, _, err := d.executeAndDrain(context.Background(), fb, "p", agent.ExecOptions{ResumeSessionID: "stale"}, slog.Default(), "task-seq", &msgSeq)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("expected failed first result, got %+v", result)
	}

	result, _, err = d.executeAndDrain(context.Background(), fb, "p", agent.ExecOptions{}, slog.Default(), "task-seq", &msgSeq)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("expected completed retry result, got %+v", result)
	}

	got := rec.snapshot()
	if len(got) != 4 {
		t.Fatalf("expected 4 messages total, got %d: %+v", len(got), got)
	}
	for i, m := range got {
		if m.Seq != i+1 {
			t.Fatalf("expected strictly ascending seq across the retry, got %+v", got)
		}
	}
}

// sessionBackend hands out a pre-built session, leaving the test in control
// of message and result delivery.
type sessionBackend struct {
	session *agent.Session
}

func (b sessionBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	return b.session, nil
}

// TestExecuteAndDrain_ContextCancelled_FlushesPendingTranscript pins the same
// flush-before-return contract for the drainCtx.Done() terminal: when a
// cancellation (or timeout/watchdog) ends the run with a message still
// pending, executeAndDrain must not return until that tail is reported —
// otherwise runTask fails-and-broadcasts while the last batch is in flight.
func TestExecuteAndDrain_ContextCancelled_FlushesPendingTranscript(t *testing.T) {
	t.Parallel()

	d, rec := newTranscriptRecorder(t)

	// Unbuffered: the send below returns only once the drain loop has
	// consumed the message. The result channel never delivers, so only the
	// context cancellation can end the drain.
	msgCh := make(chan agent.Message)
	b := sessionBackend{session: &agent.Session{Messages: msgCh, Result: make(chan agent.Result)}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	type ret struct {
		result agent.Result
		err    error
	}
	retCh := make(chan ret, 1)
	go func() {
		result, _, err := d.executeAndDrain(ctx, b, "p", agent.ExecOptions{}, slog.Default(), "task-cancel-flush", new(atomic.Int32))
		retCh <- ret{result, err}
	}()

	msgCh <- agent.Message{Type: agent.MessageText, Content: "pending tail"}
	cancel()

	r := <-retCh
	if r.err != nil {
		t.Fatalf("executeAndDrain: %v", r.err)
	}
	if r.result.Status != "cancelled" {
		t.Fatalf("expected status=cancelled, got %q (err=%q)", r.result.Status, r.result.Error)
	}

	got := rec.snapshot()
	if len(got) != 1 || got[0].Type != "text" || got[0].Content != "pending tail" {
		t.Fatalf("expected the pending tail flushed before the cancelled return, got %+v", got)
	}
}

// blockingBackend returns a Session whose Result channel is never written to,
// so executeAndDrain can only exit via the drainCtx.Done() path.
type blockingBackend struct{}

func (blockingBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result)
	close(msgCh)
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

type countedRunningBackend struct {
	release <-chan struct{}
}

func (b countedRunningBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message)
	resCh := make(chan agent.Result, 1)
	go func() {
		<-b.release
		close(msgCh)
		resCh <- agent.Result{Status: "completed"}
	}()
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrainTracksRunningTaskCount(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := d.executeAndDrain(
			context.Background(),
			countedRunningBackend{release: release},
			"p",
			agent.ExecOptions{},
			slog.Default(),
			"task-counted-running",
			new(atomic.Int32),
		)
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for d.runningTasks.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := d.runningTasks.Load(); got != 1 {
		t.Fatalf("running task count while backend is live = %d, want 1", got)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executeAndDrain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("executeAndDrain did not return after backend release")
	}
	if got := d.runningTasks.Load(); got != 0 {
		t.Fatalf("running task count after backend exit = %d, want 0", got)
	}
}

func TestExecuteAndDrain_ContextCancelled_ReportsCancelled(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, _, err := d.executeAndDrain(ctx, blockingBackend{}, "p", agent.ExecOptions{}, slog.Default(), "t", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "cancelled" {
		t.Fatalf("expected status=cancelled when parent ctx is cancelled, got %q (err=%q)", result.Status, result.Error)
	}
}

// idleWatchdogBackend simulates the MUL-2225 hang: emit one message to mark
// activity, then go silent forever. With a short AgentIdleWatchdog, the
// watchdog should fire and short-circuit executeAndDrain. With no wall-clock
// cap (opts.Timeout = 0) the drain loop imposes no deadline of its own, so the
// idle watchdog is the only thing that ends this otherwise-forever-silent run.
type idleWatchdogBackend struct {
	emitOne bool // when true, emit one message before going silent; when false, never emit anything
}

func (b idleWatchdogBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message, 1)
	resCh := make(chan agent.Result)
	if b.emitOne {
		msgCh <- agent.Message{Type: agent.MessageText, Content: "hello"}
	}
	// Deliberately do NOT close msgCh and never write to resCh — this models
	// a backend whose subprocess is hung and will never naturally complete.
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// TestIdleWatchdogTickInterval pins the ceiling, which is the reason the helper
// exists: at window/2 alone the default 2h budget would only be polled hourly,
// so a stuck run would hold its slot for up to 3h, and the overshoot would grow
// with every increase to the budget instead of staying bounded.
func TestIdleWatchdogTickInterval(t *testing.T) {
	tests := []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		// Tiny budgets keep the raw half-window so the watchdog tests below,
		// which use millisecond windows, still see it fire within a few ticks.
		{name: "millisecond test window halves", window: 50 * time.Millisecond, want: 25 * time.Millisecond},
		{name: "half rate at one minute", window: time.Minute, want: 30 * time.Second},
		{name: "half rate below the ceiling", window: 8 * time.Minute, want: 4 * time.Minute},
		{name: "ceiling engages exactly at its double", window: 10 * time.Minute, want: idleWatchdogMaxTick},
		{name: "ceiling caps the default budget", window: 2 * time.Hour, want: idleWatchdogMaxTick},
		{name: "ceiling holds for very large budgets", window: 24 * time.Hour, want: idleWatchdogMaxTick},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idleWatchdogTickInterval(tt.window); got != tt.want {
				t.Fatalf("idleWatchdogTickInterval(%s) = %s, want %s", tt.window, got, tt.want)
			}
		})
	}
}

// TestIdleWatchdogTickInterval_NeverPollsFasterThanThirtySecondsInProduction
// keeps the guarantee the removed 30 s floor was written for. The floor itself
// was unreachable (window >= 1 min implies window/2 >= 30 s), so this asserts
// the property directly against every production-shaped budget instead of
// re-introducing a branch that can never run.
func TestIdleWatchdogTickInterval_NeverPollsFasterThanThirtySecondsInProduction(t *testing.T) {
	for window := time.Minute; window <= 4*time.Hour; window += time.Second {
		if got := idleWatchdogTickInterval(window); got < 30*time.Second {
			t.Fatalf("idleWatchdogTickInterval(%s) = %s, want >= 30s", window, got)
		}
	}
}

func TestExecuteAndDrain_IdleWatchdog_FiresOnInactivity(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	result, _, err := d.executeAndDrain(ctx, idleWatchdogBackend{emitOne: true}, "p", agent.ExecOptions{}, slog.Default(), "t-idle", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "idle_watchdog" {
		t.Fatalf("expected status=idle_watchdog, got %q (err=%q)", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "idle watchdog") {
		t.Fatalf("expected error to mention idle watchdog, got %q", result.Error)
	}
	// The watchdog should fire within a few ticks (interval = window/2 with
	// no floor for sub-minute windows). 5× window is generous and keeps the
	// test from racing in slow CI.
	if elapsed := time.Since(start); elapsed > 5*d.cfg.AgentIdleWatchdog {
		t.Fatalf("watchdog took too long to fire: %s (window=%s)", elapsed, d.cfg.AgentIdleWatchdog)
	}
}

// waitForAgentMessageBackend holds Execute until the wrapped backend has
// processed a selected protocol message, then hands the full stream to the
// daemon. It makes watchdog cancellation tests deterministic without changing
// the production watchdog window or relying on child-process scheduling speed.
type waitForAgentMessageBackend struct {
	agent.Backend
	match   func(agent.Message) bool
	onMatch func()
}

func (b waitForAgentMessageBackend) Execute(ctx context.Context, prompt string, opts agent.ExecOptions) (*agent.Session, error) {
	session, err := b.Backend.Execute(ctx, prompt, opts)
	if err != nil {
		return nil, err
	}
	messages := make(chan agent.Message, 256)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-session.Messages:
			if !ok {
				return nil, errors.New("wrapped backend closed before the expected message")
			}
			messages <- msg
			if !b.match(msg) {
				continue
			}
			if b.onMatch != nil {
				b.onMatch()
			}
			go func() {
				defer close(messages)
				for msg := range session.Messages {
					messages <- msg
				}
			}()
			return &agent.Session{
				ToolActivity:             session.ToolActivity,
				InterruptBackgroundTools: session.InterruptBackgroundTools,
				TerminalObserved:         session.TerminalObserved,
				Messages:                 messages,
				Result:                   session.Result,
			}, nil
		}
	}
}

func TestExecuteAndDrain_IdleWatchdog_FiresWhenNoMessageEverArrives(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// emitOne=false models a backend that hangs before sending any message.
	// lastActivityAt is initialised at executeAndDrain entry, so the same
	// window applies even with zero traffic.
	result, _, err := d.executeAndDrain(ctx, idleWatchdogBackend{emitOne: false}, "p", agent.ExecOptions{}, slog.Default(), "t-idle-zero", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "idle_watchdog" {
		t.Fatalf("expected status=idle_watchdog when backend never emits, got %q (err=%q)", result.Status, result.Error)
	}
}

func TestExecuteAndDrain_IdleWatchdog_UsesPerRunOverride(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	result, _, err := d.executeAndDrain(
		ctx,
		idleWatchdogBackend{emitOne: true},
		"p",
		agent.ExecOptions{IdleWatchdogTimeout: 50 * time.Millisecond},
		slog.Default(),
		"t-idle-override",
		new(atomic.Int32),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "idle_watchdog" {
		t.Fatalf("expected status=idle_watchdog, got %q (err=%q)", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "50ms") {
		t.Fatalf("expected error to report the per-run threshold, got %q", result.Error)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("per-run watchdog override did not fire promptly: %s", elapsed)
	}
}

func TestExecuteAndDrain_IdleWatchdog_GlobalDisableWinsOverPerRunOverride(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 0

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	result, _, err := d.executeAndDrain(
		ctx,
		idleWatchdogBackend{emitOne: true},
		"p",
		agent.ExecOptions{IdleWatchdogTimeout: 20 * time.Millisecond},
		slog.Default(),
		"t-idle-global-off",
		new(atomic.Int32),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "cancelled" {
		t.Fatalf("global watchdog disable must win; got status=%q (err=%q)", result.Status, result.Error)
	}
}

func TestExecuteAndDrain_IdleWatchdog_PerRunOverrideCannotExtendGlobalWindow(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	result, _, err := d.executeAndDrain(
		ctx,
		idleWatchdogBackend{emitOne: true},
		"p",
		agent.ExecOptions{IdleWatchdogTimeout: 500 * time.Millisecond},
		slog.Default(),
		"t-idle-global-bound",
		new(atomic.Int32),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "idle_watchdog" {
		t.Fatalf("expected status=idle_watchdog, got %q (err=%q)", result.Status, result.Error)
	}
	if !strings.Contains(result.Error, "50ms") {
		t.Fatalf("provider override must not extend the global threshold, got %q", result.Error)
	}
}

func TestExecuteAndDrain_IdleWatchdog_DisabledWhenZero(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	// Default zero value — watchdog disabled. Without a parent cancel the
	// blockingBackend would otherwise hang the test, so we cancel after a
	// short delay to confirm the run does NOT terminate as idle_watchdog.
	d.cfg.AgentIdleWatchdog = 0

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(80*time.Millisecond, cancel)

	result, _, err := d.executeAndDrain(ctx, idleWatchdogBackend{emitOne: true}, "p", agent.ExecOptions{}, slog.Default(), "t-idle-off", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status == "idle_watchdog" {
		t.Fatalf("watchdog should not fire when AgentIdleWatchdog=0, got status=%q", result.Status)
	}
	if result.Status != "cancelled" {
		t.Fatalf("expected status=cancelled (parent ctx fired), got %q", result.Status)
	}
}

func TestExecuteAndDrain_IdleWatchdog_HappyPathDoesNotFire(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 200 * time.Millisecond

	// fakeBackend completes immediately with a normal result, well inside the
	// idle window. The watchdog must not corrupt the disposition.
	fb := &fakeBackend{
		results: []agent.Result{
			{Status: "completed", Output: "done"},
		},
	}

	result, _, err := d.executeAndDrain(context.Background(), fb, "p", agent.ExecOptions{}, slog.Default(), "t-idle-happy", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("expected status=completed on happy path, got %q (err=%q)", result.Status, result.Error)
	}
	if result.Output != "done" {
		t.Fatalf("expected output preserved, got %q", result.Output)
	}
}

// longToolCallBackend simulates a legitimate long-running tool call (e.g.
// `npm install`, `docker build`, full test suite). The backend emits a
// tool_use, stays silent past the idle window while the tool runs, then emits
// a tool_result and completes. This is the false-positive case the watchdog
// must NOT misfire on: an in-flight tool call is forward progress, not a hang.
type longToolCallBackend struct {
	toolSilence time.Duration // how long to stay silent between tool_use and tool_result
}

func (b longToolCallBackend) Execute(ctx context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message, 4)
	resCh := make(chan agent.Result, 1)

	msgCh <- agent.Message{
		Type:   agent.MessageToolUse,
		Tool:   "Bash",
		CallID: "call-1",
		Input:  map[string]any{"cmd": "npm install"},
	}

	go func() {
		select {
		case <-time.After(b.toolSilence):
		case <-ctx.Done():
			// Watchdog cancelled us — propagate so the caller sees aborted.
			resCh <- agent.Result{Status: "aborted", Error: ctx.Err().Error()}
			close(msgCh)
			close(resCh)
			return
		}
		msgCh <- agent.Message{
			Type:   agent.MessageToolResult,
			Tool:   "Bash",
			CallID: "call-1",
			Output: "installed 142 packages",
		}
		msgCh <- agent.Message{Type: agent.MessageText, Content: "done"}
		close(msgCh)
		resCh <- agent.Result{Status: "completed", Output: "done"}
		close(resCh)
	}()

	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrain_IdleWatchdog_DoesNotFireDuringInFlightToolCall(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	// 50 ms window; tool stays silent for ~4× the window. Without the
	// in-flight-tool gate, the watchdog would fire and the run would come
	// back as idle_watchdog. With the gate, it must complete normally.
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond

	result, _, err := d.executeAndDrain(
		context.Background(),
		longToolCallBackend{toolSilence: 200 * time.Millisecond},
		"p",
		agent.ExecOptions{},
		slog.Default(),
		"t-long-tool",
		new(atomic.Int32),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status == "idle_watchdog" {
		t.Fatalf("watchdog must not fire while a tool_use is in flight, got status=%q (err=%q)", result.Status, result.Error)
	}
	if result.Status != "completed" {
		t.Fatalf("expected status=completed, got %q (err=%q)", result.Status, result.Error)
	}
}

func TestExecuteAndDrain_IdleWatchdog_PerRunOverrideStillUsesToolWindow(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 500 * time.Millisecond
	d.cfg.AgentToolWatchdog = 500 * time.Millisecond

	result, _, err := d.executeAndDrain(
		context.Background(),
		longToolCallBackend{toolSilence: 200 * time.Millisecond},
		"p",
		agent.ExecOptions{IdleWatchdogTimeout: 50 * time.Millisecond},
		slog.Default(),
		"t-long-tool-override",
		new(atomic.Int32),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("per-run idle override must not replace the tool window; got status=%q (err=%q)", result.Status, result.Error)
	}
}

// stuckInFlightToolBackend models a hung tool: it emits a tool_use and then
// goes silent forever — the matching tool_result never arrives, so inFlightTools
// stays at 1 (e.g. a child process that never returns). With no wall-clock cap
// (the MUL-3064 default), AgentToolWatchdog is the only thing that ends it.
type stuckInFlightToolBackend struct{}

func (stuckInFlightToolBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message, 2)
	resCh := make(chan agent.Result)
	msgCh <- agent.Message{Type: agent.MessageToolUse, Tool: "Bash", CallID: "c1"}
	// Deliberately leave msgCh open, never emit tool_result, never write resCh.
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrain_IdleWatchdog_FiresOnStuckInFlightTool(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	// The normal idle window would be skipped while a tool is in flight; the
	// AgentToolWatchdog budget is what must fire here.
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond
	d.cfg.AgentToolWatchdog = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	start := time.Now()
	result, _, err := d.executeAndDrain(ctx, stuckInFlightToolBackend{}, "p", agent.ExecOptions{}, slog.Default(), "t-stuck-tool", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "idle_watchdog" {
		t.Fatalf("expected status=idle_watchdog for a hung in-flight tool, got %q (err=%q)", result.Status, result.Error)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("tool watchdog took too long to fire: %s (window=%s)", elapsed, d.cfg.AgentToolWatchdog)
	}
}

// tailIdleAfterToolBackend exercises the boundary case: a tool call completes,
// and THEN the backend goes silent without ever finishing. After the
// tool_result lands, in-flight count returns to zero and lastActivityAt is
// fresh; the watchdog should fire exactly one window later, not earlier.
type tailIdleAfterToolBackend struct{}

func (tailIdleAfterToolBackend) Execute(_ context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
	msgCh := make(chan agent.Message, 4)
	resCh := make(chan agent.Result)
	msgCh <- agent.Message{Type: agent.MessageToolUse, Tool: "Bash", CallID: "c1"}
	msgCh <- agent.Message{Type: agent.MessageToolResult, Tool: "Bash", CallID: "c1", Output: "ok"}
	// Deliberately leave msgCh open and never write to resCh.
	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

func TestExecuteAndDrain_IdleWatchdog_FiresAfterToolResultIfBackendStaysSilent(t *testing.T) {
	t.Parallel()

	d := newTestDaemon(t)
	d.cfg.AgentIdleWatchdog = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	result, _, err := d.executeAndDrain(ctx, tailIdleAfterToolBackend{}, "p", agent.ExecOptions{}, slog.Default(), "t-tail-idle", new(atomic.Int32))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "idle_watchdog" {
		t.Fatalf("expected status=idle_watchdog after tool_result with no further activity, got %q (err=%q)", result.Status, result.Error)
	}
}

// ensureRepoReady must refresh `workspaceState.settings` on every checkout —
// even when the repo cache already holds the URL. The /repo/checkout handler
// reads `workspaceCoAuthoredByEnabled` right after, and the 30s workspace
// sync tick is too slow to make a freshly-flipped GitHub toggle feel live.
// PR #2847 review by Emacs caught this fast-path regression; the test
// asserts the cached-repo path still issues exactly one refresh.
func TestEnsureRepoReadyCachedRepoStillRefreshesSettings(t *testing.T) {
	t.Parallel()

	sourceRepo := createDaemonTestRepo(t)
	var refreshCalls atomic.Int32
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/workspaces/ws-1/repos" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{{URL: sourceRepo}},
			ReposVersion: "v2",
			Settings:     json.RawMessage(`{"github_enabled":false,"co_authored_by_enabled":true}`),
		})
	})
	if err := d.repoCache.Sync("ws-1", []repocache.RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("seed repo cache: %v", err)
	}
	// Workspace starts with the master switch ON. The server above will return
	// the user's just-flipped OFF state — ensureRepoReady must pick that up
	// before the handler reads workspaceCoAuthoredByEnabled.
	d.workspaces["ws-1"] = newWorkspaceState(
		"ws-1",
		nil,
		"v1",
		[]RepoData{{URL: sourceRepo}},
		json.RawMessage(`{"github_enabled":true,"co_authored_by_enabled":true}`),
	)
	if !d.workspaceCoAuthoredByEnabled("ws-1") {
		t.Fatalf("precondition: expected co-author hook enabled before checkout")
	}

	if err := d.ensureRepoReady(context.Background(), "ws-1", sourceRepo); err != nil {
		t.Fatalf("ensureRepoReady: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 refresh call on cached repo, got %d", got)
	}
	if d.workspaceCoAuthoredByEnabled("ws-1") {
		t.Fatalf("expected co-author hook disabled after server-side toggle; daemon used stale workspaceState.settings via cache fast path")
	}
}

func TestEnsureRepoReadyTrimsURL(t *testing.T) {
	t.Parallel()

	sourceRepo := createDaemonTestRepo(t)
	var refreshCalls atomic.Int32
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/workspaces/ws-1/repos" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{{URL: sourceRepo}},
			ReposVersion: "v2",
		})
	})
	if err := d.repoCache.Sync("ws-1", []repocache.RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("seed repo cache: %v", err)
	}
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "v1", []RepoData{{URL: sourceRepo}}, nil)

	// URL with trailing whitespace should still resolve to the cached repo.
	if err := d.ensureRepoReady(context.Background(), "ws-1", "  "+sourceRepo+"  "); err != nil {
		t.Fatalf("ensureRepoReady with padded URL: %v", err)
	}
	// Even on cache hit we refresh settings once so toggle flips feel live.
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("expected 1 refresh call for trimmed URL, got %d", got)
	}
}

func TestEnsureRepoReadyRefreshesOnMiss(t *testing.T) {
	t.Parallel()

	sourceRepo := createDaemonTestRepo(t)
	var refreshCalls atomic.Int32
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/workspaces/ws-1/repos" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{{URL: sourceRepo}},
			ReposVersion: "v2",
		})
	})
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)

	if err := d.ensureRepoReady(context.Background(), "ws-1", sourceRepo); err != nil {
		t.Fatalf("ensureRepoReady: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("expected 1 refresh call, got %d", got)
	}
	if d.repoCache.Lookup("ws-1", sourceRepo) == "" {
		t.Fatal("expected repo to be cached after refresh")
	}
}

// A project github_repo URL that the workspace itself does not bind must still
// be allowed for `multica repo checkout` after registerTaskRepos runs. Without
// this, the new project-repos-override-workspace-repos behavior would surface
// repos in the meta-skill that the agent then can't actually clone.
func TestRegisterTaskReposAllowsProjectOnlyURL(t *testing.T) {
	t.Parallel()

	sourceRepo := createDaemonTestRepo(t)
	var refreshCalls atomic.Int32
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		// If the workspace endpoint is hit it returns an empty list — the
		// project-only URL must NOT depend on this for allowlist membership.
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{},
			ReposVersion: "v1",
		})
	})
	// Workspace has zero workspace-bound repos; the project resource gives us
	// the only repo URL the agent should be able to check out.
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)

	d.registerTaskRepos("ws-1", "task-project-only", []RepoData{{URL: sourceRepo}})

	// The async clone goroutine in registerTaskRepos may not have finished;
	// poll briefly until the cache is populated so the test isn't racy.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.repoCache.Lookup("ws-1", sourceRepo) != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.repoCache.Lookup("ws-1", sourceRepo) == "" {
		t.Fatalf("expected repo to be cached after registerTaskRepos, but Lookup returned empty")
	}

	if !d.workspaceRepoAllowed("ws-1", sourceRepo) {
		t.Fatal("expected project repo to pass workspaceRepoAllowed")
	}

	if err := d.ensureRepoReady(context.Background(), "ws-1", sourceRepo); err != nil {
		t.Fatalf("ensureRepoReady: %v", err)
	}
	// ensureRepoReady refreshes settings on every call (RFC MUL-2414 §4.8; PR
	// #2847 review by Emacs) so a freshly-flipped GitHub toggle takes effect
	// without waiting for the 30s sync tick. We expect exactly one refresh —
	// the project-only URL still skips re-cloning because the cache is warm.
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("expected 1 workspace-repos refresh (settings live-refresh on checkout), got %d", got)
	}
}

// Confirms that a workspace refresh wiping allowedRepoURLs does not also wipe
// task-scoped URLs (project repos). Without the separate taskRepoURLs map a
// concurrent refresh would silently revoke project-only URLs and the next
// checkout would fail.
func TestRegisterTaskReposSurvivesWorkspaceRefresh(t *testing.T) {
	t.Parallel()

	sourceRepo := createDaemonTestRepo(t)
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{},
			ReposVersion: "v2",
		})
	})
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)
	d.registerTaskRepos("ws-1", "task-refresh", []RepoData{{URL: sourceRepo}})

	// Wait for the registration to populate the cache.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && d.repoCache.Lookup("ws-1", sourceRepo) == "" {
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := d.refreshWorkspaceRepos(context.Background(), "ws-1"); err != nil {
		t.Fatalf("refreshWorkspaceRepos: %v", err)
	}

	if !d.workspaceRepoAllowed("ws-1", sourceRepo) {
		t.Fatal("project repo URL was wiped by workspace refresh")
	}
}

func TestTaskRepoDefaultRefScopedByTask(t *testing.T) {
	t.Parallel()

	const repoURL = "https://github.com/example/shared"
	d := &Daemon{
		workspaces: map[string]*workspaceState{
			"ws-1": newWorkspaceState("ws-1", nil, "", nil, nil),
		},
	}

	d.registerTaskRepos("ws-1", "task-a", []RepoData{
		{URL: repoURL, Ref: "release/a"},
		{URL: repoURL, Ref: "late-duplicate"},
	})
	d.registerTaskRepos("ws-1", "task-b", []RepoData{{URL: repoURL, Ref: "release/b"}})

	if got := d.taskRepoDefaultRef("ws-1", "task-a", repoURL); got != "release/a" {
		t.Fatalf("task-a default ref = %q, want release/a", got)
	}
	if got := d.taskRepoDefaultRef("ws-1", "task-b", repoURL); got != "release/b" {
		t.Fatalf("task-b default ref = %q, want release/b", got)
	}

	d.clearTaskRepoRefs("ws-1", "task-a")

	if got := d.taskRepoDefaultRef("ws-1", "task-a", repoURL); got != "" {
		t.Fatalf("task-a default ref after cleanup = %q, want empty", got)
	}
	if got := d.taskRepoDefaultRef("ws-1", "task-b", repoURL); got != "release/b" {
		t.Fatalf("task-b default ref after task-a cleanup = %q, want release/b", got)
	}
}

func TestEnsureRepoReadyReturnsNotConfigured(t *testing.T) {
	t.Parallel()

	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{},
			ReposVersion: "v1",
		})
	})
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)

	err := d.ensureRepoReady(context.Background(), "ws-1", "git@example.com:team/api.git")
	if !errors.Is(err, ErrRepoNotConfigured) {
		t.Fatalf("expected ErrRepoNotConfigured, got %v", err)
	}
}

func TestEnsureRepoReadyReportsSyncFailure(t *testing.T) {
	t.Parallel()

	missingRepo := filepath.Join(t.TempDir(), "missing-repo")
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{{URL: missingRepo}},
			ReposVersion: "v1",
		})
	})
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)

	err := d.ensureRepoReady(context.Background(), "ws-1", missingRepo)
	if err == nil || !strings.Contains(err.Error(), "repo is configured but not synced:") {
		t.Fatalf("expected sync failure error, got %v", err)
	}
	if got := d.workspaceLastRepoSyncErr("ws-1"); got == "" {
		t.Fatal("expected lastRepoSyncErr to be recorded")
	}
}

// repoRefreshWaitContext reports when ensureRepoReady reaches the cancellable
// lock wait, after recording whether the repo was cached on entry.
type repoRefreshWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan<- struct{}
}

func (c *repoRefreshWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { c.waiting <- struct{}{} })
	return c.Context.Done()
}

func TestEnsureRepoReadyConcurrentMissRefreshesOnce(t *testing.T) {
	t.Parallel()

	sourceRepo := createDaemonTestRepo(t)
	var refreshCalls atomic.Int32
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/daemon/workspaces/ws-1/repos" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		json.NewEncoder(w).Encode(WorkspaceReposResponse{
			WorkspaceID:  "ws-1",
			Repos:        []RepoData{{URL: sourceRepo}},
			ReposVersion: "v2",
		})
	})
	ws := newWorkspaceState("ws-1", nil, "", nil, nil)
	d.workspaces["ws-1"] = ws

	// Keep the cache cold until every caller has recorded a miss and reached
	// the lock. Merely starting goroutines also permits late warm-cache calls,
	// which intentionally refresh settings again.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.repoRefreshMu.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(ws.repoRefreshMu.Unlock)
	const concurrency = 8
	waiting := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		unlock()
		wg.Wait()
	}()
	errCh := make(chan error, concurrency)
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitCtx := &repoRefreshWaitContext{Context: ctx, waiting: waiting}
			errCh <- d.ensureRepoReady(waitCtx, "ws-1", sourceRepo)
		}()
	}
	for range concurrency {
		select {
		case <-waiting:
		case <-ctx.Done():
			t.Fatal("ensureRepoReady callers did not all reach the cold-cache lock wait")
		}
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("expected no refresh before releasing cold-cache callers, got %d", got)
	}
	unlock()
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("ensureRepoReady returned error: %v", err)
		}
	}
	// All 8 goroutines race on a cold miss; the per-workspace mutex
	// must serialize them so the server is only called once.
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 refresh call, got %d", got)
	}
}

func TestContextLockCancelsWaitWithoutConsumingToken(t *testing.T) {
	t.Parallel()

	var lock contextLock
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lock.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock error = %v, want deadline exceeded", err)
	}
	lock.Unlock()

	if err := lock.Lock(context.Background()); err != nil {
		t.Fatalf("Lock after cancelled waiter: %v", err)
	}
	lock.Unlock()
}

// reportTaskResultRecorder captures which terminal endpoint
// (.../complete or .../fail) reportTaskResult hits and the body it
// posts, so the tests can assert the disposition (success vs fail)
// independently of the rest of handleTask.
type reportTaskResultRecorder struct {
	mu      sync.Mutex
	path    string
	method  string
	payload map[string]any
}

func (r *reportTaskResultRecorder) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if len(body) > 0 {
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("decode body: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		r.mu.Lock()
		r.path = req.URL.Path
		r.method = req.Method
		r.payload = payload
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func TestReportTaskResult_CompletedHitsCompleteEndpoint(t *testing.T) {
	t.Parallel()

	rec := &reportTaskResultRecorder{}
	srv := httptest.NewServer(rec.handler(t))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	d.reportTaskResult(context.Background(), "task-1", TaskResult{
		Status:     "completed",
		Comment:    "all good",
		BranchName: "agent/foo",
		SessionID:  "ses-1",
		WorkDir:    "/tmp/foo",
	}, slog.Default())

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.path != "/api/daemon/tasks/task-1/complete" {
		t.Fatalf("expected /complete endpoint, got %s", rec.path)
	}
	if rec.payload["output"] != "all good" {
		t.Errorf("output: got %v", rec.payload["output"])
	}
	if rec.payload["branch_name"] != "agent/foo" {
		t.Errorf("branch_name: got %v", rec.payload["branch_name"])
	}
	if rec.payload["session_id"] != "ses-1" {
		t.Errorf("session_id: got %v", rec.payload["session_id"])
	}
}

func TestReportTaskResult_CancelledParentStillReportsTerminalState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		result     TaskResult
		wantSuffix string
	}{
		{
			name: "complete",
			result: TaskResult{
				Status:     "completed",
				Comment:    "all good",
				BranchName: "agent/foo",
				SessionID:  "ses-complete",
				WorkDir:    "/tmp/complete",
			},
			wantSuffix: "/complete",
		},
		{
			name: "fail",
			result: TaskResult{
				Status:        "blocked",
				Comment:       "provider unavailable",
				SessionID:     "ses-fail",
				WorkDir:       "/tmp/fail",
				FailureReason: "agent_error.provider_unavailable",
			},
			wantSuffix: "/fail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if !strings.HasSuffix(req.URL.Path, tc.wantSuffix) {
					t.Errorf("terminal callback path = %q, want suffix %q", req.URL.Path, tc.wantSuffix)
				}
				calls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
			d.reportTaskResult(ctx, "task-cancelled-parent", tc.result, slog.Default())

			if got := calls.Load(); got != 1 {
				t.Fatalf("terminal callback calls = %d, want 1", got)
			}
		})
	}
}

// Pins the GitHub multica#1952 fail-closed behaviour: a task whose
// agent run never produced a real result (blocked, cancelled, or any
// future status we forget to enumerate) MUST go through FailTask, so
// the UI never shows a green "Completed" badge for a run that didn't
// actually do anything (e.g. provider 429 / out-of-credit).
func TestReportTaskResult_NonCompletedHitsFailEndpoint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		status            string
		comment           string
		failureReasonIn   string
		wantFailureReason string
	}{
		{
			name:              "blocked with explicit reason preserves it",
			status:            "blocked",
			comment:           "rate limit reached",
			failureReasonIn:   "iteration_limit",
			wantFailureReason: "iteration_limit",
		},
		{
			// MUL-2946: when the daemon doesn't supply a refined
			// reason, the comment text is run through
			// taskfailure.Classify so the failure_reason column
			// lands in the canonical refined taxonomy instead of
			// the legacy "agent_error" coarse bucket.
			name:              "blocked without reason classifies comment as rate-limit",
			status:            "blocked",
			comment:           "rate limit reached",
			failureReasonIn:   "",
			wantFailureReason: "agent_error.provider_capacity_or_rate_limit",
		},
		{
			name:              "blocked without reason and unrecognized comment lands in agent_error.unknown",
			status:            "blocked",
			comment:           "the agent gave up for reasons we don't recognize",
			failureReasonIn:   "",
			wantFailureReason: "agent_error.unknown",
		},
		{
			name:              "cancelled defaults to cancelled reason regardless of comment",
			status:            "cancelled",
			comment:           "rate limit reached",
			failureReasonIn:   "",
			wantFailureReason: "cancelled",
		},
		{
			name:              "unknown status routes through classifier",
			status:            "weird_new_status",
			comment:           "rate limit reached",
			failureReasonIn:   "",
			wantFailureReason: "agent_error.provider_capacity_or_rate_limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &reportTaskResultRecorder{}
			srv := httptest.NewServer(rec.handler(t))
			t.Cleanup(srv.Close)

			d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
			d.reportTaskResult(context.Background(), "task-x", TaskResult{
				Status:        tc.status,
				Comment:       tc.comment,
				SessionID:     "ses-x",
				WorkDir:       "/tmp/x",
				FailureReason: tc.failureReasonIn,
			}, slog.Default())

			rec.mu.Lock()
			defer rec.mu.Unlock()
			if rec.path != "/api/daemon/tasks/task-x/fail" {
				t.Fatalf("expected /fail endpoint for status=%q, got %s", tc.status, rec.path)
			}
			if rec.payload["error"] != tc.comment {
				t.Errorf("error body: got %v", rec.payload["error"])
			}
			if got := rec.payload["failure_reason"]; got != tc.wantFailureReason {
				t.Errorf("failure_reason: got %v, want %q", got, tc.wantFailureReason)
			}
			if rec.payload["session_id"] != "ses-x" {
				t.Errorf("session_id should be forwarded on failure paths so chat resume keeps working, got %v", rec.payload["session_id"])
			}
		})
	}
}

// Regression test for the MUL-2780 incident: a short 502 burst on the
// /complete callback used to (a) drop the task at the first failure and
// (b) wrongly fall back to /fail, surfacing a successful run as red.
// With the retry helper in place, a transient 502 followed by a 200 must
// resolve via /complete without ever touching /fail.
func TestReportTaskResult_RetriesTransientCompleteThenSucceeds(t *testing.T) {
	defer noSleepRetry(t)()

	var completeCalls, failCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/complete"):
			n := completeCalls.Add(1)
			if n == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(req.URL.Path, "/fail"):
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	d.reportTaskResult(context.Background(), "task-retry", TaskResult{
		Status:  "completed",
		Comment: "ok",
	}, slog.Default())

	if got := completeCalls.Load(); got != 2 {
		t.Fatalf("expected 2 complete attempts (one 502, one 200), got %d", got)
	}
	if got := failCalls.Load(); got != 0 {
		t.Fatalf("transient 502 must not fall back to /fail (would lose successful result), got %d /fail calls", got)
	}
}

// Pins the new "don't downgrade success to failure on transient errors"
// rule: when /complete is 502 across the entire retry schedule, we must
// NOT fall through to /fail — that would surface a real success as a
// failure in the UI. The task is left in running for a future recovery
// path to pick up.
func TestReportTaskResult_TransientCompleteExhaustedDoesNotFallback(t *testing.T) {
	defer noSleepRetry(t)()

	prevSchedule := defaultTerminalRetrySchedule
	defaultTerminalRetrySchedule = []time.Duration{time.Nanosecond, time.Nanosecond}
	t.Cleanup(func() { defaultTerminalRetrySchedule = prevSchedule })

	var completeCalls, failCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/complete"):
			completeCalls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		case strings.HasSuffix(req.URL.Path, "/fail"):
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	d.reportTaskResult(context.Background(), "task-stuck", TaskResult{
		Status:  "completed",
		Comment: "ok",
	}, slog.Default())

	if got := completeCalls.Load(); got != int32(len(defaultTerminalRetrySchedule)+1) {
		t.Fatalf("expected %d complete attempts, got %d", len(defaultTerminalRetrySchedule)+1, got)
	}
	if got := failCalls.Load(); got != 0 {
		t.Fatalf("exhausted transient retries must NOT fall back to /fail; got %d /fail calls", got)
	}
}

// A permanent response can become recoverable after a daemon/server upgrade
// or credential refresh. Preserve the successful result instead of replacing
// it with a synthetic failure payload.
func TestReportTaskResult_PermanentCompleteDoesNotReplaceOriginal(t *testing.T) {
	defer noSleepRetry(t)()

	var completeCalls, failCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/complete"):
			completeCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		case strings.HasSuffix(req.URL.Path, "/fail"):
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	d.reportTaskResult(context.Background(), "task-bad", TaskResult{
		Status:  "completed",
		Comment: "ok",
	}, slog.Default())

	if got := completeCalls.Load(); got != 1 {
		t.Fatalf("permanent 400 should not retry, got %d complete attempts", got)
	}
	if got := failCalls.Load(); got != 0 {
		t.Fatalf("permanent /complete must not replace the original with /fail, got %d calls", got)
	}
}

func TestReportTaskResult_CancelledParentStillPreservesPermanentCompletion(t *testing.T) {
	defer noSleepRetry(t)()

	var completeCalls, failCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/complete"):
			completeCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		case strings.HasSuffix(req.URL.Path, "/fail"):
			failCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := &Daemon{client: NewClient(srv.URL), logger: slog.Default()}
	d.reportTaskResult(ctx, "task-cancelled-fallback", TaskResult{
		Status:  "completed",
		Comment: "ok",
	}, slog.Default())

	if got := completeCalls.Load(); got != 1 {
		t.Fatalf("complete calls = %d, want 1", got)
	}
	if got := failCalls.Load(); got != 0 {
		t.Fatalf("fallback fail calls = %d, want 0", got)
	}
}

func TestHandleTask_BareErrorReportsFailureWithCancelledParent(t *testing.T) {
	t.Parallel()

	var failCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/fail") {
			t.Errorf("unexpected daemon call: %s %s", req.Method, req.URL.Path)
		}
		failCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		runtimeIndex:       map[string]Runtime{"rt-1": {ID: "rt-1", Provider: "codex"}},
		cancelPollInterval: time.Hour,
	}
	d.runner = taskRunnerFunc(func(runCtx context.Context, _ Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		if !errors.Is(runCtx.Err(), context.Canceled) {
			t.Errorf("runner context error = %v, want context.Canceled", runCtx.Err())
		}
		return TaskResult{}, errors.New("runner exited during shutdown")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.handleTask(ctx, Task{ID: "task-bare-error", RuntimeID: "rt-1"}, 0)

	if got := failCalls.Load(); got != 1 {
		t.Fatalf("fail callback calls = %d, want 1", got)
	}
}

// TestHandleTask_UntrackedRuntimeFailsBackForRetry covers the window between a
// batch claim leaving with a runtime ID and the claimed task arriving here.
//
// A below-minimum demotion or a drift convergence to zero drops runtime rows
// without coordinating with in-flight claims — deliberately, since gating claims
// on that is the machinery this daemon does not have. So the task can arrive for
// a runtime the daemon no longer holds, and the unchecked map read used to hand
// it a zero-value Runtime: an empty provider that died hundreds of lines later
// as `no agent configured for provider ""`, a reason the server does not retry.
func TestHandleTask_UntrackedRuntimeFailsBackForRetry(t *testing.T) {
	t.Parallel()

	var failBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/fail") {
			t.Errorf("unexpected daemon call: %s %s", req.Method, req.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		failBody.Store(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		runtimeIndex:       map[string]Runtime{},
		cancelPollInterval: time.Hour,
	}
	d.runner = taskRunnerFunc(func(context.Context, Task, string, int, *slog.Logger) (TaskResult, error) {
		t.Error("runner ran for a runtime the daemon no longer hosts")
		return TaskResult{}, nil
	})

	d.handleTask(context.Background(), Task{ID: "task-gone-runtime", RuntimeID: "rt-demoted"}, 0)

	body, _ := failBody.Load().(map[string]any)
	if body == nil {
		t.Fatal("task was never reported as failed; the server would wait out its dispatch timeout")
	}
	if got := body["failure_reason"]; got != "runtime_offline" {
		t.Errorf("failure_reason = %v, want runtime_offline — the reason the server auto-retries on", got)
	}
}

// TestHandleTask_ReportsUsageBeforeCancel verifies that ReportTaskUsage is called
// even when the server marks the task as cancelled during the post-run status
// check. Regression test for the ordering bug where the cancel check ran before
// usage was reported, silently discarding accumulated tokens.
func TestHandleTask_ReportsUsageBeforeCancel(t *testing.T) {
	t.Parallel()

	var callOrder []string
	var mu sync.Mutex
	recordCall := func(name string) {
		mu.Lock()
		callOrder = append(callOrder, name)
		mu.Unlock()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			recordCall("start")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/usage"):
			recordCall("usage")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/status"):
			recordCall("status")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"cancelled"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"rt-1": {ID: "rt-1", Provider: "claude"}},
		cancelPollInterval: time.Hour, // effectively disable poll-cancel path; we want the post-run status check
	}

	// Inject a fake runner that returns a result with usage tokens, bypassing
	// real agent process execution.
	d.runner = taskRunnerFunc(func(_ context.Context, _ Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		return TaskResult{
			Status: "completed",
			Usage: []TaskUsageEntry{
				{Provider: "anthropic", Model: "claude-opus-4-6", InputTokens: 100, OutputTokens: 50},
			},
		}, nil
	})

	task := Task{
		ID:        "task-abc",
		RuntimeID: "rt-1",
		IssueID:   "issue-xyz",
		Agent:     &AgentData{Name: "test-agent"},
	}

	d.handleTask(context.Background(), task, 0)

	mu.Lock()
	order := make([]string, len(callOrder))
	copy(order, callOrder)
	mu.Unlock()

	// usage must appear before status in the call order.
	usageIdx, statusIdx := -1, -1
	for i, name := range order {
		switch name {
		case "usage":
			usageIdx = i
		case "status":
			statusIdx = i
		}
	}

	if usageIdx == -1 {
		t.Fatal("ReportTaskUsage was never called — usage is lost for cancelled tasks")
	}
	if statusIdx == -1 {
		t.Fatal("GetTaskStatus was never called")
	}
	if usageIdx > statusIdx {
		t.Fatalf("usage was reported AFTER status check (order: %v) — regression", order)
	}
}

// TestHandleTask_ReportsUsageWhenCancelledByPoll verifies that ReportTaskUsage is
// called even when the task is cancelled mid-execution by the poll goroutine.
// Regression test for the cancelledByPoll early-return path that previously
// discarded accumulated usage before calling ReportTaskUsage.
func TestHandleTask_ReportsUsageWhenCancelledByPoll(t *testing.T) {
	t.Parallel()

	var callOrder []string
	var mu sync.Mutex
	recordCall := func(name string) {
		mu.Lock()
		callOrder = append(callOrder, name)
		mu.Unlock()
	}

	// statusCallCount lets the poll goroutine return "cancelled" on first call
	// while still handling later calls from the post-run status check.
	var statusCallCount atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/usage"):
			recordCall("usage")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/status"):
			// First call is from the poll goroutine — return "cancelled" to
			// trigger runCancel() and close(cancelledByPoll).
			if statusCallCount.Add(1) == 1 {
				recordCall("poll-status")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"cancelled"}`))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"running"}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"rt-1": {ID: "rt-1", Provider: "claude"}},
		cancelPollInterval: 10 * time.Millisecond, // fire quickly so test is fast
	}

	// Inject a runner that blocks until runCtx is cancelled (simulating a real
	// agent being interrupted), then returns usage tokens as claude.go does.
	d.runner = taskRunnerFunc(func(runCtx context.Context, _ Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		<-runCtx.Done()
		return TaskResult{
			Status: "aborted",
			Usage: []TaskUsageEntry{
				{Provider: "anthropic", Model: "claude-opus-4-6", InputTokens: 200, OutputTokens: 80},
			},
		}, nil
	})

	task := Task{
		ID:        "task-poll",
		RuntimeID: "rt-1",
		IssueID:   "issue-poll",
		Agent:     &AgentData{Name: "test-agent"},
	}

	d.handleTask(context.Background(), task, 0)

	mu.Lock()
	order := make([]string, len(callOrder))
	copy(order, callOrder)
	mu.Unlock()

	// Verify the poll goroutine actually fired — without this assertion the test
	// could pass via the post-run GetTaskStatus check without ever taking the
	// cancelledByPoll path, making it a vacuous regression guard.
	pollStatusIdx := -1
	usageIdx := -1
	for i, name := range order {
		switch name {
		case "poll-status":
			pollStatusIdx = i
		case "usage":
			usageIdx = i
		}
	}
	if pollStatusIdx == -1 {
		t.Fatalf("poll goroutine never fired (order: %v) — cancelledByPoll path not exercised", order)
	}
	if usageIdx == -1 {
		t.Fatalf("ReportTaskUsage was never called on poll-cancelled path (order: %v) — tokens lost", order)
	}
	// poll-status must precede usage: poll fires → runCtx cancelled → runner unblocks → usage flushed.
	// If usage comes first, usage was reported before the runner was interrupted, which is impossible
	// given that the runner blocks on runCtx.Done().
	if usageIdx < pollStatusIdx {
		t.Fatalf("usage reported before poll-status (order: %v) — poll-status must come first", order)
	}
}

// TestWatchTaskCancellation_ReconcileBroadcastTriggersImmediateCheck pins the
// fix for #4665. The 5s ticker in watchTaskCancellation is too coarse to catch
// a server-side cancellation that landed during a WS disconnect: without the
// reconcile broadcast, a reconnect followed by an immediate status flip is
// invisible until the next tick fires. The test sets the ticker to a long
// interval (so a tick would fail the test) and asserts the watcher reacts to
// reconcile.broadcast() sub-second.
func TestWatchTaskCancellation_ReconcileBroadcastTriggersImmediateCheck(t *testing.T) {
	t.Parallel()

	var status atomic.Value
	status.Store("running")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"` + status.Load().(string) + `"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:    NewClient(srv.URL),
		logger:    slog.Default(),
		reconcile: newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0 // tight timing under test
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// 30s ticker: if the watcher only reacts to its own ticker the test will
	// time out at 2s, which is exactly the failure we want to catch.
	cancelled := d.watchTaskCancellation(ctx, "task-reconcile", 30*time.Second, slog.Default())

	// Simulate the gap: status flips during the WS disconnect.
	status.Store("cancelled")

	// And then the WS reconnects — broadcast must be heard.
	if !d.reconcile.broadcast() {
		t.Fatal("broadcast() returned false; expected first broadcast to fire")
	}

	select {
	case <-cancelled:
		// Expected: reconcile woke the watcher, it called GetTaskStatus,
		// saw cancelled, and closed the channel.
	case <-time.After(2 * time.Second):
		t.Fatalf("watchTaskCancellation did not react to reconcile broadcast within 2s (calls=%d)", calls.Load())
	}

	if calls.Load() == 0 {
		t.Fatal("GetTaskStatus was never called — reconcile path did not fire")
	}
}

// TestWatchTaskCancellation_ReconcileWithRunningTaskStaysAlive ensures the
// reconcile path does not falsely interrupt a still-running task. The watcher
// must call GetTaskStatus on broadcast, see status=running, and continue.
func TestWatchTaskCancellation_ReconcileWithRunningTaskStaysAlive(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:    NewClient(srv.URL),
		logger:    slog.Default(),
		reconcile: newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cancelled := d.watchTaskCancellation(ctx, "task-still-running", 30*time.Second, slog.Default())

	// Three broadcasts back-to-back: watcher should call GetTaskStatus at
	// least once and NOT close the cancelled channel.
	for i := 0; i < 3; i++ {
		d.reconcile.broadcast()
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-cancelled:
		t.Fatal("watchTaskCancellation closed cancelled channel for a running task")
	case <-time.After(200 * time.Millisecond):
		// Expected: still running.
	}

	if calls.Load() == 0 {
		t.Fatal("reconcile broadcasts did not result in any GetTaskStatus call")
	}
}

// TestWorkspaceSyncLoop_ReconcileBroadcastTriggersImmediateSync pins that the
// long-period workspace consistency timer is short-circuited by reconnect
// broadcasts. Without this, changes made during a WS disconnect stay invisible
// until the next fallback sync.
func TestWorkspaceSyncLoop_ReconcileBroadcastTriggersImmediateSync(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:     NewClient(srv.URL),
		logger:     slog.Default(),
		workspaces: make(map[string]*workspaceState),
		reconcile:  newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		d.workspaceSyncLoop(ctx)
	}()

	// Two broadcasts spaced apart. workspaceSyncLoop should re-acquire its
	// subscription after the first wake and react to the second too.
	d.reconcile.broadcast()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() < 1 {
		cancel()
		<-loopDone
		t.Fatal("workspaceSyncLoop did not react to first reconcile broadcast within 2s")
	}

	d.reconcile.broadcast()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() < 2 {
		cancel()
		<-loopDone
		t.Fatalf("workspaceSyncLoop did not react to second reconcile broadcast within 2s (calls=%d)", calls.Load())
	}

	cancel()
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("workspaceSyncLoop did not return after ctx cancel")
	}
}

func TestWorkspaceSyncLoop_WorkspaceChangeTriggersImmediateSync(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:           NewClient(srv.URL),
		logger:           slog.Default(),
		workspaces:       make(map[string]*workspaceState),
		workspaceChanges: newWorkspaceChangeSignal(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		d.workspaceSyncLoop(ctx)
	}()

	d.workspaceChanges.broadcast()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 1 {
		cancel()
		<-loopDone
		t.Fatal("workspace change did not trigger an immediate sync")
	}

	cancel()
	<-loopDone
}

// TestWorkspaceSyncLoop_DoesNotDropChangeAfterSuccessfulSync covers the
// request-changes race from MUL-4480: a real membership hint arriving within
// one second of a completed sync must trigger another sync, not be treated as
// a duplicate of the earlier read and deferred to the 30-minute fallback.
func TestWorkspaceSyncLoop_DoesNotDropChangeAfterSuccessfulSync(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:           NewClient(srv.URL),
		logger:           slog.Default(),
		workspaces:       make(map[string]*workspaceState),
		workspaceChanges: newWorkspaceChangeSignal(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		d.workspaceSyncLoop(ctx)
	}()

	waitForCalls := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && calls.Load() < want {
			time.Sleep(10 * time.Millisecond)
		}
		if got := calls.Load(); got < want {
			t.Fatalf("workspace sync calls = %d, want at least %d", got, want)
		}
	}

	d.workspaceChanges.broadcast()
	waitForCalls(1)
	deadline := time.Now().Add(time.Second)
	for !d.reloading.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("first workspace sync did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	d.reloading.Unlock()
	// The second edge lands immediately after a successful API read. It
	// represents a later commit and therefore cannot be coalesced backward.
	d.workspaceChanges.broadcast()
	waitForCalls(2)

	cancel()
	<-loopDone
}

func TestWorkspaceSyncBackoff(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: 30 * time.Second},
		{failures: 1, want: time.Minute},
		{failures: 2, want: 2 * time.Minute},
		{failures: 10, want: DefaultWorkspaceLegacySyncInterval},
	}
	for _, tt := range tests {
		if got := workspaceSyncBackoff(30*time.Second, tt.failures); got != tt.want {
			t.Fatalf("workspaceSyncBackoff(30s, %d) = %s, want %s", tt.failures, got, tt.want)
		}
	}
}

// TestWorkspaceSyncLoop_ReplaysBroadcastFromBeforeStart pins the daemon-
// startup race fix: broadcast() that fired while no one was subscribed
// MUST still wake the loop on its first subscription. Without the
// reconcile broadcaster's replay slot, this race manifests in production
// when the WS connects (and broadcasts) before workspaceSyncLoop has
// finished its first notify() call.
func TestWorkspaceSyncLoop_ReplaysBroadcastFromBeforeStart(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:     NewClient(srv.URL),
		logger:     slog.Default(),
		workspaces: make(map[string]*workspaceState),
		reconcile:  newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0

	// Broadcast BEFORE the loop subscribes — the level-triggered replay slot
	// must hold the event for the loop's first notify().
	if !d.reconcile.broadcast() {
		t.Fatal("seed broadcast suppressed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		d.workspaceSyncLoop(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 1 {
		cancel()
		<-loopDone
		t.Fatal("workspaceSyncLoop did not replay broadcast issued before its start")
	}

	cancel()
	<-loopDone
}

// TestWatchTaskCancellation_BroadcastWakesAllConcurrentWatchers ensures the
// fix scales: a single WS reconnect that fires broadcast() must drive every
// in-flight task's watcher to re-check the server. Without fan-out, a busy
// daemon with N tasks would still hit a wall of sequential 5s gaps.
func TestWatchTaskCancellation_BroadcastWakesAllConcurrentWatchers(t *testing.T) {
	t.Parallel()

	var status atomic.Value
	status.Store("running")
	var totalCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		totalCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"` + status.Load().(string) + `"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:    NewClient(srv.URL),
		logger:    slog.Default(),
		reconcile: newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	const watchers = 8
	chans := make([]<-chan struct{}, watchers)
	for i := 0; i < watchers; i++ {
		// 30s ticker: only the broadcast path can satisfy the assertion.
		chans[i] = d.watchTaskCancellation(ctx, fmt.Sprintf("task-%d", i), 30*time.Second, slog.Default())
	}

	// Server state flips during the WS gap; broadcast lands the news to
	// every watcher at once.
	status.Store("cancelled")
	if !d.reconcile.broadcast() {
		t.Fatal("broadcast suppressed")
	}

	deadline := time.After(3 * time.Second)
	for i, ch := range chans {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("watcher %d did not react to broadcast (totalCalls=%d, woke=%d)", i, totalCalls.Load(), i)
		}
	}
}

// TestWatchTaskCancellation_ReconcileDoesNotPanicAfterCtxCancel ensures
// teardown order is safe: even if a broadcast arrives after ctx is cancelled,
// the watcher must exit cleanly without panic or double-close of cancelled.
func TestWatchTaskCancellation_ReconcileDoesNotPanicAfterCtxCancel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:    NewClient(srv.URL),
		logger:    slog.Default(),
		reconcile: newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := d.watchTaskCancellation(ctx, "task-ctx-cancelled", 30*time.Second, slog.Default())

	cancel()
	// Give the watcher a beat to observe ctx.Done() and exit.
	time.Sleep(50 * time.Millisecond)

	// Broadcast after cancel — must not panic and must not unblock the
	// cancelled channel (the task was not interrupted server-side).
	for i := 0; i < 5; i++ {
		d.reconcile.broadcast()
	}

	select {
	case <-cancelled:
		t.Fatal("cancelled channel was closed after ctx cancel; server still reported running")
	case <-time.After(150 * time.Millisecond):
		// Expected: nothing happened.
	}
}

// TestWatchTaskCancellation_ReconcileRunningKeepsTickerAlive proves that a
// reconcile broadcast that sees status=running does not disturb the ticker;
// a subsequent ticker fire still detects a later cancellation.
func TestWatchTaskCancellation_ReconcileRunningKeepsTickerAlive(t *testing.T) {
	t.Parallel()

	var status atomic.Value
	status.Store("running")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"` + status.Load().(string) + `"}`))
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:    NewClient(srv.URL),
		logger:    slog.Default(),
		reconcile: newReconcileBroadcaster(),
	}
	d.reconcile.minBroadcastInterval = 0
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Short ticker so the test stays fast.
	cancelled := d.watchTaskCancellation(ctx, "task-runs-then-cancelled", 50*time.Millisecond, slog.Default())

	// First broadcast: server still reports running — watcher must NOT
	// close cancelled.
	if !d.reconcile.broadcast() {
		t.Fatal("first broadcast suppressed")
	}
	select {
	case <-cancelled:
		t.Fatal("watcher closed cancelled on a running task")
	case <-time.After(100 * time.Millisecond):
	}

	// Now flip status — ticker should pick it up within ~50ms.
	status.Store("cancelled")
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatalf("ticker did not detect cancellation after broadcast-running path (calls=%d)", calls.Load())
	}
}

// TestSanitizeAgentEnv asserts the effective env handed to the Hermes overlay
// for ${VAR} expansion drops daemon-blocklisted keys (so a blocked HOME in
// custom_env can't repoint external_dirs away from what the child sees) while
// keeping ordinary vars.
func TestSanitizeAgentEnv(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"HOME":        "/evil",
		"PATH":        "/evil/bin",
		"MULTICA_X":   "1",
		"TEAM_SKILLS": "/srv/team",
		"HERMES_HOME": "/some/home",
	}
	got := sanitizeAgentEnv(in)
	for _, blocked := range []string{"HOME", "PATH", "MULTICA_X"} {
		if _, ok := got[blocked]; ok {
			t.Errorf("blocklisted key %q must be dropped from the effective env", blocked)
		}
	}
	if got["TEAM_SKILLS"] != "/srv/team" {
		t.Errorf("ordinary var dropped: %v", got)
	}
	// HERMES_HOME is not blocklisted (it drives source-home resolution), so it
	// survives here even though the child's value is the overlay.
	if got["HERMES_HOME"] != "/some/home" {
		t.Errorf("HERMES_HOME should survive sanitization, got %v", got)
	}
	if sanitizeAgentEnv(nil) != nil {
		t.Error("nil in should yield nil out")
	}
}

// TestHandleTask_AcksCancelAfterPollCancelled verifies the daemon posts
// cancel-ack when the poll goroutine interrupts the run — by then
// runner.run has returned, so the transcript flush is complete and the
// server may settle its deferred chat finalization (#5219).
func TestHandleTask_AcksCancelAfterPollCancelled(t *testing.T) {
	t.Parallel()

	var callOrder []string
	var mu sync.Mutex
	recordCall := func(name string) {
		mu.Lock()
		callOrder = append(callOrder, name)
		mu.Unlock()
	}

	var statusCallCount atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			recordCall("cancel-ack")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/status"):
			if statusCallCount.Add(1) == 1 {
				recordCall("poll-status")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"cancelled"}`))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"running"}`))
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"rt-1": {ID: "rt-1", Provider: "claude"}},
		cancelPollInterval: 10 * time.Millisecond,
	}

	d.runner = taskRunnerFunc(func(runCtx context.Context, _ Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		<-runCtx.Done()
		return TaskResult{Status: "aborted"}, nil
	})

	task := Task{
		ID:        "task-ack-poll",
		RuntimeID: "rt-1",
		IssueID:   "issue-ack-poll",
		Agent:     &AgentData{Name: "test-agent"},
	}

	d.handleTask(context.Background(), task, 0)

	mu.Lock()
	order := make([]string, len(callOrder))
	copy(order, callOrder)
	mu.Unlock()

	pollStatusIdx, ackIdx := -1, -1
	for i, name := range order {
		switch name {
		case "poll-status":
			pollStatusIdx = i
		case "cancel-ack":
			ackIdx = i
		}
	}
	if pollStatusIdx == -1 {
		t.Fatalf("poll goroutine never fired (order: %v)", order)
	}
	if ackIdx == -1 {
		t.Fatalf("cancel-ack was never posted on the poll-cancelled path (order: %v)", order)
	}
	if ackIdx < pollStatusIdx {
		t.Fatalf("cancel-ack before the poll observed the cancellation (order: %v)", order)
	}
}

// TestHandleTask_AcksCancelOnPostRunStatusCheck verifies cancel-ack is also
// posted when the cancellation is only discovered by the pre-completion
// status check (the run finished before the poll noticed).
func TestHandleTask_AcksCancelOnPostRunStatusCheck(t *testing.T) {
	t.Parallel()

	var ackCalls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			ackCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/status"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"cancelled"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"rt-1": {ID: "rt-1", Provider: "claude"}},
		cancelPollInterval: time.Hour, // disable the poll path; exercise the post-run check
	}

	d.runner = taskRunnerFunc(func(_ context.Context, _ Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		return TaskResult{Status: "completed"}, nil
	})

	task := Task{
		ID:        "task-ack-postrun",
		RuntimeID: "rt-1",
		IssueID:   "issue-ack-postrun",
		Agent:     &AgentData{Name: "test-agent"},
	}

	d.handleTask(context.Background(), task, 0)

	if got := ackCalls.Load(); got != 1 {
		t.Fatalf("cancel-ack calls = %d, want 1", got)
	}
}

func TestConvertDisabledRuntimeSkillsForEnvScopesToClaimedRuntime(t *testing.T) {
	t.Parallel()

	agentData := &AgentData{DisabledRuntimeSkills: []DisabledRuntimeSkillData{
		{RuntimeID: "runtime-1", Provider: "claude", Root: "provider", Key: "review", Name: "Review"},
		{RuntimeID: "runtime-2", Provider: "claude", Root: "provider", Key: "other-runtime"},
		{RuntimeID: "runtime-1", Provider: "codex", Root: "provider", Key: "other-provider"},
	}}
	got := convertDisabledRuntimeSkillsForEnv(agentData, "runtime-1", "claude")
	if len(got) != 1 || got[0].Key != "review" || got[0].Name != "Review" {
		t.Fatalf("unexpected scoped runtime skill policy: %+v", got)
	}
}

// nestedPatchSecretBackend emits a Codex-shaped file-edit payload carrying a
// credential nested inside changes[] — the legacy protocol reports a deletion
// as the whole outgoing file, so deleting a .env puts its contents here.
type nestedPatchSecretBackend struct{}

func (nestedPatchSecretBackend) Execute(
	_ context.Context,
	_ string,
	_ agent.ExecOptions,
) (*agent.Session, error) {
	msgCh := make(chan agent.Message, 2)
	resCh := make(chan agent.Result, 1)

	msgCh <- agent.Message{
		Type:   agent.MessageToolUse,
		Tool:   "patch_apply",
		CallID: "patch-1",
		Input: map[string]any{
			"changes": []any{
				map[string]any{
					"path":    ".env",
					"kind":    "delete",
					"content": "GITHUB_TOKEN=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn\n",
				},
			},
		},
	}
	close(msgCh)
	resCh <- agent.Result{Status: "completed", Output: "done"}
	close(resCh)

	return &agent.Session{Messages: msgCh, Result: resCh}, nil
}

// TestExecuteAndDrain_RedactsNestedToolInputBeforeSending pins the daemon side
// of the redaction contract. The server scrubs again on ingest, but that is the
// remote end: a daemon that self-updated ahead of the server, or one talking to
// a server mid-rollout, must not put whole-file edit contents on the wire in
// cleartext. Deployment order is not a control we have, so this side has to be
// safe on its own.
func TestExecuteAndDrain_RedactsNestedToolInputBeforeSending(t *testing.T) {
	t.Parallel()

	d, rec := newTranscriptRecorder(t)

	if _, _, err := d.executeAndDrain(
		context.Background(),
		nestedPatchSecretBackend{},
		"p",
		agent.ExecOptions{},
		slog.Default(),
		"task-redact",
		new(atomic.Int32),
	); err != nil {
		t.Fatalf("executeAndDrain: %v", err)
	}

	msgs := rec.snapshot()
	var toolUse *TaskMessageData
	for i := range msgs {
		if msgs[i].Type == "tool_use" {
			toolUse = &msgs[i]
			break
		}
	}
	if toolUse == nil {
		t.Fatalf("no tool_use message reported: %+v", msgs)
	}

	blob, err := json.Marshal(toolUse.Input)
	if err != nil {
		t.Fatalf("marshal reported input: %v", err)
	}
	if strings.Contains(string(blob), "ghp_ABCDEFGH") {
		t.Fatalf("nested token left the daemon unredacted: %s", blob)
	}
	// The surrounding structure must survive, or the transcript loses the edit.
	if !strings.Contains(string(blob), ".env") {
		t.Fatalf("redaction destroyed the change metadata: %s", blob)
	}
}

// TestBuildPromptSquadLeaderReplyCommandCarvesOutNoAction renders the COMPLETE
// leader prompt and pins the exception's scope relation (MUL-5442 #6493
// review): the no_action rule and the reply imperative appear in the same
// prompt, so the imperative must carry the carve-out itself — a bare
// unconditional "Post your reply as a comment" anywhere in a leader prompt
// re-opens the contradiction the carve-out exists to close.
func TestBuildPromptSquadLeaderReplyCommandCarvesOutNoAction(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Task{
		IssueID:               "issue-1",
		TriggerCommentID:      "comment-1",
		TriggerCommentContent: "team update posted",
		TriggerAuthorType:     "member",
		TriggerAuthorName:     "Bohan",
		IsLeaderTask:          true,
		LeaderRoleResolved:    true,
		Agent: &AgentData{
			Name:         "Lead",
			Instructions: "Some instructions\n\n## Squad Operating Protocol\n\nYou are the LEADER...",
		},
	}, "claude")

	if !strings.Contains(prompt, "Unless your outcome is `no_action`, post your reply as a comment") {
		t.Fatalf("leader prompt missing the carve-out reply imperative\n---\n%s", prompt)
	}
	if strings.Contains(prompt, "Post your reply as a comment") {
		t.Fatalf("leader prompt still carries the unconditional reply imperative\n---\n%s", prompt)
	}
}

// TestBuildPromptSquadLeaderMultiThreadCarvesOutNoAction renders the complete
// leader prompt on the cross-thread fan-out path (MUL-5442 #6493 review): the
// fan-out imperative fires AFTER the no_action rule, so its scope sentence
// must govern the ENTIRE block — assert it precedes every later obligation,
// not just the first verb. The ordinary fan-out output keeps the
// unconditional form byte-for-byte.
func TestBuildPromptSquadLeaderMultiThreadCarvesOutNoAction(t *testing.T) {
	t.Parallel()

	leaderTask := Task{
		IssueID:               "issue-1",
		TriggerCommentID:      "comment-9",
		TriggerThreadID:       "thread-B",
		TriggerCommentContent: "second update",
		TriggerAuthorType:     "agent",
		TriggerAuthorName:     "Worker",
		CoalescedComments: []CoalescedCommentData{
			{ID: "comment-8", ThreadID: "thread-A", Content: "first update"},
		},
		IsLeaderTask:       true,
		LeaderRoleResolved: true,
		Agent: &AgentData{
			Name:         "Lead",
			Instructions: "Some instructions\n\n## Squad Operating Protocol\n\nYou are the LEADER...",
		},
	}
	prompt := BuildPrompt(leaderTask, "claude")
	scope := strings.Index(prompt, "skip this ENTIRE fan-out block")
	if scope < 0 {
		t.Fatalf("leader multi-thread prompt missing the whole-block scope sentence\n---\n%s", prompt)
	}
	// Obligation strings track the converged fan-out block (MUL-5825). Pin
	// ledger: "Post the replies in the order listed below" → the order rule
	// merged into the targets header ("OLDEST thread first"); "For EACH
	// thread above" → the embedded cookbook collapsed to the
	// `## Comment Formatting` pointer plus the per-thread file delta
	// ("DISTINCT body file per thread"). The assertion shape is unchanged:
	// every obligation must sit AFTER the no_action scope sentence.
	for _, obligation := range []string{
		"multiple replies are required and correct",
		"OLDEST thread first",
		"DISTINCT body file per thread",
	} {
		idx := strings.Index(prompt, obligation)
		if idx < 0 {
			t.Fatalf("leader multi-thread prompt missing obligation %q\n---\n%s", obligation, prompt)
		}
		if idx < scope {
			t.Fatalf("obligation %q precedes the no_action scope sentence — it escapes the carve-out\n---\n%s", obligation, prompt)
		}
	}
	if strings.Contains(prompt, ". Post ONE reply per thread") {
		t.Fatalf("leader multi-thread prompt still carries the unconditional imperative\n---\n%s", prompt)
	}

	ordinaryTask := leaderTask
	ordinaryTask.IsLeaderTask = false
	ordinaryTask.Agent = &AgentData{Name: "Reg", Instructions: "You are a regular agent."}
	ordinary := BuildPrompt(ordinaryTask, "claude")
	if !strings.Contains(ordinary, ". Post ONE reply per thread") {
		t.Fatalf("ordinary multi-thread prompt missing the unconditional imperative\n---\n%s", ordinary)
	}
	if strings.Contains(ordinary, "Unless your outcome is") || strings.Contains(ordinary, "skip this ENTIRE fan-out block") {
		t.Fatalf("ordinary multi-thread prompt leaked the leader carve-out\n---\n%s", ordinary)
	}
}
