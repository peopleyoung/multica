package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func (h *Handler) validateNativeAgentModel(w http.ResponseWriter, r *http.Request, workspace pgtype.UUID, provider, model string) bool {
	if provider != "native" || model == "" {
		return true
	}
	if _, err := h.workspaceModelClient(r.Context(), workspace, model); err != nil {
		writeError(w, http.StatusBadRequest, "select an enabled model from this workspace")
		return false
	}
	return true
}

// NativeModelConfig exposes capabilities only, never endpoint or credentials.
func (h *Handler) NativeModelConfig(w http.ResponseWriter, r *http.Request) {
	if middleware.DaemonWorkspaceIDFromContext(r.Context()) == "" && r.Header.Get("X-User-ID") == "" {
		writeError(w, http.StatusUnauthorized, "daemon authentication required")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	client := h.LLM
	workspace := middleware.DaemonWorkspaceIDFromContext(r.Context())
	if taskID := r.URL.Query().Get("task_id"); taskID != "" {
		id, ok := parseUUIDOrBadRequest(w, taskID, "task_id")
		if !ok {
			return
		}
		task, err := h.Queries.GetAgentTask(r.Context(), id)
		if err != nil {
			writeError(w, 403, "native run authorization failed")
			return
		}
		claim := llm.NativeRequest{Attempt: task.Attempt, DispatchedAt: task.DispatchedAt.Time.Format(time.RFC3339Nano)}
		if _, err = h.loadNativeInferenceRun(r, taskID, claim); err != nil {
			writeError(w, 403, "native run authorization failed")
			return
		}
		client, err = h.nativeTaskModel(r.Context(), task)
		if err != nil {
			writeError(w, 503, "selected model is unavailable")
			return
		}
	} else if workspace != "" {
		var err error
		client, err = h.workspaceModelClient(r.Context(), parseUUID(workspace), "")
		if err != nil {
			writeError(w, 503, "default model is unavailable")
			return
		}
	}
	caps, err := client.NativeCapabilities()
	writeJSON(w, 200, llm.Deployment{Configured: err == nil, Model: client.DefaultModel(), Capabilities: caps})
}

// authorizeNativeInference deliberately avoids membership/runtime/auth caches:
// a paid model step must still belong to this daemon and claim generation now.
func (h *Handler) loadNativeInferenceRun(r *http.Request, taskID string, req llm.NativeRequest) (db.AgentTaskQueue, error) {
	ctx := r.Context()
	workspace := middleware.DaemonWorkspaceIDFromContext(ctx)
	daemonID := middleware.DaemonIDFromContext(ctx)
	if workspace == "" || daemonID == "" {
		return db.AgentTaskQueue{}, errors.New("daemon token required")
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(token, "mdt_") {
		return db.AgentTaskQueue{}, errors.New("daemon token required")
	}
	credential, err := h.Queries.GetDaemonTokenByHash(ctx, auth.HashToken(token))
	if err != nil || uuidToString(credential.WorkspaceID) != workspace || credential.DaemonID != daemonID {
		return db.AgentTaskQueue{}, errors.New("daemon credential no longer valid")
	}
	taskUUID, err := util.ParseUUID(taskID)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	task, err := h.Queries.GetAgentTask(ctx, taskUUID)
	if err != nil {
		return db.AgentTaskQueue{}, errors.New("run is unavailable")
	}
	dispatched, err := time.Parse(time.RFC3339Nano, req.DispatchedAt)
	if err != nil || task.Status != "running" || task.Attempt != req.Attempt || !task.DispatchedAt.Valid || !task.DispatchedAt.Time.Equal(dispatched) || !task.RuntimeID.Valid {
		return db.AgentTaskQueue{}, errors.New("run is not an active claim")
	}
	runtime, err := h.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{ID: task.RuntimeID, WorkspaceID: credential.WorkspaceID})
	if err != nil || runtime.Provider != "native" || !runtime.DaemonID.Valid || runtime.DaemonID.String != daemonID || runtime.Status != "online" {
		return db.AgentTaskQueue{}, errors.New("run is not owned by this native runtime")
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: runtime.OwnerID, WorkspaceID: credential.WorkspaceID}); err != nil {
		return db.AgentTaskQueue{}, errors.New("runtime owner is no longer a member")
	}
	agent, err := h.Queries.GetAgent(ctx, task.AgentID)
	if err != nil || agent.WorkspaceID != credential.WorkspaceID {
		return db.AgentTaskQueue{}, errors.New("agent is unavailable")
	}
	resolved, err := h.TaskService.ResolveTaskWorkspaceIDChecked(ctx, task)
	if err != nil || resolved != workspace {
		return db.AgentTaskQueue{}, errors.New("run workspace is unavailable")
	}
	return task, nil
}

