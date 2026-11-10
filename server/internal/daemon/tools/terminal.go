package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type terminalSet struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession
	stop     chan struct{}
	closed   bool
}
type terminalSession struct {
	mu             sync.Mutex
	inputMu        sync.Mutex
	id, owner      string
	cmd            *exec.Cmd
	pty            *os.File
	done, readDone chan struct{}
	output         []byte
	start          int64
	status         string
	exit           *int
	touched        time.Time
	closeOnce      sync.Once
	closeErr       error
}

func terminalDefinition() llm.Tool {
	return llm.Tool{Name: "terminal", Description: "Manage an interactive PTY shell in this execution environment. open, send(session_id,text), read(session_id,offset?), interrupt(session_id), resize(session_id,rows,cols), close(session_id), list. Shell variables/cwd and REPL state persist. Read/send waits are bounded; output silence is NOT readiness or success. Use read offsets; close before finishing. Sessions expire after ten idle minutes and do not recover after service restart. No full-screen UI contract.", Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["open","send","read","interrupt","resize","close","list"]},"session_id":{"type":"string"},"text":{"type":"string","maxLength":8192},"offset":{"type":"integer","minimum":0},"wait_ms":{"type":"integer","minimum":0,"maximum":1000},"rows":{"type":"integer","minimum":1,"maximum":300},"cols":{"type":"integer","minimum":1,"maximum":500}},"required":["action"],"additionalProperties":false}`)}
}
func (h *Host) terminal(ctx context.Context, call llm.ToolCall) (string, error) {
	if !h.permissions.Shell {
		return "", errors.New("interactive shell is not authorized")
	}
	var args struct {
		Action string `json:"action"`
		ID     string `json:"session_id"`
		Text   string `json:"text"`
		Offset int64  `json:"offset"`
		Wait   int    `json:"wait_ms"`
		Rows   uint16 `json:"rows"`
		Cols   uint16 `json:"cols"`
	}
	var fields map[string]json.RawMessage
	if len(call.Arguments) > 16384 || json.Unmarshal(call.Arguments, &args) != nil || json.Unmarshal(call.Arguments, &fields) != nil || fields == nil || args.Wait < 0 || args.Wait > 1000 || args.Offset < 0 || args.Rows > 300 || args.Cols > 500 || len(args.Text) > 8192 {
		return "", errors.New("invalid terminal arguments")
	}
	allowed := map[string]bool{"action": true}
	switch args.Action {
	case "open":
		allowed["rows"] = true
		allowed["cols"] = true
	case "list":
	case "send":
		allowed["text"] = true
		allowed["wait_ms"] = true
		allowed["offset"] = true
		allowed["session_id"] = true
	case "read":
		allowed["offset"] = true
		allowed["wait_ms"] = true
		allowed["session_id"] = true
	case "resize":
		allowed["rows"] = true
		allowed["cols"] = true
		allowed["session_id"] = true
	case "interrupt", "close":
		allowed["session_id"] = true
	default:
		return "", errors.New("unknown terminal action")
	}
	for k, v := range fields {
		if !allowed[k] || string(v) == "null" {
			return "", errors.New("unexpected terminal argument")
		}
	}
	if args.Rows == 0 {
		args.Rows = 24
	}
	if args.Cols == 0 {
		args.Cols = 100
	}
	owner := jobOwner(ctx)
	set := h.terminals
	set.mu.Lock()
	if set.closed {
		set.mu.Unlock()
		return "", errors.New("terminal host is closed")
	}
	// Reap abandoned sessions before admission; per-owner handles remain bounded.
	active := 0
	for id, s := range set.sessions {
		s.mu.Lock()
		expired := time.Since(s.touched) > 10*time.Minute
		s.mu.Unlock()
		if expired {
			s.close()
			delete(set.sessions, id)
		} else if s.owner == owner {
			active++
		}
	}
	if args.Action == "open" {
		if active >= 4 || len(set.sessions) >= 64 {
			set.mu.Unlock()
			return "", errors.New("terminal limit reached; close existing sessions")
		}
		cmd, err := h.jobs.policy.command(h.jobs.workDir, h.jobs.env, "", true)
		if err != nil {
			set.mu.Unlock()
			return "", err
		}
		f, err := startTerminal(cmd, args.Rows, args.Cols)
		if err != nil {
			set.mu.Unlock()
			return "", fmt.Errorf("terminal allocation failed: %w", err)
		}
		s := &terminalSession{id: "terminal-" + uuid.NewString(), owner: owner, cmd: cmd, pty: f, done: make(chan struct{}), readDone: make(chan struct{}), status: "running", touched: time.Now()}
		set.sessions[s.id] = s
		set.mu.Unlock()
		go s.capture()
		go s.wait()
		return s.observe(0)
	}
	if args.Action == "list" {
		list := []map[string]string{}
		for _, s := range set.sessions {
			if s.owner == owner {
				s.mu.Lock()
				list = append(list, map[string]string{"session_id": s.id, "status": s.status})
				s.mu.Unlock()
			}
		}
		set.mu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i]["session_id"] < list[j]["session_id"] })
		raw, _ := json.Marshal(list)
		return string(raw), nil
	}
	s := set.sessions[args.ID]
	set.mu.Unlock()
	if s == nil || s.owner != owner {
		return "", errors.New("terminal not found in this session; use terminal list")
	}
	s.mu.Lock()
	s.touched = time.Now()
	s.mu.Unlock()
	switch args.Action {
	case "send":
		if args.Text == "" {
			return "", errors.New("terminal send needs text")
		}
		s.inputMu.Lock()
		written := make(chan error, 1)
		go func() { _, err := s.pty.WriteString(args.Text); written <- err }()
		var err error
		timer := time.NewTimer(time.Second)
		select {
		case err = <-written:
		case <-ctx.Done():
			s.close()
			err = ErrOutcomeUnknown
		case <-timer.C:
			s.close()
			err = ErrOutcomeUnknown
		}
		timer.Stop()
		s.inputMu.Unlock()
		if err != nil {
			return "", err
		}
	case "interrupt":
		if err := interruptTerminal(s.pty, s.cmd); err != nil {
			return "", err
		}
	case "resize":
		if err := resizeTerminal(s.pty, args.Rows, args.Cols); err != nil {
			return "", err
		}
	case "close":
		if err := s.close(); err != nil {
			return "", err
		}
		set.mu.Lock()
		delete(set.sessions, s.id)
		set.mu.Unlock()
	}
	if args.Wait > 0 {
		timer := time.NewTimer(time.Duration(args.Wait) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-s.done:
		case <-timer.C:
		}
	}
	return s.observe(args.Offset)
}
func (s *terminalSession) capture() {
	defer close(s.readDone)
	buf := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.output = append(s.output, buf[:n]...)
			if extra := len(s.output) - (256 << 10); extra > 0 {
				s.start += int64(extra)
				s.output = s.output[extra:]
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
func (s *terminalSession) wait() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.status = "exited"
	if err != nil {
		s.status = "failed"
	}
	if s.cmd.ProcessState != nil {
		v := s.cmd.ProcessState.ExitCode()
		s.exit = &v
	}
	s.mu.Unlock()
	close(s.done)
}
func (s *terminalSession) observe(offset int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.start + int64(len(s.output))
	if offset > end {
		return "", errors.New("terminal offset exceeds output")
	}
	truncated := offset < s.start
	offset = max(offset, s.start)
	next := min(end, offset+(8<<10))
	out := s.output[offset-s.start : next-s.start]

	raw, _ := json.Marshal(map[string]any{"session_id": s.id, "status": s.status, "exit_code": s.exit, "output": strings.ToValidUTF8(string(out), "�"), "output_start": offset, "next_offset": next, "output_truncated": truncated, "readiness": "unknown"})
	return string(raw), nil
}
func (s *terminalSession) close() error {
	s.closeOnce.Do(func() {
		s.closeErr = stopTerminal(s.cmd)
		_ = s.pty.Close()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for _, done := range []chan struct{}{s.done, s.readDone} {
			select {
			case <-done:
			case <-timer.C:
				s.closeErr = errors.Join(s.closeErr, errors.New("terminal cleanup not confirmed"))
				return
			}
		}
	})
	return s.closeErr
}
func (s *terminalSet) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.stop)
	var failures []error
	for _, session := range s.sessions {
		failures = append(failures, session.close())
	}
	return errors.Join(failures...)
}

func (s *terminalSet) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			for id, t := range s.sessions {
				t.mu.Lock()
				expired := time.Since(t.touched) > 10*time.Minute
				t.mu.Unlock()
				if expired {
					t.close()
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}
