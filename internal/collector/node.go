// Package collector polls algod nodes: /metrics, the round watcher, REST
// status, peers, participation, process and host stats, and the log tailer.
// Each node has its own goroutines. Collectors send batches to a Sink, the
// storage writer in the daemon.
package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/fsx"
	"algod.top/internal/model"
	"algod.top/internal/promparse"
)

// Sink receives everything a collector produces.
type Sink interface {
	Batch(*model.Batch)
	Event(model.Event)
	Session(model.Session)
	Usage(node string, us []model.PeerUsage)
	LogLine(LogLine)
	Annotate(model.Annotation)
}

// LogLine is a raw log line kept for drill-down.
type LogLine struct {
	Time     int64
	Node     string
	Level    string
	Template string
	Line     string
}

// Options configure a node collector.
type Options struct {
	Name          string
	Source        Source
	Sink          Sink
	Scrape        time.Duration // 10 s
	PeersEvery    time.Duration // 30 s
	PartEvery     time.Duration // 5 min
	AccountsEvery time.Duration // 5 min
	SupplyEvery   time.Duration // 10 min
	Accounts      []string
	StateDir      string // per-node state file, empty for live-only mode
	Logs          LogOptions
	Register      func(name, genesisID, genesisHash string)
	ProcStart     func(pid int) int64
	LastRound     func() uint64 // newest stored round, for header backfill
	Logf          func(string, ...any)
	Now           func() time.Time
}

// Node collects one algod.
type Node struct {
	o Options

	mu         sync.RWMutex
	st         State
	client     *algod.Client
	clientAddr string
	files      *Files

	effective    *algod.Config // config at the last restart
	pendingHash  string
	pendingReset bool
	knownPID     int
	versionKey   string
	prevCtr      map[string]float64
	prevCtrAt    time.Time
	rounds       ring
	lastArrival  time.Time
	lastExact    bool // lastArrival came from a long-poll
	lastRound    uint64
	lastTC       uint64
	failures     int
	sess         *sessionTracker
	parser       *promparse.Parser
	filesErr     string          // the last error reading the data directory
	metricsBuf   bytes.Buffer    // reused for every scrape
	names        map[string]bool // metric names in the last scrape
	oldNames     map[string]bool // names before a version change
	tail         *tailer
	persist      persisted
	wake         chan struct{}
}

// persisted is the per-node state file.
type persisted struct {
	EffectiveConfig json.RawMessage `json:"effective_config,omitempty"`
	EffectiveHash   string          `json:"effective_hash,omitempty"`
	PendingHash     string          `json:"pending_hash,omitempty"`
	PID             int             `json:"pid,omitempty"`
	Version         string          `json:"version,omitempty"`
	GenesisHash     string          `json:"genesis_hash,omitempty"`
	Sessions        []model.Session `json:"open_sessions,omitempty"`
	Tail            *tailState      `json:"tail,omitempty"`
	LastRound       uint64          `json:"last_round,omitempty"`
}

// NewNode returns a collector. Run starts it.
func NewNode(o Options) *Node {
	if o.Scrape <= 0 {
		o.Scrape = 10 * time.Second
	}
	if o.PeersEvery <= 0 {
		o.PeersEvery = 30 * time.Second
	}
	if o.PartEvery <= 0 {
		o.PartEvery = 5 * time.Minute
	}
	if o.AccountsEvery <= 0 {
		o.AccountsEvery = 5 * time.Minute
	}
	if o.SupplyEvery <= 0 {
		o.SupplyEvery = 10 * time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	n := &Node{o: o, rounds: ring{n: 200}, wake: make(chan struct{}, 1)}
	n.st.Name = o.Name
	n.st.Kind = o.Source.Kind()
	if ls, ok := o.Source.(*LocalSource); ok {
		n.st.DataDir = ls.Dir
	}
	if rs, ok := o.Source.(*RESTSource); ok {
		n.st.URL = rs.URL
	}
	// Persisted open sessions must be loaded before the tracker reads them.
	n.loadPersisted()
	n.sess = newSessionTracker(n)
	if o.Source.Local() && !o.Logs.Disable {
		n.tail = newTailer(n, n.persist.Tail)
	}
	return n
}

// Name returns the node name.
func (n *Node) Name() string { return n.o.Name }

// State returns a copy of the live state.
func (n *Node) State() State {
	n.mu.RLock()
	defer n.mu.RUnlock()
	s := n.st
	s.PeerList = append([]PeerInfo(nil), n.st.PeerList...)
	return s
}

func (n *Node) update(fn func(s *State)) {
	n.mu.Lock()
	fn(&n.st)
	n.mu.Unlock()
}

// PID returns the current algod PID for local nodes, or 0.
func (n *Node) PID() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !n.st.Up || !n.o.Source.Local() {
		return 0
	}
	return n.st.PID
}

// DataDir returns the local data directory, or "".
func (n *Node) DataDir() string { return n.st.DataDir }

func (n *Node) statePath() string {
	if n.o.StateDir == "" {
		return ""
	}
	return filepath.Join(n.o.StateDir, "node-"+strings.ReplaceAll(n.o.Name, "/", "~")+".json")
}

