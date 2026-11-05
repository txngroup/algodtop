//go:build windows

package fsx

import (
	"os"
	"syscall"
	"time"
)

const isWindows = true

// OpenShared opens a file for reading with FILE_SHARE_DELETE, so another
// process (algod rotating node.log, or the algodtop deleter) can rename or
// delete it while it is open. os.Open omits that flag.
func OpenShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// Rename renames a file, retrying briefly when a reader holds the target.
func Rename(from, to string) error {
	var err error
	for range 5 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}
