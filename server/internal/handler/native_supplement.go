package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Accepting an input and saving its model history are one transaction. A lost
// HTTP response cannot turn a committed acceptance into a failed receipt or
// cause a second injection. Lock the task first, matching terminal settlement.
func (h *Handler) saveNativeSupplement(r *http.Request, task db.AgentTaskQueue, params db.SaveNativeCheckpointParams, commentID pgtype.UUID) (db.NativeSession, error) {
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return db.NativeSession{}, err
	}
	defer tx.Rollback(r.Context())
	q := h.Queries.WithTx(tx)
	locked, err := q.LockAgentTaskStartClaim(r.Context(), db.LockAgentTaskStartClaimParams{ID: task.ID, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt})
	if err != nil {
		return db.NativeSession{}, err
	}
	if locked.Status != "running" {
		return db.NativeSession{}, pgx.ErrNoRows
	}
	receipt, err := q.GetTaskSupplementForRun(r.Context(), db.GetTaskSupplementForRunParams{CommentID: commentID, TaskID: task.ID, WorkspaceID: params.WorkspaceID})
	if err != nil {
		return db.NativeSession{}, err
	}
	if receipt.Status != "delivering" {
		return db.NativeSession{}, pgx.ErrNoRows
	}
	row, err := q.SaveNativeCheckpoint(r.Context(), params)
	if err != nil {
		return db.NativeSession{}, err
	}
	receipt, err = q.AckTaskSupplementDelivered(r.Context(), db.AckTaskSupplementDeliveredParams{TaskID: task.ID, CommentID: commentID})
	if err != nil {
		return db.NativeSession{}, err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return db.NativeSession{}, err
	}
	h.publishTaskSupplementUpdate(r, receipt)
	return row, nil
}
