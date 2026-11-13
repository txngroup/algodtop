package collector

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"algod.top/internal/disk"
	"algod.top/internal/fsx"
	"algod.top/internal/model"
	"algod.top/internal/sysstat"
)

// HostOptions configure the host collector.
type HostOptions struct {
	ProcEvery time.Duration // 10 s
	NetEvery  time.Duration // 60 s
	NTPServer string        // pool.ntp.org, "" disables
	NTPEvery  time.Duration // 10 min
	Now       func() time.Time
}

// Host collects process and host stats for every local node in one pass,
// so macOS runs one ps for all nodes at once.
type Host struct {
	o     HostOptions
	sys   *sysstat.Collector
	sink  Sink
	nodes func() []*Node

	mu       sync.Mutex
	start    map[int]int64
	prevCPU  map[string][2]float64 // node -> cpu seconds, unix seconds
	prevNet  map[string]netPoint   // node -> last process net counters
	diskHist map[string][]diskPoint
	offset   float64
	offKnown bool
}

type diskPoint struct {
	t    float64
	used float64
}

// netPoint is one reading of a process's cumulative network bytes.
type netPoint struct {
	in, out, t float64 // t in unix seconds
	method     string
}

// netRate is the byte rate between two readings. Two methods count
// different things, such as one process's sockets and a whole namespace's
// interfaces, so a rate needs two readings from the same method.
func netRate(prev, cur netPoint) (in, out float64, ok bool) {
	if prev.t <= 0 || cur.t <= prev.t || cur.method != prev.method || cur.in < prev.in || cur.out < prev.out {
		return 0, 0, false
	}
	dt := cur.t - prev.t
	return (cur.in - prev.in) / dt, (cur.out - prev.out) / dt, true
}