func (n *Node) loadPersisted() {
	p := n.statePath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	if json.Unmarshal(b, &n.persist) != nil {
		return
	}
	// The stored JSON is re-indented when the state file is written, so the
	// hash is kept explicitly rather than recomputed from it.
	if len(n.persist.EffectiveConfig) > 0 {
		if c, err := algod.ParseConfig(n.persist.EffectiveConfig); err == nil {
			if n.persist.EffectiveHash != "" {
				c.Hash = n.persist.EffectiveHash
			}
			n.effective = c
		}
	} else if h := n.persist.EffectiveHash; h != "" {
		n.effective = &algod.Config{Values: map[string]any{}, Hash: h}
	}
	n.pendingHash = n.persist.PendingHash
	n.knownPID = n.persist.PID
	n.versionKey = n.persist.Version
	n.lastRound = n.persist.LastRound
	n.st.GenesisHash = n.persist.GenesisHash
}

func (n *Node) savePersisted() {
	p := n.statePath()
	if p == "" {
		return
	}
	// Take the session and tail state before the node lock: both of those
	// hold their own lock while they update the node, so taking them under
	// n.mu would invert the lock order and deadlock.
	sessions := n.sess.open()
	var tail *tailState
	if n.tail != nil {
		ts := n.tail.state()
		tail = &ts
	}
	n.mu.Lock()
	if n.effective != nil {
		n.persist.EffectiveConfig = json.RawMessage(nil)
		if len(n.effective.Raw) > 0 && json.Valid(n.effective.Raw) {
			n.persist.EffectiveConfig = append(json.RawMessage(nil), n.effective.Raw...)
		}
		n.persist.EffectiveHash = n.effective.Hash
	}
	n.persist.PendingHash = n.pendingHash
	n.persist.PID = n.knownPID
	n.persist.Version = n.versionKey
	n.persist.GenesisHash = n.st.GenesisHash
	n.persist.LastRound = n.lastRound
	n.persist.Sessions = sessions
	if tail != nil {
		n.persist.Tail = tail
	}
	b, err := json.MarshalIndent(n.persist, "", " ")
	n.mu.Unlock()
	if err == nil {
		fsx.WriteAtomic(p, b, 0o600)
	}
}

func (n *Node) event(kind, sev, msg string, attrs map[string]any) {
	e := model.Event{Time: n.o.Now().UnixNano(), Node: n.o.Name, Kind: kind, Severity: sev, Msg: msg, Attrs: attrs}
	n.o.Sink.Event(e)
	if kind != model.EvConnect && kind != model.EvDisconnect {
		n.update(func(s *State) { s.LastEvent = &e })
	}
}

func (n *Node) batch(g model.Group, t time.Time, samples []model.Sample) {
	if len(samples) == 0 {
		return
	}
	n.o.Sink.Batch(&model.Batch{Node: n.o.Name, Group: g, Time: t.UnixNano(), Samples: samples})
}

// Run collects until ctx is done.
func (n *Node) Run(ctx context.Context) {
	defer n.o.Source.Close()
	n.refreshFiles(ctx, true)
	var wg sync.WaitGroup
	loops := []func(context.Context){n.watchLoop, n.scrapeLoop, n.roundLoop, n.peersLoop, n.slowLoop}
	if n.tail != nil {
		loops = append(loops, n.tail.run)
	}
	for _, f := range loops {
		wg.Add(1)
		go func(f func(context.Context)) {
			defer wg.Done()
			f(ctx)
		}(f)
	}
	save := time.NewTicker(30 * time.Second)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			n.sess.closeAll()
			n.savePersisted()
			return
		case <-save.C:
			n.savePersisted()
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// watchLoop re-reads the data directory: algod.pid, algod.net and
// config.json. Local nodes check every 2 s. SSH nodes every 10 minutes, or
// sooner after a connection failure.
func (n *Node) watchLoop(ctx context.Context) {
	// Local nodes check every 5 s; each scrape also checks algod.pid, so
	// a restart is seen at the next scrape at the latest.
	every := 5 * time.Second
	switch n.o.Source.Kind() {
	case "ssh":
		every = 10 * time.Minute
	case "rest":
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		case <-time.After(every):
		}
		n.refreshFiles(ctx, false)
	}
}

// poke asks the watch loop to re-read files soon, after a failure.
func (n *Node) poke() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *Node) refreshFiles(ctx context.Context, first bool) {
	f, err := n.o.Source.Files(ctx)
	if err != nil {
		n.update(func(s *State) { s.LastError = err.Error() })
		n.mu.Lock()
		n.filesErr = err.Error()
		n.mu.Unlock()
		return
	}
	now := n.o.Now()
	n.mu.Lock()
	n.files = f
	n.filesErr = ""
	if f.Genesis.Network != "" {
		n.st.Network = f.Genesis.Network
		n.st.GenesisID = f.Genesis.GenesisID()
	}
	local := n.o.Source.Local()
	n.mu.Unlock()

	// Client: build or rebuild when the address or tokens change.
	addr := f.Net
	if addr != "" {
		n.mu.Lock()
		if n.client == nil || n.clientAddr != addr {
			dial, poll := n.o.Source.Dialers(addr)
			base := addr
			if n.o.Source.Kind() == "ssh" {
				base = "http://algod" // the dialer ignores the host
			}
			c, err := algod.NewClient(base, algod.Options{Token: f.Token, AdminToken: f.AdminToken, Dial: dial, PollDial: poll})
			if err == nil {
				if n.client != nil {
					n.client.CloseIdle()
				}
				n.client, n.clientAddr = c, addr
				n.st.HasAdmin = f.AdminToken != ""
			}
		} else {
			n.client.SetTokens(f.Token, f.AdminToken)
			n.st.HasAdmin = f.AdminToken != ""
		}
		n.mu.Unlock()
	}

	// Process presence and restarts, for nodes with a data directory.
	if local || n.o.Source.Kind() == "ssh" {
		n.checkPID(f.PID, f.Start, now, first)
	}
	// Config changes.
	if f.Config != nil {
		n.checkConfig(f.Config, now, first)
	}
}

