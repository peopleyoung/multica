package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeInferenceAuthorizesClaimAndRedactsErrors(t *testing.T) {
	const token = "mdt_native_inference_fixture"
	const machine = "native-test-machine"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native inference", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native inference", runtimeID)
	issueID := dbfx.Issue(t, "native inference")
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 2, "dispatched_at": dispatched})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: srv.URL, DefaultModel: "fixture-model", Capabilities: caps})}
	input := llm.NativeRequest{Attempt: 2, DispatchedAt: dispatched.Format(time.RFC3339Nano), Input: llm.Request{Messages: []llm.ModelMessage{{Role: "user", Content: "private text"}}, MaxOutputTokens: 100}}
	leaseToken := uuid.NewString()
	history, err := json.Marshal(map[string]any{"messages": input.Input.Messages})
	if err != nil {
		t.Fatal(err)
	}
	input.SessionID = dbfx.Insert(t, "native_session", testutil.Cols{
		"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": "issue", "scope_id": issueID,
		"daemon_id": machine, "work_dir": t.TempDir(), "active_task_id": taskID, "active_dispatch": dispatched,
		"lease_token": leaseToken, "lease_until": time.Now().Add(time.Minute), "checkpoint": history,
	})
	input.LeaseToken = leaseToken
	request := func(body any, daemon string) *http.Request {
		r := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/inference", body), "taskId", taskID)
		r.Header.Set("Authorization", "Bearer "+token)
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, daemon))
	}
	for _, tc := range []struct {
		name    string
		body    any
		machine string
		status  int
	}{
		{"other daemon", input, "other-machine", http.StatusForbidden},
		{"stale attempt", llm.NativeRequest{Attempt: 1, DispatchedAt: input.DispatchedAt, Input: input.Input}, machine, http.StatusForbidden},
		{"stale generation", llm.NativeRequest{Attempt: 2, DispatchedAt: dispatched.Add(-time.Second).Format(time.RFC3339Nano), Input: input.Input}, machine, http.StatusForbidden},
		{"model override", `{"attempt":2,"input":{"model":"other-model"}}`, machine, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) { testutil.Call(t, h.NativeInference, request(tc.body, tc.machine)).Want(tc.status) })
	}
	if requests != 0 {
		t.Fatal("unauthorized requests reached the model")
	}
	stale := input
	stale.SessionVersion++
	testutil.Call(t, h.NativeInference, request(stale, machine)).Want(http.StatusForbidden)
	otherLease := input
	otherLease.LeaseToken = uuid.NewString()
	testutil.Call(t, h.NativeInference, request(otherLease, machine)).Want(http.StatusForbidden)
	otherHistory := input
	otherHistory.Input.Messages = []llm.ModelMessage{{Role: "user", Content: "not saved"}}
	testutil.Call(t, h.NativeInference, request(otherHistory, machine)).Want(http.StatusForbidden)
	otherSession := input
	otherSession.SessionID = uuid.NewString()
	testutil.Call(t, h.NativeInference, request(otherSession, machine)).Want(http.StatusForbidden)
	otherWorkspace := request(input, machine)
	otherWorkspace = otherWorkspace.WithContext(middleware.WithDaemonContext(otherWorkspace.Context(), uuid.NewString(), machine))
	testutil.Call(t, h.NativeInference, otherWorkspace).Want(http.StatusForbidden)
	response := testutil.Call(t, h.NativeInference, request(input, machine)).Want(http.StatusOK)
	if !strings.Contains(response.Body.String(), `"response"`) || !strings.Contains(response.Body.String(), "hello") || requests != 1 {
		t.Fatalf("unexpected stream: %s calls=%d", response.Body.String(), requests)
	}
	// Revocation is checked against the database even with cached daemon context.
	if _, err := testPool.Exec(context.Background(), "DELETE FROM daemon_token WHERE token_hash=$1", auth.HashToken(token)); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, h.NativeInference, request(input, machine)).Want(http.StatusForbidden)
	if requests != 1 {
		t.Fatal("revoked credential reached the model")
	}
}

