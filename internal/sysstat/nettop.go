package sysstat

import (
	"bufio"
	"strconv"
	"strings"
)

// nettopTracker turns nettop's per-socket byte counts into a cumulative
// counter per process. nettop's process line sums the sockets that are open
// now, so it drops whenever a peer disconnects, and a drop reads as a
// counter reset that counts the whole sum again. Like sock_diag on Linux,
// the tracker adds each socket's growth since the last poll. It misses the
// bytes a socket moved between the last poll and its close.
type nettopTracker struct {
	procs map[int]*nettopProc
}

type nettopProc struct {
	last  map[string][2]float64 // socket -> in, out at the last poll
	total [2]float64            // cumulative in, out
}

// nettopSample is one process in one nettop run.
type nettopSample struct {
	proc  [2]float64            // the process line: sum of open sockets
	socks map[string][2]float64 // socket line -> in, out
}

// parseNettop reads the output of nettop -L 1 -J bytes_in,bytes_out -x for
// the given PIDs. Each process line, "algod.<pid>,in,out,", is followed by
// one line per socket, "tcp4 <local><-><remote>,in,out,". A listening
// socket has empty counts and is skipped.
func parseNettop(out string, pids []int) map[int]*nettopSample {
	want := make(map[int]bool, len(pids))
	for _, p := range pids {
		want[p] = true
	}
	res := map[int]*nettopSample{}
	var cur *nettopSample
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		if strings.Contains(f[0], "<->") {
			if cur == nil || f[1] == "" || f[2] == "" {
				continue
			}
			in, err1 := strconv.ParseFloat(f[1], 64)
			ob, err2 := strconv.ParseFloat(f[2], 64)
			if err1 != nil || err2 != nil {
				continue
			}
			// Unconnected UDP sockets can share a name.
			key := f[0]
			for n := 2; ; n++ {
				if _, dup := cur.socks[key]; !dup {
					break
				}
				key = f[0] + "#" + strconv.Itoa(n)
			}
			cur.socks[key] = [2]float64{in, ob}
			continue
		}
		cur = nil
		i := strings.LastIndexByte(f[0], '.')
		if i < 0 {
			continue
		}
		pid, err := strconv.Atoi(f[0][i+1:])
		if err != nil || !want[pid] {
			continue
		}
		in, _ := strconv.ParseFloat(f[1], 64)
		ob, _ := strconv.ParseFloat(f[2], 64)
		cur = &nettopSample{proc: [2]float64{in, ob}, socks: map[string][2]float64{}}
		res[pid] = cur
	}
	return res
}

// update folds one nettop run into the counters and returns the cumulative
// bytes of each process in it. A process seen for the first time starts at
// the sum of its open sockets. A process that is not in pids is forgotten,
// so a restarted algod starts fresh under its new PID.
func (t *nettopTracker) update(samples map[int]*nettopSample, pids []int) map[int]NetIO {
	if t.procs == nil {
		t.procs = map[int]*nettopProc{}
	}
	out := map[int]NetIO{}
	for pid, s := range samples {
		socks := s.socks
		if len(socks) == 0 && (s.proc[0] > 0 || s.proc[1] > 0) {
			// No socket lines, but the process line has bytes: count the
			// process line as one socket rather than report nothing.
			socks = map[string][2]float64{"": s.proc}
		}
		p := t.procs[pid]
		if p == nil {
			p = &nettopProc{}
			t.procs[pid] = p
		}
		for k, cur := range socks {
			prev, seen := p.last[k]
			if seen && cur[0] >= prev[0] && cur[1] >= prev[1] {
				p.total[0] += cur[0] - prev[0]
				p.total[1] += cur[1] - prev[1]
			} else {
				// A new socket, or a new connection that reused the name
				// of one that closed: all of its bytes are new.
				p.total[0] += cur[0]
				p.total[1] += cur[1]
			}
		}
		p.last = socks
		out[pid] = NetIO{In: p.total[0], Out: p.total[1], Method: "nettop"}
	}
	keep := make(map[int]bool, len(pids))
	for _, pid := range pids {
		keep[pid] = true
	}
	for pid := range t.procs {
		if !keep[pid] {
			delete(t.procs, pid)
		}
	}
	return out
}
