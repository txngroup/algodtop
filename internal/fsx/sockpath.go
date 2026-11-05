package fsx

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var chdirMu sync.Mutex

// SocketAddr returns an address for a unix socket path. sun_path holds 104
// bytes on macOS and 108 on Linux, so a deeper path cannot be bound or
// dialed directly. Then the process changes into the socket's directory
// and uses the bare name. algodtop uses absolute paths everywhere else, so
// the working directory change is harmless.
func SocketAddr(path string) string {
	limit := 103
	if runtime.GOOS == "linux" {
		limit = 107
	}
	if len(path) <= limit {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	chdirMu.Lock()
	defer chdirMu.Unlock()
	if err := os.Chdir(filepath.Dir(abs)); err != nil {
		return path
	}
	return filepath.Base(abs)
}
