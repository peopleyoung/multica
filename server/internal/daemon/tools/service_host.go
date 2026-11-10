package tools

import (
	"context"
	"errors"
)

// ServiceLimits bound a separately supervised execution service. They are
// deployment settings, never model arguments or agent-widened permissions.
type ServiceLimits struct {
	MaxActive  int           `json:"max_active"`
	MaxRecords int           `json:"max_records"`
	LogBytes   int64         `json:"log_bytes"`
	WriteFiles bool          `json:"write_files,omitempty"`
	Sandbox    SandboxPolicy `json:"sandbox,omitempty"`
}

func NewServiceHost(workDir string, env []string, dir string, limits ServiceLimits) (*Host, error) {
	if dir == "" || limits.MaxActive < 1 || limits.MaxActive > 64 || limits.MaxRecords < limits.MaxActive || limits.MaxRecords > 100000 || limits.LogBytes < jobRetention || limits.LogBytes > 1<<30 {
		return nil, errors.New("invalid independent job service limits")
	}
	h, err := New(workDir, Permissions{Shell: true, WriteFiles: limits.WriteFiles}, env, dir)
	if err != nil {
		return nil, err
	}
	if err := h.configureSandbox(context.Background(), limits.Sandbox); err != nil {
		_ = h.Close()
		return nil, err
	}
	h.jobs.maxActive, h.jobs.maxRecords, h.jobs.logLimit = limits.MaxActive, limits.MaxRecords, limits.LogBytes
	return h, nil
}

// FindSubmission observes an admitted identity without starting work, even if
// the caller lost the original submit response. Absence is explicit.
func (h *Host) FindSubmission(ctx context.Context, key string) (string, bool, error) {
	owner := jobOwner(ctx)
	h.jobs.mu.Lock()
	err := h.jobs.load(owner)
	job := h.jobs.jobs[jobID(owner, key)]
	h.jobs.mu.Unlock()
	if err != nil || job == nil {
		return "", false, err
	}
	out, err := job.read(0)
	return out, true, err
}
