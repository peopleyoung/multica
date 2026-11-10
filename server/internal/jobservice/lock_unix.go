//go:build !windows

package jobservice

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockDirectory(dir string) (func() error, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f.Close, nil
}
