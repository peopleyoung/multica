package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type nativeWriteBuffer struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (b *nativeWriteBuffer) Header() http.Header { return b.header }
func (b *nativeWriteBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *nativeWriteBuffer) Write(body []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.body.Len()+len(body) > 128<<10 {
		b.overflow = true
		return 0, errors.New("native write response exceeds limit")
	}
	return b.body.Write(body)
}

func nativeWriteRoute(name, method, path string) bool {
	management := map[string]struct{ base, suffix, update string }{
		"multica_manage_project":   {"/api/projects", "resources", http.MethodPut},
		"multica_manage_agent":     {"/api/agents", "skills/add", http.MethodPut},
		"multica_manage_squad":     {"/api/squads", "members", http.MethodPut},
		"multica_manage_autopilot": {"/api/autopilots", "triggers", http.MethodPatch},
	}
	if spec, ok := management[name]; ok {
		if path == spec.base {
			return method == http.MethodPost
		}
		if !strings.HasPrefix(path, spec.base+"/") {
			return false
		}
		parts := strings.SplitN(strings.TrimPrefix(path, spec.base+"/"), "/", 2)
		if !nativeWriteResourceID(parts[0]) {
			return false
		}
		return len(parts) == 1 && method == spec.update || len(parts) == 2 && parts[1] == spec.suffix && method == http.MethodPost
	}
	switch name {
	case "multica_create_issue":
		return method == http.MethodPost && path == "/api/issues"
	case "multica_upload_artifact":
		return method == http.MethodPost && path == "/api/upload-file"
	case "multica_create_comment", "multica_update_issue_status", "multica_record_squad_evaluation":
		parts := strings.Split(strings.TrimPrefix(path, "/api/issues/"), "/")
		if !strings.HasPrefix(path, "/api/issues/") || parts[0] == "" || len(parts[0]) > 200 {
			return false
		}
		if name == "multica_create_comment" {
			return method == http.MethodPost && len(parts) == 2 && parts[1] == "comments"
		}
		if name == "multica_record_squad_evaluation" {
			return method == http.MethodPost && len(parts) == 2 && parts[1] == "squad-evaluated"
		}
		return method == http.MethodPut && len(parts) == 1
	}
	return false
}

func nativeWriteResourceID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func nativeWriteCall(cp agentstate.Checkpoint, id string, allowUnknown bool) (llm.ToolCall, bool) {
	for _, record := range cp.Tools {
		if record.Call.ID == id && (record.State == agentstate.ToolStarted || allowUnknown && record.State == agentstate.ToolUnknown) && agentstate.NativeWriteTool(record.Call.Name) {
			return record.Call, true
		}
	}
	return llm.ToolCall{}, false
}

func nativeWriteKey(task db.AgentTaskQueue, cp agentstate.Checkpoint, call llm.ToolCall) string {
	var context struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(task.Context, &context)
	return agentstate.WriteKey(cp.InputTaskID, call, context.Type == "quick_create")
}

