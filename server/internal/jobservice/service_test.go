package jobservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

const tokenA = "test-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const tokenB = "test-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fixture(t *testing.T) (Config, *Service) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX job service")
	}
	cfg := Config{WorkDir: t.TempDir(), DataDir: filepath.Join(t.TempDir(), "jobs"), Keys: []Credential{{Namespace: "agent-a", Token: tokenA}, {Namespace: "agent-b", Token: tokenB}}, Limits: tools.ServiceLimits{MaxActive: 2, MaxRecords: 32, LogBytes: 1 << 20}}
	s, err := New(cfg, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return cfg, s
}

func request(t *testing.T, s *Service, token, method, path string, body any, want int) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s status=%d want=%d response=%s", method, path, w.Code, want, w.Body.String())
	}
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("invalid response")
	}
	return out
}

func TestServiceOwnsLifetimeDeduplicatesAndSeparatesCredentials(t *testing.T) {
	cfg, s := fixture(t)
	request(t, s, "invalid", http.MethodGet, "/v1/jobs", nil, 401)
	body := map[string]string{"key": "once", "command": "printf x >> marker; sleep 0.1; printf complete"}
	first := request(t, s, tokenA, "POST", "/v1/jobs", body, 200)
	id := first["job_id"].(string)
	again := request(t, s, tokenA, "POST", "/v1/jobs", body, 200)
	if again["job_id"] != id {
		t.Fatal("duplicate admission")
	}
	request(t, s, tokenA, "POST", "/v1/jobs", map[string]string{"key": "once", "command": "printf duplicate >> marker"}, 400)
	request(t, s, tokenB, "GET", "/v1/jobs/"+id, nil, 404)
	final := request(t, s, tokenA, "GET", "/v1/jobs/"+id+"?wait_ms=2000", nil, 200)
	if final["status"] != "completed" || final["output"] != "complete" {
		t.Fatal("lost independent settlement")
	}
	if data, err := os.ReadFile(filepath.Join(cfg.WorkDir, "marker")); err != nil || string(data) != "x" {
		t.Fatal("submission executed more than once")
	}
	found := request(t, s, tokenA, "GET", "/v1/submissions/once", nil, 200)
	if found["job_id"] != id {
		t.Fatal("cannot recover lost submit response")
	}
	if second, err := New(cfg, os.Environ()); err == nil {
		_ = second.Close()
		t.Fatal("two processes can own the same job journal")
	}
}

func TestNativeLostResponseCanRecoverAndTaskCancellationDoesNotStopServiceJob(t *testing.T) {
	_, s := fixture(t)
	var drop atomic.Bool
	drop.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && drop.Swap(false) {
			s.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		s.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: server.URL, TokenEnv: "JOBS_TOKEN", AllowWrite: true}}}
	client, err := tools.NewExternal(cfg, nativeruntime.Limits{HTTPOrigins: []string{server.URL}}, map[string]string{"JOBS_TOKEN": tokenA})
	if err != nil {
		t.Fatal(err)
	}
	task, cancel := context.WithCancel(context.Background())
	ctx := agentstate.WithWriteClaim(task, agentstate.Session{ID: "native-session"}, "submit")
	call := llm.ToolCall{ID: "submit", Name: "persistent_job", Arguments: json.RawMessage(`{"action":"submit","command":"sleep 0.15; printf survives-native-task"}`)}
	if _, err := client.Execute(ctx, call); !errors.Is(err, tools.ErrOutcomeUnknown) {
		t.Fatalf("lost submit response did not preserve uncertainty: %v", err)
	}
	cancel()
	ctx = agentstate.WithWriteClaim(context.Background(), agentstate.Session{ID: "native-session"}, "lookup")
	out, found, err := client.RecoverPersistentTool(ctx, call)
	if err != nil || !found {
		t.Fatalf("cannot recover admission without resubmission: %v", err)
	}
	var result struct {
		ID string `json:"job_id"`
	}
	if json.Unmarshal([]byte(out), &result) != nil {
		t.Fatal("invalid receipt")
	}
	query, _ := json.Marshal(map[string]any{"action": "output", "job_id": result.ID, "wait_ms": 2000})
	out, err = client.Execute(ctx, llm.ToolCall{Name: "persistent_job", Arguments: query})
	if err != nil || !strings.Contains(out, "survives-native-task") || !strings.Contains(out, `"status":"completed"`) {
		t.Fatalf("task context stopped service job: %s %v", out, err)
	}
}