// checkPID handles algod.pid appearing, disappearing or changing.
// fileStart is the start time a remote source reported with the files.
func (n *Node) checkPID(pid int, fileStart int64, now time.Time, first bool) {
	local := n.o.Source.Local()
	alive := pid > 0
	if local && pid > 0 {
		if a, ok := isAlive(pid); ok {
			alive = a
		}
	}
	n.mu.Lock()
	prevPID := n.knownPID
	n.st.PID = pid
	var start int64
	if alive && n.o.ProcStart != nil && local {
		start = n.o.ProcStart(pid)
	} else if alive && !local {
		start = fileStart
	}
	n.mu.Unlock()
	if !alive {
		if local {
			n.markDown(now, "process not running, no live algod.pid")
		}
		return
	}
	restarted := prevPID != 0 && pid != prevPID
	n.mu.Lock()
	// A remote start time comes from ps in whole seconds against the
	// hub's clock, so it moves a little between reads: only a local one
	// detects a restart under the same pid.
	if local && start != 0 && n.st.StartTime != 0 && start != n.st.StartTime && pid == prevPID {
		restarted = true
	}
	if start != 0 {
		n.st.StartTime = start
	}
	n.knownPID = pid
	n.mu.Unlock()
	// A node that was down and is back under the same pid did not restart:
	// the pid or the start time says when it did. Counting "was down" as
	// a restart logged one on every check until the node was marked up,
	// and on every network blip of a remote node.
	if restarted {
		n.onRestart(now, fmt.Sprintf("algod restarted (pid %d -> %d)", prevPID, pid))
	} else if prevPID == 0 && !first {
		n.onRestart(now, fmt.Sprintf("algod started (pid %d)", pid))
	}
}

// processAlive is the liveness check the host collector installs,
// sysstat.Alive. Every daemon in a process installs it while others run,
// as the hub and agent of one test do, so it is atomic.
var processAlive atomic.Pointer[func(pid int) bool]

// SetProcessAlive installs the liveness check.
func SetProcessAlive(f func(pid int) bool) { processAlive.Store(&f) }

// isAlive reports whether pid is running. ok is false when no check is
// installed.
func isAlive(pid int) (alive, ok bool) {
	f := processAlive.Load()
	if f == nil || *f == nil {
		return false, false
	}
	return (*f)(pid), true
}

func (n *Node) markDown(now time.Time, why string) {
	n.mu.Lock()
	wasUp := n.st.Up
	n.st.Up = false
	n.st.Healthy = false
	n.st.Ready = false
	n.st.LastError = why
	n.st.StatusErr, n.st.StatusErrAt = "", 0
	if n.st.DownSince == 0 {
		n.st.DownSince = now.UnixNano()
	}
	n.mu.Unlock()
	if wasUp {
		n.event(model.EvNodeDown, model.SevCritical, "algod is down: "+why, nil)
		n.sess.closeAll()
	}
}

func (n *Node) markUp(now time.Time) {
	n.mu.Lock()
	wasUp := n.st.Up
	downSince := n.st.DownSince
	n.st.Up = true
	n.st.DownSince = 0
	n.st.LastError = ""
	if !wasUp {
		n.st.UpSince = now.UnixNano()
	}
	n.mu.Unlock()
	if !wasUp && downSince != 0 {
		n.event(model.EvNodeUp, model.SevInfo, "algod is up", map[string]any{"down_since": downSince})
	}
}

// onRestart marks a counter reset, records the restart and makes any
// pending config effective.
func (n *Node) onRestart(now time.Time, msg string) {
	n.mu.Lock()
	n.pendingReset = true
	n.prevCtr = nil
	n.lastArrival = time.Time{}
	f := n.files
	n.mu.Unlock()
	n.event(model.EvRestart, model.SevWarn, msg, nil)
	if f != nil && f.Config != nil {
		n.makeEffective(f.Config, now)
	}
	// Versions are re-read after a restart.
	n.mu.Lock()
	n.versionKey = n.versionKey + "?"
	n.mu.Unlock()
}

