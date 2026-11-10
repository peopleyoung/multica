package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeWriteReceiptsFenceReplayAndReconcileAfterRestart(t *testing.T) {
	for _, mode := range []string{"confirmed", "uncertain", "quick create", "rejected", "upload", "webhook configuration", "squad evaluation"} {
		t.Run(mode, func(t *testing.T) {
			const machine, daemonToken = "native-write-machine", "mdt_native_write_fixture"
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			runtimeID := dbfx.Runtime(t, "native write", testutil.Cols{"provider": "native", "daemon_id": machine})
			agentID := dbfx.Agent(t, "native write", runtimeID)
			taskContext := `{"workspace_id":"` + testWorkspaceID + `"}`
			if mode == "quick create" {
				taskContext = `{"type":"quick_create","workspace_id":"` + testWorkspaceID + `"}`
			}
			taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "running", "attempt": 1, "dispatched_at": dispatched, "context": taskContext})
			if mode != "quick create" {
				issueID := dbfx.Issue(t, "native write scope")
				dbfx.Exec(t, "UPDATE agent_task_queue SET issue_id=$2 WHERE id=$1", taskID, issueID)
			}
			dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(daemonToken), "expires_at": time.Now().Add(time.Hour)})
			call := llm.ToolCall{ID: "create-once", Name: "multica_create_issue", Arguments: json.RawMessage(`{"title":"Write once"}`)}
			path := "/api/issues"
			if mode == "upload" {
				call.Name, path = "multica_upload_artifact", "/api/upload-file"
			}
			if mode == "webhook configuration" {
				call.Name, path = "multica_manage_autopilot", "/api/autopilots/automation/triggers"
			}
			if mode == "squad evaluation" {
				call.Name, path = "multica_record_squad_evaluation", "/api/issues/issue/squad-evaluated"
			}
			cp := agentstate.Checkpoint{InputTaskID: taskID, Messages: []llm.ModelMessage{{Role: "user", Content: "Capture"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}}, Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolStarted}}}
			raw, _ := json.Marshal(cp)
			lease := uuid.NewString()
			sessionID := dbfx.Insert(t, "native_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": "run", "scope_id": taskID, "daemon_id": machine, "work_dir": t.TempDir(), "active_task_id": taskID, "active_dispatch": dispatched, "lease_token": lease, "lease_until": time.Now().Add(time.Minute), "checkpoint": raw})
			h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService}
			claim := agentstate.WriteClaim{SessionID: sessionID, LeaseToken: lease, CallID: call.ID}
			request := func(body string) *http.Request {
				r := newRequest(http.MethodPost, path, json.RawMessage(body))
				r.Header.Set("X-Actor-Source", "task_token")
				r.Header.Set("X-Task-ID", taskID)
				r.Header.Set("X-Agent-ID", agentID)
				raw, _ := json.Marshal(claim)
				r.Header.Set(agentstate.NativeWriteHeader, string(raw))
				return r
			}
			var calls atomic.Int32
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			wrapped := h.NativeWrite(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(entered)
				<-release
				if mode == "uncertain" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if mode == "rejected" {
					writeError(w, http.StatusForbidden, "not permitted")
					return
				}
				if mode == "upload" {
					writeJSON(w, http.StatusCreated, map[string]string{"id": "file-1", "filename": "result.txt", "url": "secret-storage", "download_url": "secret-capability"})
					return
				}
				if mode == "webhook configuration" {
					writeJSON(w, http.StatusCreated, map[string]string{"id": "trigger-1", "kind": "webhook", "webhook_token": "secret-token", "webhook_url": "secret-capability", "future_secret": "secret-future"})
					return
				}
				writeJSON(w, http.StatusCreated, map[string]string{"id": "created-resource"})
			}))
			first := httptest.NewRecorder()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				defer close(done)
				wrapped.ServeHTTP(first, request(`{"title":"Write once"}`).WithContext(ctx))
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("write did not start")
			}
			testutil.Call(t, wrapped.ServeHTTP, request(`{"title":"Write once"}`)).Want(http.StatusServiceUnavailable)
			// Simulate losing the client after its side effect. The bounded
			// receipt write still records the observed handler outcome.
			cancel()
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("write did not finish")
			}
			status := http.StatusCreated
			if mode == "uncertain" {
				status = http.StatusServiceUnavailable
			}
			if mode == "rejected" {
				status = http.StatusForbidden
			}
			if first.Code != status || calls.Load() != 1 {
				t.Fatalf("write=%d calls=%d", first.Code, calls.Load())
			}
			if mode == "upload" {
				var receipts string
				if err := testPool.QueryRow(context.Background(), "SELECT write_outcomes::text FROM native_session WHERE id=$1", sessionID).Scan(&receipts); err != nil || strings.Contains(receipts+first.Body.String(), "secret-") || !strings.Contains(receipts, "result.txt") {
					t.Fatalf("upload capability reached receipt/response: %s body=%s error=%v", receipts, first.Body.String(), err)
				}
			}
			if mode == "webhook configuration" {
				var receipts string
				if err := testPool.QueryRow(context.Background(), "SELECT write_outcomes::text FROM native_session WHERE id=$1", sessionID).Scan(&receipts); err != nil || strings.Contains(receipts+first.Body.String(), "secret-") || !strings.Contains(receipts, "trigger-1") {
					t.Fatalf("webhook capability reached receipt/response: %s body=%s error=%v", receipts, first.Body.String(), err)
				}
			}
			testutil.Call(t, wrapped.ServeHTTP, request(`{"title":"Write once"}`)).Want(status)
			testutil.Call(t, wrapped.ServeHTTP, request(`{"title":"Changed arguments"}`)).Want(http.StatusServiceUnavailable)
			// A different workspace cannot retrieve or replay the receipt.
			foreign := request(`{"title":"Write once"}`)
			foreign.Header.Set("X-Workspace-ID", uuid.NewString())
			testutil.Call(t, wrapped.ServeHTTP, foreign).Want(http.StatusNotFound)
			if mode == "quick create" {
				cp.Tools[0].Call.ID, cp.Messages[1].ToolCalls[0].ID = "different-call", "different-call"
				claim.CallID = "different-call"
				raw, _ := json.Marshal(cp)
				dbfx.Exec(t, "UPDATE native_session SET checkpoint=$2 WHERE id=$1", sessionID, raw)
				testutil.Call(t, wrapped.ServeHTTP, request(`{"title":"Write once"}`)).Want(http.StatusServiceUnavailable)
				cp.Tools[0].Call.ID, cp.Messages[1].ToolCalls[0].ID = call.ID, call.ID
				raw, _ = json.Marshal(cp)
				dbfx.Exec(t, "UPDATE native_session SET checkpoint=$2 WHERE id=$1", sessionID, raw)
			}
			newLease := uuid.NewString()
			dbfx.Exec(t, "UPDATE native_session SET lease_token=$2 WHERE id=$1", sessionID, newLease)
			cp.Tools[0].State = agentstate.ToolUnknown
			raw, _ = json.Marshal(cp)
			dbfx.Exec(t, "UPDATE native_session SET checkpoint=$2 WHERE id=$1", sessionID, raw)
			query := agentstate.WriteResultRequest{Claim: llm.NativeRequest{Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano)}, Session: agentstate.Session{ID: sessionID, LeaseToken: newLease}, CallID: call.ID}
			r := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/write-result", query), "taskId", taskID)
			r.Header.Set("Authorization", "Bearer "+daemonToken)
			r = r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine))
			restarted := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService}
			var result agentstate.WriteResult
			testutil.Call(t, restarted.NativeWriteResult, r).Want(http.StatusOK).JSON(&result)
			if result.Known != (mode != "uncertain") || result.Known && result.Status != status || calls.Load() != 1 {
				t.Fatalf("reconciliation result=%+v calls=%d", result, calls.Load())
			}
			state := agentstate.ToolSucceeded
			if mode == "rejected" {
				state = agentstate.ToolFailed
			}
			if result.Known && (result.Session == nil || result.Session.Version != 1 || result.Session.Checkpoint.Tools[0].State != state) {
				t.Fatalf("confirmed receipt did not resolve the unknown checkpoint: %+v", result)
			}
		})
	}
}
