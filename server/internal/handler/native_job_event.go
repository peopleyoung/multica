package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Independent-job terminal observations are facts, not business acceptance or
// permission to repeat training. The deterministic comment and follow-up commit
// together; lost acknowledgements cannot create another runnable task.
func (h *Handler) NativeJobEvent(w http.ResponseWriter, r *http.Request) {
	source, ws, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, chi.URLParam(r, "taskId"))
	if !ok {
		return
	}

	runtime, err := h.Queries.GetAgentRuntime(r.Context(), source.RuntimeID)
	executorID := middleware.DaemonIDFromContext(r.Context())
	ownsExecutor := executorID != "" && runtime.DaemonID.String == executorID
	// The persistent observer runs under the daemon control-plane credential,
	// which may outlive the short-lived task MDT. Only the authenticated runtime
	// owner (not another workspace member/admin) may use that existing auth path.
	switch middleware.DaemonAuthPathFromContext(r.Context()) {
	case middleware.DaemonAuthPathPAT, middleware.DaemonAuthPathCloudPAT, middleware.DaemonAuthPathJWT:
		ownsExecutor = executorID == "" && requestUserID(r) != "" && requestUserID(r) == uuidToString(runtime.OwnerID)
	}
	if err != nil || runtime.Provider != "native" || uuidToString(runtime.WorkspaceID) != ws || !ownsExecutor {
		writeError(w, 403, "job observation executor does not own the source runtime")
		return
	}

	var event nativeruntime.JobEvent
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&event) != nil || dec.Decode(new(any)) != io.EOF || event.Validate() != nil {
		writeError(w, 400, "invalid job completion observation")
		return
	}
	if !source.IssueID.Valid {
		writeError(w, 400, "job follow-up requires an issue source")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 503, "job feedback storage unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)

	// Follow the owner -> issue -> task order used by teardown. Current issue
	// assignment/status and agent policy remain stable through enqueue/commit.
	agent, err := q.GetAgentForUpdate(r.Context(), source.AgentID)
	if err != nil || uuidToString(agent.WorkspaceID) != ws {
		writeError(w, 404, "job agent unavailable")
		return
	}
	issue, err := q.LockIssueForDescriptionUpdate(r.Context(), db.LockIssueForDescriptionUpdateParams{ID: source.IssueID, WorkspaceID: parseUUID(ws)})
	if err != nil {
		writeError(w, 404, "job issue unavailable")
		return
	}
	source, err = q.LockNativeJobSourceTask(r.Context(), source.ID)
	if err != nil {
		writeError(w, 503, "job source unavailable")
		return
	}
	if !isTerminalTaskStatus(source.Status) {
		writeJSON(w, 200, map[string]bool{"acknowledged": false, "pending_source_run": true})
		return
	}
	eventID := parseUUID(uuid.NewSHA1(uuid.NameSpaceOID, []byte("native-job:"+uuidToString(source.ID)+":"+event.Key)).String())
	if _, err := q.GetCommentInWorkspace(r.Context(), db.GetCommentInWorkspaceParams{ID: eventID, WorkspaceID: parseUUID(ws)}); err == nil {
		writeJSON(w, 200, map[string]any{"acknowledged": true, "comment_id": uuidToString(eventID), "duplicate": true})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 503, "job receipt unavailable")
		return
	}

	// A retry or another turn may already own this Native issue session. Leave
	// the durable observation pending rather than competing with that lease or
	// the one-pending-task index. Duplicate receipts above still acknowledge.
	active, err := q.HasActiveTaskForIssueAndAgent(r.Context(), db.HasActiveTaskForIssueAndAgentParams{IssueID: issue.ID, AgentID: agent.ID})
	if err != nil {
		writeError(w, 503, "job follow-up availability unknown")
		return
	}
	if active {
		writeJSON(w, 200, map[string]bool{"acknowledged": false, "pending_source_run": true})
		return
	}

	allowed := !agent.ArchivedAt.Valid && agent.RuntimeID.Valid && agent.RuntimeID == source.RuntimeID && source.Status != "cancelled" && h.canInvokeAgent(r.Context(), agent, "system", "", uuidToString(source.OriginatorUserID), ws)
	if !source.OriginatorUserID.Valid {
		allowed = false
	} else if _, err := q.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: source.OriginatorUserID, WorkspaceID: parseUUID(ws)}); err != nil {
		allowed = false
	}
	if _, err := q.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: runtime.OwnerID, WorkspaceID: parseUUID(ws)}); err != nil {
		allowed = false
	}
	// Reassignment, closure and revocation keep the observation visible without
	// silently restarting an old worker under a new owner's workflow.
	assigned := issue.AssigneeType.String == "agent" && issue.AssigneeID == agent.ID || issue.AssigneeType.String == "squad" && issue.AssigneeID == source.SquadID && source.SquadID.Valid
	allowed = allowed && assigned && issue.Status != "triage"
	if issue.Status == "done" || issue.Status == "closed" || issue.Status == "cancelled" {
		allowed = false
	}
	category, err := issuestatus.CategoryWithError(r.Context(), q, issue.WorkspaceID, issue.Status)
	if err != nil {
		writeError(w, 503, "issue status catalog unavailable")
		return
	}
	if category == "done" || category == "closed" {
		allowed = false
	}

	if issue.AssigneeType.String == "squad" && source.SquadID.Valid && !source.IsLeaderTask {
		member, err := q.IsSquadMember(r.Context(), db.IsSquadMemberParams{SquadID: source.SquadID, MemberType: "agent", MemberID: source.AgentID})
		if err != nil || !member {
			allowed = false
		}
	}
	if source.IsLeaderTask {
		squad, err := q.GetSquad(r.Context(), source.SquadID)
		if err != nil || squad.LeaderID != source.AgentID {
			allowed = false
		}
	}
	next := "Automatic follow-up was withheld because the source was cancelled, ownership changed, or current invocation access is unavailable. Review the observation before continuing."
	if allowed {
		next = "Inspect the existing job output and artifacts, then continue the requested task and parent handoff. Do not resubmit or relaunch training merely to query progress. A successful process does not prove the task is accepted; failure or interruption needs reconciliation."
	}
	exit := "unknown"
	if event.ExitCode != nil {
		exit = fmt.Sprint(*event.ExitCode)
	}
	content := fmt.Sprintf("[Native independent job observation]\nJob: %s\nStatus: %s\nExit code: %s\nSource run: %s\nSubmission: %s\n\n%s", event.JobID, event.Status, exit, uuidToString(source.ID), event.Key, next)
	// progress_update + source_task_id is reserved for delegated failure recovery.
	// Keep job observations ordinary system comments to avoid a second outbox.
	created, err := q.CreateComment(r.Context(), db.CreateCommentParams{ID: eventID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: content, Type: "comment", SourceTaskID: source.ID})
	if err != nil {
		writeError(w, 503, "job feedback could not be recorded")
		return
	}
	var followup db.AgentTaskQueue
	if allowed {
		overlay, apps := h.TaskService.BuildRuntimeMCPOverlayForMerge(r.Context(), source.OriginatorUserID, agent)
		origin := source.OriginatorSource
		if origin.String == "" {
			origin = pgtype.Text{String: "unattributed", Valid: true}
		}
		followup, err = q.CreateAgentTask(r.Context(), db.CreateAgentTaskParams{ID: dbid.NewV7(), AgentID: source.AgentID, RuntimeID: agent.RuntimeID, IssueID: issue.ID, Priority: source.Priority, TriggerCommentID: created.ID, TriggerSummary: pgtype.Text{String: "Independent job " + event.JobID + " " + event.Status, Valid: true}, HandoffNote: pgtype.Text{String: next, Valid: true}, IsLeaderTask: pgtype.Bool{Bool: source.IsLeaderTask, Valid: true}, SquadID: source.SquadID, OriginatorUserID: source.OriginatorUserID, AccountableUserID: source.AccountableUserID, OriginatorSource: origin, DelegatedFromTaskID: source.ID, RuleVersionID: source.RuleVersionID, TriggerEvidenceKind: pgtype.Text{String: "comment", Valid: true}, TriggerEvidenceRefID: created.ID, RuntimeMcpOverlay: overlay, RuntimeConnectedApps: apps})
		if err != nil {
			writeError(w, 503, "job follow-up could not be queued")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 503, "job feedback commit was not confirmed")
		return
	}
	resp := commentToResponse(created.Comment(), nil, nil)
	resp.IssueRevision = created.IssueRevision
	h.publish(protocol.EventCommentCreated, ws, "system", "", map[string]any{"comment": resp, "issue_revision": created.IssueRevision, "issue_status": issue.Status})
	if followup.ID.Valid {
		h.TaskService.NotifyTaskEnqueued(r.Context(), followup)
		h.publish(protocol.EventTaskQueued, ws, "system", "", map[string]any{"task_id": uuidToString(followup.ID), "agent_id": uuidToString(followup.AgentID), "issue_id": uuidToString(followup.IssueID), "status": "queued"})
	}
	writeJSON(w, 200, map[string]any{"acknowledged": true, "comment_id": uuidToString(eventID), "followup_queued": followup.ID.Valid, "status": strings.TrimSpace(event.Status)})
}
