//go:build !windows

package tools

import (
	"os"
	"syscall"
)

// Nonblocking open prevents a concurrently substituted FIFO from hanging the
// daemon before read can check the opened descriptor's regular-file type.
func openForRead(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
