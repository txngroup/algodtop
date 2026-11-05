//go:build windows

package daemon

import (
	"context"
	"syscall"
)

// Windows has no SIGHUP: `algodtop reload` is the reload method there.
func (d *Daemon) handleSignals(ctx context.Context) func() { return func() {} }

const belowNormalPriorityClass = 0x00004000

// lowerPriority sets BELOW_NORMAL_PRIORITY_CLASS on the daemon.
func lowerPriority() {
	k := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k.NewProc("GetCurrentProcess").Call()
	k.NewProc("SetPriorityClass").Call(h, belowNormalPriorityClass)
}
