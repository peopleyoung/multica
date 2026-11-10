package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Automatic retries keep the original input identity. A saved final response
// is reused after runtime recovery instead of executing its side effects again.
// A deliberate manual rerun is a new input and does not follow rerun_of_task_id.
func (h *Handler) nativeInputTaskID(ctx context.Context, task db.AgentTaskQueue) (string, error) {
	for range 32 {
		if !task.RetryOfTaskID.Valid {
			return uuidToString(task.ID), nil
		}
		previous, err := h.Queries.GetAgentTask(ctx, task.RetryOfTaskID)
		if err != nil || !rerunSourceMatchesTaskScope(task, previous) {
			return "", errors.New("native retry source is unavailable or outside the run scope")
		}
		task = previous
	}
	return "", errors.New("native retry lineage exceeds the supported limit")
}

func decodeNativeSessionRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid native session request")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "expected one request object")
		return false
	}
	return true
}

func nativeSessionResponse(row db.NativeSession) (agentstate.Session, error) {
	var cp agentstate.Checkpoint
	if err := json.Unmarshal(row.Checkpoint, &cp); err != nil {
		return agentstate.Session{}, err
	}
	cp.Version = row.Version
	return agentstate.Session{ID: uuidToString(row.ID), Version: row.Version, LeaseToken: uuidToString(row.LeaseToken), Checkpoint: cp}, nil
}

func (h *Handler) AcquireNativeSession(w http.ResponseWriter, r *http.Request) {
	var req agentstate.SessionRequest
	if !decodeNativeSessionRequest(w, r, &req) {
		return
	}
	leaseToken, ok := parseUUIDOrBadRequest(w, req.LeaseToken, "lease_token")
	if !ok {
		return
	}
	taskID := chi.URLParam(r, "taskId")
	if _, ok := parseUUIDOrBadRequest(w, taskID, "task_id"); !ok {
		return
	}
	task, err := h.loadNativeInferenceRun(r, taskID, req.Claim)
	if err != nil {
		writeError(w, http.StatusForbidden, "native run authorization failed")
		return
	}
	// Work directories are daemon-local identifiers, never server paths. Bind
	// them to the session so a second machine cannot silently reuse its tools.
	if req.WorkDir == "" || len(req.WorkDir) > 4096 || strings.ContainsRune(req.WorkDir, 0) || !nativeAbsoluteWorkDir(req.WorkDir) {
		writeError(w, http.StatusBadRequest, "absolute work directory required")
		return
	}
	scopeType, scopeID := "run", task.ID
	if task.ChatSessionID.Valid {
		scopeType, scopeID = "chat", task.ChatSessionID
	} else if task.IssueID.Valid {
		scopeType, scopeID = "issue", task.IssueID
	}
	workspace := parseUUID(middleware.DaemonWorkspaceIDFromContext(r.Context()))
	row, err := h.Queries.AcquireNativeSession(r.Context(), db.AcquireNativeSessionParams{WorkspaceID: workspace, AgentID: task.AgentID, ScopeType: scopeType, ScopeID: scopeID, DaemonID: middleware.DaemonIDFromContext(r.Context()), WorkDir: req.WorkDir, ActiveTaskID: task.ID, ActiveDispatch: task.DispatchedAt, LeaseToken: leaseToken, FreshStart: task.ForceFreshSession && !task.RerunOfTaskID.Valid && !task.RetryOfTaskID.Valid})
	if err != nil {
		nativeSessionWriteError(w, err)
		return
	}
	response, err := nativeSessionResponse(row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid stored native checkpoint")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// Paths belong to the remote daemon's OS. Never clean or open them with the
// API server's filesystem rules (a Windows daemon may use a Linux API server).
func nativeAbsoluteWorkDir(path string) bool {
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) {
		return true
	}
	return len(path) >= 3 && (path[0] >= 'a' && path[0] <= 'z' || path[0] >= 'A' && path[0] <= 'Z') && path[1] == ':' && (path[2] == '/' || path[2] == '\\')
}

func nativeSessionWriteError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "native session lease or version conflict")
		return
	}
	writeError(w, http.StatusInternalServerError, "native session storage unavailable")
}

