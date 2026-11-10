package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func jobCall(t *testing.T, h *Host, owner, id, name, args string) map[string]any {
	t.Helper()
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: owner}, id)
	out, err := h.Execute(ctx, llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid job result %q: %v", out, err)
	}
	return result
}

func TestShellYieldsAndCollectsWithoutRelaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	h, err := New(dir, Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	args := `{"command":"printf x >> launched; sleep 0.2; printf finished","wait_ms":1}`
	first := jobCall(t, h, "owner", "launch", "shell", args)
	if first["status"] != "running" || first["job_id"] == "" {
		t.Fatalf("did not yield a running job: %v", first)
	}
	again := jobCall(t, h, "owner", "launch", "shell", args)
	if again["job_id"] != first["job_id"] {
		t.Fatal("same durable call launched twice")
	}
	query, _ := json.Marshal(map[string]any{"job_id": first["job_id"], "wait_ms": 2000})
	final := jobCall(t, h, "owner", "poll", "job_output", string(query))
	if final["status"] != "completed" || final["exit_code"] != float64(0) || final["output"] != "finished" {
		t.Fatalf("lost settlement: %v", final)
	}
	data, err := os.ReadFile(filepath.Join(dir, "launched"))
	if err != nil || string(data) != "x" {
		t.Fatalf("side effect repeated: %q %v", data, err)
	}
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "different-owner"}, "poll")
	if _, err := h.Execute(ctx, llm.ToolCall{Name: "job_output", Arguments: query}); err == nil {
		t.Fatal("job crossed session ownership")
	}
}

func TestJobKillSettlesAndHostCloseStopsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	h, err := New(dir, Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	first := jobCall(t, h, "owner", "launch", "shell", `{"command":"sleep 30; printf leaked > leak","run_in_background":true}`)
	query, _ := json.Marshal(map[string]any{"job_id": first["job_id"]})
	result := jobCall(t, h, "owner", "kill", "job_kill", string(query))
	if result["status"] != "cancelled" {
		t.Fatalf("kill did not settle: %v", result)
	}
	jobCall(t, h, "owner", "second", "shell", `{"command":"sleep 0.3; printf leaked > leak","run_in_background":true}`)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "leak")); !os.IsNotExist(err) {
		t.Fatal("background process escaped host teardown")
	}
}

func TestJobJournalRestoresReceiptAndOutputWithoutReexecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir, journal := t.TempDir(), t.TempDir()
	h, err := New(dir, Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	first := jobCall(t, h, "owner", "launch", "shell", `{"command":"printf x >> marker; printf durable","wait_ms":2000}`)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, err = New(dir, Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// jsonb reorders keys: identity is semantic rather than raw JSON bytes.
	again := jobCall(t, h, "owner", "launch", "shell", `{ "wait_ms": 2000, "command": "printf x >> marker; printf durable" }`)
	if again["job_id"] != first["job_id"] || again["status"] != "completed" || again["output"] != "durable" {
		t.Fatalf("lost durable receipt: %v", again)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "marker")); err != nil || string(raw) != "x" {
		t.Fatal("restoration repeated command")
	}
}

func TestJobRestartMarksUnsettledAdmissionInterrupted(t *testing.T) {
	dir, journal := t.TempDir(), t.TempDir()
	owner, id := "owner", jobID("owner", "launch")
	ownerDir := filepath.Join(journal, jobHash(owner))
	if err := os.MkdirAll(ownerDir, 0700); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"command":"printf forbidden > replayed"}`)
	record := jobRecord{ID: id, Owner: owner, Digest: jobArgumentsDigest(args), Status: "running"}
	raw, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(ownerDir, id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	h, err := New(dir, Permissions{Shell: true}, os.Environ(), journal)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: owner}, "launch")
	out, found, err := h.RecoverTool(ctx, llm.ToolCall{ID: "launch", Name: "shell", Arguments: args})
	var result map[string]any
	_ = json.Unmarshal([]byte(out), &result)
	if err != nil || !found || result["status"] != "interrupted" {
		t.Fatalf("missing interrupted evidence: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "replayed")); !os.IsNotExist(err) {
		t.Fatal("restart ran an uncertain command")
	}
}

func TestJobOutputOffsetsAreRepeatableAndBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	h, err := New(t.TempDir(), Permissions{Shell: true}, os.Environ(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	first := jobCall(t, h, "owner", "launch", "shell", `{"command":"head -c 300000 /dev/zero; printf final-line","wait_ms":2000}`)
	if first["output_truncated"] != false || len(first["output"].(string)) > jobReadLimit {
		t.Fatal("unbounded job output")
	}
	query, _ := json.Marshal(map[string]any{"job_id": first["job_id"], "offset": first["output_end"].(float64) - 10, "wait_ms": 0})
	for range 2 {
		last := jobCall(t, h, "owner", "read", "job_output", string(query))
		if last["output"] != "final-line" {
			t.Fatalf("tail or repeatable offset lost: %v", last)
		}
	}
}

func TestManagedShellSurvivesThirtySecondWaitBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	h, err := New(t.TempDir(), Permissions{Shell: true}, os.Environ(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	start := time.Now()
	first := jobCall(t, h, "owner", "launch", "shell", `{"command":"sleep 31; printf survived","wait_ms":30000}`)
	if first["status"] != "running" {
		t.Fatalf("command did not survive wait boundary: %v", first)
	}
	query, _ := json.Marshal(map[string]any{"job_id": first["job_id"], "wait_ms": 5000})
	final := jobCall(t, h, "owner", "poll", "job_output", string(query))
	if time.Since(start) < 30*time.Second || final["status"] != "completed" || final["output"] != "survived" {
		t.Fatalf("old shell deadline still affects managed jobs: %v", final)
	}
}