func TestServiceRestartRetainsLogsAndNeverReplaysUnsettledAdmission(t *testing.T) {
	cfg, s := fixture(t)
	body := map[string]string{"key": "old", "command": "printf x >> marker; printf early-error; head -c 300000 /dev/zero"}
	first := request(t, s, tokenA, "POST", "/v1/jobs", body, 200)
	id := first["job_id"].(string)
	request(t, s, tokenA, "GET", "/v1/jobs/"+id+"?wait_ms=2000", nil, 200)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate an unconfirmed hard-crash tail using only this fixture's
	// admission; startup must preserve evidence rather than replay it.
	err := filepath.WalkDir(cfg.DataDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		record["status"] = "running"
		log, err := os.OpenFile(path+".log", os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		_, writeErr := log.WriteString("late-durable-output")
		closeErr := log.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		raw, _ = json.Marshal(record)
		return os.WriteFile(path, raw, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	result := request(t, restarted, tokenA, "POST", "/v1/jobs", body, 200)
	if result["job_id"] != id || result["status"] != "interrupted" || !strings.HasPrefix(result["output"].(string), "early-error") {
		t.Fatal("restart lost evidence or reran job")
	}
	if raw, err := os.ReadFile(filepath.Join(cfg.WorkDir, "marker")); err != nil || string(raw) != "x" {
		t.Fatal("restart repeated side effects")
	}
	search := request(t, restarted, tokenA, "GET", "/v1/jobs/"+id+"/search?text=early-error", nil, 200)
	if len(search["matches"].([]any)) != 1 {
		t.Fatal("restart lost early logs")
	}
	late := request(t, restarted, tokenA, "GET", "/v1/jobs/"+id+"/search?text=late-durable-output", nil, 200)
	if len(late["matches"].([]any)) != 1 {
		t.Fatal("restart lost log bytes written after the last observation")
	}
}

func TestServiceCapacityCancellationDiskQuotaAndListPagination(t *testing.T) {
	_, s := fixture(t)
	ids := []string{}
	for _, key := range []string{"a", "b"} {
		out := request(t, s, tokenA, "POST", "/v1/jobs", map[string]string{"key": key, "command": "sleep 30"}, 200)
		ids = append(ids, out["job_id"].(string))
	}
	request(t, s, tokenA, "POST", "/v1/jobs", map[string]string{"key": "overflow", "command": "printf forbidden"}, 429)
	for _, id := range ids {
		result := request(t, s, tokenA, "POST", "/v1/jobs/"+id+"/cancel", nil, 200)
		if result["status"] != "cancelled" {
			t.Fatal("cancel returned before settlement")
		}
	}
	job := request(t, s, tokenA, "POST", "/v1/jobs", map[string]string{"key": "large", "command": "head -c 1100000 /dev/zero; printf tail"}, 200)
	id := job["job_id"].(string)
	result := request(t, s, tokenA, "GET", "/v1/jobs/"+id+"?wait_ms=2000", nil, 200)
	if result["log_truncated"] != true || result["log_bytes"] != float64(1<<20) {
		t.Fatal("disk quota not enforced or not visible")
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 4; i++ {
		page := request(t, s, tokenA, "GET", "/v1/jobs?limit=1&after="+cursor, nil, 200)
		for _, value := range page["jobs"].([]any) {
			id := value.(map[string]any)["job_id"].(string)
			if seen[id] {
				t.Fatal("pagination duplicated a job")
			}
			seen[id] = true
		}
		cursor = page["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatal("list pagination lost jobs")
	}
}