// NewHost returns a host collector over the nodes returned by nodes.
func NewHost(o HostOptions, sink Sink, nodes func() []*Node) *Host {
	if o.ProcEvery <= 0 {
		o.ProcEvery = 10 * time.Second
	}
	if o.NetEvery <= 0 {
		o.NetEvery = 60 * time.Second
	}
	if o.NTPEvery <= 0 {
		o.NTPEvery = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	h := &Host{o: o, sys: sysstat.New(), sink: sink, nodes: nodes, start: map[int]int64{}, prevCPU: map[string][2]float64{},
		prevNet: map[string]netPoint{}, diskHist: map[string][]diskPoint{}}
	SetProcessAlive(sysstat.Alive)
	return h
}

// ProcStart returns a process's start time, for restart detection.
func (h *Host) ProcStart(pid int) int64 {
	h.mu.Lock()
	t, ok := h.start[pid]
	h.mu.Unlock()
	if ok {
		return t
	}
	m := h.sys.Procs([]int{pid})
	if p, ok := m[pid]; ok {
		h.mu.Lock()
		h.start[pid] = p.StartTime
		h.mu.Unlock()
		return p.StartTime
	}
	return 0
}

// ClockOffset returns the last measured offset in seconds.
func (h *Host) ClockOffset() (float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.offset, h.offKnown
}

// Run collects until ctx is done.
func (h *Host) Run(ctx context.Context) {
	defer h.sys.Close()
	proc := time.NewTicker(h.o.ProcEvery)
	netT := time.NewTicker(h.o.NetEvery)
	slow := time.NewTicker(time.Minute)
	defer proc.Stop()
	defer netT.Stop()
	defer slow.Stop()
	var lastNTP, lastSize time.Time
	h.procPass()
	h.diskPass(true)
	// Two early network passes give a first rate within 20 s.
	early := []*time.Timer{time.AfterFunc(3*time.Second, h.procPass), time.AfterFunc(5*time.Second, h.netPass), time.AfterFunc(20*time.Second, h.netPass)}
	defer func() {
		for _, t := range early {
			t.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-proc.C:
			h.procPass()
		case <-netT.C:
			h.netPass()
		case <-slow.C:
			sizes := time.Since(lastSize) >= 10*time.Minute
			if sizes {
				lastSize = time.Now()
			}
			h.diskPass(sizes)
			if h.o.NTPServer != "" && time.Since(lastNTP) >= h.o.NTPEvery {
				lastNTP = time.Now()
				go h.ntpPass(ctx)
			}
		}
	}
}

func (h *Host) local() []*Node {
	var out []*Node
	for _, n := range h.nodes() {
		if n.o.Source.Local() {
			out = append(out, n)
		}
	}
	return out
}

func method(stat string) model.Labels {
	return model.Labels{{Name: "method", Value: sysstat.Methods()[stat]}}
}

func (h *Host) procPass() {
	nodes := h.local()
	var pids []int
	byPID := map[int]*Node{}
	for _, n := range nodes {
		if pid := n.PID(); pid > 0 {
			pids = append(pids, pid)
			byPID[pid] = n
		}
	}
	now := h.o.Now()
	procs := h.sys.Procs(pids)
	l1, l5, l15, lok := h.sys.Load()
	in, out, ierr := h.sys.Interfaces()
	var unavailable []string
	for _, s := range []string{sysstat.StatDiskIO, sysstat.StatLoad, sysstat.StatHandles, sysstat.StatThreads} {
		if !sysstat.Available(s) {
			unavailable = append(unavailable, s)
		}
	}
	for pid, n := range byPID {
		p, ok := procs[pid]
		if !ok {
			continue
		}
		h.mu.Lock()
		h.start[pid] = p.StartTime
		prev := h.prevCPU[n.o.Name]
		h.prevCPU[n.o.Name] = [2]float64{p.CPUSeconds, float64(now.UnixNano()) / 1e9}
		h.mu.Unlock()
		samples := []model.Sample{
			{Name: "proc_cpu_seconds_total", Labels: method(sysstat.StatCPU), Kind: model.KindCounter, Value: p.CPUSeconds},
			{Name: "proc_resident_bytes", Labels: method(sysstat.StatRSS), Kind: model.KindGauge, Value: p.RSSBytes},
		}
		if p.Threads != sysstat.NA {
			samples = append(samples, model.Sample{Name: "proc_threads", Labels: method(sysstat.StatThreads), Kind: model.KindGauge, Value: p.Threads})
		}
		if p.Handles != sysstat.NA {
			samples = append(samples, model.Sample{Name: "proc_open_handles", Labels: method(sysstat.StatHandles), Kind: model.KindGauge, Value: p.Handles})
		}
		if p.ReadBytes != sysstat.NA {
			samples = append(samples,
				model.Sample{Name: "proc_read_bytes_total", Labels: method(sysstat.StatDiskIO), Kind: model.KindCounter, Value: p.ReadBytes},
				model.Sample{Name: "proc_write_bytes_total", Labels: method(sysstat.StatDiskIO), Kind: model.KindCounter, Value: p.WriteBytes})
		}
		n.batch(model.GroupProc, now, samples)
		cpu := 0.0
		if prev[1] > 0 {
			if dt := float64(now.UnixNano())/1e9 - prev[1]; dt > 0 && p.CPUSeconds >= prev[0] {
				cpu = (p.CPUSeconds - prev[0]) / dt * 100
			}
		}
		n.update(func(s *State) {
			s.CPU, s.RSS = cpu, p.RSSBytes
			s.Threads, s.Handles = p.Threads, p.Handles
			s.Unavailable = unavailable
			if lok {
				s.Load1 = l1
			}
		})
	}
	// Host-wide stats go under every local node, so each node's charts
	// work alone.
	for _, n := range nodes {
		var hs []model.Sample
		if lok {
			hs = append(hs,
				model.Sample{Name: "host_load1", Kind: model.KindGauge, Value: l1},
				model.Sample{Name: "host_load5", Kind: model.KindGauge, Value: l5},
				model.Sample{Name: "host_load15", Kind: model.KindGauge, Value: l15})
		}
		if ierr == nil {
			hs = append(hs,
				model.Sample{Name: "host_net_bytes_total", Labels: model.Labels{{Name: "dir", Value: "in"}, {Name: "method", Value: sysstat.Methods()[sysstat.StatIfBytes]}}, Kind: model.KindCounter, Value: in},
				model.Sample{Name: "host_net_bytes_total", Labels: model.Labels{{Name: "dir", Value: "out"}, {Name: "method", Value: sysstat.Methods()[sysstat.StatIfBytes]}}, Kind: model.KindCounter, Value: out})
		}
		if off, ok := h.ClockOffset(); ok {
			hs = append(hs, model.Sample{Name: "host_clock_offset_seconds", Kind: model.KindGauge, Value: off})
		}
		n.batch(model.GroupHost, now, hs)
	}
}

func (h *Host) netPass() {
	nodes := h.local()
	var pids []int
	byPID := map[int]*Node{}
	for _, n := range nodes {
		if pid := n.PID(); pid > 0 {
			pids = append(pids, pid)
			byPID[pid] = n
		}
	}
	if len(pids) == 0 {
		return
	}
	// A stable order: macOS restarts its long-lived nettop when the PID
	// list changes, and map order changes on every pass.
	sort.Ints(pids)
	now := h.o.Now()
	m := h.sys.NetBytes(pids)
	for pid, n := range byPID {
		v, ok := m[pid]
		if !ok {
			continue
		}
		lbl := func(dir string) model.Labels {
			return model.Labels{{Name: "dir", Value: dir}, {Name: "method", Value: v.Method}}
		}
		n.batch(model.GroupNet, now, []model.Sample{
			{Name: "proc_net_bytes_total", Labels: lbl("in"), Kind: model.KindCounter, Value: v.In},
			{Name: "proc_net_bytes_total", Labels: lbl("out"), Kind: model.KindCounter, Value: v.Out},
		})
		t := float64(now.UnixNano()) / 1e9
		h.mu.Lock()
		cur := netPoint{v.In, v.Out, t, v.Method}
		prev := h.prevNet[n.o.Name]
		h.prevNet[n.o.Name] = cur
		h.mu.Unlock()
		if in, out, ok := netRate(prev, cur); ok {
			n.update(func(s *State) {
				s.NetIn, s.NetOut, s.NetMethod = in, out, v.Method
			})
		} else {
			n.update(func(s *State) { s.NetMethod = v.Method })
		}
	}
}

func (h *Host) diskPass(sizes bool) {
	now := h.o.Now()
	for _, n := range h.local() {
		dir := n.DataDir()
		if dir == "" {
			continue
		}
		u, err := disk.Stat(dir)
		if err != nil {
			continue
		}
		samples := []model.Sample{
			{Name: "host_disk_free_bytes", Kind: model.KindGauge, Value: float64(u.Free)},
			{Name: "host_disk_total_bytes", Kind: model.KindGauge, Value: float64(u.Total)},
		}
		var total float64
		if sizes {
			ents, _ := os.ReadDir(dir)
			var files float64
			for _, e := range ents {
				if e.IsDir() {
					sz := float64(fsx.DirSize(filepath.Join(dir, e.Name())))
					total += sz
					samples = append(samples, model.Sample{Name: "host_datadir_bytes", Labels: model.Labels{{Name: "sub", Value: e.Name()}}, Kind: model.KindGauge, Value: sz})
				} else if fi, err := e.Info(); err == nil {
					files += float64(fi.Size())
				}
			}
			total += files
			samples = append(samples, model.Sample{Name: "host_datadir_bytes", Labels: model.Labels{{Name: "sub", Value: "(files)"}}, Kind: model.KindGauge, Value: files})
		}
		n.batch(model.GroupHost, now, samples)
		days := h.daysUntilFull(dir, now, float64(u.Total-u.Free), float64(u.Total))
		n.update(func(s *State) {
			s.DiskFree, s.DiskTotal = float64(u.Free), float64(u.Total)
			if sizes {
				s.DataSize = total
			}
			s.DiskDays = days
		})
	}
}

// daysUntilFull fits a line to the last 7 days of used space.
func (h *Host) daysUntilFull(key string, now time.Time, used, total float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := float64(now.Unix())
	hist := append(h.diskHist[key], diskPoint{t, used})
	cut := t - 7*86400
	for len(hist) > 0 && hist[0].t < cut {
		hist = hist[1:]
	}
	if len(hist) > 2000 {
		hist = hist[len(hist)-2000:]
	}
	h.diskHist[key] = hist
	if len(hist) < 3 || hist[len(hist)-1].t-hist[0].t < 3600 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for _, p := range hist {
		x := p.t - hist[0].t
		sx += x
		sy += p.used
		sxx += x * x
		sxy += x * p.used
	}
	n := float64(len(hist))
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	slope := (n*sxy - sx*sy) / den // bytes per second
	if slope <= 0 {
		return 0
	}
	return (total - used) / slope / 86400
}

func (h *Host) ntpPass(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	off, _, err := ClockOffset(cctx, h.o.NTPServer)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.offset, h.offKnown = off.Seconds(), true
	h.mu.Unlock()
	for _, n := range h.local() {
		n.update(func(s *State) { s.ClockOff, s.ClockKnown = off.Seconds(), true })
	}
}

// SetClockOffset applies a known offset to a node, for remote nodes whose
// agent measured it.
func (n *Node) SetClockOffset(off float64) {
	n.update(func(s *State) { s.ClockOff, s.ClockKnown = off, true })
}