func (h *Handler) authorizeNativeInference(r *http.Request, taskID string, req llm.NativeRequest) error {
	task, err := h.loadNativeInferenceRun(r, taskID, req)
	if err != nil {
		return err
	}
	sessionID, err := util.ParseUUID(req.SessionID)
	if err != nil {
		return errors.New("native inference requires a session")
	}
	lease, err := util.ParseUUID(req.LeaseToken)
	if err != nil {
		return errors.New("native inference requires a lease")
	}
	row, err := h.Queries.GetNativeSessionForClaim(r.Context(), db.GetNativeSessionForClaimParams{
		ID: sessionID, WorkspaceID: parseUUID(middleware.DaemonWorkspaceIDFromContext(r.Context())),
		AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: lease, ExpectedVersion: req.SessionVersion,
	})
	if err != nil {
		return errors.New("native inference lease is no longer current")
	}
	var checkpoint agentstate.Checkpoint
	if json.Unmarshal(row.Checkpoint, &checkpoint) != nil || !checkpoint.MatchesMessages(req.Input.Messages) {
		return errors.New("native inference must use the durable model history")
	}
	return nil
}

// NativeInference is a constrained model-step endpoint, never an arbitrary
// OpenAI proxy. It accepts no upstream headers, endpoint or model overrides.
func (h *Handler) NativeInference(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	if _, ok := parseUUIDOrBadRequest(w, taskID, "task_id"); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req llm.NativeRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid native model request")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "expected one request object")
		return
	}
	if err := h.authorizeNativeInference(r, taskID, req); err != nil {
		writeError(w, http.StatusForbidden, "native run authorization failed")
		return
	}
	task, err := h.loadNativeInferenceRun(r, taskID, req)
	if err != nil {
		writeError(w, 403, "native run authorization failed")
		return
	}
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
	if err := req.Input.Validate(caps); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	ctx = llm.WithCallScope(ctx, llm.CallScope{WorkspaceID: middleware.DaemonWorkspaceIDFromContext(r.Context()), TaskID: taskID, SessionID: req.SessionID, Purpose: "execution"})
	defer h.watchNativeInference(ctx, cancel, r, taskID, req)()
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	send := func(frame llm.Frame) error {
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := encoder.Encode(frame); err != nil {
			return err
		}
		return controller.Flush()
	}
	input, err := h.hydrateNativeAttachments(r.WithContext(ctx), taskID, req)
	if err != nil {
		_ = send(llm.Frame{Error: "attachment_input_unavailable"})
		return
	}
	response, err := client.Complete(ctx, input, func(event llm.Event) error { return send(llm.Frame{Event: &event}) })
	if err != nil {
		// Upstream errors may contain private prompts or gateway diagnostics.
		// Only stable, credential-free codes cross the daemon protocol.
		code := llm.FailureCode(err)
		slog.Warn("native model step failed", "task_id", taskID, "failure_code", code)
		_ = send(llm.Frame{Error: code})
		return
	}
	if h.authorizeNativeInference(r.WithContext(ctx), taskID, req) != nil {
		_ = send(llm.Frame{Error: "model_step_cancelled"})
		return
	}
	_ = send(llm.Frame{Response: response})
}

// Both execution and summary requests stop paying for upstream generation as
// soon as their credential, claim, history or lease is no longer authorized.
func (h *Handler) watchNativeInference(ctx context.Context, cancel context.CancelFunc, r *http.Request, taskID string, req llm.NativeRequest) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if h.authorizeNativeInference(r.WithContext(ctx), taskID, req) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
