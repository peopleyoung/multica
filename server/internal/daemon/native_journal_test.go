package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeJournalReconcilesUnconfirmedResultWithNewLease(t *testing.T) {
	call := llm.ToolCall{ID: "write-once", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","old_text":"","new_text":"saved"}`)}
	current := agentstate.Session{ID: uuid.NewString(), Version: 3, LeaseToken: uuid.NewString(), Checkpoint: agentstate.Checkpoint{
		Version: 3, InputTaskID: "run", Messages: []llm.ModelMessage{{Role: "user", Content: "write"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}},
		Tools: []agentstate.ToolRecord{{Call: call, State: agentstate.ToolStarted}},
	}}
	requests := 0
	failSave := true
	newLease := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/session") {
			current.LeaseToken = newLease
			_ = json.NewEncoder(w).Encode(current)
			return
		}
		requests++
		if failSave {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req agentstate.CheckpointRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Session.LeaseToken != newLease || req.Session.Version != current.Version {
			t.Error("reconciliation must use the current lease and version")
		}
		if err := agentstate.ValidateTransition(current.Checkpoint, req.Session.Checkpoint, 65536); err != nil {
			t.Error(err)
		}
		current = req.Session
		current.Version++
		current.Checkpoint.Version = current.Version
		_ = json.NewEncoder(w).Encode(current)
	}))
	defer srv.Close()
	client := NewClient(srv.URL)
	client.SetToken("mdt_never_persist_this")
	store := newNativeStore(client, Task{ID: "run", RemoteMCPDaemonToken: "mdt_never_persist_this"})
	store.journalDir = t.TempDir()
	// The file side effect has completed; only its result failed to reach SQL.
	finished := current
	finished.Checkpoint.Tools = append([]agentstate.ToolRecord(nil), current.Checkpoint.Tools...)
	if err := finished.Checkpoint.Tools[0].Finish("file written", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), finished); err == nil || requests != 1 {
		t.Fatalf("unconfirmed save was retried or accepted: requests=%d err=%v", requests, err)
	}
	path, _ := store.pendingPath(current.ID)
	raw, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(raw), "mdt_") || strings.Contains(string(raw), "lease_token") {
		t.Fatalf("journal missing or contains control credentials: %v", err)
	}
	// Simulate a new daemon process after lease expiry. Acquire reconciles the
	// observed outcome before the engine can recover or run any tool.
	restarted := newNativeStore(client, Task{ID: "run"})
	restarted.journalDir = store.journalDir
	restarted.leaseToken = newLease
	failSave = false
	saved, err := restarted.Acquire(context.Background(), t.TempDir())
	if err != nil || saved.Version != 4 || saved.Checkpoint.Tools[0].State != agentstate.ToolSucceeded || requests != 2 {
		t.Fatalf("recovery=%+v requests=%d error=%v", saved, requests, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("confirmed journal was not removed")
	}
}

func TestNativeJournalRejectsFutureVersionAndDiscardsCommittedCopy(t *testing.T) {
	store := &nativeStore{journalDir: t.TempDir()}
	stored := agentstate.Session{ID: uuid.NewString(), Version: 5}
	if err := store.stageResult(stored); err != nil {
		t.Fatal(err)
	}
	older := stored
	older.Version--
	if _, err := store.reconcilePending(context.Background(), older); err == nil {
		t.Fatal("local journal ahead of SQL must not overwrite SQL")
	}
	newer := stored
	newer.Version++
	if _, err := store.reconcilePending(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	path, _ := store.pendingPath(stored.ID)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale journal was not discarded")
	}
}

func TestNativeJournalPreservesObservedResultAfterCancellation(t *testing.T) {
	call := llm.ToolCall{ID: "write-cancelled", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","old_text":"","new_text":"saved"}`)}
	result := "file written before cancellation"
	session := agentstate.Session{ID: uuid.NewString(), Version: 3, LeaseToken: uuid.NewString(), Checkpoint: agentstate.Checkpoint{
		Messages: []llm.ModelMessage{{Role: "user", Content: "write"}, {Role: "assistant", ToolCalls: []llm.ToolCall{call}}},
		Tools:    []agentstate.ToolRecord{{Call: call, State: agentstate.ToolSucceeded, Result: &result}},
	}}
	store := newNativeStore(NewClient("http://unused.invalid"), Task{ID: "cancelled"})
	store.journalDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Save(ctx, session); err == nil {
		t.Fatal("cancelled save unexpectedly succeeded")
	}
	path, err := store.pendingPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), result) {
		t.Fatalf("observed result was lost after cancellation: %v", err)
	}
}