// checkConfig records "config pending" when config.json changes on disk.
// algod reads it only at startup.
func (n *Node) checkConfig(c *algod.Config, now time.Time, first bool) {
	n.mu.Lock()
	eff := n.effective
	pending := n.pendingHash
	start := n.st.StartTime
	n.mu.Unlock()
	if eff == nil {
		// First sight: if config.json changed after algod started, the
		// running config is unknown and the file is pending.
		if first && n.o.Source.Local() && start != 0 {
			if fi, err := os.Stat(filepath.Join(n.st.DataDir, "config.json")); err == nil && fi.ModTime().UnixNano() > start+int64(time.Minute) {
				// A file written within a minute of the start is the one algod
				// loaded; editing config.json right after a start is rare.
				n.mu.Lock()
				n.effective = &algod.Config{Values: map[string]any{}, Hash: "unknown"}
				n.pendingHash = c.Hash
				n.mu.Unlock()
				n.event(model.EvConfigPending, model.SevInfo, "config.json changed after algod started; it takes effect at the next restart", map[string]any{"hash": c.Hash})
				return
			}
		}
		n.mu.Lock()
		n.effective = c
		n.st.ConfigHash = c.Hash
		n.mu.Unlock()
		return
	}
	if c.Hash == eff.Hash {
		if pending != "" {
			n.mu.Lock()
			n.pendingHash = ""
			n.mu.Unlock()
		}
		return
	}
	if c.Hash == pending {
		return
	}
	diff := algod.Diff(eff, c)
	n.mu.Lock()
	n.pendingHash = c.Hash
	n.mu.Unlock()
	if len(diff) == 0 && eff.Hash != "unknown" {
		// Formatting only: the running config is the same.
		n.mu.Lock()
		n.effective = c
		n.pendingHash = ""
		n.mu.Unlock()
		return
	}
	n.event(model.EvConfigPending, model.SevInfo, "config.json changed: "+diffSummary(diff)+" (pending until restart)",
		map[string]any{"diff": diff, "hash": c.Hash})
}

func (n *Node) makeEffective(c *algod.Config, now time.Time) {
	n.mu.Lock()
	eff := n.effective
	n.effective = c
	n.pendingHash = ""
	n.st.ConfigHash = c.Hash
	n.mu.Unlock()
	if eff == nil || eff.Hash == c.Hash {
		return
	}
	diff := algod.Diff(eff, c)
	if eff.Hash == "unknown" {
		diff = nil
	}
	msg := "config effective"
	if len(diff) > 0 {
		msg = diffSummary(diff)
	}
	e := model.Event{Time: now.UnixNano(), Node: n.o.Name, Kind: model.EvConfigEffective, Severity: model.SevInfo, Msg: msg,
		Attrs: map[string]any{"diff": diff, "hash": c.Hash}}
	n.o.Sink.Event(e)
	n.update(func(s *State) { s.ConfigChanged = &e; s.LastEvent = &e })
	if len(diff) > 0 {
		n.o.Sink.Annotate(model.Annotation{Node: n.o.Name, Start: now.UnixNano(), Text: msg, Auto: true})
	}
}

func diffSummary(d []algod.ConfigDiff) string {
	if len(d) == 0 {
		return "no key changes"
	}
	parts := make([]string, 0, len(d))
	for _, x := range d {
		parts = append(parts, x.String())
	}
	s := strings.Join(parts, ", ")
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	return s
}

func (n *Node) getClient() *algod.Client {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.client
}

// scrapeLoop fetches /metrics, /health, /ready, /v2/status and the pool
// size every scrape interval.
func (n *Node) scrapeLoop(ctx context.Context) {
	t := time.NewTicker(n.o.Scrape)
	defer t.Stop()
	n.scrape(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.scrape(ctx)
		}
	}
}

