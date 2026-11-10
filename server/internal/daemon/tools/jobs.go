package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/processtree"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

const (
	ShellWaitMaxMS     = 30000
	ShellWaitDefaultMS = 1000
	ShellMaxActiveJobs = 4
	jobRetention       = 256 << 10
	jobReadLimit       = 8 << 10
	maxSessionJobs     = 128
)

// A shell call admits one job. Waiting never owns the process deadline. Durable
// admission is keyed by engine-owned session/call identity, before any spawn.
// The host owns live processes; restored records are evidence, never restart instructions.
type shellJobs struct {
	policy                SandboxPolicy
	mu                    sync.Mutex
	jobs                  map[string]*shellJob
	loaded                map[string]bool
	workDir, dir          string
	env                   []string
	closed                bool
	maxActive, maxRecords int
	logLimit              int64
}

type jobRecord struct {
	ID           string     `json:"job_id"`
	Owner        string     `json:"owner"`
	Digest       string     `json:"arguments_digest"`
	Status       string     `json:"status"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	Output       []byte     `json:"output"`
	OutputStart  int64      `json:"output_start"`
	LogBytes     int64      `json:"log_bytes,omitempty"`
	LogTruncated bool       `json:"log_truncated,omitempty"`
	LogError     string     `json:"log_error,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

type shellJob struct {
	mu       sync.Mutex
	record   jobRecord
	path     string
	done     chan struct{}
	cancel   context.CancelCauseFunc
	storeErr error
	log      *os.File
	logLimit int64
}

type jobArgs struct {
	Command    string `json:"command"`
	Background bool   `json:"run_in_background"`
	WaitMS     *int   `json:"wait_ms"`
	ID         string `json:"job_id"`
	Offset     *int64 `json:"offset"`
	Text       string `json:"text"`
}

func shellJobDefinitions() []llm.Tool {
	return []llm.Tool{
		{Name: "shell", Description: "Run one command with the daemon user's authority, network and filesystem access. Returns JSON with job_id, status, output, next_offset and exit_code when known. Wait defaults to 1000 ms, at most 30000 ms; a wait expiry returns the SAME running job without killing or restarting it. Use run_in_background for long work, then job_output with job_id and next_offset. Never relaunch a command to check progress. Jobs stop when this task ends or is cancelled; use a remote persistent job runner for work that must outlive the task. Stopping SSH does not prove remote work stopped.", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"run_in_background":{"type":"boolean"},"wait_ms":{"type":"integer","minimum":0,"maximum":30000}},"required":["command"],"additionalProperties":false}`)},
		{Name: "job_output", Description: "Read a session-owned job's status and bounded output. Reads are repeatable: offset is an absolute byte offset, next_offset continues the returned chunk. Defaults: offset 0, wait 1000 ms; wait at most 30000 ms for completion without cancelling the job. output_truncated indicates earlier bytes were evicted. An interrupted job must be reconciled against remote state before a new launch.", Parameters: json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"offset":{"type":"integer","minimum":0},"wait_ms":{"type":"integer","minimum":0,"maximum":30000}},"required":["job_id"],"additionalProperties":false}`)},
		{Name: "job_list", Description: "List this session's admitted jobs and status. Copy the complete short job_id exactly; use this after a not-found error. Use job_output to inspect output. Does not expose other sessions.", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "job_search", Description: "Search retained on-disk job output for literal text, starting at an absolute byte offset. Returns bounded matches and a continuation offset. Logs have a disk quota; inspect log_truncated.", Parameters: json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"text":{"type":"string","minLength":1,"maxLength":512},"offset":{"type":"integer","minimum":0}},"required":["job_id","text"],"additionalProperties":false}`)},
		{Name: "job_kill", Description: "Stop a session-owned local job and its descendants, wait for settlement, and return status. Cancelling an SSH job may leave remote work running; reconcile remote status before another launch.", Parameters: json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"}},"required":["job_id"],"additionalProperties":false}`)},
	}
}

func isShellJobTool(name string) bool {
	return name == "shell" || name == "job_output" || name == "job_list" || name == "job_kill" || name == "job_search"
}

func openShellJobs(workDir string, env []string, dir string) (*shellJobs, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	return &shellJobs{jobs: map[string]*shellJob{}, loaded: map[string]bool{}, workDir: workDir, env: append([]string(nil), env...), dir: dir, maxActive: ShellMaxActiveJobs, maxRecords: maxSessionJobs, logLimit: jobLogLimit}, nil
}

