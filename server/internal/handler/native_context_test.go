package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeContextCompactionIsAtomicAndUsesDeploymentModel(t *testing.T) {
	testNativeContextCompaction(t, 0, true, "Prior objective completed.", http.StatusOK, false)
}

func TestNativeContextCompactionAllowsSlowSummary(t *testing.T) {
	testNativeContextCompaction(t, 26*time.Second, false, "Prior objective completed.", http.StatusOK, false)
}

func TestNativeContextCompactionRejectsBlankSummaryWithoutLosingHistory(t *testing.T) {
	testNativeContextCompaction(t, 0, false, "\n \t", http.StatusBadGateway, false)
}

func TestNativeContextCompactionRecoversFromOneEmptySummary(t *testing.T) {
	testNativeContextCompaction(t, 0, false, "Recovered objective and confirmed tool outcomes.", http.StatusOK, true)
}

func testNativeContextCompaction(t *testing.T, delay time.Duration, testFailure bool, summary string, wantStatus int, emptyFirst bool) {
	t.Helper()
	const machine, token = "native-context-fixture", "mdt_native_context_fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native context", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native context", runtimeID)
	issueID := dbfx.Issue(t, "native context")
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	checkpoint := agentstate.Checkpoint{InputTaskID: taskID, Messages: []llm.ModelMessage{{Role: "system", Content: "permissions"}, {Role: "user", Content: "prior objective"}, {Role: "assistant", Content: "prior result"}, {Role: "user", Content: "current objective"}}}
	raw, _ := json.Marshal(checkpoint)
	lease := uuid.NewString()
	sessionID := dbfx.Insert(t, "native_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "scope_type": "issue", "scope_id": issueID, "daemon_id": machine, "work_dir": t.TempDir(), "active_task_id": taskID, "active_dispatch": dispatched, "lease_token": lease, "lease_until": time.Now().Add(time.Minute), "checkpoint": raw})
	fail, calls := false, 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input struct {
			Model string
			Tools []json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != "one-model" || len(input.Tools) > 0 {
			t.Error("summary did not use single deployment without tools")
		}
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		content := summary
		if emptyFirst {
			if calls == 1 {
				content = "\n\n"
			} else {
				var version int64
				var stored []byte
				var after agentstate.Checkpoint
				if err := testPool.QueryRow(context.Background(), "SELECT version, checkpoint FROM native_session WHERE id=$1", sessionID).Scan(&version, &stored); err != nil {
					t.Error("cannot read original checkpoint during summary recovery")
				} else if json.Unmarshal(stored, &after) != nil || version != 0 || !reflect.DeepEqual(after, checkpoint) {
					t.Error("empty summary changed durable history before recovery")
				}
			}
		}
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": "stop"}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
	}))
	defer model.Close()
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, LLM: llm.New(llm.Config{BaseURL: model.URL, DefaultModel: "one-model", Capabilities: llm.Capabilities{StreamingToolCalls: true, ContextTokens: 8192, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}})}
	session := agentstate.Session{ID: sessionID, LeaseToken: lease, Checkpoint: checkpoint}
	request := func(operation string) *http.Request {
		r := testutil.WithURLParams(testutil.JSONRequest(http.MethodPost, "/native/checkpoint/"+operation, agentstate.CheckpointRequest{Claim: llm.NativeRequest{Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano)}, Session: session}), "taskId", taskID, "operation", operation)
		r.Header.Set("Authorization", "Bearer "+token)
		return r.WithContext(middleware.WithDaemonContext(r.Context(), testWorkspaceID, machine))
	}
	if testFailure {
		fail = true
		testutil.Call(t, h.NativeCheckpoint, request("compact")).Want(http.StatusBadGateway)
	}
	// A failed summary must leave the original version available for retry.
	fail = false
	response := testutil.Call(t, h.NativeCheckpoint, request("compact")).Want(wantStatus)
	if wantStatus != http.StatusOK {
		var version int64
		var stored []byte
		if err := testPool.QueryRow(context.Background(), "SELECT version, checkpoint FROM native_session WHERE id=$1", sessionID).Scan(&version, &stored); err != nil {
			t.Fatal(err)
		}
		var after agentstate.Checkpoint
		if err := json.Unmarshal(stored, &after); err != nil {
			t.Fatal(err)
		}
		if version != 0 || after.Summary != nil || len(after.Messages) != len(checkpoint.Messages) {
			t.Fatal("blank summary changed the checkpoint")
		}
		if calls != 2 {
			t.Fatalf("empty summary retry was not bounded: calls=%d", calls)
		}
		return
	}
	response.JSON(&session)
	if session.Version != 1 || session.Checkpoint.Summary == nil || len(session.Checkpoint.Messages) != len(checkpoint.Messages) {
		t.Fatalf("summary lost source history: %+v", session)
	}
	if err := session.Checkpoint.Validate(4096); err != nil {
		t.Fatal(err)
	}
	// The failed request uses the configured transport retry budget; the
	// successful summary itself must make exactly one model request.
	wantCalls := 1
	if testFailure {
		wantCalls = 4
	}
	if emptyFirst {
		wantCalls = 2
	}
	if calls != wantCalls {
		t.Fatalf("calls=%d", calls)
	}
	session.Checkpoint.Summary.Text = "forged summary"
	testutil.Call(t, h.NativeCheckpoint, request("save")).Want(http.StatusBadRequest)
}
