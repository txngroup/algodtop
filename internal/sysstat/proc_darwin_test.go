//go:build darwin

package sysstat

import (
	"testing"
	"time"
)

// Close kills a command that a pass is still waiting on, so the host
// collector does not hold up daemon shutdown for a slow nettop (issue #15).
func TestCloseKillsRunningCommand(t *testing.T) {
	c := New()
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.Close()
	}()
	t0 := time.Now()
	c.mu.Lock()
	_, err := run(c.ctx, "sleep", "30")
	c.mu.Unlock()
	if err == nil {
		t.Fatal("sleep finished, want it killed")
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Errorf("command ran %v after Close", d)
	}
	if _, _, _, ok := c.Load(); ok {
		t.Error("Load ran a command after Close")
	}
}
