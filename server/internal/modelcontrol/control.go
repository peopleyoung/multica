// Package modelcontrol owns shared admission and usage for the deployment model.
// PostgreSQL is authoritative across API replicas; no upstream request runs
// inside a database transaction or retains a pooled connection.
package modelcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type Limits struct {
	GlobalConcurrent           int64 `json:"global_concurrent"`
	WorkspaceConcurrent        int64 `json:"workspace_concurrent"`
	GlobalRequestsPerMinute    int64 `json:"global_requests_per_minute"`
	WorkspaceRequestsPerMinute int64 `json:"workspace_requests_per_minute"`
	GlobalTokensPerMinute      int64 `json:"global_tokens_per_minute"`
	WorkspaceTokensPerMinute   int64 `json:"workspace_tokens_per_minute"`
}

func DefaultLimits() Limits {
	return Limits{16, 4, 600, 60, 4_000_000, 1_000_000}
}

// ParseLimits overlays explicit positive limits onto the deployment defaults.
// Configuration is shared by all API replicas; zero never disables a guard.
func ParseLimits(raw string) (Limits, error) {
	limits := DefaultLimits()
	if strings.TrimSpace(raw) != "" {
		if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
			return Limits{}, errors.New("model limits must be a JSON object")
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(&limits) != nil || dec.Decode(new(any)) != io.EOF {
			return Limits{}, errors.New("model limits must be one JSON object with known fields")
		}
	}
	if limits.GlobalConcurrent < 1 || limits.WorkspaceConcurrent < 1 || limits.GlobalRequestsPerMinute < 1 || limits.WorkspaceRequestsPerMinute < 1 || limits.GlobalTokensPerMinute < 1 || limits.WorkspaceTokensPerMinute < 1 || limits.GlobalTokensPerMinute > 1_000_000_000_000 || limits.WorkspaceTokensPerMinute > 1_000_000_000_000 {
		return Limits{}, errors.New("model limits must be positive and token limits at most 1000000000000")
	}
	return limits, nil
}

type txStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

type Controller struct {
	txs     txStarter
	limits  Limits
	metrics *metrics.BusinessMetrics
}

func New(txs txStarter, limits Limits, recorder *metrics.BusinessMetrics) *Controller {
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	return &Controller{txs: txs, limits: limits, metrics: recorder}
}

func (c *Controller) Acquire(ctx context.Context, call llm.Call) (func(context.Context, llm.CallResult) error, error) {
	workspace, err := util.ParseUUID(call.Scope.WorkspaceID)
	if err != nil || c.txs == nil || call.ReservedTokens <= 0 || call.ReservedTokens > 1_000_000_000 || len(call.Model) > 256 {
		return nil, llm.ErrAdmissionUnavailable
	}
	var task, session pgtype.UUID
	if call.Scope.TaskID != "" {
		task, err = util.ParseUUID(call.Scope.TaskID)
		if err != nil {
			return nil, llm.ErrAdmissionUnavailable
		}
	}
	if call.Scope.SessionID != "" {
		session, err = util.ParseUUID(call.Scope.SessionID)
		if err != nil {
			return nil, llm.ErrAdmissionUnavailable
		}
	}
	key := ""
	switch call.Scope.Purpose {
	case "connection_test":
		key = "connection_test:" + call.Scope.WorkspaceID
	case "execution", "summary":
		if !task.Valid {
			return nil, llm.ErrAdmissionUnavailable
		}
		key = "run:" + util.UUIDToString(task)
	case "title", "quick_actions":
		if !session.Valid {
			return nil, llm.ErrAdmissionUnavailable
		}
		key = call.Scope.Purpose + ":" + util.UUIDToString(session)
	default:
		return nil, llm.ErrAdmissionUnavailable
	}
	admitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := c.txs.Begin(admitCtx)
	if err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	defer tx.Rollback(context.Background())
	q := db.New(tx)
	// Lock the workspace first, matching teardown's ownership fence. Never
	// insert an audit row after that workspace's deletion has committed.
	if _, err := q.LockModelCallWorkspace(admitCtx, workspace); err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	if err := q.LockModelAdmission(admitCtx); err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	totals, err := q.GetModelAdmissionTotals(admitCtx, db.GetModelAdmissionTotalsParams{WorkspaceID: workspace, ScopeKey: key})
	if err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	l := c.limits
	if totals.GlobalActive >= l.GlobalConcurrent || totals.WorkspaceActive >= l.WorkspaceConcurrent || totals.ScopeActive > 0 || totals.GlobalRequests >= l.GlobalRequestsPerMinute || totals.WorkspaceRequests >= l.WorkspaceRequestsPerMinute || call.ReservedTokens > l.GlobalTokensPerMinute-totals.GlobalTokens || call.ReservedTokens > l.WorkspaceTokensPerMinute-totals.WorkspaceTokens {
		c.metrics.RecordModelAdmission(call.Scope.Purpose, "denied")
		return nil, llm.ErrQuotaExceeded
	}
	id, err := q.ReserveModelCall(admitCtx, db.ReserveModelCallParams{WorkspaceID: workspace, TaskID: task, SessionID: session, Purpose: call.Scope.Purpose, ScopeKey: key, Model: call.Model, RetryAttempt: int32(call.RetryAttempt), ReservedTokens: call.ReservedTokens})
	if err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	if err := q.PruneModelCalls(admitCtx); err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	if err := tx.Commit(admitCtx); err != nil {
		return nil, llm.ErrAdmissionUnavailable
	}
	c.metrics.RecordModelAdmission(call.Scope.Purpose, "allowed")
	return func(ctx context.Context, result llm.CallResult) error {
		finishTx, err := c.txs.Begin(ctx)
		if err != nil {
			return err
		}
		defer finishTx.Rollback(context.Background())
		params := db.FinishModelCallParams{ID: id, WorkspaceID: workspace, Outcome: result.Outcome,
			DurationMs: pgtype.Int8{Int64: result.Duration.Milliseconds(), Valid: true}, HttpStatus: pgtype.Int4{Int32: int32(result.HTTPStatus), Valid: true}}
		if u := result.Usage; u != nil {
			params.InputTokens = pgtype.Int8{Int64: u.InputTokens, Valid: true}
			params.OutputTokens = pgtype.Int8{Int64: u.OutputTokens, Valid: true}
			params.CachedTokens = pgtype.Int8{Int64: u.CachedTokens, Valid: true}
		}
		n, err := db.New(finishTx).FinishModelCall(ctx, params)
		if err != nil {
			return err
		}
		if err := finishTx.Commit(ctx); err != nil {
			return err
		}
		if n > 0 {
			c.metrics.RecordModelCall(call, result)
			slog.Info("model API attempt finished", "call_id", util.UUIDToString(id), "workspace_id", call.Scope.WorkspaceID, "task_id", call.Scope.TaskID, "purpose", call.Scope.Purpose, "outcome", result.Outcome, "duration_ms", result.Duration.Milliseconds(), "retry_attempt", call.RetryAttempt, "usage_reported", result.Usage != nil)
		}
		return nil
	}, nil
}
