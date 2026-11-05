//go:build !windows

package disk

import "syscall"

// Stat returns the usage of the volume holding path.
func Stat(path string) (Usage, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return Usage{}, err
	}
	bs := uint64(s.Bsize)
	return Usage{Free: uint64(s.Bavail) * bs, Total: uint64(s.Blocks) * bs}, nil
}
