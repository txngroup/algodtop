//go:build !windows

package daemon

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// handleSignals reloads on SIGHUP. SIGTERM and SIGINT are handled by the
// caller's context.
func (d *Daemon) handleSignals(ctx context.Context) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				if err := d.Reload(); err != nil {
					d.logf("reload: %v", err)
				} else {
					d.logf("config reloaded")
				}
			}
		}
	}()
	return func() { signal.Stop(ch) }
}

// lowerPriority runs the daemon at nice 10 so it never competes with algod.
func lowerPriority() {
	cur, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err == nil && cur < 10 {
		syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	}
}