// NativeCheckpoint handles save/renew/release under the same claim and lease
// fencing. Every request rechecks live execution authorization first.
func (h *Handler) NativeCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req agentstate.CheckpointRequest
	if !decodeNativeSessionRequest(w, r, &req) {
		return
	}
	taskID := chi.URLParam(r, "taskId")
	if _, ok := parseUUIDOrBadRequest(w, taskID, "task_id"); !ok {
		return
	}
	task, err := h.loadNativeInferenceRun(r, taskID, req.Claim)
	if err != nil {
		writeError(w, http.StatusForbidden, "native run authorization failed")
		return
	}
	sessionID, ok := parseUUIDOrBadRequest(w, req.Session.ID, "session_id")
	if !ok {
		return
	}
	lease, ok := parseUUIDOrBadRequest(w, req.Session.LeaseToken, "lease_token")
	if !ok {
		return
	}
	workspace := parseUUID(middleware.DaemonWorkspaceIDFromContext(r.Context()))
	// Renewal fences the claim and lease token, not the checkpoint version.
	// A summary may commit a new version concurrently with a heartbeat.
	if chi.URLParam(r, "operation") == "renew" {
		row, err := h.Queries.RenewNativeSession(r.Context(), db.RenewNativeSessionParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: lease})
		if err != nil {
			nativeSessionWriteError(w, err)
			return
		}
		response, err := nativeSessionResponse(row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid stored checkpoint")
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	current, err := h.Queries.GetNativeSessionForClaim(r.Context(), db.GetNativeSessionForClaimParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: lease, ExpectedVersion: req.Session.Version})
	if err != nil {
		nativeSessionWriteError(w, err)
		return
	}
	var row db.NativeSession
	switch chi.URLParam(r, "operation") {
	case "compact":
		row, err = h.compactNativeCheckpoint(r, task, current, req.Session)
		if err != nil {
			writeNativeCompactionError(w, err)
			return
		}
	case "save":
		client, err := h.nativeTaskModel(r.Context(), task)
		if err != nil {
			writeError(w, 503, "selected model is unavailable")
			return
		}
		caps, err := client.NativeCapabilities()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "native model is not configured")
			return
		}
		old, err := nativeSessionResponse(current)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid stored checkpoint")
			return
		}
		req.Session.Checkpoint.Version = req.Session.Version
		if err := agentstate.ValidateTransition(old.Checkpoint, req.Session.Checkpoint, caps.MaxToolArgumentBytes); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		raw, err := json.Marshal(req.Session.Checkpoint)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid checkpoint")
			return
		}
		params := db.SaveNativeCheckpointParams{Checkpoint: raw, ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: lease, ExpectedVersion: req.Session.Version}
		if len(req.Session.Checkpoint.SupplementIDs) > len(old.Checkpoint.SupplementIDs) {
			commentID, ok := parseUUIDOrBadRequest(w, req.Session.Checkpoint.SupplementIDs[len(old.Checkpoint.SupplementIDs)], "supplement_id")
			if !ok {
				return
			}
			row, err = h.saveNativeSupplement(r, task, params, commentID)
		} else {
			row, err = h.Queries.SaveNativeCheckpoint(r.Context(), params)
		}
		if err != nil {
			nativeSessionWriteError(w, err)
			return
		}
	case "release":
		n, err := h.Queries.ReleaseNativeSession(r.Context(), db.ReleaseNativeSessionParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: lease})
		if err != nil {
			nativeSessionWriteError(w, err)
			return
		}
		if n == 0 {
			writeError(w, http.StatusConflict, "native session lease conflict")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		writeError(w, http.StatusNotFound, "unknown checkpoint operation")
		return
	}
	response, err := nativeSessionResponse(row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid stored checkpoint")
		return
	}
	writeJSON(w, http.StatusOK, response)
}
