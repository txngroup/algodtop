package sysstat

import (
	"os"
	"runtime"
	"testing"
)

// Each platform's process-stats code is tested against the test process.
func TestSelfProcess(t *testing.T) {
	c := New()
	defer c.Close()
	pid := os.Getpid()
	m := c.Procs([]int{pid})
	p, ok := m[pid]
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("no process stats on", runtime.GOOS)
	}
	if !ok {
		t.Fatal("no stats for own process")
	}
	if p.RSSBytes <= 0 {
		t.Errorf("rss %v", p.RSSBytes)
	}
	if p.CPUSeconds < 0 {
		t.Errorf("cpu %v", p.CPUSeconds)
	}
	if p.Threads <= 0 {
		t.Errorf("threads %v", p.Threads)
	}
	if runtime.GOOS != "darwin" && p.Handles <= 0 {
		t.Errorf("handles %v", p.Handles)
	}
	if p.StartTime == 0 {
		t.Errorf("no start time")
	}
	if !Alive(pid) {
		t.Error("own process not alive")
	}
	if Alive(1 << 30) {
		t.Error("impossible pid alive")
	}
	if _, _, err := c.Interfaces(); err != nil {
		t.Errorf("interfaces: %v", err)
	}
	for _, s := range []string{StatCPU, StatRSS, StatDiskFree} {
		if !Available(s) {
			t.Errorf("%s not available", s)
		}
	}
}

func TestParseCPUTime(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip()
	}
}
