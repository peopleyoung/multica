//go:build !linux

package tools

import (
	"errors"
	"os"
	"os/exec"
)

func startTerminal(*exec.Cmd, uint16, uint16) (*os.File, error) {
	return nil, errors.New("interactive terminals currently require Linux")
}
func resizeTerminal(*os.File, uint16, uint16) error {
	return errors.New("interactive terminals currently require Linux")
}
func interruptTerminal(*os.File, *exec.Cmd) error {
	return errors.New("interactive terminals currently require Linux")
}
func stopTerminal(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
