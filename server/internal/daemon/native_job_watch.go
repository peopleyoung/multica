package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
)

// Private, bounded observation obligations survive task and daemon teardown.
// Tokens remain on the executor (0600), never model history or API requests.
type nativeJobWatch struct {
	TaskID  string    `json:"task_id"`
	Key     string    `json:"key"`
	BaseURL string    `json:"base_url"`
	Token   string    `json:"token"`
	Created time.Time `json:"created_at"`
}

func (d *Daemon) jobWatchDirectory() string {
	return filepath.Join(d.cfg.WorkspacesRoot, ".native-job-watches")
}
func (d *Daemon) registerNativeJobWatch(task Task, cfg nativeruntime.Config, env map[string]string, key string) error {
	if task.IssueID == "" {
		return nil
	}
	var service nativeruntime.Service
	for _, s := range cfg.Services {
		if s.Name == "multica_jobs" {
			service = s
		}
	}
	source := task.NativeInputTaskID
	if source == "" {
		source = task.ID
	}
	watch := nativeJobWatch{TaskID: source, Key: key, BaseURL: service.BaseURL, Token: env[service.TokenEnv], Created: time.Now().UTC()}
	if watch.validate() != nil {
		return errors.New("cannot register durable job completion observation")
	}
	dir := d.jobWatchDirectory()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("job observation directory must be private")
	}
	digest := sha256.Sum256([]byte(source + ":" + key))
	path := filepath.Join(dir, hex.EncodeToString(digest[:])+".json")

	checkExisting := func() error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("existing job observation is not private")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var existing nativeJobWatch
		if json.Unmarshal(raw, &existing) != nil || existing.validate() != nil || existing.TaskID != source || existing.Key != key || existing.BaseURL != watch.BaseURL || existing.Token != watch.Token {
			return errors.New("existing job observation identity differs")
		}
		return nil
	}
	if err := checkExisting(); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) >= 4096 {
		return errors.New("job observation limit reached; reconcile outstanding observations")
	}
	raw, _ := json.Marshal(watch)
	f, err := os.CreateTemp(dir, ".watch-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if err = os.Link(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return checkExisting()
		}
		return err
	}
	return syncNativeJournalDir(dir)
}
func (d *Daemon) nativeJobWatchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	cursor := 0
	nextWarning := time.Time{}
	for {
		failed := d.pollNativeJobWatches(ctx, &cursor)
		if failed > 0 && time.Now().After(nextWarning) {
			d.logger.Warn("native job observations pending; inspect private watch records and service/platform access", "failed", failed)
			nextWarning = time.Now().Add(5 * time.Minute)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (d *Daemon) pollNativeJobWatches(ctx context.Context, cursor *int) int {
	failed := 0
	paths, _ := filepath.Glob(filepath.Join(d.jobWatchDirectory(), "*.json"))
	sort.Strings(paths)
	if len(paths) == 0 {
		return 0
	}
	if len(paths) > 4096 {
		return len(paths)
	}
	tick, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for n := 0; n < min(len(paths), 64); n++ {
		path := paths[*cursor%len(paths)]
		*cursor = (*cursor + 1) % len(paths)
		if tick.Err() != nil {
			return failed + 1
		}
		if err := d.pollNativeJobWatch(tick, path); err != nil {
			failed++
		}
	}
	return failed
}
func (d *Daemon) pollNativeJobWatch(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("job observation must be a private regular file")
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 32769))
	_ = f.Close()
	if err != nil || len(raw) > 32768 {
		return errors.New("invalid job observation record")
	}
	var watch nativeJobWatch
	if json.Unmarshal(raw, &watch) != nil || watch.validate() != nil {
		return errors.New("invalid job observation identity")
	}
	// A recorded URL never widens the current owner's service-origin policy.
	cfg := nativeruntime.Config{Services: []nativeruntime.Service{{Name: "multica_jobs", BaseURL: watch.BaseURL}}}
	if err := cfg.Check(d.nativeLimits()); err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probe, "GET", strings.TrimRight(watch.BaseURL, "/")+"/v1/submissions/"+watch.Key, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+watch.Token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("job service unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("job receipt unavailable; no command will be replayed")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("invalid job observation")
	}
	var observation struct {
		ID     string `json:"job_id"`
		Status string `json:"status"`
		Exit   *int   `json:"exit_code"`
	}
	if json.Unmarshal(data, &observation) != nil {
		return errors.New("invalid job observation")
	}
	if observation.Status == "running" || observation.Status == "starting" {
		return nil
	}
	event := nativeruntime.JobEvent{Key: watch.Key, JobID: observation.ID, Status: observation.Status, ExitCode: observation.Exit}
	if err := event.Validate(); err != nil {
		return err
	}
	var receipt struct {
		Acknowledged bool `json:"acknowledged"`
	}
	if err := d.client.postJSON(ctx, "/api/daemon/tasks/"+watch.TaskID+"/native/job-event", event, &receipt); err != nil {
		return err
	}
	if !receipt.Acknowledged {
		return nil // Source run is still active; keep the obligation for the next tick.
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncNativeJournalDir(filepath.Dir(path))
}

func (w nativeJobWatch) validate() error {
	if _, err := util.ParseUUID(w.TaskID); err != nil {
		return errors.New("invalid job observation task")
	}
	// Reuse the terminal event's strict key/identifier grammar before making URLs.
	if (nativeruntime.JobEvent{Key: w.Key, JobID: "validation", Status: "completed"}).Validate() != nil || len(w.Token) < 32 || len(w.Token) > 512 || strings.ContainsAny(w.Token, " \r\n\t") || nativeruntime.Origin(w.BaseURL) == "" || len(w.BaseURL) > 4096 || w.Created.IsZero() {
		return errors.New("invalid job observation record")
	}
	return nil
}
