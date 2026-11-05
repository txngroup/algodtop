//go:build !windows

package fsx

import (
	"errors"
	"os"
	"syscall"
)

// ErrLocked is returned when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock takes an exclusive advisory lock on path. Close the file to release.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}
