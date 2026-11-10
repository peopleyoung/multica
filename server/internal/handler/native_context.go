package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type nativeCompactionError struct {
	status int
	code   string
}

func (e *nativeCompactionError) Error() string { return e.code }

func writeNativeCompactionError(w http.ResponseWriter, err error) {
	status, code := http.StatusConflict, "native_summary_conflict"
	var failure *nativeCompactionError
	if errors.As(err, &failure) {
		status, code = failure.status, failure.code
	}
	writeJSON(w, status, map[string]string{"error": "native context compaction failed; original history is preserved", "code": code})
}

// Only the API can create a context summary. The daemon cannot rewrite stored
// history or insert an arbitrary summary through the ordinary save operation.
func (h *Handler) compactNativeCheckpoint(r *http.Request, task db.AgentTaskQueue, current db.NativeSession, identity agentstate.Session) (db.NativeSession, error) {
	client, err := h.nativeTaskModel(r.Context(), task)
	if err != nil {
		return db.NativeSession{}, &nativeCompactionError{http.StatusServiceUnavailable, "native_summary_model_unavailable"}
	}
	caps, err := client.NativeCapabilities()
	if err != nil {
		return db.NativeSession{}, err
	}
	session, err := nativeSessionResponse(current)
	if err != nil {
		return db.NativeSession{}, err
	}
	if err := session.Checkpoint.Validate(caps.MaxToolArgumentBytes); err != nil {
		return db.NativeSession{}, err
	}
	request, cut, err := session.Checkpoint.SummaryInput(caps)
	if err != nil {
		return db.NativeSession{}, &nativeCompactionError{http.StatusUnprocessableEntity, "native_summary_input_too_large"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	ctx = llm.WithCallScope(ctx, llm.CallScope{WorkspaceID: uuidToString(current.WorkspaceID), TaskID: uuidToString(task.ID), SessionID: uuidToString(current.ID), Purpose: "summary"})
	claim := llm.NativeRequest{Attempt: task.Attempt, DispatchedAt: task.DispatchedAt.Time.Format(time.RFC3339Nano), SessionID: session.ID, SessionVersion: session.Version, LeaseToken: session.LeaseToken, Input: llm.Request{Messages: session.Checkpoint.ModelMessages()}}
	defer h.watchNativeInference(ctx, cancel, r, uuidToString(task.ID), claim)()
	response, err := client.Complete(ctx, request, nil)
	// A summary has no tools and does not commit until CAS below. Retry one
	// empty completion without replaying the run or any completed side effects.
	if errors.Is(err, llm.ErrEmptyResponse) && ctx.Err() == nil {
		if _, claimErr := h.loadNativeInferenceRun(r.WithContext(ctx), uuidToString(task.ID), claim); claimErr != nil {
			return db.NativeSession{}, claimErr
		}
		slog.Warn("retrying empty native model summary", "task_id", uuidToString(task.ID), "attempt", 2)
		response, err = client.Complete(ctx, request, nil)
	}
	if err != nil {
		slog.Warn("native model summary failed", "task_id", uuidToString(task.ID), "failure_code", llm.FailureCode(err))
		return db.NativeSession{}, &nativeCompactionError{http.StatusBadGateway, llm.FailureCode(err)}
	}
	if response == nil || strings.TrimSpace(response.Message.Content) == "" || len(response.Message.ToolCalls) > 0 {
		return db.NativeSession{}, &nativeCompactionError{http.StatusBadGateway, "model_response_empty"}
	}
	// Recheck the claim after generation. CAS below also rejects a stale
	// version or expired lease; no partial summary is committed on failure.
	if _, err := h.loadNativeInferenceRun(r.WithContext(ctx), uuidToString(task.ID), claim); err != nil {
		return db.NativeSession{}, err
	}
	next := session.Checkpoint.WithSummary(cut, response.Message.Content)
	if err := next.Validate(caps.MaxToolArgumentBytes); err != nil {
		return db.NativeSession{}, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return db.NativeSession{}, err
	}
	return h.Queries.SaveNativeCheckpoint(ctx, db.SaveNativeCheckpointParams{Checkpoint: raw, ID: current.ID, WorkspaceID: current.WorkspaceID, AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: current.LeaseToken, ExpectedVersion: identity.Version})
}
