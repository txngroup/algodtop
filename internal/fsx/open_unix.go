//go:build !windows

package fsx

import "os"

const isWindows = false

// OpenShared opens a file for reading. On Unix any open file can be renamed
// or deleted, so it is a plain os.Open.
func OpenShared(path string) (*os.File, error) { return os.Open(path) }

// Rename renames a file.
func Rename(from, to string) error { return os.Rename(from, to) }
