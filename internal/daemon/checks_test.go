package daemon

import (
	"testing"
	"time"
)

func TestClockJump(t *testing.T) {
	var c clockWatch
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).UnixNano()
	c.observe(base, 0)
	if _, ok := c.observe(base+int64(10*time.Second), 10*time.Second); ok {
		t.Fatal("steady clock reported a jump")
	}
	// The wall clock moves back an hour while 10 s pass.
	jump, ok := c.observe(base+int64(20*time.Second)-int64(time.Hour), 20*time.Second)
	if !ok || jump > -59*time.Minute {
		t.Fatalf("missed a backward jump: %v %v", jump, ok)
	}
}
