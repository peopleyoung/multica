package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

type RemoteEnvironment struct {
	mu          sync.Mutex
	external    *External
	permissions Permissions
	terminals   map[string]bool
	owner       string
	Info        json.RawMessage
}

func NewRemoteEnvironment(ctx context.Context, external *External, permissions Permissions) (*RemoteEnvironment, error) {
	if external == nil {
		return nil, errors.New("remote environment service missing")
	}
	service, ok := external.services["multica_jobs"]
	if !ok {
		return nil, errors.New("remote environment requires multica_jobs")
	}
	if (permissions.Shell || permissions.WriteFiles) && !service.AllowWrite {
		return nil, errors.New("remote writes and commands require service write permission")
	}
	h := &RemoteEnvironment{external: external, permissions: permissions, terminals: map[string]bool{}}
	raw, err := h.request(ctx, "GET", "v1/environment", nil, 65536, false)
	if err != nil {
		return nil, err
	}
	var info struct {
		WorkDir  string `json:"work_dir"`
		Write    bool   `json:"write_files"`
		Sandbox  string `json:"sandbox"`
		Platform string `json:"platform"`
		Network  bool   `json:"network"`
	}
	if json.Unmarshal(raw, &info) != nil || info.WorkDir == "" || len(info.WorkDir) > 4096 {
		return nil, errors.New("invalid remote execution environment response")
	}
	if permissions.WriteFiles && !info.Write {
		return nil, errors.New("remote host does not allow file writes")
	}
	h.Info, _ = json.Marshal(info)
	return h, nil
}
func (h *RemoteEnvironment) request(ctx context.Context, method, path string, body any, limit int64, write bool) ([]byte, error) {
	s := h.external.services["multica_jobs"]
	if write && !s.AllowWrite {
		return nil, errors.New("remote execution service is read-only")
	}
	var input io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		input = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+"/"+path, input)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.external.tokens["multica_jobs"])
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		if write {
			return nil, ErrOutcomeUnknown
		}
		return nil, errors.New("remote environment unavailable")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		if write {
			return nil, ErrOutcomeUnknown
		}
		return nil, errors.New("remote environment response exceeds limit")
	}
	if res.StatusCode != 200 {
		if write && (res.StatusCode >= 500 || res.StatusCode >= 300 && res.StatusCode < 400) {
			return nil, ErrOutcomeUnknown
		}
		return nil, errors.New("remote operation refused; check execution permissions and relative paths")
	}
	return raw, nil
}
func (h *RemoteEnvironment) Definitions() []llm.Tool {
	defs := environmentDefinitions(h.permissions)
	for i := range defs {
		defs[i].Description = "Remote execution environment: " + defs[i].Description
		if defs[i].Name == "shell" {
			defs[i].Description = "Run a command on the SAME remote host/work directory as file tools. Returns an independently hosted job. It outlives this run; inspect job_output/job_search and never relaunch to poll. Use persistent_job for named-service admission lookup."
		}
	}
	return defs
}
func (h *RemoteEnvironment) IsReadOnly(call llm.ToolCall) bool {
	switch call.Name {
	case "read_file", "read_rules", "list_directory", "search_text", "job_list", "job_output", "job_search":
		return true
	}
	return false
}
func (h *RemoteEnvironment) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if isShellJobTool(call.Name) && !h.permissions.Shell || call.Name == "terminal" && !h.permissions.Shell {
		return "", errors.New("remote shell disabled for this run")
	}
	if (call.Name == "apply_patch" || call.Name == "make_directory") && !h.permissions.WriteFiles {
		return "", errors.New("remote file writes disabled for this run")
	}
	var background bool
	if call.Name == "shell" {
		args, err := parseJobArgs(call)
		if err != nil {
			return "", err
		}
		background = args.Background
		if background {
			key, err := submissionKey(ctx, call.ID)
			if err != nil {
				return "", err
			}
			if h.external.ObserveSubmission != nil {
				if err := h.external.ObserveSubmission(ctx, key); err != nil {
					return "", err
				}
			}
		}
	}

	scope := ""
	if claim, ok := agentstate.WriteClaimFromContext(ctx); ok {
		scope = claim.SessionID
		h.mu.Lock()
		h.owner = scope
		h.mu.Unlock()
	}
	key := ""
	if call.Name == "shell" {
		var err error
		key, err = submissionKey(ctx, call.ID)
		if err != nil {
			return "", err
		}
	}
	body := map[string]any{"name": call.Name, "arguments": json.RawMessage(call.Arguments), "scope": scope, "key": key}
	raw, err := h.request(ctx, "POST", "v1/environment/tools", body, (2 << 20), !h.IsReadOnly(call))
	if err != nil {
		// A lost foreground response may already represent a durable long job.
		// Observe its existing admission even when the model ends this run now.
		if call.Name == "shell" && errors.Is(err, ErrOutcomeUnknown) && h.external.ObserveSubmission != nil {
			if watchErr := h.external.ObserveSubmission(ctx, key); watchErr != nil {
				return "", errors.Join(err, watchErr)
			}
		}
		return "", err
	}
	var result struct {
		Output *string `json:"output"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Output == nil {
		if !h.IsReadOnly(call) {
			return "", ErrOutcomeUnknown
		}
		return "", errors.New("invalid remote tool response")
	}
	if call.Name == "shell" && !background && h.external.ObserveSubmission != nil {
		var job struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(*result.Output), &job) == nil && (job.Status == "running" || job.Status == "starting") {
			key, _ := submissionKey(ctx, call.ID)
			if err := h.external.ObserveSubmission(ctx, key); err != nil {
				return "", ErrOutcomeUnknown
			}
		}
	}
	if call.Name == "terminal" {
		var v struct {
			ID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(*result.Output), &v) == nil && v.ID != "" {
			h.mu.Lock()
			var action struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(call.Arguments, &action)
			if action.Action == "close" {
				delete(h.terminals, v.ID)
			} else {
				h.terminals[v.ID] = true
			}
			h.mu.Unlock()
		}
	}
	return *result.Output, nil
}
func (h *RemoteEnvironment) Rules(ctx context.Context, path string) (string, error) {
	raw, _ := json.Marshal(map[string]string{"path": path})
	return h.Execute(ctx, llm.ToolCall{Name: "read_rules", Arguments: raw})
}
func (h *RemoteEnvironment) ReadArtifact(path string) ([]byte, error) {
	raw, err := h.request(context.Background(), "POST", "v1/environment/artifact", map[string]string{"path": path}, (2 << 20), false)
	if err != nil {
		return nil, err
	}
	var v struct {
		Data []byte `json:"data"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Data) > fileLimit {
		return nil, errors.New("invalid remote artifact")
	}
	return v.Data, nil
}
func (h *RemoteEnvironment) RecoverTool(ctx context.Context, call llm.ToolCall) (string, bool, error) {
	if call.Name != "shell" {
		return "", false, nil
	}
	raw, _ := json.Marshal(map[string]string{"action": "submit"})
	out, found, err := h.external.RecoverPersistentTool(ctx, llm.ToolCall{ID: call.ID, Name: "persistent_job", Arguments: raw})
	if found && err == nil && h.external.ObserveSubmission != nil {
		key, keyErr := submissionKey(ctx, call.ID)
		if keyErr != nil {
			return "", true, keyErr
		}
		if err := h.external.ObserveSubmission(ctx, key); err != nil {
			return "", true, err
		}
	}
	return out, found, err
}
func (h *RemoteEnvironment) PendingJobs(context.Context) ([]string, error) { return nil, nil }
func (h *RemoteEnvironment) JobNotifications(context.Context, []llm.ModelMessage) ([]string, error) {
	return nil, nil
}
func (h *RemoteEnvironment) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	var errs []error
	for id := range h.terminals {
		raw, err := h.request(ctx, "POST", "v1/environment/tools", map[string]any{"name": "terminal", "scope": h.owner, "arguments": map[string]string{"action": "close", "session_id": id}}, 65536, true)
		_ = raw
		if err != nil {
			errs = append(errs, err)
		} else {
			delete(h.terminals, id)
		}
	}
	return errors.Join(errs...)
}