// NativeWrite records intent before the existing authorized business handler.
// A started operation without a confirmed response stays unknown: the business
// transaction and this receipt are deliberately not described as exactly-once.
func (h *Handler) NativeWrite(next http.Handler) http.Handler {
	guarded := middleware.RequireWorkspaceMember(h.Queries)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded := r.Header.Get(agentstate.NativeWriteHeader)
		var claim agentstate.WriteClaim
		if len(encoded) > 1024 || json.Unmarshal([]byte(encoded), &claim) != nil || claim.CallID == "" || len(claim.CallID) > 200 || claim.Version < 0 || r.URL.RawQuery != "" || r.Header.Get("X-Actor-Source") != "task_token" {
			writeError(w, http.StatusBadRequest, "invalid native write identity")
			return
		}
		sessionID, ok := parseUUIDOrBadRequest(w, claim.SessionID, "session_id")
		if !ok {
			return
		}
		lease, ok := parseUUIDOrBadRequest(w, claim.LeaseToken, "lease_token")
		if !ok {
			return
		}
		taskID, ok := parseUUIDOrBadRequest(w, r.Header.Get("X-Task-ID"), "task_id")
		if !ok {
			return
		}
		workspace, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
		if !ok {
			return
		}
		task, err := h.Queries.GetAgentTask(r.Context(), taskID)
		if err != nil || task.Status != "running" || !task.RuntimeID.Valid || uuidToString(task.AgentID) != r.Header.Get("X-Agent-ID") {
			writeError(w, http.StatusForbidden, "native write run is unavailable")
			return
		}
		runtime, err := h.Queries.GetAgentRuntimeForWorkspace(r.Context(), db.GetAgentRuntimeForWorkspaceParams{ID: task.RuntimeID, WorkspaceID: workspace})
		if err != nil || runtime.Provider != "native" || runtime.Status != "online" {
			writeError(w, http.StatusForbidden, "native write runtime is unavailable")
			return
		}
		params := db.GetNativeSessionForWriteParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, LeaseToken: lease, ExpectedVersion: claim.Version}
		current, err := h.Queries.GetNativeSessionForWrite(r.Context(), params)
		var cp agentstate.Checkpoint
		if err != nil || !current.ActiveDispatch.Time.Equal(task.DispatchedAt.Time) || json.Unmarshal(current.Checkpoint, &cp) != nil || cp.InputTaskID == "" {
			writeError(w, http.StatusConflict, "native write lease is no longer current")
			return
		}
		call, ok := nativeWriteCall(cp, claim.CallID, false)
		if !ok || !nativeWriteRoute(call.Name, r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, "native write is not a started tool operation")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "native write body exceeds limit")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		digest := sha256.Sum256(append([]byte(r.Method+"\n"+r.URL.Path+"\n"+r.Header.Get("Content-Type")+"\n"), body...))
		key := nativeWriteKey(task, cp, call)
		outcome := agentstate.WriteOutcome{State: "started", Digest: hex.EncodeToString(digest[:]), CallID: call.ID}
		raw, _ := json.Marshal(outcome)
		n, err := h.Queries.ReserveNativeWrite(r.Context(), db.ReserveNativeWriteParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, LeaseToken: lease, ExpectedVersion: claim.Version, OperationKey: key, Intent: raw})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "native write receipt storage unavailable")
			return
		}
		if n == 0 {
			current, err = h.Queries.GetNativeSessionForWrite(r.Context(), params)
			var entries map[string]agentstate.WriteOutcome
			if err == nil && json.Unmarshal(current.WriteOutcomes, &entries) == nil {
				previous := entries[key]
				if previous.State == "finished" && previous.Digest == outcome.Digest && previous.CallID == call.ID {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(previous.Status)
					_, _ = w.Write(previous.Body)
					return
				}
			}
			writeError(w, http.StatusServiceUnavailable, "native write already attempted or receipt budget reached; reconcile before continuing")
			return
		}
		buffer := &nativeWriteBuffer{header: make(http.Header)}
		next.ServeHTTP(buffer, r)
		if buffer.status == 0 {
			buffer.status = http.StatusOK
		}
		if buffer.overflow || buffer.status >= 500 || !json.Valid(buffer.body.Bytes()) {
			writeError(w, http.StatusServiceUnavailable, "native write outcome is unknown; reconcile before continuing")
			return
		}
		result := json.RawMessage(buffer.body.Bytes())
		if buffer.status >= 200 && buffer.status < 300 {
			result, err = agentstate.ProjectBusinessResponse(result, call.Name == "multica_upload_artifact")
			if err == nil {
				result, err = agentstate.ProjectManagementResponse(call.Name, result)
			}
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "native write response could not be projected")
				return
			}
		}
		outcome.State, outcome.Status, outcome.Body = "finished", buffer.status, result
		raw, _ = json.Marshal(outcome)
		// A disconnected caller does not erase an already observed business
		// outcome. Use a bounded independent context solely to store its receipt.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		n, err = h.Queries.FinishNativeWrite(ctx, db.FinishNativeWriteParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, OperationKey: key, Digest: outcome.Digest, Outcome: raw})
		if err != nil || n != 1 {
			writeError(w, http.StatusServiceUnavailable, "native write receipt was not confirmed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(outcome.Status)
		_, _ = w.Write(outcome.Body)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(agentstate.NativeWriteHeader) == "" {
			next.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

// NativeWriteResult reconciles the current pending operation without invoking
// its business handler. Authorization uses the newly acquired daemon lease.
func (h *Handler) NativeWriteResult(w http.ResponseWriter, r *http.Request) {
	var req agentstate.WriteResultRequest
	if !decodeNativeSessionRequest(w, r, &req) {
		return
	}
	task, err := h.loadNativeInferenceRun(r, chi.URLParam(r, "taskId"), req.Claim)
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
	workspace, ok := parseUUIDOrBadRequest(w, middleware.DaemonWorkspaceIDFromContext(r.Context()), "workspace_id")
	if !ok {
		return
	}
	current, err := h.Queries.GetNativeSessionForWrite(r.Context(), db.GetNativeSessionForWriteParams{ID: sessionID, WorkspaceID: workspace, AgentID: task.AgentID, TaskID: task.ID, LeaseToken: lease, ExpectedVersion: req.Session.Version})
	var cp agentstate.Checkpoint
	if err != nil {
		nativeSessionWriteError(w, err)
		return
	}
	if json.Unmarshal(current.Checkpoint, &cp) != nil {
		writeError(w, http.StatusInternalServerError, "invalid stored native checkpoint")
		return
	}
	call, ok := nativeWriteCall(cp, req.CallID, true)
	if !ok {
		writeError(w, http.StatusBadRequest, "no started native write")
		return
	}
	var entries map[string]agentstate.WriteOutcome
	if json.Unmarshal(current.WriteOutcomes, &entries) != nil {
		writeError(w, http.StatusInternalServerError, "invalid stored native write receipts")
		return
	}
	outcome := entries[nativeWriteKey(task, cp, call)]
	result := agentstate.WriteResult{}
	if outcome.State == "finished" && outcome.CallID == call.ID {
		result = agentstate.WriteResult{Known: true, Status: outcome.Status, Body: outcome.Body}
		output, failed, err := result.ToolOutput(call.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid confirmed native write result")
			return
		}
		for i := range cp.Tools {
			if cp.Tools[i].Call.ID == call.ID {
				cp.Tools[i].Result = &output
				cp.Tools[i].State = agentstate.ToolSucceeded
				if failed {
					cp.Tools[i].State = agentstate.ToolFailed
				}
			}
		}
		raw, _ := json.Marshal(cp)
		// Only this server-owned transition may promote an unknown write,
		// and only from a matching durable receipt. Ordinary saves still
		// cannot fabricate certainty or reopen an unknown operation.
		saved, err := h.Queries.SaveNativeCheckpoint(r.Context(), db.SaveNativeCheckpointParams{ID: current.ID, WorkspaceID: current.WorkspaceID, AgentID: task.AgentID, TaskID: task.ID, DispatchedAt: task.DispatchedAt, LeaseToken: lease, ExpectedVersion: current.Version, Checkpoint: raw})
		if err != nil {
			nativeSessionWriteError(w, err)
			return
		}
		session, err := nativeSessionResponse(saved)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid reconciled native checkpoint")
			return
		}
		result.Session = &session
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}
