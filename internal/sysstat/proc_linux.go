//go:build linux

package sysstat

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"algod.top/internal/algod"
)

// Linux reports clock ticks at USER_HZ, which is 100 on every mainstream
// architecture. Reading it needs sysconf, which needs cgo.
const clkTck = 100

// Methods names how each stat is measured.
func Methods() map[string]string {
	return map[string]string{
		StatCPU:       "/proc/<pid>/stat",
		StatRSS:       "/proc/<pid>/status",
		StatThreads:   "/proc/<pid>/status",
		StatHandles:   "/proc/<pid>/fd",
		StatDiskIO:    "/proc/<pid>/io",
		StatProcNet:   "sock_diag",
		StatIfBytes:   "/proc/net/dev",
		StatDiskFree:  "statfs",
		StatLoad:      "/proc/loadavg",
		StatRestartBy: "pid and start time",
	}
}

type platformState struct {
	diag *sockDiag
}

func (p *platformState) init()  { p.diag = newSockDiag() }
func (p *platformState) close() {}

var bootTime = func() int64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n * int64(time.Second)
		}
	}
	return 0
}()

func (p *platformState) procs(pids []int) map[int]Proc {
	out := map[int]Proc{}
	for _, pid := range pids {
		if pr, ok := readProc(pid); ok {
			out[pid] = pr
		}
	}
	return out
}

func readProc(pid int) (Proc, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	pr := Proc{PID: pid, Threads: NA, Handles: NA, ReadBytes: NA, WriteBytes: NA, RSSBytes: NA}
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return pr, false
	}
	// The command name is in parentheses and may contain spaces.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return pr, false
	}
	f := strings.Fields(string(stat[i+1:]))
	// f[0] is field 3 (state); utime is field 14, stime 15, starttime 22.
	if len(f) > 19 {
		ut, _ := strconv.ParseFloat(f[11], 64)
		st, _ := strconv.ParseFloat(f[12], 64)
		pr.CPUSeconds = (ut + st) / clkTck
		start, _ := strconv.ParseInt(f[19], 10, 64)
		pr.StartTime = bootTime + start*int64(time.Second)/clkTck
	}
	if b, err := os.ReadFile(dir + "/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(l, "VmRSS:"):
				ff := strings.Fields(l)
				if len(ff) >= 2 {
					kb, _ := strconv.ParseFloat(ff[1], 64)
					pr.RSSBytes = kb * 1024
				}
			case strings.HasPrefix(l, "Threads:"):
				ff := strings.Fields(l)
				if len(ff) >= 2 {
					pr.Threads, _ = strconv.ParseFloat(ff[1], 64)
				}
			}
		}
	}
	if ents, err := os.ReadDir(dir + "/fd"); err == nil {
		pr.Handles = float64(len(ents))
	}
	if b, err := os.ReadFile(dir + "/io"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "read_bytes: "); ok {
				pr.ReadBytes, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			}
			if v, ok := strings.CutPrefix(l, "write_bytes: "); ok {
				pr.WriteBytes, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			}
		}
	}
	return pr, true
}

func (p *platformState) netBytes(pids []int) map[int]NetIO {
	out := map[int]NetIO{}
	if p.diag != nil {
		if m, err := p.diag.bytes(pids); err == nil {
			return m
		}
	}
	in, o, err := interfaces()
	if err != nil {
		return out
	}
	for _, pid := range pids {
		out[pid] = NetIO{In: in, Out: o, Method: "interface totals"}
	}
	return out
}

func interfaces() (float64, float64, error) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	var in, out float64
	for _, l := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseFloat(f[0], 64)
		t, _ := strconv.ParseFloat(f[8], 64)
		in += r
		out += t
	}
	return in, out, nil
}

func loadAvg() (float64, float64, float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	c, _ := strconv.ParseFloat(f[1], 64)
	d, _ := strconv.ParseFloat(f[2], 64)
	return a, c, d, true
}

func listAlgod() []algod.Process {
	ents, _ := os.ReadDir("/proc")
	var out []algod.Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(b) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if filepath.Base(args[0]) != "algod" {
			continue
		}
		out = append(out, algod.Process{PID: pid, Args: args})
	}
	return out
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
