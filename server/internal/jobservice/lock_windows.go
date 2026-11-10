//go:build windows

package jobservice

import "errors"

func lockDirectory(string) (func() error, error) {
	return nil, errors.New("independent job service currently requires a POSIX host")
}
