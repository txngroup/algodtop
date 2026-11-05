//go:build !windows

package tui

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize delivers a value on every SIGWINCH.
func watchResize() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				select {
				case ch <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return ch, func() { signal.Stop(sig); close(done) }
}

func enableVT(*os.File) {}
