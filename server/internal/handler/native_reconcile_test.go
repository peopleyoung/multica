package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeOperatorReconciliationIsVersionedAuthorizedAndIdle(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "native reconciliation", testRuntimeID)
	issueID := dbfx.Issue(t, "native reconciliation")
	call := llm.ToolCall{ID: "legacy", Name: "shell", Arguments: json.RawMessage(`{"command":"do not replay"}`)}
	cp := agentstate.Checkpoint{Messages: []llm.ModelMessage{{Role: "user", Content: "task"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolUnknown}}}
	raw, _ := json.Marshal(cp)
	sessionID := dbfx.Insert(t, "native_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": "issue", "scope_id": issueID, "daemon_id": "reconcile-test", "work_dir": t.TempDir(), "version": 7, "checkpoint": string(raw)})
	operator := parseUUID(testUserID)
	_, err := testHandler.Queries.GetNativeSessionForOperator(ctx, db.GetNativeSessionForOperatorParams{ID: parseUUID(sessionID), OperatorID: operator})
	if err != nil {
		t.Fatal(err)
	}
	wrongOperator := parseUUID(uuid.NewString())
	if _, err := testHandler.Queries.GetNativeSessionForOperator(ctx, db.GetNativeSessionForOperatorParams{ID: parseUUID(sessionID), OperatorID: wrongOperator}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("non-manager could inspect recovery checkpoint")
	}
	next, err := cp.ReconcileShell(call.ID, "failed", testUserID, "Remote process exited; training log confirms missing weights.", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(next)
	params := db.ReconcileInactiveNativeCheckpointParams{ID: parseUUID(sessionID), OperatorID: operator, ExpectedVersion: 7, Checkpoint: raw}
	reject := func(p db.ReconcileInactiveNativeCheckpointParams) {
		t.Helper()
		if _, err := testHandler.Queries.ReconcileInactiveNativeCheckpoint(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unsafe reconciliation accepted: %v", err)
		}
	}
	stale := params
	stale.ExpectedVersion = 6
	reject(stale)
	unauthorized := params
	unauthorized.OperatorID = wrongOperator
	reject(unauthorized)
	if _, err := testPool.Exec(ctx, "UPDATE native_session SET lease_until=now()+interval '1 minute' WHERE id=$1", sessionID); err != nil {
		t.Fatal(err)
	}
	reject(params)
	if _, err := testPool.Exec(ctx, "UPDATE native_session SET lease_until=NULL WHERE id=$1", sessionID); err != nil {
		t.Fatal(err)
	}
	running := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issueID, "status": "running"})
	reject(params)
	if _, err := testPool.Exec(ctx, "UPDATE agent_task_queue SET status='completed' WHERE id=$1", running); err != nil {
		t.Fatal(err)
	}
	row, err := testHandler.Queries.ReconcileInactiveNativeCheckpoint(ctx, params)
	if err != nil || row.Version != 8 {
		t.Fatalf("verified idle reconciliation failed: %v", err)
	}
	var saved agentstate.Checkpoint
	if err := json.Unmarshal(row.Checkpoint, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Tools[0].State != agentstate.ToolFailed || saved.Tools[0].Result == nil || len(saved.Messages) != len(cp.Messages) {
		t.Fatal("verified result or complete history lost")
	}
	reject(params)
}
