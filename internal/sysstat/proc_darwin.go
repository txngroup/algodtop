//go:build darwin

package sysstat

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"algod.top/internal/algod"
)

// Methods names how each stat is measured.
func Methods() map[string]string {
	return map[string]string{
		StatCPU:       "ps time=",
		StatRSS:       "ps rss=",
		StatThreads:   "ps -M",
		StatHandles:   "lsof, every 5 min",
		StatDiskIO:    "", // needs proc_pid_rusage in libproc, which needs cgo
		StatProcNet:   "nettop",
		StatIfBytes:   "netstat -ib",
		StatDiskFree:  "statfs",
		StatLoad:      "sysctl vm.loadavg",
		StatRestartBy: "pid and start time",
	}
}

type platformState struct {
	lsof   cache[map[int]int]
	lsofOf string
	nettop nettopTracker
}

func (p *platformState) init() {}

func (p *platformState) close() {}

// cmdTimeout caps one command, so a hung ps or lsof cannot stall a pass.
const cmdTimeout = 20 * time.Second

// output runs a command in the C locale and returns its stdout. Cancelling
// ctx kills the command.
func output(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Env = append(os.Environ(), env...)
	c.WaitDelay = time.Second
	return c.Output()
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return output(ctx, []string{"LC_ALL=C", "LANG=C"}, name, args...)
}

func joinPIDs(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// procs runs one ps for every PID. CPU comes from the cumulative time=
// field, because %cpu on macOS is a decaying average.
func (p *platformState) procs(ctx context.Context, pids []int) map[int]Proc {
	out := map[int]Proc{}
	if len(pids) == 0 {
		return out
	}
	list := joinPIDs(pids)
	b, err := run(ctx, "ps", "-o", "pid=,rss=,time=,lstart=", "-p", list)
	if err != nil && len(b) == 0 {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		pr := Proc{PID: pid, Threads: NA, Handles: NA, ReadBytes: NA, WriteBytes: NA}
		kb, _ := strconv.ParseFloat(f[1], 64)
		pr.RSSBytes = kb * 1024
		pr.CPUSeconds = parseCPUTime(f[2])
		if t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(f[3:], " "), time.Local); err == nil {
			pr.StartTime = t.UnixNano()
		}
		out[pid] = pr
	}
	// Threads: ps -M prints one line per thread, each with the PID.
	if b, err := run(ctx, "ps", "-M", "-p", list); err == nil || len(b) > 0 {
		counts := map[int]int{}
		for i, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if i == 0 || len(f) < 2 {
				continue
			}
			for _, x := range f[:2] {
				if pid, err := strconv.Atoi(x); err == nil {
					counts[pid]++
					break
				}
			}
		}
		for pid, n := range counts {
			if pr, ok := out[pid]; ok {
				pr.Threads = float64(n)
				out[pid] = pr
			}
		}
	}
	// Open files from lsof, refreshed every 5 minutes.
	if p.lsofOf != list {
		p.lsof = cache[map[int]int]{}
		p.lsofOf = list
	}
	fds := p.lsof.get(5*time.Minute, func() map[int]int { return lsofCounts(ctx, list) })
	for pid, n := range fds {
		if pr, ok := out[pid]; ok {
			pr.Handles = float64(n)
			out[pid] = pr
		}
	}
	return out
}

func lsofCounts(ctx context.Context, list string) map[int]int {
	b, _ := run(ctx, "lsof", "-n", "-P", "-F", "f", "-p", list)
	counts := map[int]int{}
	cur := 0
	for _, l := range strings.Split(string(b), "\n") {
		if len(l) < 2 {
			continue
		}
		switch l[0] {
		case 'p':
			cur, _ = strconv.Atoi(l[1:])
		case 'f':
			if cur > 0 {
				counts[cur]++
			}
		}
	}
	return counts
}

// parseCPUTime parses ps time= output such as 411:17.33 or 1-02:03:04.
func parseCPUTime(s string) float64 {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	parts := strings.Split(s, ":")
	var secs float64
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		secs = secs*60 + v
	}
	return days*86400 + secs
}

// netBytes runs one nettop for every algod PID. A single sample (-L 1)
// prints cumulative bytes per socket at once, so the call costs about
// 0.1 s. A long-lived nettop in logging mode would avoid the exec, but its
// output sits in a stdio buffer when stdout is a pipe and arrives late.
// -n keeps addresses numeric: nettop resolves names on some runs and not
// on others, and a socket whose name changes would be counted twice.
func (p *platformState) netBytes(ctx context.Context, pids []int) map[int]NetIO {
	if len(pids) == 0 {
		return map[int]NetIO{}
	}
	args := []string{"-n", "-L", "1", "-J", "bytes_in,bytes_out", "-x"}
	for _, pid := range pids {
		args = append(args, "-p", strconv.Itoa(pid))
	}
	b, err := output(ctx, []string{"LC_ALL=C"}, "nettop", args...)
	if err != nil && len(b) == 0 {
		return map[int]NetIO{}
	}
	return p.nettop.update(parseNettop(string(b), pids), pids)
}

func interfaces(ctx context.Context) (float64, float64, error) {
	b, err := run(ctx, "netstat", "-ibn")
	if err != nil {
		return 0, 0, err
	}
	var in, out float64
	seen := map[string]bool{}
	lines := strings.Split(string(b), "\n")
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		// Only <Link#n> rows carry per-interface totals once.
		if len(f) < 10 || !strings.HasPrefix(f[2], "<Link") || seen[f[0]] {
			continue
		}
		if strings.HasPrefix(f[0], "lo") {
			continue
		}
		seen[f[0]] = true
		// With no address column, Ibytes is field 6 and Obytes field 9.
		off := 0
		if len(f) >= 11 {
			off = 1
		}
		ib, _ := strconv.ParseFloat(f[5+off], 64)
		ob, _ := strconv.ParseFloat(f[8+off], 64)
		in += ib
		out += ob
	}
	return in, out, nil
}

func loadAvg(ctx context.Context) (float64, float64, float64, bool) {
	b, err := run(ctx, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return 0, 0, 0, false
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(b)), "{}"))
	if len(f) < 3 {
		return 0, 0, 0, false
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	c, _ := strconv.ParseFloat(f[1], 64)
	d, _ := strconv.ParseFloat(f[2], 64)
	return a, c, d, true
}

func listAlgod() []algod.Process {
	b, err := run(context.Background(), "ps", "-axo", "pid=,args=")
	if err != nil {
		return nil
	}
	var out []algod.Process
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		exe := f[1]
		if i := strings.LastIndexByte(exe, '/'); i >= 0 {
			exe = exe[i+1:]
		}
		if exe != "algod" {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		out = append(out, algod.Process{PID: pid, Args: f[1:]})
	}
	return out
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