func jobHash(text string) string { h := sha256.Sum256([]byte(text)); return hex.EncodeToString(h[:]) }
func jobArgumentsDigest(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var parsed any
	if decoder.Decode(&parsed) != nil {
		return jobHash(string(raw))
	}
	canonical, _ := json.Marshal(parsed)
	return jobHash(string(canonical))
}
func jobID(owner, call string) string { return "job_" + jobHash(owner+"\x00"+call) }
func jobHandle(id string) string      { return "shell-" + jobHash(id)[:16] }
func liveJob(status string) bool      { return status == "starting" || status == "running" }
func jobOwner(ctx context.Context) string {
	if claim, ok := agentstate.WriteClaimFromContext(ctx); ok && claim.SessionID != "" {
		return claim.SessionID
	}
	return "standalone"
}

func parseJobArgs(call llm.ToolCall) (jobArgs, error) {
	var args jobArgs
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return args, errors.New("tool arguments must be a JSON object")
	}
	allowed := map[string]bool{}
	switch call.Name {
	case "shell":
		allowed = map[string]bool{"command": true, "run_in_background": true, "wait_ms": true}
	case "job_output":
		allowed = map[string]bool{"job_id": true, "offset": true, "wait_ms": true}
	case "job_kill":
		allowed = map[string]bool{"job_id": true}
	case "job_search":
		allowed = map[string]bool{"job_id": true, "text": true, "offset": true}
	}
	for key, value := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return args, errors.New("unknown or null job argument")
		}
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return args, errors.New("invalid job argument type")
	}
	if call.Name == "shell" && (strings.TrimSpace(args.Command) == "" || len(args.Command) > 32768 || call.ID == "") {
		return args, errors.New("shell requires a nonempty command and call identity")
	}
	if call.Name != "shell" && call.Name != "job_list" && args.ID == "" {
		return args, errors.New("job_id is required")
	}
	if args.WaitMS != nil && (*args.WaitMS < 0 || *args.WaitMS > ShellWaitMaxMS) {
		return args, errors.New("wait_ms must be between 0 and 30000")
	}
	if args.Offset != nil && *args.Offset < 0 {
		return args, errors.New("offset must be nonnegative")
	}
	if call.Name == "job_search" && (args.Text == "" || len(args.Text) > 512) {
		return args, errors.New("text must contain 1 to 512 bytes")
	}
	return args, nil
}

func (m *shellJobs) execute(ctx context.Context, call llm.ToolCall) (string, error) {
	args, err := parseJobArgs(call)
	if err != nil {
		return "", err
	}
	owner := jobOwner(ctx)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", errors.New("job host is closed")
	}
	err = m.load(owner)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	if call.Name == "job_list" {
		return m.list(owner)
	}
	var job *shellJob
	if call.Name == "shell" {
		job, err = m.start(ctx, owner, call, args.Command)
	} else {
		m.mu.Lock()
		job = m.jobs[args.ID]
		if job == nil {
			for _, candidate := range m.jobs {
				if candidate.record.Owner == owner && jobHandle(candidate.record.ID) == args.ID {
					if job != nil {
						m.mu.Unlock()
						return "", errors.New("ambiguous job handle; use job_list")
					}
					job = candidate
				}
			}
		}
		m.mu.Unlock()
		if job == nil || job.record.Owner != owner {
			return "", errors.New("job not found in this session; call job_list and copy its full job_id exactly; jobs from another session cannot be accessed")
		}
	}
	if err != nil {
		return "", err
	}
	if call.Name == "job_search" {
		offset := int64(0)
		if args.Offset != nil {
			offset = *args.Offset
		}
		return job.search(offset, args.Text)
	}
	if call.Name == "job_kill" {
		job.cancel(errors.New("stopped by job_kill; remote processes may still be running"))
		if err := job.wait(ctx, 0, true); err != nil {
			return "", err
		}
	} else if !args.Background {
		wait := ShellWaitDefaultMS
		if args.WaitMS != nil {
			wait = *args.WaitMS
		}
		if err := job.wait(ctx, wait, false); err != nil {
			return "", err
		}
	}
	offset := int64(0)
	if args.Offset != nil {
		offset = *args.Offset
	}
	return job.read(offset)
}

