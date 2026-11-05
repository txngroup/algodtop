//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// execSelf replaces this process with exe and the same arguments, so a
// service manager keeps tracking the same process after an upgrade.
func execSelf(exe string) error { return syscall.Exec(exe, os.Args, os.Environ()) }