func (n *Node) scrape(ctx context.Context) {
	c := n.getClient()
	now := n.o.Now()
	if c == nil {
		if !n.o.Source.Local() {
			n.mu.RLock()
			why := n.filesErr
			n.mu.RUnlock()
			if why == "" {
				why = "no REST address: algod.net is missing or empty"
			} else {
				why = "cannot read the data directory: " + why
			}
			n.markDown(now, why)
		}
		return
	}
	livePID := 0 // a local algod whose process is known to be running
	if ls, ok := n.o.Source.(*LocalSource); ok {
		n.mu.RLock()
		pid := n.st.PID
		n.mu.RUnlock()
		// Check the PID right before scraping, so the first scrape after
		// a restart carries the reset marker.
		cur := ls.PIDOnly()
		if cur != pid || cur == 0 {
			n.refreshFiles(ctx, false)
			c = n.getClient()
		}
		// A stale algod.pid naming a dead process means algod is down,
		// even if something answers on the old REST address.
		a, ok := isAlive(cur)
		if cur == 0 || (ok && !a) || c == nil {
			return
		}
		if ok {
			livePID = cur
		}
	}
	rctx, cancel := context.WithTimeout(ctx, n.o.Scrape)
	defer cancel()
	t0 := time.Now()
	err := c.MetricsInto(rctx, &n.metricsBuf)
	body := n.metricsBuf.Bytes()
	dur := time.Since(t0)
	var rest []model.Sample
	if err != nil {
		n.failures++
		n.update(func(s *State) { s.MissedScrapes++; s.LastError = err.Error(); s.Reachable = false })
		rest = append(rest, model.Sample{Name: "algodtop_scrape_errors_total", Kind: model.KindCounter, Value: float64(n.st.MissedScrapes)})
		if n.failures >= 2 {
			why := "REST unreachable: " + err.Error()
			if livePID != 0 {
				// A hung algod needs a kill, a crashed one a start.
				why += fmt.Sprintf("; algod pid %d is still running", livePID)
			}
			n.markDown(now, why)
			n.poke()
		}
		rest = append(rest, model.Sample{Name: "algodtop_up", Kind: model.KindGauge, Value: 0})
		n.batch(model.GroupREST, now, rest)
		return
	}
	n.failures = 0
	n.markUp(now)
	n.update(func(s *State) { s.Reachable = true })
	if n.parser == nil {
		n.parser = promparse.NewParser()
	}
	res, perr := n.parser.Parse(bytes.NewReader(body))
	if perr == nil {
		n.handleMetrics(now, res)
	}
	healthy := c.Health(rctx) == nil
	ready := c.Ready(rctx) == nil
	st, serr := c.Status(rctx)
	pool, perr2 := c.PoolSize(rctx)
	rest = append(rest,
		model.Sample{Name: "algodtop_up", Kind: model.KindGauge, Value: 1},
		model.Sample{Name: "algodtop_health_ok", Kind: model.KindGauge, Value: b2f(healthy)},
		model.Sample{Name: "algodtop_ready", Kind: model.KindGauge, Value: b2f(ready)},
		model.Sample{Name: "algodtop_scrape_duration_seconds", Kind: model.KindGauge, Value: dur.Seconds()},
		model.Sample{Name: "algodtop_scrape_bytes", Kind: model.KindGauge, Value: float64(len(body))},
	)
	if serr == nil {
		rest = append(rest,
			model.Sample{Name: "algodtop_sync_lag_seconds", Kind: model.KindGauge, Value: float64(st.TimeSinceLastRound) / 1e9},
			model.Sample{Name: "algodtop_catchup", Kind: model.KindGauge, Value: b2f(st.InCatchup())},
			model.Sample{Name: "algodtop_last_round", Kind: model.KindGauge, Value: float64(st.LastRound)},
		)
		n.noteCatchup(st.InCatchup())
	}
	if perr2 == nil {
		rest = append(rest, model.Sample{Name: "algodtop_txpool_size", Kind: model.KindGauge, Value: float64(pool)})
	}
	var why string
	if serr != nil {
		why = statusProblem(serr, c.HasToken(), n.o.Source.Kind())
	}
	n.update(func(s *State) {
		s.Healthy, s.Ready = healthy, ready
		if serr == nil {
			s.StatusLag = float64(st.TimeSinceLastRound) / 1e9
			if st.LastRound > s.Round {
				s.Round = st.LastRound
			}
			s.StatusErr, s.StatusErrAt = "", 0
		} else {
			s.StatusErr = why
			if s.StatusErrAt == 0 {
				s.StatusErrAt = now.UnixNano()
			}
		}
		if perr2 == nil {
			s.PoolSize = float64(pool)
		}
	})
	n.batch(model.GroupREST, now, rest)
	n.ensureVersion(rctx, c)
}

// statusProblem words a /v2/status failure for the health reason. A
// missing or rejected token is the usual cause, and /metrics and /health
// answer without one, so the node otherwise looks up and fine.
func statusProblem(err error, hasToken bool, kind string) string {
	var he *algod.HTTPError
	if !errors.As(err, &he) || (he.Status != http.StatusUnauthorized && he.Status != http.StatusForbidden) {
		return err.Error()
	}
	switch {
	case hasToken:
		return fmt.Sprintf("algod rejects the API token (HTTP %d)", he.Status)
	case kind == "local" || kind == "ssh":
		return fmt.Sprintf("algod.token is missing or unreadable (HTTP %d)", he.Status)
	}
	return fmt.Sprintf("no API token is set (HTTP %d)", he.Status)
}