// load is called under the registry lock. A previous host cannot own live work
// after teardown. A hard crash leaves interrupted evidence, not a runnable job.
func (m *shellJobs) load(owner string) error {
	if m.loaded[owner] {
		return nil
	}
	if m.dir == "" {
		m.loaded[owner] = true
		return nil
	}
	dir := filepath.Join(m.dir, jobHash(owner))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		count++
		if count > m.maxRecords {
			return errors.New("session job journal exceeds record limit")
		}
		path := filepath.Join(dir, entry.Name())
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 2*jobRetention+8193))
		_ = file.Close()
		var record jobRecord
		if readErr != nil || len(data) > 2*jobRetention+8192 || json.Unmarshal(data, &record) != nil || record.Owner != owner || entry.Name() != record.ID+".json" || len(record.ID) != 68 || !strings.HasPrefix(record.ID, "job_") || len(record.Output) > jobRetention || record.OutputStart < 0 || record.LogBytes < 0 || record.LogBytes > m.logLimit {
			return errors.New("invalid session job journal; reconcile before continuing")
		}
		job := &shellJob{record: record, path: path, done: make(chan struct{}), cancel: func(error) {}, logLimit: m.logLimit}
		if err := job.recoverLog(); err != nil {
			return err
		}
		if liveJob(record.Status) {
			now := time.Now().UTC()
			job.record.FinishedAt = &now
			job.record.Status = "interrupted"
			job.record.Detail = "Runtime restarted before settlement. Local outcome is not confirmed; reconcile files and remote job status before any new launch. This command will not be replayed."
			if err := job.persist(); err != nil {
				return err
			}
		}
		close(job.done)
		m.jobs[record.ID] = job
	}
	m.loaded[owner] = true
	return nil
}

func (m *shellJobs) start(ctx context.Context, owner string, call llm.ToolCall, command string) (*shellJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("job host is closed")
	}
	id := jobID(owner, call.ID)
	digest := jobArgumentsDigest(call.Arguments)
	if existing := m.jobs[id]; existing != nil {
		if existing.record.Digest != digest {
			return nil, errors.New("shell call identity reused with different arguments")
		}
		return existing, nil
	}
	for existingID, job := range m.jobs {
		if job.record.Owner == owner && jobHandle(existingID) == jobHandle(id) {
			return nil, errors.New("short job identifier collision; admission refused without executing")
		}
	}
	active, total := 0, 0
	for _, j := range m.jobs {
		if j.record.Owner != owner {
			continue
		}
		total++
		j.mu.Lock()
		if liveJob(j.record.Status) {
			active++
		}
		j.mu.Unlock()
	}
	if active >= m.maxActive || total >= m.maxRecords {
		return nil, errors.New("session shell job limit reached; collect or stop existing jobs")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithCancelCause(ctx)
	job := &shellJob{record: jobRecord{ID: id, Owner: owner, Digest: digest, Status: "running", StartedAt: time.Now().UTC()}, done: make(chan struct{}), cancel: cancel, logLimit: m.logLimit}
	if m.dir != "" {
		job.path = filepath.Join(m.dir, jobHash(owner), id+".json")
	}
	if err := job.openLog(); err != nil {
		cancel(err)
		return nil, err
	}
	if err := job.persist(); err != nil {
		job.closeLog()
		cancel(err)
		return nil, err
	}
	m.jobs[id] = job
	go func() {
		defer cancel(nil)
		cmd, buildErr := m.policy.command(m.workDir, m.env, command, false)
		if buildErr != nil {
			job.mu.Lock()
			job.record.Status = "failed"
			job.record.Detail = buildErr.Error()
			now := time.Now().UTC()
			job.record.FinishedAt = &now
			job.storeErr = errors.Join(job.closeLog(), job.persist())
			close(job.done)
			job.mu.Unlock()
			return
		}

		cmd.Stdout = job
		cmd.Stderr = job
		err := processtree.Run(jobCtx, cmd, time.Second)
		job.mu.Lock()
		defer job.mu.Unlock()
		job.record.Status = "completed"
		if err != nil {
			job.record.Status = "failed"
			job.record.Detail = err.Error()
		}
		if jobCtx.Err() != nil {
			job.record.Status = "cancelled"
			job.record.Detail = fmt.Sprintf("%v; remote processes may still be running", context.Cause(jobCtx))
		}
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			job.record.ExitCode = &code
		}
		now := time.Now().UTC()
		job.record.FinishedAt = &now
		if job.path != "" {
			job.trimOutput(16 << 10)
			job.record.Output = bytes.Clone(job.record.Output)
		}
		job.storeErr = errors.Join(job.storeErr, job.closeLog(), job.persist())
		close(job.done)
	}()
	return job, nil
}

func (j *shellJob) Write(data []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := len(data)
	j.appendLog(data)
	j.record.Output = append(j.record.Output, data...)
	j.trimOutput(jobRetention)
	return n, nil
}

func (j *shellJob) trimOutput(limit int) {
	if extra := len(j.record.Output) - limit; extra > 0 {
		j.record.Output = j.record.Output[extra:]
		j.record.OutputStart += int64(extra)
	}
}

