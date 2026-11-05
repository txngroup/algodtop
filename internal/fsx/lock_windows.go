//go:build windows

package fsx

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// ErrLocked is returned when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

const (
	lockfileExclusiveLock   = 2
	lockfileFailImmediately = 1
	errorLockViolation      = 33
)

// Lock takes an exclusive lock on path. Close the file to release.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	var ol syscall.Overlapped
	r, _, e := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		f.Close()
		if errno, ok := e.(syscall.Errno); ok && errno == errorLockViolation {
			return nil, ErrLocked
		}
		return nil, e
	}
	return f, nil
}
