package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

func TestNativeSessionLeaseVersionAndRecovery(t *testing.T) {
	const machine = "native-session-machine"
	const token = "mdt_native_session_fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native session", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native session", runtimeID)
	issueID := dbfx.Issue(t, "native session")
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 2, "dispatched_at": dispatched})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	t.Cleanup(func() {
		if err := testHandler.Queries.DeleteNativeSessionsByScope(context.Background(), db.DeleteNativeSessionsByScopeParams{WorkspaceID: parseUUID(testWorkspaceID), ScopeType: "issue", ScopeID: parseUUID(issueID)}); err != nil {
			t.Error(err)
		}
	})
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: "http://unused.invalid", DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}})}
	claim := llm.NativeRequest{Attempt: 2, DispatchedAt: dispatched.Format(time.RFC3339Nano)}
	request := func(body any, operation string) *http.Request {
		r := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/session", body), "taskId", taskID, "operation", operation)
		r.Header.Set("Authorization", "Bearer "+token)
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine))
	}
	acquire := agentstate.SessionRequest{Claim: claim, WorkDir: t.TempDir(), LeaseToken: uuid.NewString()}
	var session agentstate.Session
	testutil.Call(t, h.AcquireNativeSession, request(acquire, "")).Want(http.StatusOK).JSON(&session)
	var duplicate agentstate.Session
	testutil.Call(t, h.AcquireNativeSession, request(acquire, "")).Want(http.StatusOK).JSON(&duplicate)
	if session.ID != duplicate.ID || session.LeaseToken != duplicate.LeaseToken {
		t.Fatal("same executor acquire was not idempotent")
	}
	competitor := acquire
	competitor.LeaseToken = uuid.NewString()
	testutil.Call(t, h.AcquireNativeSession, request(competitor, "")).Want(http.StatusConflict)
	competitor = acquire
	competitor.WorkDir = t.TempDir()
	testutil.Call(t, h.AcquireNativeSession, request(competitor, "")).Want(http.StatusConflict)
	session.Checkpoint.Messages = []llm.ModelMessage{{Role: "user", Content: "change a file"}}
	old := session
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: session}, "save")).Want(http.StatusOK).JSON(&session)
	if session.Version != 1 {
		t.Fatal("checkpoint version did not increment")
	}
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: old}, "save")).Want(http.StatusConflict)
	// A heartbeat racing a successful save/summary may carry the old version.
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: old}, "renew")).Want(http.StatusOK)
	otherLease := old
	otherLease.LeaseToken = uuid.NewString()
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: otherLease}, "renew")).Want(http.StatusConflict)
	call := llm.ToolCall{ID: "patch-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt"}`)}
	session.Checkpoint.Messages = append(session.Checkpoint.Messages, llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{call}})
	session.Checkpoint.Tools = []agentstate.ToolRecord{{Call: call, State: agentstate.ToolPlanned}}
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: session}, "save")).Want(http.StatusOK).JSON(&session)
	if err := session.Checkpoint.Tools[0].Start(); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: session}, "save")).Want(http.StatusOK).JSON(&session)
	rollback := session
	rollback.Checkpoint.Tools = append([]agentstate.ToolRecord(nil), session.Checkpoint.Tools...)
	rollback.Checkpoint.Tools[0].State = agentstate.ToolPlanned
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: rollback}, "save")).Want(http.StatusBadRequest)
	// Simulate a process crash. A new executor obtains a new fencing token;
	// the previous executor cannot save its late tool result.
	if _, err := testPool.Exec(context.Background(), "UPDATE native_session SET lease_until=now()-interval '1 second' WHERE id=$1", session.ID); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: session}, "renew")).Want(http.StatusConflict)
	acquire.LeaseToken = uuid.NewString()
	var recovered agentstate.Session
	testutil.Call(t, h.AcquireNativeSession, request(acquire, "")).Want(http.StatusOK).JSON(&recovered)
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: session}, "save")).Want(http.StatusConflict)
	recovered.Checkpoint.Recover()
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: recovered}, "save")).Want(http.StatusOK).JSON(&recovered)
	if recovered.Checkpoint.Tools[0].State != agentstate.ToolUnknown || recovered.Checkpoint.Tools[0].Start() == nil {
		t.Fatal("uncertain write was replayable")
	}
	if !recovered.Checkpoint.CloseInterruptedGroup() {
		t.Fatal("unknown result cannot be handed back for investigation")
	}
	forged := recovered
	forged.Checkpoint.Messages = append([]llm.ModelMessage(nil), recovered.Checkpoint.Messages...)
	forged.Checkpoint.Messages[len(forged.Checkpoint.Messages)-1].Content = "The operation failed, retry it."
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: forged}, "save")).Want(http.StatusBadRequest)
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: recovered}, "save")).Want(http.StatusOK).JSON(&recovered)
	if len(recovered.Checkpoint.Tools) != 0 || !strings.Contains(recovered.Checkpoint.Messages[len(recovered.Checkpoint.Messages)-1].Content, "TOOL_OUTCOME_UNKNOWN") {
		t.Fatal("API lost explicit uncertainty")
	}
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: recovered}, "release")).Want(http.StatusNoContent)
	testutil.Call(t, h.NativeCheckpoint, request(agentstate.CheckpointRequest{Claim: claim, Session: recovered}, "renew")).Want(http.StatusConflict)
}

func TestNativeSessionFreshStartIsFencedAndIdempotent(t *testing.T) {
	const machine, token = "native-fresh-machine", "mdt_native_fresh_fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native fresh", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native fresh", runtimeID)
	issueID := dbfx.Issue(t, "native fresh")
	oldTask := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "completed"})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "force_fresh_session": true})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	sessionID := dbfx.Insert(t, "native_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": "issue", "scope_id": issueID,
		"daemon_id": machine, "work_dir": t.TempDir(), "generation_task_id": oldTask, "version": 7,
		"checkpoint":     `{"messages":[{"role":"user","content":"previous"},{"role":"assistant","content":"done"}]}`,
		"write_outcomes": `{"old-operation":{"state":"finished","digest":"previous","call_id":"old","status":201,"body":{"id":"old-resource"}}}`,
	})
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService}
	claim := llm.NativeRequest{Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano)}
	input := agentstate.SessionRequest{Claim: claim, WorkDir: t.TempDir(), LeaseToken: uuid.NewString()}
	request := func() *http.Request {
		r := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/session", input), "taskId", taskID)
		r.Header.Set("Authorization", "Bearer "+token)
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine))
	}
	var first, duplicate agentstate.Session
	testutil.Call(t, h.AcquireNativeSession, request()).Want(http.StatusOK).JSON(&first)
	testutil.Call(t, h.AcquireNativeSession, request()).Want(http.StatusOK).JSON(&duplicate)
	if first.ID != sessionID || first.Version != 8 || duplicate.Version != first.Version || duplicate.LeaseToken != first.LeaseToken || len(first.Checkpoint.Messages) != 0 {
		t.Fatalf("fresh acquire lost fencing/idempotency: first=%+v duplicate=%+v", first, duplicate)
	}
	var receipts string
	if err := testPool.QueryRow(context.Background(), "SELECT write_outcomes::text FROM native_session WHERE id=$1", sessionID).Scan(&receipts); err != nil || receipts != "{}" {
		t.Fatalf("fresh generation retained previous write receipts: %q, error=%v", receipts, err)
	}
}

func TestNativeInputIdentityFollowsOnlyAutomaticRetryWithinScope(t *testing.T) {
	agentID := dbfx.Agent(t, "native input", testRuntimeID)
	issueID := dbfx.Issue(t, "native input")
	root := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": testRuntimeID, "issue_id": issueID})
	child := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": testRuntimeID, "issue_id": issueID, "retry_of_task_id": root})
	last := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": testRuntimeID, "issue_id": issueID, "retry_of_task_id": child})
	manual := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": testRuntimeID, "issue_id": issueID, "rerun_of_task_id": root})
	foreign := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": testRuntimeID, "issue_id": dbfx.Issue(t, "different native scope"), "retry_of_task_id": root})
	for id, expected := range map[string]string{root: root, child: root, last: root, manual: manual, foreign: ""} {
		row, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		actual, err := testHandler.nativeInputTaskID(context.Background(), row)
		if actual != expected || (err != nil) != (expected == "") {
			t.Fatalf("input %s: got=%q expected=%q error=%v", id, actual, expected, err)
		}
	}
}
