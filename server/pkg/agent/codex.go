package agent

import (
	"strings"
)

// CodexSemanticInactivityMarker prefixes timeout errors emitted when Codex
// stops making semantic progress while the process is still alive.
const CodexSemanticInactivityMarker = "codex semantic inactivity timeout"

// CodexFirstTurnNoProgressMarker identifies the app-server failure mode where
// Codex accepts a turn and then never emits any item, completion, or error.
const CodexFirstTurnNoProgressMarker = "codex app-server no progress timeout"

// codexResumeMarker and codexLineOverflowMarker are the two halves of the
// error text a resume-overflow produces: the method that failed (written by
// startOrResumeThread) and bufio's own ErrTooLong wording, which reaches the
// string through the reader goroutine.
const (
	codexResumeMarker       = "thread/resume failed"
	codexLineOverflowMarker = "token too long"
)

// CodexResumeOverflowError reports whether an agent error string is the
// resume-overflow failure — the thread/resume response did not fit in our
// stdout line buffer, so the thread cannot be handed to us at all.
//
// This lives next to the code that writes both halves of the text so the two
// cannot drift apart, and it is exported because the daemon has only the error
// STRING at report time: by then the typed bufio.ErrTooLong that
// isCodexResumeOverflow matches in-process is long gone. Both markers are
// required — "thread/resume failed" alone covers ordinary rejections that a
// plain retry handles, and "token too long" alone would also match an overflow
// on some other RPC, where dropping the session pointer cures nothing.
//
// The session behind such a failure is unusable for resume until it shrinks,
// which it never does — codex rollouts are append-only. Callers use this to
// stop handing the same oversized thread to the next task (MUL-5722).
func CodexResumeOverflowError(errText string) bool {
	if errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	return strings.Contains(lower, codexResumeMarker) && strings.Contains(lower, codexLineOverflowMarker)
}

// codexRetiredCompactPath is the request path only the legacy remote-compaction
// route ever calls. The v2 route compacts on the ordinary /responses stream
// instead, so this segment appears only when the legacy route was selected.
const codexRetiredCompactPath = "responses/compact"

// codexRemoteCompactMarker opens the error Codex writes when a remote
// compaction task fails. Both routes share it — compact_remote_v2.rs labels its
// own failures with the same phrase — so it establishes only that compaction is
// what failed, never which route ran.
const codexRemoteCompactMarker = "remote compact task"

// CodexRetiredCompactionError reports whether an agent error is Codex failing
// to compact a conversation over the route OpenAI has retired, e.g.
//
//	Error running remote compact task: unexpected status 404 Not Found: {"detail":"Not Found"},
//	url: https://chatgpt.com/backend-api/codex/responses/compact, cf-ray: ..., request id: ...
//
// The route is chosen by turning `remote_compaction_v2` off. Older Codex builds
// ignored that setting, so a value added as a workaround for an unrelated,
// since-fixed defect can start failing on upgrade without the user touching
// anything (GH #8000, upstream openai/codex#42468). Nothing in the text names
// the setting, so the failure is unreadable on its own — which is the whole
// reason this predicate exists: callers use it to attach the remedy.
//
// The retired PATH is what has to be present. The compaction marker alone is
// not evidence of anything: the v2 route wraps its own failures in the same
// "Error running remote compact task" prefix, and a v2 stream can return 404
// too — so keying on the marker plus a status code would tell a user whose
// setting is already correct to go turn it off, the exact opposite of the fix.
// The path cannot be produced that way; a compaction request reaching
// /responses/compact at all means the legacy route was selected, whatever
// status came back.
//
// That leaves a deliberate false negative: a Codex build that reports this
// failure without the url tail gets no hint. There is no second signal in the
// text that identifies the route, and the two errors are otherwise
// indistinguishable, so the choice is between missing some cases and
// misdirecting the users whose config was never the problem. A missed hint
// costs what today already costs; a wrong hint spends someone's afternoon.
//
// Deliberately NOT a resume-safety predicate. The stuck thread is recoverable:
// once the setting is right compaction succeeds and the same conversation
// continues, so retiring the session here would throw away exactly the context
// the fix restores. Text only.
//
// Delete once no Codex old enough to select the retired route is still in use.
// Upstream intends to mark the flag Stage::Removed or fall back on the 404,
// and under either fix this predicate simply stops matching: it overrides no
// config and stands between nobody and a setting they may legitimately want
// off, so it costs one substring check on an already-failing path until then
// and comes out in a single commit. That is the difference from forcing
// `--enable remote_compaction_v2`, which was declined for the opposite reason
// (GH #8019) — an override outlives the defect and blocks the legitimate case.
func CodexRetiredCompactionError(errText string) bool {
	if errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	return strings.Contains(lower, codexRemoteCompactMarker) &&
		strings.Contains(lower, codexRetiredCompactPath)
}
