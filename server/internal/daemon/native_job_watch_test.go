package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

func TestNativeJobWatchRestoresAfterLostAckWithoutResubmitting(t *testing.T) {
	ctx := context.Background()
	lookups, launches := 0, 0
	jobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			launches++
			w.WriteHeader(405)
			return
		}
		lookups++
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("watch lost service credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "job-existing", "status": "completed", "exit_code": 0})
	}))
	defer jobs.Close()
	accepted := map[string]bool{}
	queued := 0
	loseAck := true
	active := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e nativeruntime.JobEvent
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Error(err)
		}
		if active {
			_ = json.NewEncoder(w).Encode(map[string]bool{"acknowledged": false})
			return
		}
		if !accepted[e.Key] {
			accepted[e.Key] = true
			queued++
		}
		if loseAck {
			loseAck = false
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"acknowledged": true})
	}))
	defer api.Close()
	cfg := Config{WorkspacesRoot: t.TempDir(), NativeHTTPOrigins: []string{jobs.URL}}
	d := &Daemon{cfg: cfg, client: NewClient(api.URL), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	task := Task{ID: uuid.NewString(), NativeInputTaskID: uuid.NewString(), IssueID: uuid.NewString()}
	key := strings.Repeat("a", 64)
	policy := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: jobs.URL, TokenEnv: "JOBS_TOKEN"}}}
	if err := d.registerNativeJobWatch(task, policy, map[string]string{"JOBS_TOKEN": strings.Repeat("t", 32)}, key); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(d.jobWatchDirectory(), "*.json"))
	if len(paths) != 1 {
		t.Fatal("obligation was not durable before submission")
	}
	info, _ := os.Stat(paths[0])
	if info.Mode().Perm() != 0600 {
		t.Fatal("watch credential permissions")
	}
	if err := d.pollNativeJobWatch(ctx, paths[0]); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatal("active source was resumed concurrently")
	}
	active = false
	if err := d.pollNativeJobWatch(ctx, paths[0]); err == nil {
		t.Fatal("lost acknowledgement treated as success")
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatal("lost acknowledgement erased obligation")
	}
	// A fresh daemon must use the saved identity, not another command admission.
	restarted := &Daemon{cfg: cfg, client: NewClient(api.URL), logger: d.logger}
	if err := restarted.pollNativeJobWatch(ctx, paths[0]); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || launches != 0 || lookups != 3 {
		t.Fatalf("feedback duplicated or training replayed: queued=%d launches=%d lookups=%d", queued, launches, lookups)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("acknowledged obligation not removed")
	}
}

func TestNativeJobWatchKeepsMissingReceiptsAndRejectsMalformedRecords(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(404) }))
	defer srv.Close()
	d := &Daemon{cfg: Config{WorkspacesRoot: t.TempDir(), NativeHTTPOrigins: []string{srv.URL}}, client: NewClient(srv.URL)}
	task := Task{ID: uuid.NewString(), IssueID: uuid.NewString()}
	cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: srv.URL, TokenEnv: "JOBS_TOKEN"}}}
	env := map[string]string{"JOBS_TOKEN": strings.Repeat("x", 32)}
	if err := d.registerNativeJobWatch(task, cfg, env, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(d.jobWatchDirectory(), "*.json"))
	path := paths[0]
	if err := d.pollNativeJobWatch(context.Background(), path); err == nil {
		t.Fatal("missing receipt became completion")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unknown admission erased")
	}
	// Revoked origin must block even a previously valid saved URL.
	d.cfg.NativeHTTPOrigins = nil
	if err := d.pollNativeJobWatch(context.Background(), path); err == nil {
		t.Fatal("origin revocation ignored")
	}
	if requests != 1 {
		t.Fatal("revoked origin contacted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := d.pollNativeJobWatch(context.Background(), path); err == nil {
		t.Fatal("public credential record accepted")
	}
}
