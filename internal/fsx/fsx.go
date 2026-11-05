// Package fsx holds small file helpers shared across packages: atomic
// writes, directory fsync and shared-delete opens on Windows.
package fsx

import (
	"os"
	"path/filepath"
)

// WriteAtomic writes data to path through a temp file, fsync and rename.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil && !isWindows {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return SyncDir(dir)
}

// SyncDir fsyncs a directory so a rename or create inside it is durable.
// Windows has no directory fsync, so it is a no-op there.
func SyncDir(dir string) error {
	if isWindows {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// DirSize returns the total size of regular files under dir.
func DirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
