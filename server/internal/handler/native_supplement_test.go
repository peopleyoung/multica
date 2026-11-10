package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeSupplementAcceptanceAndCheckpointCommitTogether(t *testing.T) {
	const machine, token = "native-supplement-machine", "mdt_native_supplement_fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	f := newSupplementFixture(t, "native", "running", true)
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id=$2 WHERE id=$1`, f.runtimeID, machine)
	dbfx.Exec(t, `UPDATE agent_task_queue SET attempt=1, dispatched_at=$2 WHERE id=$1`, f.taskID, dispatched)
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	var comment CommentResponse
	supplementRequest(t, f, uuid.NewString(), "keep the original objective and add tests").Want(http.StatusCreated).JSON(&comment)
	claim, err := testHandler.Queries.ClaimNextTaskSupplement(t.Context(), parseUUID(f.taskID))
	if err != nil || uuidToString(claim.CommentID) != comment.ID {
		t.Fatalf("initial claim: %+v %v", claim, err)
	}
	// A restart before the checkpoint can reclaim the durable input.
	claim, err = testHandler.Queries.ClaimNextTaskSupplement(t.Context(), parseUUID(f.taskID))
	if err != nil || uuidToString(claim.CommentID) != comment.ID || claim.AttemptCount != 2 {
		t.Fatalf("reclaim: %+v %v", claim, err)
	}
	h := &Handler{Queries: testHandler.Queries, TxStarter: testPool, Bus: testHandler.Bus, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: "http://unused.invalid", DefaultModel: "fixture", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 32}})}
	request := func(body any, operation string) *http.Request {
		r := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/session", body), "taskId", f.taskID, "operation", operation)
		r.Header.Set("Authorization", "Bearer "+token)
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine))
	}
	inputClaim := llm.NativeRequest{Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano)}
	var session agentstate.Session
	testutil.Call(t, h.AcquireNativeSession, request(agentstate.SessionRequest{Claim: inputClaim, WorkDir: t.TempDir(), LeaseToken: uuid.NewString()}, "")).Want(http.StatusOK).JSON(&session)
	session.Checkpoint.InputTaskID = f.taskID
	session.Checkpoint.Messages = []llm.ModelMessage{{Role: "user", Content: "original objective"}}
	save := func() *testutil.Response {
		return testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: inputClaim, Session: session}, "save"))
	}
	save().Want(http.StatusOK).JSON(&session)
	session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "user", Content: claim.Content})
	session.Checkpoint.SupplementIDs = []string{uuid.NewString()}
	save().Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM native_session WHERE id=$1 AND version=1 AND jsonb_array_length(checkpoint->'messages')=1`, session.ID); n != 1 {
		t.Fatal("invalid receipt committed model history")
	}
	session.Checkpoint.SupplementIDs = []string{comment.ID}
	// A receipt write failure must also roll back the preceding history save.
	dbfx.Exec(t, `ALTER TABLE task_supplement ADD CONSTRAINT native_supplement_test_reject CHECK (comment_id <> '`+comment.ID+`'::uuid OR status <> 'delivered') NOT VALID`)
	t.Cleanup(func() {
		dbfx.Exec(t, `ALTER TABLE task_supplement DROP CONSTRAINT IF EXISTS native_supplement_test_reject`)
	})
	save().Want(http.StatusInternalServerError)
	if n := dbfx.Count(t, `SELECT count(*) FROM native_session WHERE id=$1 AND version=1`, session.ID); n != 1 {
		t.Fatal("receipt failure left a committed checkpoint")
	}
	dbfx.Exec(t, `ALTER TABLE task_supplement DROP CONSTRAINT native_supplement_test_reject`)
	save().Want(http.StatusOK).JSON(&session)
	if session.Version != 2 || len(session.Checkpoint.SupplementIDs) != 1 {
		t.Fatal("accepted input not checkpointed")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement WHERE task_id=$1 AND comment_id=$2 AND status='delivered' AND delivered_at IS NOT NULL`, f.taskID, comment.ID); n != 1 {
		t.Fatal("checkpoint committed without a delivered receipt")
	}
	if _, err := testHandler.Queries.AckTaskSupplementFailed(t.Context(), db.AckTaskSupplementFailedParams{TaskID: parseUUID(f.taskID), CommentID: parseUUID(comment.ID)}); err == nil {
		t.Fatal("lost response downgraded durable acceptance")
	}
	if _, err := testHandler.Queries.ClaimNextTaskSupplement(t.Context(), parseUUID(f.taskID)); err == nil {
		t.Fatal("accepted input was redelivered")
	}
}