func TestNativeInferenceRevocationCancelsAnOpenStream(t *testing.T) {
	for _, revoke := range []string{"credential", "runtime", "claim", "lease", "issue", "chat", "builder", "summary credential", "summary claim", "summary lease"} {
		t.Run(revoke, func(t *testing.T) {
			compact := strings.HasPrefix(revoke, "summary ")
			revoke = strings.TrimPrefix(revoke, "summary ")
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			machine, token := "native-revoke-"+uuid.NewString(), "mdt_"+uuid.NewString()
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			runtimeID := dbfx.Runtime(t, "native revoke", testutil.Cols{"provider": "native", "daemon_id": machine})
			agentCols := testutil.Cols{}
			if revoke == "builder" {
				agentCols["kind"], agentCols["system_key"] = "system", "agent_builder:"+uuid.NewString()
			}
			agentID := dbfx.Agent(t, "native revoke", runtimeID, agentCols)
			scopeType, scopeID := "issue", dbfx.Issue(t, "native revoke")
			taskCols := testutil.Cols{"runtime_id": runtimeID, "issue_id": scopeID, "status": "running", "attempt": 1, "dispatched_at": dispatched}
			if revoke == "chat" || revoke == "builder" {
				scopeType, scopeID = "chat", dbfx.ChatSession(t, agentID)
				delete(taskCols, "issue_id")
				taskCols["chat_session_id"] = scopeID
			}
			taskID := dbfx.Task(t, agentID, taskCols)
			dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
			input := llm.NativeRequest{Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), LeaseToken: uuid.NewString(), Input: llm.Request{MaxOutputTokens: 100, Messages: []llm.ModelMessage{{Role: "user", Content: "private request"}}}}
			if compact {
				input.Input.Messages = []llm.ModelMessage{{Role: "system", Content: "permissions"}, {Role: "user", Content: "prior request"}, {Role: "assistant", Content: "prior result"}, {Role: "user", Content: "private request"}}
			}
			history, _ := json.Marshal(map[string]any{"messages": input.Input.Messages})
			input.SessionID = dbfx.Insert(t, "native_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": scopeType, "scope_id": scopeID,
				"daemon_id": machine, "work_dir": t.TempDir(), "active_task_id": taskID, "active_dispatch": dispatched,
				"lease_token": input.LeaseToken, "lease_until": time.Now().Add(time.Minute), "checkpoint": history})
			opened, stopped := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(stopped)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
				w.(http.Flusher).Flush()
				close(opened)
				<-r.Context().Done()
			}))
			defer upstream.Close()
			defer cancel()
			h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService,
				LLM: llm.New(llm.Config{BaseURL: upstream.URL, DefaultModel: "fixture-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}})}
			req := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/inference", input).WithContext(ctx), "taskId", taskID)
			handle := h.NativeInference
			if compact {
				handle = h.NativeCheckpoint
				req = testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/checkpoint/compact", agentstate.CheckpointRequest{Claim: input, Session: agentstate.Session{ID: input.SessionID, LeaseToken: input.LeaseToken}}).WithContext(ctx), "taskId", taskID, "operation", "compact")
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req = req.WithContext(middleware.WithDaemonContext(req.Context(), testWorkspaceID, machine))
			response, done := httptest.NewRecorder(), make(chan struct{})
			go func() { defer close(done); handle(response, req) }()
			select {
			case <-opened:
			case <-done:
				t.Fatalf("model step rejected before opening stream: %d %s", response.Code, response.Body.String())
			case <-ctx.Done():
				t.Fatal("model stream did not open")
			}
			switch revoke {
			case "issue":
				r := testutil.WithURLParams(newRequest(http.MethodDelete, "/api/issues/"+scopeID, nil), "id", scopeID)
				testutil.Call(t, testHandler.DeleteIssue, withChatTestWorkspaceCtx(t, r)).Want(http.StatusNoContent)
			case "chat", "builder":
				r := testutil.WithURLParams(newRequest(http.MethodDelete, "/api/chat/sessions/"+scopeID, nil), "sessionId", scopeID)
				testutil.Call(t, testHandler.DeleteChatSession, withChatTestWorkspaceCtx(t, r)).Want(http.StatusNoContent)
			case "credential":
				dbfx.Exec(t, "DELETE FROM daemon_token WHERE token_hash=$1", auth.HashToken(token))
			case "runtime":
				dbfx.Exec(t, "UPDATE agent_runtime SET status='offline' WHERE id=$1", runtimeID)
			case "claim":
				dbfx.Exec(t, "UPDATE agent_task_queue SET attempt=attempt+1 WHERE id=$1", taskID)
			case "lease":
				dbfx.Exec(t, "UPDATE native_session SET lease_until=now()-interval '1 second' WHERE id=$1", input.SessionID)
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("revoked run kept its model stream open")
			}
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("revocation did not cancel the upstream request")
			}
			if revoke == "issue" || revoke == "chat" || revoke == "builder" {
				if dbfx.Count(t, "SELECT count(*) FROM native_session WHERE id=$1", input.SessionID) != 0 {
					t.Fatal("deleted scope regained model history")
				}
				late := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/inference", input).WithContext(ctx), "taskId", taskID)
				late.Header.Set("Authorization", "Bearer "+token)
				late = late.WithContext(middleware.WithDaemonContext(late.Context(), testWorkspaceID, machine))
				testutil.Call(t, h.NativeInference, late).Want(http.StatusForbidden)
			}
			body := response.Body.String()
			wantError := "model_step_cancelled"
			if compact {
				wantError = "original history is preserved"
			}
			if !strings.Contains(body, wantError) || strings.Contains(body, `"response"`) || strings.Contains(body, "private request") {
				t.Fatalf("revoked stream produced a final response or leaked input: %s", body)
			}
		})
	}
}
