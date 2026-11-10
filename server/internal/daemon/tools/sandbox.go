package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SandboxPolicy belongs to the execution host owner, never tool arguments.
// Confined modes refuse to execute when the kernel/runner cannot enforce them.
type SandboxPolicy struct {
	Mode        string   `json:"mode,omitempty"`
	Network     bool     `json:"network,omitempty"`
	ReadPaths   []string `json:"read_paths,omitempty"`
	DevicePaths []string `json:"device_paths,omitempty"`
}

func (p SandboxPolicy) command(workDir string, env []string, command string, interactive bool) (*exec.Cmd, error) {
	shell := "/bin/sh"
	args := []string{"-c", command}
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
		args = []string{"/d", "/s", "/c", command}
	}
	if interactive {
		shell = "/bin/bash"
		args = []string{"--noprofile", "--norc", "-i"}
	}
	if p.Mode == "" || p.Mode == "unconfined" {
		cmd := exec.Command(shell, args...)
		cmd.Dir = workDir
		cmd.Env = env
		return cmd, nil
	}
	if runtime.GOOS != "linux" || p.Mode != "workspace_write" && p.Mode != "read_only" {
		return nil, errors.New("requested sandbox mode is unsupported on this execution host")
	}
	runner, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, errors.New("confined execution requires bubblewrap; refusing unconfined fallback")
	}
	argv := []string{"--die-with-parent", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--cap-drop", "ALL", "--clearenv"}
	if !interactive {
		argv = append(argv, "--new-session")
	}
	if !p.Network {
		argv = append(argv, "--unshare-net")
	}
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc/ssl", "/etc/resolv.conf", "/etc/hosts"} {
		if _, err := os.Stat(path); err == nil {
			argv = append(argv, "--ro-bind", path, path)
		}
	}
	if len(p.ReadPaths) > 16 || len(p.DevicePaths) > 16 {
		return nil, errors.New("sandbox mount limit exceeded")
	}
	for _, path := range p.ReadPaths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsRune(path, 0) {
			return nil, errors.New("invalid sandbox read mount")
		}
		argv = append(argv, "--ro-bind", path, path)
	}
	argv = append(argv, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, path := range p.DevicePaths {
		if !strings.HasPrefix(path, "/dev/") || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			return nil, errors.New("invalid sandbox device mount")
		}
		argv = append(argv, "--dev-bind", path, path)
	}
	mode := "--ro-bind"
	if p.Mode == "workspace_write" {
		mode = "--bind"
	}
	argv = append(argv, mode, workDir, workDir, "--chdir", workDir)
	// Never inherit service/daemon credentials into a confined child.
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (key == "PATH" || key == "LANG" || key == "LC_ALL" || key == "PYTHONPATH" || key == "CUDA_VISIBLE_DEVICES") {
			argv = append(argv, "--setenv", key, value)
		}
	}
	argv = append(argv, "--setenv", "HOME", "/tmp", "--setenv", "TERM", "xterm-256color", "--", shell)
	argv = append(argv, args...)
	cmd := exec.Command(runner, argv...)
	cmd.Dir = workDir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd, nil
}
func (h *Host) configureSandbox(ctx context.Context, p SandboxPolicy) error {
	// Construction-only: the effective policy is immutable after publication.
	if p.Mode == "read_only" && h.permissions.WriteFiles {
		return errors.New("read-only sandbox cannot grant file writes")
	}
	cmd, err := p.command(h.jobs.workDir, h.jobs.env, "true", false)
	if err != nil {
		return err
	}
	if p.Mode != "" && p.Mode != "unconfined" {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		run := exec.CommandContext(probe, cmd.Path, cmd.Args[1:]...)
		run.Dir = cmd.Dir
		run.Env = cmd.Env
		if err := run.Run(); err != nil {
			return fmt.Errorf("sandbox enforcement probe failed; no commands started: %w", err)
		}
	}
	h.jobs.policy = p
	return nil
}