func (n *Node) noteCatchup(on bool) {
	n.mu.Lock()
	was := n.st.Catchup
	n.st.Catchup = on
	n.mu.Unlock()
	if on && !was {
		n.event(model.EvCatchupStart, model.SevWarn, "catchup started", nil)
	} else if !on && was {
		n.event(model.EvCatchupEnd, model.SevInfo, "catchup finished", nil)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// handleMetrics stores a scrape and updates live rates. A REST-only node
// has no PID, so a drop in most of its counters in one scrape marks the
// restart.
func (n *Node) handleMetrics(now time.Time, res *promparse.Result) {
	samples := make([]model.Sample, 0, len(res.Series))
	cur := make(map[string]float64, len(res.Series))
	for _, s := range res.Series {
		samples = append(samples, model.Sample{Name: s.Name, Labels: s.Labels, Kind: s.Kind, Value: s.Value, LabelKey: s.Key})
		if s.Kind.IsCumulative() {
			full := s.Full
			if full == "" {
				full = s.Name + "{" + s.Labels.String() + "}"
			}
			cur[full] = s.Value
		}
	}
	n.mu.Lock()
	prev, prevAt := n.prevCtr, n.prevCtrAt
	reset := n.pendingReset
	n.pendingReset = false
	pidKnown := n.st.PID != 0
	n.mu.Unlock()
	var dropped []string
	if prev != nil && !reset {
		both, down := 0, 0
		for k, v := range cur {
			if p, ok := prev[k]; ok && p > 0 {
				both++
				if v < p {
					down++
					dropped = append(dropped, k)
				}
			}
		}
		if !pidKnown && both >= 10 && down*2 > both {
			reset = true
			n.event(model.EvRestart, model.SevWarn, fmt.Sprintf("algod restarted (%d of %d counters reset)", down, both), nil)
			n.mu.Lock()
			n.lastArrival = time.Time{}
			n.versionKey += "?"
			f := n.files
			n.mu.Unlock()
			if f != nil && f.Config != nil {
				n.makeEffective(f.Config, now)
			}
			dropped = nil
		}
	}
	if len(dropped) > 0 {
		if len(dropped) > 20 {
			dropped = dropped[:20]
		}
		n.event(model.EvCounterReset, model.SevInfo, fmt.Sprintf("%d counters went down without a restart", len(dropped)), map[string]any{"series": dropped})
	}
	n.o.Sink.Batch(&model.Batch{Node: n.o.Name, Group: model.GroupMetrics, Time: now.UnixNano(), Samples: samples, Reset: reset})
	// Live app-level bandwidth from algod's own counters.
	if prev != nil && !reset {
		dt := now.Sub(prevAt).Seconds()
		rate := func(k string) float64 {
			if dt <= 0 {
				return 0
			}
			d := cur[k] - prev[k]
			if d < 0 {
				return 0
			}
			return d / dt
		}
		n.update(func(s *State) {
			s.WSIn = rate("algod_network_received_bytes_total{}")
			s.WSOut = rate("algod_network_sent_bytes_total{}")
			s.P2PIn = rate("algod_network_p2p_received_bytes_total{}")
			s.P2POut = rate("algod_network_p2p_sent_bytes_total{}")
		})
	}
	names := make(map[string]bool, 512)
	for _, s := range res.Series {
		names[s.Name] = true
	}
	n.mu.Lock()
	n.prevCtr, n.prevCtrAt = cur, now
	old := n.oldNames
	n.oldNames = nil
	n.names = names
	n.mu.Unlock()
	if old != nil {
		var added, removed []string
		for k := range names {
			if !old[k] {
				added = append(added, k)
			}
		}
		for k := range old {
			if !names[k] {
				removed = append(removed, k)
			}
		}
		if len(added)+len(removed) > 0 {
			sort.Strings(added)
			sort.Strings(removed)
			n.event("metric-names", model.SevInfo, fmt.Sprintf("after the upgrade: %d metric names added, %d removed", len(added), len(removed)),
				map[string]any{"added": added, "removed": removed})
		}
	}
}

// ensureVersion reads /versions at start and after a restart, and records
// a version change with an annotation.
func (n *Node) ensureVersion(ctx context.Context, c *algod.Client) {
	n.mu.RLock()
	key := n.versionKey
	have := n.st.Version != ""
	n.mu.RUnlock()
	if have && !strings.HasSuffix(key, "?") {
		return
	}
	v, err := c.Versions(ctx)
	if err != nil {
		return
	}
	ver := v.Version()
	newKey := ver + " " + v.Build.Branch + " " + v.Build.CommitHash
	prev := strings.TrimRight(key, "?")
	n.mu.Lock()
	n.st.Version = ver
	n.st.Branch = v.Build.Branch
	n.st.Channel = v.Build.Channel
	n.st.GenesisID = v.GenesisID
	oldHash := n.st.GenesisHash
	n.st.GenesisHash = v.GenesisHash
	if n.st.Network == "" {
		n.st.Network = strings.TrimSuffix(strings.SplitN(v.GenesisID, "-v", 2)[0], "-")
	}
	n.versionKey = newKey
	n.mu.Unlock()
	if n.o.Register != nil && (oldHash != v.GenesisHash || !have) {
		n.o.Register(n.o.Name, v.GenesisID, v.GenesisHash)
	}
	if prev != "" && prev != newKey {
		// The next scrape reports metric names added or removed by the
		// upgrade, so renames show up rather than silently breaking charts.
		n.mu.Lock()
		n.oldNames = n.names
		n.mu.Unlock()
		msg := fmt.Sprintf("version %s -> %s", strings.SplitN(prev, " ", 2)[0], ver)
		if strings.SplitN(prev, " ", 2)[0] == ver {
			msg = fmt.Sprintf("build changed: %s -> %s", prev, newKey)
		}
		n.event(model.EvVersion, model.SevInfo, msg, map[string]any{"from": prev, "to": newKey})
		n.o.Sink.Annotate(model.Annotation{Node: n.o.Name, Start: n.o.Now().UnixNano(), Text: msg, Auto: true})
	}
}

// roundLoop long-polls wait-for-block-after. It returns as soon as the
// next round lands, which gives the exact local arrival time per round.
// Block headers are fetched by a separate worker, so the long-poll goes
// out again at once and a slow header fetch never skews arrival times.
func (n *Node) roundLoop(ctx context.Context) {
	backoff := time.Second
	backfilled := false
	// follow is true when the previous answer came just before this call.
	// Only then was the long-poll waiting on the node's current round. The
	// first call after a start, a pause or an error asks about a round that
	// may be long past, and its answer comes back at once.
	follow := false
	jobs := make(chan headerJob, 64)
	go n.headerWorker(ctx, jobs)
	for ctx.Err() == nil {
		c := n.getClient()
		if c == nil || !n.State().Up {
			follow = false
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		n.mu.RLock()
		last := n.lastRound
		n.mu.RUnlock()
		var st *algod.Status
		var err error
		if last == 0 {
			st, err = c.Status(ctx)
		} else {
			st, err = c.WaitForBlockAfter(ctx, last)
		}
		if err != nil {
			follow = false
			if ctx.Err() != nil {
				return
			}
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		now := n.o.Now()
		if !backfilled {
			backfilled = true
			go n.backfillHeaders(ctx, c, st.LastRound)
		}
		// Only a long-poll that followed the previous answer is an arrival
		// time. A plain status read lands partway through a round, and so
		// does a long-poll on a stale round, such as the one restored from
		// the state file at startup.
		exact := follow && last != 0
		follow = true
		if n.onRound(st, now, exact) {
			select {
			case jobs <- headerJob{round: st.LastRound, at: now}:
			default: // the worker is behind; the next round catches up
			}
		}
	}
}

type headerJob struct {
	round uint64
	at    time.Time
}

// onRound records a round's arrival. It returns true for a new round.
func (n *Node) onRound(st *algod.Status, now time.Time, exact bool) bool {
	n.mu.Lock()
	prevRound := n.lastRound
	prevArr := n.lastArrival
	prevExact := n.lastExact
	n.lastExact = exact
	if st.LastRound <= prevRound && prevRound != 0 && !prevArr.IsZero() {
		n.mu.Unlock()
		return false
	}
	n.lastRound = st.LastRound
	n.lastArrival = now
	var gap float64
	consecutive := prevRound != 0 && st.LastRound == prevRound+1 && !prevArr.IsZero() && prevExact && exact
	if consecutive {
		gap = now.Sub(prevArr).Seconds()
		n.rounds.add(gap)
	}
	n.st.Round = st.LastRound
	n.st.RoundAt = now.UnixNano()
	n.st.RoundTime = n.rounds.mean()
	n.st.RoundP99 = n.rounds.quantile(0.99)
	n.mu.Unlock()
	samples := []model.Sample{{Name: "algodtop_round", Kind: model.KindGauge, Value: float64(st.LastRound)}}
	if consecutive {
		samples = append(samples, model.Sample{Name: "algodtop_round_time_seconds", Kind: model.KindGauge, Value: gap})
	}
	n.batch(model.GroupRound, now, samples)
	return true
}

// headerWorker fetches the header of each new round, in order, for the
// transaction counter. When rounds were skipped, transactions per block is
// the average over them.
func (n *Node) headerWorker(ctx context.Context, jobs <-chan headerJob) {
	var lastRound, lastTC uint64
	for {
		var j headerJob
		select {
		case <-ctx.Done():
			return
		case j = <-jobs:
		}
		c := n.getClient()
		if c == nil {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		h, err := c.BlockHeader(hctx, j.round)
		cancel()
		if err != nil {
			continue
		}
		samples := []model.Sample{{Name: "algodtop_txn_counter", Kind: model.KindCounter, Value: float64(h.TxnCounter)},
			{Name: "algodtop_block_timestamp", Kind: model.KindGauge, Value: float64(h.Timestamp)}}
		if lastRound != 0 && j.round > lastRound && h.TxnCounter >= lastTC && j.round-lastRound <= 100 {
			tpb := float64(h.TxnCounter-lastTC) / float64(j.round-lastRound)
			samples = append(samples, model.Sample{Name: "algodtop_block_txns", Kind: model.KindGauge, Value: tpb})
			n.update(func(s *State) { s.TxnsPerBlk = tpb })
		}
		lastRound, lastTC = j.round, h.TxnCounter
		n.batch(model.GroupBlock, j.at, samples)
	}
}

// backfillHeaders fetches block headers missed while the daemon was down,
// at a capped rate, so transactions per block has no hole.
func (n *Node) backfillHeaders(ctx context.Context, c *algod.Client, cur uint64) {
	if n.o.LastRound == nil {
		return
	}
	from := n.o.LastRound()
	if from == 0 || from >= cur {
		return
	}
	if cur-from > 20000 {
		from = cur - 20000
	}
	var prevTC uint64
	t := time.NewTicker(100 * time.Millisecond) // 10 headers per second
	defer t.Stop()
	for r := from; r < cur && ctx.Err() == nil; r++ {
		h, err := c.BlockHeader(ctx, r)
		if err != nil {
			return // the node no longer holds this block
		}
		ts := time.Unix(h.Timestamp, 0)
		samples := []model.Sample{{Name: "algodtop_txn_counter", Kind: model.KindCounter, Value: float64(h.TxnCounter)}}
		if prevTC != 0 && h.TxnCounter >= prevTC {
			samples = append(samples, model.Sample{Name: "algodtop_block_txns", Kind: model.KindGauge, Value: float64(h.TxnCounter - prevTC)})
		}
		prevTC = h.TxnCounter
		n.o.Sink.Batch(&model.Batch{Node: n.o.Name, Group: model.GroupBlock, Time: ts.UnixNano(), Samples: samples})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// slowLoop polls participation, accounts and supply.
func (n *Node) slowLoop(ctx context.Context) {
	next := map[string]time.Time{}
	for {
		c := n.getClient()
		now := n.o.Now()
		if c != nil && n.State().Up {
			if now.After(next["supply"]) {
				next["supply"] = now.Add(n.o.SupplyEvery)
				if s, err := c.Supply(ctx); err == nil {
					n.update(func(st *State) { st.OnlineMoney = float64(s.OnlineMoney) })
					n.batch(model.GroupREST, now, []model.Sample{{Name: "algodtop_online_stake", Kind: model.KindGauge, Value: float64(s.OnlineMoney)}})
				}
			}
			if now.After(next["part"]) && c.HasAdmin() {
				next["part"] = now.Add(n.o.PartEvery)
				n.pollParticipation(ctx, c, now)
			}
			if now.After(next["acct"]) && len(n.o.Accounts) > 0 {
				next["acct"] = now.Add(n.o.AccountsEvery)
				n.pollAccounts(ctx, c, now)
			}
		}
		if !sleepCtx(ctx, 5*time.Second) {
			return
		}
	}
}

func (n *Node) pollParticipation(ctx context.Context, c *algod.Client, now time.Time) {
	keys, err := c.Participation(ctx)
	if err != nil {
		return
	}
	st := n.State()
	rt := st.RoundTime
	if rt <= 0 {
		rt = 2.8
	}
	var out []PartKeyState
	var samples []model.Sample
	for _, k := range keys {
		ks := PartKeyState{ID: k.ID, Address: k.Address, First: k.Key.VoteFirst, Last: k.Key.VoteLast, VoteKey: k.Key.VoteKey,
			LastVote: k.LastVote, LastProposal: k.LastBlockProp}
		ks.RoundsLeft = int64(k.Key.VoteLast) - int64(st.Round)
		ks.DaysLeft = float64(ks.RoundsLeft) * rt / 86400
		out = append(out, ks)
	}
	markKeys(out, st.Accounts, st.Round)
	// One series per account: a renewed account has two keys, and only
	// the one it votes with counts down to a lapse.
	for _, k := range ExpiryKeys(out) {
		samples = append(samples, model.Sample{Name: "algodtop_partkey_rounds_left", Labels: model.Labels{{Name: "account", Value: short(k.Address)}}, Kind: model.KindGauge, Value: float64(k.RoundsLeft)})
	}
	n.update(func(s *State) { s.Keys = out })
	n.batch(model.GroupREST, now, samples)
}

func short(a string) string {
	if len(a) > 8 {
		return a[:8]
	}
	return a
}

func (n *Node) pollAccounts(ctx context.Context, c *algod.Client, now time.Time) {
	st := n.State()
	prev := map[string]AccountState{}
	for _, a := range st.Accounts {
		prev[a.Address] = a
	}
	var out []AccountState
	var samples []model.Sample
	for _, addr := range n.o.Accounts {
		a, err := c.Account(ctx, addr)
		if err != nil {
			if p, ok := prev[addr]; ok {
				out = append(out, p)
			}
			continue
		}
		as := AccountState{Address: addr, Status: a.Status, Balance: a.Amount, IncentiveEligible: a.IncentiveEligible,
			LastProposed: a.LastProposed, LastHeartbeat: a.LastHeartbeat}
		if a.Participation != nil {
			as.VoteKey = a.Participation.VoteKey
		}
		// Offline with the vote key still set is a suspension.
		if a.Status == "Offline" && as.VoteKey != "" {
			as.Status = "Suspended"
		}
		p, had := prev[addr]
		as.WasOnline = had && (p.Status == "Online" || p.WasOnline)
		as.WasEligible = had && (p.IncentiveEligible || p.WasEligible)
		for _, k := range st.Keys {
			if k.Address == addr {
				if k.VoteKey == as.VoteKey {
					as.KeyMatches = true
				}
				as.LastVote = max(as.LastVote, k.LastVote)
			}
		}
		if st.OnlineMoney > 0 && a.Amount > 0 {
			// Expected rounds between votes for a share of online stake:
			// the committee samples stake, so the interval scales inversely.
			as.ExpectedInterval = st.OnlineMoney / float64(a.Amount)
		}
		if as.LastVote > 0 && st.Round > as.LastVote {
			as.RoundsSinceVote = int64(st.Round - as.LastVote)
		}
		out = append(out, as)
		samples = append(samples, model.Sample{Name: "algodtop_account_online", Labels: model.Labels{{Name: "account", Value: short(addr)}}, Kind: model.KindGauge, Value: b2f(as.Status == "Online")})
	}
	n.update(func(s *State) {
		s.Accounts = out
		keys := append([]PartKeyState(nil), s.Keys...)
		markKeys(keys, out, s.Round)
		s.Keys = keys
	})
	n.batch(model.GroupREST, now, samples)
}

// ErrNoClient is returned before the first REST address is known.
var ErrNoClient = errors.New("collector: no REST client yet")
