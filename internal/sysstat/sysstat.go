// Package sysstat collects process and host stats behind one interface with
// an implementation per platform. Every stat has one canonical name on every
// platform, and Methods says how each was measured or that it is not
// available.
package sysstat

import (
	"runtime"
	"sync"
	"time"

	"algod.top/internal/algod"
)

// NA marks a stat the platform cannot provide.
const NA = -1.0

// Proc is one process sample. Fields set to NA are not available.
type Proc struct {
	PID        int
	CPUSeconds float64
	RSSBytes   float64
	Threads    float64
	Handles    float64
	ReadBytes  float64
	WriteBytes float64
	StartTime  int64 // unix ns, or 0
}

// NetIO is cumulative network bytes for one process.
type NetIO struct {
	In     float64
	Out    float64
	Method string
}

// Canonical stat names.
const (
	StatCPU       = "cpu"
	StatRSS       = "rss"
	StatThreads   = "threads"
	StatHandles   = "handles"
	StatDiskIO    = "disk_io"
	StatProcNet   = "proc_net"
	StatIfBytes   = "if_bytes"
	StatDiskFree  = "disk_free"
	StatLoad      = "load"
	StatRestartBy = "restart"
)

// Platform is the running OS.
const Platform = runtime.GOOS

// Available reports whether a stat exists on this platform.
func Available(stat string) bool { return Methods()[stat] != "" }

// Collector holds per-platform state such as the long-lived nettop process
// on macOS or socket counters on Linux.
type Collector struct {
	mu sync.Mutex
	p  platformState
}

// New returns a collector.
func New() *Collector {
	c := &Collector{}
	c.p.init()
	return c
}

// Procs samples the given processes in one pass.
func (c *Collector) Procs(pids []int) map[int]Proc {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.p.procs(pids)
}

// NetBytes returns cumulative per-process network bytes.
func (c *Collector) NetBytes(pids []int) map[int]NetIO {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.p.netBytes(pids)
}

// Interfaces returns total bytes received and sent on non-loopback
// interfaces.
func (c *Collector) Interfaces() (in, out float64, err error) { return interfaces() }

// Load returns the load averages, or ok=false where there is none.
func (c *Collector) Load() (l1, l5, l15 float64, ok bool) { return loadAvg() }

// Close stops helper processes.
func (c *Collector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.p.close()
}

// ListAlgod lists running processes named algod with their arguments.
func ListAlgod() []algod.Process { return listAlgod() }

// Alive reports whether pid is a running process.
func Alive(pid int) bool { return pid > 0 && alive(pid) }

// cache holds an expensive stat that refreshes on its own interval, such
// as lsof on macOS.
type cache[T any] struct {
	at  time.Time
	val T
}

func (c *cache[T]) get(every time.Duration, fetch func() T) T {
	if time.Since(c.at) >= every || c.at.IsZero() {
		c.val = fetch()
		c.at = time.Now()
	}
	return c.val
}
