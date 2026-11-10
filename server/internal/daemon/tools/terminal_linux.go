//go:build linux

package tools

import (
	"errors"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func startTerminal(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
}
func resizeTerminal(f *os.File, rows, cols uint16) error {
	return pty.Setsize(f, &pty.Winsize{Rows: rows, Cols: cols})
}
func interruptTerminal(f *os.File, cmd *exec.Cmd) error {
	group, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return err
	}
	if group <= 1 {
		return errors.New("terminal foreground process group unavailable")
	}
	return syscall.Kill(-group, syscall.SIGINT)
}
func stopTerminal(cmd *exec.Cmd) error {
	var failures []error
	// Interactive job control places children in different process groups. Kill
	// every group in this PTY session, rather than only the shell's group.
	sid := cmd.Process.Pid
	entries, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 4 {
			continue
		}
		session, _ := strconv.Atoi(fields[3])
		group, _ := strconv.Atoi(fields[2])
		if session == sid && group > 1 {
			if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				failures = append(failures, err)
			}
		}
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
