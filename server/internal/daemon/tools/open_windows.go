//go:build windows

package tools

import "os"

func openForRead(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