func (j *shellJob) wait(ctx context.Context, ms int, untilDone bool) error {
	var timer <-chan time.Time
	if !untilDone {
		t := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-j.done:
		return nil
	case <-timer:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *shellJob) read(offset int64) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.storeErr != nil {
		return "", fmt.Errorf("persist shell job: %w: %v", ErrOutcomeUnknown, j.storeErr)
	}
	truncated := false
	end := j.record.OutputStart + int64(len(j.record.Output))
	if offset > end {
		return "", errors.New("offset exceeds captured output")
	}
	var out []byte
	if j.path != "" && offset < j.record.LogBytes {
		var err error
		out, err = j.readLog(offset, jobReadLimit)
		if err != nil {
			return "", err
		}
	} else {
		truncated = offset < j.record.OutputStart
		if truncated {
			offset = j.record.OutputStart
		}
		next := min(end, offset+jobReadLimit)
		out = j.record.Output[offset-j.record.OutputStart : next-j.record.OutputStart]
	}
	next := offset + int64(len(out))
	// Saving an observation before returning makes the handle and output
	// available even when the checkpoint response is lost.
	if err := j.persist(); err != nil {
		return "", fmt.Errorf("persist shell job: %w: %v", ErrOutcomeUnknown, err)
	}
	raw, err := json.Marshal(map[string]any{"job_id": jobHandle(j.record.ID), "status": j.record.Status, "exit_code": j.record.ExitCode, "output": strings.ToValidUTF8(string(out), "�"), "output_start": offset, "next_offset": next, "output_end": end, "output_truncated": truncated, "log_bytes": j.record.LogBytes, "log_truncated": j.record.LogTruncated, "log_error": j.record.LogError, "detail": j.record.Detail})
	return string(raw), err
}

func (m *shellJobs) list(owner string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := []map[string]any{}
	ids := []string{}
	for id, j := range m.jobs {
		if j.record.Owner == owner {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		j := m.jobs[id]
		j.mu.Lock()
		list = append(list, map[string]any{"job_id": jobHandle(id), "status": j.record.Status, "exit_code": j.record.ExitCode, "started_at": j.record.StartedAt, "finished_at": j.record.FinishedAt})
		j.mu.Unlock()
	}
	raw, err := json.Marshal(list)
	return string(raw), err
}

// persist is called before publishing a record or with its mutex held. The
// journal is outside the task working directory and does not copy the process environment.
func (j *shellJob) persist() error {
	if j.path == "" {
		return nil
	}
	if j.log != nil {
		if err := j.log.Sync(); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(j.record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(j.path), ".job-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), j.path); err != nil {
		return err
	}
	return syncJobDir(filepath.Dir(j.path))
}

func (m *shellJobs) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	jobs := make([]*shellJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
		j.cancel(errors.New("task execution ended"))
	}
	m.mu.Unlock()
	for _, j := range jobs {
		<-j.done
	}
}

// RecoverTool only returns an existing admission receipt for a started shell
// call. It cannot start a process or resolve unrelated unknown writes.
func (h *Host) RecoverTool(ctx context.Context, call llm.ToolCall) (string, bool, error) {
	if call.Name != "shell" || !h.permissions.Shell {
		return "", false, nil
	}
	owner := jobOwner(ctx)
	h.jobs.mu.Lock()
	err := h.jobs.load(owner)
	j := h.jobs.jobs[jobID(owner, call.ID)]
	h.jobs.mu.Unlock()
	if err != nil || j == nil {
		return "", false, err
	}
	if j.record.Digest != jobArgumentsDigest(call.Arguments) {
		return "", false, errors.New("shell recovery arguments differ from admission")
	}
	out, err := j.read(0)
	return out, true, err
}

// PendingJobs lets the loop keep its task lifecycle open until owned local work
// settles. It reveals only IDs; the observation is logged by the engine.
func (h *Host) PendingJobs(ctx context.Context) ([]string, error) {
	if !h.permissions.Shell {
		return nil, nil
	}
	owner := jobOwner(ctx)
	h.jobs.mu.Lock()
	defer h.jobs.mu.Unlock()
	if err := h.jobs.load(owner); err != nil {
		return nil, err
	}
	ids := []string{}
	for id, j := range h.jobs.jobs {
		if j.record.Owner != owner {
			continue
		}
		j.mu.Lock()
		live := liveJob(j.record.Status)
		j.mu.Unlock()
		if live {
			ids = append(ids, jobHandle(id))
		}
	}
	sort.Strings(ids)
	return ids, nil
}
