// Package daemon runs the collectors, the storage writer, health, alerts
// and the API. One goroutine owns all storage writes. Collectors send
// batches to it over a channel, and queries never block it.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"algod.top/internal/alert"
	"algod.top/internal/algod"
	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/disk"
	"algod.top/internal/health"
	"algod.top/internal/model"
	"algod.top/internal/query"
	"algod.top/internal/remote"
	"algod.top/internal/store"
	"algod.top/internal/sysstat"
	"algod.top/internal/version"
)

// Options configure a daemon.
type Options struct {
	ConfigPath string
	Config     *config.Config
	Static     fs.FS // the embedded web UI, nil for agents
	Agent      bool  // agent mode: collectors and a local buffer
	Logger     *log.Logger
	// OnListen is called with the TCP address once the web listener is up.
	OnListen func(addr string)
	// Exe is the binary an upgrade replaces, default the running one.
	Exe string
	// Supervised means a service manager restarts the process when Run
	// returns after an upgrade. Windows cannot exec, so a Windows service
	// restarts that way.
	Supervised bool
}

// Daemon is a running algodtop daemon or agent.
type Daemon struct {
	o     Options
	logf  func(string, ...any)
	cfgMu sync.RWMutex
	cfg   *config.Config
	db    *store.DB
	qe    *query.Engine
	in    chan item
	// inMu guards closing in: senders hold it for reading, and shutdown
	// takes it to close, so a late collector never sends on a closed
	// channel.
	inMu     sync.RWMutex
	inClosed bool
	drops    atomic.Int64
	started  time.Time
	host     *collector.Host
	alerts   *alert.Engine
	alertErr error // rules left out at startup
	sil      *silences
	auth     *api.Auth
	hostNm   string

	nodesMu  sync.RWMutex
	nodes    map[string]*nodeRun
	order    []string
	health   map[string]health.Status
	floor    map[string]string
	creep    map[string]bool
	projOver bool

	subsMu  sync.Mutex
	subs    map[*subscriber]bool
	roundCh chan struct{} // a node confirmed a new round

	remotes *remoteSet
	outbox  *Outbox // agent mode

	runCtx    context.Context
	stop      context.CancelFunc // ends Run
	upgradeMu sync.Mutex
	// restartExe is set by an upgrade: the binary to exec after Run.
	restartExe atomic.Pointer[string]
	// latest is a newer release the update check found.
	latest    atomic.Pointer[string]
	selfPrev  [2]float64
	selfStats api.SelfStats
	selfMu    sync.Mutex
	writeBPS  float64
}

type nodeRun struct {
	cfg    config.Node
	node   *collector.Node
	cancel context.CancelFunc
	done   chan struct{}
}

type item struct {
	batch    *model.Batch
	event    *model.Event
	session  *model.Session
	usage    []model.PeerUsage
	usageFor string
	line     *store.LogLine
	snap     chan snapResult
	sync     chan error
}

type snapResult struct {
	path string
	err  error
}

// New opens the store and prepares a daemon. Run starts it.
func New(o Options) (*Daemon, error) {
	if o.Logger == nil {
		o.Logger = log.New(os.Stderr, "algod.top: ", log.LstdFlags)
	}
	d := &Daemon{o: o, cfg: o.Config, logf: o.Logger.Printf, started: time.Now(), in: make(chan item, 4096),
		nodes: map[string]*nodeRun{}, health: map[string]health.Status{}, floor: map[string]string{}, creep: map[string]bool{}, subs: map[*subscriber]bool{}, roundCh: make(chan struct{}, 1)}
	d.hostNm, _ = os.Hostname()
	cfg := d.cfg
	applyRuntime(cfg.Runtime, o.Agent)
	dir := cfg.StorePath()
	if o.Agent {
		dir = filepath.Join(dir, "agent")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ret := cfg.Retention.Windows()
	if o.Agent {
		buf := cfg.Agent.Buffer.Or(7 * 24 * time.Hour)
		ret = [4]time.Duration{buf, buf, buf, buf}
	}
	floor := cfg.Disk.FloorPercent / 100
	addrRet := cfg.Retention.Sessions.D()
	if cfg.Retention.Sessions == config.ForeverDuration {
		addrRet = 200 * 365 * 24 * time.Hour
	}
	infoDays := cfg.Logs.KeepRawInfo
	for _, n := range cfg.Nodes {
		infoDays = max(infoDays, n.KeepRawDays)
	}
	db, err := store.Open(store.Options{Dir: dir, Retention: ret, Caps: store.Caps{PerName: cfg.Caps.PerName, PerNode: cfg.Caps.PerNode, NameCaps: map[string]int{
		"log_template_lines_total": max(cfg.Caps.Templates, 2000), "log_txpool_invalid_total": max(cfg.Caps.FloodAppIDs, 100) + 1}},
		GroupCommit: cfg.Intervals.GroupCommit.D(), DiskFloor: floor, AtFloor: cfg.Disk.AtFloor, Logf: d.logf, NoRollups: o.Agent, AddrRetention: addrRet, InfoLogDays: infoDays})
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", dir, err)
	}
	d.db = db
	d.qe = query.NewEngine(db, 2, 10*time.Second)
	d.sil = openSilences(filepath.Join(dir, "state", "silences.json"))
	rules, err := alert.Compile(cfg.Alerts)
	if err != nil {
		// The daemon starts with the rules that parse. Run records the
		// rest as an event, so they show in both UIs.
		d.logf("alerts: %v", err)
		d.alertErr = err
	}
	d.alerts = alert.NewEngine(rules, cfg.Channels, filepath.Join(dir, "state", "alerts.json"), d.emitEvent, d.sil.active)
	d.alerts.Logf = d.logf
	if o.Agent {
		ob, err := OpenOutbox(filepath.Join(dir, "outbox"), cfg.Agent.Buffer.Or(7*24*time.Hour), cfg.Agent.BufferCap*(1<<20))
		if err != nil {
			db.Close()
			return nil, err
		}
		d.outbox = ob
	}
	d.remotes = newRemoteSet(d)
	return d, nil
}

func applyRuntime(r config.Runtime, agent bool) {
	limit := r.MemLimitMiB
	if limit == 0 {
		limit = 96
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(limit) << 20)
	}
	procs := r.MaxProcs
	if procs == 0 {
		procs = 2
	}
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(procs)
	}
	if !r.NoLowPrio {
		lowerPriority()
	}
}

// Config returns the current config.
func (d *Daemon) Config() *config.Config {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.cfg
}

// Store returns the store.
func (d *Daemon) Store() *store.DB { return d.db }

// Query returns the query engine.
func (d *Daemon) Query() *query.Engine { return d.qe }

// Run starts everything and blocks until ctx is done, then shuts down
// within 10 s: it stops collectors, drains the writer, flushes every head
// with its partial rollups and fsyncs.
func (d *Daemon) Run(ctx context.Context) error {
	cfg := d.Config()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.runCtx, d.stop = ctx, cancel

	var listeners []*http.Server
	sock := cfg.SocketPath()
	if d.o.Agent {
		sock = filepath.Join(filepath.Dir(sock), "agent.sock")
		if cfg.Socket != "" {
			sock = cfg.SocketPath()
		}
	}
	uln, err := api.ListenUnix(sock, cfg.SocketGroup)
	if err != nil {
		return fmt.Errorf("socket %s: %w", sock, err)
	}
	usrv := newHTTPServer(api.New(api.Options{Hub: d, Static: d.o.Static, Agent: d.o.Agent, Logf: d.logf}))
	go usrv.Serve(uln)
	listeners = append(listeners, usrv)
	d.logf("listening on %s", sock)
	defer os.Remove(sock)

	if addr := d.listenAddr(); addr != "" {
		srv, ln, err := d.tcpListener(addr)
		if err != nil {
			usrv.Close()
			return err
		}
		listeners = append(listeners, srv)
		if d.o.OnListen != nil {
			d.o.OnListen(ln.Addr().String())
		}
	}

	writerDone := make(chan struct{})
	go func() { d.writer(); close(writerDone) }()

	d.host = collector.NewHost(collector.HostOptions{ProcEvery: cfg.Intervals.Proc.D(), NetEvery: cfg.Intervals.ProcNet.D(), NTPServer: ntpServer(cfg),
		NTPEvery: cfg.NTP.Interval.D()}, d.sink(), d.localNodes)
	var bg sync.WaitGroup
	bg.Add(1)
	go func() { defer bg.Done(); d.host.Run(ctx) }()
	d.startNodes(ctx, cfg)
	d.remotes.start(ctx, cfg)
	for _, f := range []func(context.Context){d.healthLoop, d.maintenanceLoop, d.selfLoop, d.statusPublisher, d.updateLoop} {
		bg.Add(1)
		go func(f func(context.Context)) { defer bg.Done(); f(ctx) }(f)
	}
	if hb := cfg.Heartbeat.URL; hb != "" {
		bg.Add(1)
		go func() { defer bg.Done(); alert.Heartbeat(ctx, hb, cfg.Heartbeat.Interval.D(), d.logf) }()
	}
	d.emitEvent(model.Event{Kind: "algodtop-start", Severity: model.SevInfo, Msg: "algod.top " + version.String() + " started"})
	if d.alertErr != nil {
		d.emitEvent(model.Event{Kind: model.EvConfigError, Severity: model.SevError,
			Msg: "alert rules not running: " + strings.ReplaceAll(d.alertErr.Error(), "\n", "; ")})
	}
	d.noteMonitorGap()
	d.startProfile(ctx)
	stopSig := d.handleSignals(ctx)
	defer stopSig()

	<-ctx.Done()
	d.logf("shutting down")
	deadline := time.Now().Add(10 * time.Second)
	sctx, scancel := context.WithDeadline(context.Background(), deadline)
	defer scancel()
	for _, s := range listeners {
		s.Shutdown(sctx)
	}
	d.stopNodes()
	d.remotes.wait()
	bg.Wait()
	d.inMu.Lock()
	d.inClosed = true
	close(d.in)
	d.inMu.Unlock()
	select {
	case <-writerDone:
	case <-sctx.Done():
		d.logf("writer did not drain before the deadline")
	}
	d.writeCleanStop()
	if d.outbox != nil {
		d.outbox.Close()
	}
	return d.db.Close()
}

func ntpServer(cfg *config.Config) string {
	if cfg.NTP.Disable {
		return ""
	}
	if cfg.NTP.Server != "" {
		return cfg.NTP.Server
	}
	return "pool.ntp.org"
}

func newHTTPServer(h http.Handler) *http.Server {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	// The unix socket also accepts HTTP/2 without TLS for the SSH relay.
	p.SetUnencryptedHTTP2(true)
	srv.Protocols = p
	return srv
}

func (d *Daemon) listenAddr() string {
	cfg := d.Config()
	if d.o.Agent {
		return cfg.Agent.Listen
	}
	return cfg.Web.ListenAddr()
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (d *Daemon) authConfig() api.AuthConfig {
	cfg := d.Config()
	ac := api.AuthConfig{PasswordHash: cfg.Web.PasswordHash, ReadOnlyPasswordHash: cfg.Web.ReadOnlyPasswordHash,
		ReadOnlyTokenHashes: cfg.Web.ReadOnlyTokenHashes, SessionTTL: cfg.Web.SessionTTL.D(), Logf: d.logf}
	ac.TLS = cfg.Web.TLS != "" || cfg.Web.TLSCert != ""
	if h, _, err := net.SplitHostPort(cfg.Web.ListenAddr()); err == nil && h != "" {
		ac.AllowedHosts = append(ac.AllowedHosts, h)
	}
	if cfg.Web.AllowUnauthenticatedRemote || d.o.Agent {
		// A TLS agent checks its token on every request instead.
		ac.AllowedHosts = append(ac.AllowedHosts, "*")
	}
	if host, err := os.Hostname(); err == nil {
		ac.AllowedHosts = append(ac.AllowedHosts, host, strings.ToLower(host))
	}
	return ac
}

func (d *Daemon) tcpListener(addr string) (*http.Server, net.Listener, error) {
	cfg := d.Config()
	ac := d.authConfig()
	noAuth := ac.PasswordHash == "" && ac.ReadOnlyPasswordHash == "" && len(ac.ReadOnlyTokenHashes) == 0
	if !d.o.Agent && !isLoopback(addr) && noAuth {
		if !cfg.Web.AllowUnauthenticatedRemote {
			return nil, nil, fmt.Errorf("web.listen %s is not loopback and no password is set; run `algod.top passwd` or set allow_unauthenticated_remote", addr)
		}
		d.logf("warning: serving %s with no password (allow_unauthenticated_remote)", addr)
	}
	d.auth = api.NewAuth(ac)
	srvAPI := api.New(api.Options{Hub: d, Auth: d.auth, Static: d.o.Static, Agent: d.o.Agent, Logf: d.logf})
	if d.o.Agent {
		srvAPI.AgentTokenHash = cfg.Agent.TokenHash
	}
	var handler http.Handler = srvAPI
	if d.o.Agent {
		if cfg.Agent.TokenHash == "" && cfg.Agent.ClientCA == "" {
			return nil, nil, fmt.Errorf("agent.listen %s needs agent.token_hash or agent.client_ca; run `algod.top passwd --agent-token`", addr)
		}
		handler = agentGate(cfg.Agent.TokenHash, cfg.Agent.ClientCA != "", srvAPI)
	}
	srv := newHTTPServer(handler)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	tc, err := d.tlsConfig()
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	if tc != nil {
		srv.TLSConfig = tc
		ln = tls.NewListener(ln, tc)
	}
	go srv.Serve(ln)
	scheme := "http"
	if tc != nil {
		scheme = "https"
	}
	d.logf("web UI on %s://%s", scheme, ln.Addr())
	return srv, ln, nil
}

// localNodes returns the running local collectors.
func (d *Daemon) localNodes() []*collector.Node {
	d.nodesMu.RLock()
	defer d.nodesMu.RUnlock()
	var out []*collector.Node
	for _, name := range d.order {
		out = append(out, d.nodes[name].node)
	}
	return out
}

// sourceFor builds how a node is reached.
func (d *Daemon) sourceFor(n config.Node) (collector.Source, error) {
	return SourceFor(n, filepath.Join(d.Config().StorePath(), "ssh"))
}

// SourceFor builds how a node is reached: a local data directory, an
// ssh:// URL, or a REST URL. controlDir holds ssh ControlMaster sockets.
func SourceFor(n config.Node, controlDir string) (collector.Source, error) {
	envTok, envAdmin := collector.TokensFromEnv()
	switch {
	case n.DataDir != "":
		return &collector.LocalSource{Dir: algod.ExpandHome(filepath.FromSlash(n.DataDir)), TokenFile: n.TokenFile, AdminTokenFile: n.AdminTokenFile,
			Token: firstNonEmpty(n.Token), AdminToken: firstNonEmpty(n.AdminToken)}, nil
	case strings.HasPrefix(n.URL, "ssh://"):
		s, err := remote.NewSSHSource(n.URL, controlDir)
		if err != nil {
			return nil, err
		}
		s.Token, s.AdminToken = n.Token, n.AdminToken
		return s, nil
	default:
		tok := firstNonEmpty(n.Token, readFile(n.TokenFile), envTok)
		admin := firstNonEmpty(n.AdminToken, readFile(n.AdminTokenFile), envAdmin)
		return &collector.RESTSource{URL: n.URL, Token: tok, AdminToken: admin}, nil
	}
}

func readFile(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(algod.ExpandHome(p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (d *Daemon) startNodes(ctx context.Context, cfg *config.Config) {
	for _, n := range cfg.Nodes {
		if n.Disabled {
			continue
		}
		if err := d.startNode(ctx, cfg, n); err != nil {
			d.logf("node %s: %v", n.Name, err)
		}
	}
}

func (d *Daemon) startNode(ctx context.Context, cfg *config.Config, n config.Node) error {
	src, err := d.sourceFor(n)
	if err != nil {
		return err
	}
	name := n.Name
	if _, err := d.db.RegisterNode(name, "", ""); err != nil {
		return err
	}
	stateDir := filepath.Join(d.db.Dir(), "state")
	scrape := n.Scrape.Or(cfg.Intervals.Scrape.Or(10 * time.Second))
	peers := n.Peers.Or(cfg.Intervals.Peers.Or(30 * time.Second))
	logs := collector.LogOptions{Disable: cfg.Logs.DisableTailer, Backfill: n.Backfill || cfg.Logs.Backfill,
		KeepRawInfo: n.KeepRawDays > 0 || cfg.Logs.KeepRawInfo > 0, TemplateCap: cfg.Caps.Templates, FloodCap: cfg.Caps.FloodAppIDs}
	cn := collector.NewNode(collector.Options{
		Name: name, Source: src, Sink: d.sink(), Scrape: scrape, PeersEvery: peers,
		PartEvery: cfg.Intervals.Participation.D(), AccountsEvery: cfg.Intervals.Accounts.D(), SupplyEvery: cfg.Intervals.Supply.D(),
		Accounts: n.Accounts, StateDir: stateDir, Logs: logs, Logf: d.logf,
		Register: func(name, gid, gh string) {
			if _, err := d.db.RegisterNode(name, gid, gh); err != nil {
				d.logf("register %s: %v", name, err)
			}
		},
		ProcStart: func(pid int) int64 {
			if d.host == nil {
				return 0
			}
			return d.host.ProcStart(pid)
		},
		LastRound: func() uint64 { return d.lastStoredRound(name) },
	})
	nctx, cancel := context.WithCancel(ctx)
	nr := &nodeRun{cfg: n, node: cn, cancel: cancel, done: make(chan struct{})}
	d.nodesMu.Lock()
	d.nodes[name] = nr
	d.order = append(d.order, name)
	d.nodesMu.Unlock()
	go func() { cn.Run(nctx); close(nr.done) }()
	return nil
}

func (d *Daemon) stopNode(name string) {
	d.nodesMu.Lock()
	nr := d.nodes[name]
	delete(d.nodes, name)
	for i, n := range d.order {
		if n == name {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	d.nodesMu.Unlock()
	if nr != nil {
		nr.cancel()
		<-nr.done
	}
}

func (d *Daemon) stopNodes() {
	d.nodesMu.RLock()
	names := append([]string(nil), d.order...)
	d.nodesMu.RUnlock()
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) { defer wg.Done(); d.stopNode(n) }(n)
	}
	wg.Wait()
}

// lastStoredRound returns the newest stored round, for header backfill.
func (d *Daemon) lastStoredRound(name string) uint64 {
	node, ok := d.db.NodeID(name)
	if !ok {
		return 0
	}
	ms := d.db.Find(node, "algodtop_round")
	if len(ms) == 0 {
		return 0
	}
	now := time.Now().UnixNano()
	pts, _, err := d.db.Read(node, []uint32{ms[0].ID}, store.TierRaw, now-int64(30*24*time.Hour), now)
	if err != nil || len(pts) == 0 || len(pts[0].T) == 0 {
		return 0
	}
	v := pts[0].Cols[0]
	return uint64(v[len(v)-1])
}

// writer is the one goroutine that owns storage writes.
func (d *Daemon) writer() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastBytes int64
	lastT := time.Now()
	for {
		select {
		case it, ok := <-d.in:
			if !ok {
				return
			}
			d.apply(it)
		case <-tick.C:
			if err := d.db.Tick(); err != nil {
				d.logf("store: %v", err)
			}
			if b := d.db.Stats.BytesWritten.Load(); time.Since(lastT) >= 10*time.Second {
				d.selfMu.Lock()
				d.writeBPS = float64(b-lastBytes) / time.Since(lastT).Seconds()
				d.selfMu.Unlock()
				lastBytes, lastT = b, time.Now()
			}
		}
	}
}

func (d *Daemon) apply(it item) {
	var err error
	switch {
	case it.batch != nil:
		err = d.db.Append(it.batch)
	case it.event != nil:
		err = d.db.AppendEvent(*it.event)
	case it.session != nil:
		err = d.db.AppendSession(*it.session)
	case it.usage != nil:
		err = d.db.AppendUsage(it.usageFor, it.usage)
	case it.line != nil:
		err = d.db.AppendLogLine(*it.line)
	case it.snap != nil:
		p, e := d.db.Snapshot()
		it.snap <- snapResult{p, e}
	case it.sync != nil:
		it.sync <- d.db.Tick()
	}
	if err != nil {
		d.logf("store: %v", err)
	}
}

// enqueue hands an item to the writer. It never blocks a collector for
// long: after 5 s the item is dropped and counted. After shutdown it drops
// the item, and tells whoever waits on it.
func (d *Daemon) enqueue(it item) {
	d.inMu.RLock()
	defer d.inMu.RUnlock()
	if d.inClosed {
		if it.sync != nil {
			select {
			case it.sync <- errStopped:
			default:
			}
		}
		if it.snap != nil {
			select {
			case it.snap <- snapResult{err: errStopped}:
			default:
			}
		}
		return
	}
	select {
	case d.in <- it:
		return
	default:
	}
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case d.in <- it:
	case <-t.C:
		d.drops.Add(1)
	}
}

var errStopped = errors.New("the daemon has stopped")

// Flush waits until the writer has applied everything queued so far.
func (d *Daemon) Flush() error {
	ch := make(chan error, 1)
	d.enqueue(item{sync: ch})
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		return errors.New("writer did not respond")
	}
}

func (d *Daemon) emitEvent(e model.Event) {
	if e.Time == 0 {
		e.Time = time.Now().UnixNano()
	}
	if d.outbox != nil {
		d.outbox.Add(OutItem{Event: &e})
	}
	d.enqueue(item{event: &e})
	d.publish(api.Message{Event: "event", Data: e}, e.Node)
}

// daemonSink adapts the collectors' Sink to the writer queue, the SSE
// fan-out and, for agents, the outbox.
type daemonSink struct {
	d      *Daemon
	rename func(string) string // agent node name to hub name, on the hub
	// onRound moves an agent node's round forward on the hub as soon as
	// its round batch arrives.
	onRound func(node string, round uint64, at int64)
}

func (d *Daemon) sink() collector.Sink { return &daemonSink{d: d} }

func (s *daemonSink) name(n string) string {
	if s.rename == nil {
		return n
	}
	return s.rename(n)
}

func (s *daemonSink) Batch(b *model.Batch) {
	if s.rename != nil {
		b.Node = s.name(b.Node)
	}
	if s.d.outbox != nil {
		s.d.outbox.Add(OutItem{Batch: b})
	}
	s.d.enqueue(item{batch: b})
	s.d.publishBatch(b)
	if b.Group == model.GroupRound {
		if s.onRound != nil {
			for _, smp := range b.Samples {
				if smp.Name == "algodtop_round" {
					s.onRound(b.Node, uint64(smp.Value), b.Time)
				}
			}
		}
		s.d.roundArrived()
	}
}

func (s *daemonSink) Event(e model.Event) {
	if s.rename != nil && e.Node != "" {
		e.Node = s.name(e.Node)
	}
	s.d.emitEvent(e)
}

func (s *daemonSink) Session(x model.Session) {
	x.Node = s.name(x.Node)
	if s.d.outbox != nil {
		s.d.outbox.Add(OutItem{Session: &x})
	}
	s.d.enqueue(item{session: &x})
}

func (s *daemonSink) Usage(node string, us []model.PeerUsage) {
	node = s.name(node)
	if s.d.outbox != nil {
		s.d.outbox.Add(OutItem{UsageNode: node, Usage: us})
	}
	s.d.enqueue(item{usage: us, usageFor: node})
}

func (s *daemonSink) LogLine(l collector.LogLine) {
	l.Node = s.name(l.Node)
	ll := store.LogLine{Time: l.Time, Node: l.Node, Level: l.Level, Template: l.Template, Line: l.Line}
	if s.d.outbox != nil {
		s.d.outbox.Add(OutItem{Line: &ll})
	}
	s.d.enqueue(item{line: &ll})
}

func (s *daemonSink) Annotate(a model.Annotation) {
	if a.Node != "" {
		a.Node = s.name(a.Node)
	}
	if s.d.outbox != nil {
		s.d.outbox.Add(OutItem{Annotation: &a})
	}
	if _, err := s.d.db.PutAnnotation(a); err != nil {
		s.d.logf("annotation: %v", err)
	}
}

// healthLoop evaluates health and alerts from memory every 5 s. Health
// checks and alerts never read stored data, so they keep working when the
// disk is full.
func (d *Daemon) healthLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	silEvery := time.NewTicker(10 * time.Second)
	defer silEvery.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-silEvery.C:
			started, ended := d.sil.transitions(time.Now().UnixNano())
			for _, s := range started {
				d.silenceEvent(model.EvSilenceStart, "silence started: "+s.Comment, s, nil)
			}
			for _, s := range ended {
				d.silenceEvent(model.EvSilenceEnd, "silence ended: "+s.Comment, s, nil)
			}
		case <-t.C:
			d.evaluate(time.Now())
		}
	}
}

func (d *Daemon) daemonHealth() health.Daemon {
	dh := health.Daemon{Paused: d.db.IsPaused(), DroppedSeries: map[string]map[string]int64{}, FloorDeleted: map[string]string{},
		SharedVolume: map[string]float64{}, QueueDrops: d.drops.Load()}
	for _, n := range d.db.Nodes() {
		if drops := d.db.Dropped(n.ID); len(drops) > 0 {
			dh.DroppedSeries[n.Name] = drops
		}
	}
	d.nodesMu.RLock()
	for k, v := range d.floor {
		dh.FloorDeleted[k] = v
	}
	dh.ProjectedOver = d.projOver
	d.nodesMu.RUnlock()
	return dh
}

func (d *Daemon) evaluate(now time.Time) {
	states := d.states()
	dh := d.daemonHealth()
	// The daemon warns when its data directory and an algod data
	// directory share a volume that is under 15% free.
	if su, err := disk.Stat(d.db.Dir()); err == nil && su.FreeFraction() < 0.15 {
		for _, s := range states {
			if s.DataDir == "" {
				continue
			}
			if nu, err := disk.Stat(s.DataDir); err == nil && nu.Total == su.Total && nu.Free == su.Free {
				dh.SharedVolume[s.Name] = su.FreeFraction()
			}
		}
	}
	hc := health.Context{Now: now, Fleet: states, Daemon: dh}
	results := map[string]health.Status{}
	for i := range states {
		s := &states[i]
		st := health.Evaluate(s, hc)
		results[s.Name] = st
		d.alerts.Evaluate(s.Name, alert.Input{State: s, Health: st, Now: now})
	}
	for _, rn := range d.remotes.views() {
		results[rn.Name] = rn.Health
		if rn.Remote != "" {
			// Alerts for agent nodes run on the agent; the hub fires only
			// "agent unreachable".
			s := rn.State
			d.alerts.Evaluate(rn.Name, alert.Input{State: &s, Health: rn.Health, Now: now, AgentUnreachable: !d.remotes.reachable(rn.Remote)})
		}
	}
	d.nodesMu.Lock()
	d.health = results
	d.nodesMu.Unlock()
}

func (d *Daemon) states() []collector.State {
	d.nodesMu.RLock()
	defer d.nodesMu.RUnlock()
	out := make([]collector.State, 0, len(d.order))
	for _, n := range d.order {
		out = append(out, d.nodes[n].node.State())
	}
	return out
}

// Nodes implements api.Hub.
func (d *Daemon) Nodes() []api.NodeView {
	now := time.Now()
	states := d.states()
	firing := d.alerts.Firing()
	d.nodesMu.RLock()
	hs := d.health
	d.nodesMu.RUnlock()
	var out []api.NodeView
	for _, s := range states {
		h, ok := hs[s.Name]
		if !ok {
			h = health.Evaluate(&s, health.Context{Now: now, Fleet: states})
		}
		nodeID, hist := d.db.NodeID(s.Name)
		var histFrom int64
		if hist {
			for tier := store.TierRaw; tier <= store.Tier1h; tier++ {
				if lo, _, ok := d.db.Coverage(nodeID, tier); ok && (histFrom == 0 || lo < histFrom) {
					histFrom = lo
				}
			}
			// A new node has nothing flushed for its first window, up to 2 h.
			if histFrom == 0 {
				histFrom = d.db.HeadFirst(nodeID)
			}
		}
		d.nodesMu.RLock()
		creep := d.creep[s.Name]
		d.nodesMu.RUnlock()
		out = append(out, api.NodeView{State: s, Health: h, Summary: h.Summary(), Lag: s.Lag(now), Firing: firing[s.Name],
			Silenced: d.sil.active(now.UnixNano(), s.Name, ""), History: hist, HistoryFrom: histFrom, MemoryCreep: creep})
	}
	out = append(out, d.remotes.views()...)
	return out
}

// Node implements api.Hub.
func (d *Daemon) Node(name string) (api.NodeView, bool) {
	for _, n := range d.Nodes() {
		if n.Name == name {
			return n, true
		}
	}
	return api.NodeView{}, false
}

// Status implements api.Hub.
func (d *Daemon) Status() api.DaemonStatus {
	cfg := d.Config()
	ret := map[string]string{}
	w := d.db.Retention()
	for i, name := range []string{"raw", "1m", "10m", "1h"} {
		if w[i] == 0 {
			ret[name] = "forever"
		} else {
			ret[name] = config.Duration(w[i]).String()
		}
	}
	d.selfMu.Lock()
	self := d.selfStats
	d.selfMu.Unlock()
	self.QueueDepth = len(d.in)
	self.QueueDrops = d.drops.Load()
	self.SSEClients = d.subCount()
	st := api.DaemonStatus{Host: d.hostNm, Version: version.String(), VersionTime: version.Info().UnixNano(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Started: d.started.UnixNano(),
		StoreBytes: d.storeSize(), TierBytes: d.db.Sizes(), Retention: ret, Paused: d.db.IsPaused(), Self: self, Agent: d.o.Agent, ConfigPath: cfg.Path}
	if w := config.CheckPermissions(cfg.Path); w != "" {
		st.Warnings = append(st.Warnings, w)
	}
	st.Latest, st.LatestTime = d.latestStatus()
	return st
}

var sizeCache struct {
	sync.Mutex
	at time.Time
	v  int64
}

func (d *Daemon) storeSize() int64 {
	sizeCache.Lock()
	defer sizeCache.Unlock()
	if time.Since(sizeCache.at) > time.Minute {
		sizeCache.v = d.db.TotalSize()
		sizeCache.at = time.Now()
	}
	return sizeCache.v
}

// Silences implements api.Hub.
func (d *Daemon) Silences() []model.Silence { return d.sil.all() }

// silenceEvent records a silence change once for each node it covers, so
// it shows on those nodes' event lanes. A silence for every node is one
// event with no node.
func (d *Daemon) silenceEvent(kind, msg string, s model.Silence, extra map[string]any) {
	nodes := s.Nodes
	if len(nodes) == 0 {
		nodes = []string{""}
	}
	for _, n := range nodes {
		attrs := map[string]any{"id": s.ID, "nodes": s.Nodes}
		maps.Copy(attrs, extra)
		d.emitEvent(model.Event{Node: n, Kind: kind, Severity: model.SevInfo, Msg: msg, Attrs: attrs})
	}
}

// AddSilence implements api.Hub.
func (d *Daemon) AddSilence(s model.Silence) (model.Silence, error) {
	out, err := d.sil.add(s)
	if err == nil {
		d.silenceEvent(model.EvSilenceStart, "silence added: "+s.Comment, out, map[string]any{"end": out.End})
		d.sil.transitions(time.Now().UnixNano())
	}
	return out, err
}

// DeleteSilence implements api.Hub.
func (d *Daemon) DeleteSilence(id string) error { return d.sil.remove(id) }

// Snapshot implements api.Hub.
func (d *Daemon) Snapshot() (string, error) {
	ch := make(chan snapResult, 1)
	d.enqueue(item{snap: ch})
	select {
	case r := <-ch:
		return r.path, r.err
	case <-time.After(5 * time.Minute):
		return "", errors.New("snapshot timed out")
	}
}

// Discovered implements api.Hub.
func (d *Daemon) Discovered() []api.DiscoveredNode {
	cfg := d.Config()
	known := map[string]bool{}
	for _, n := range cfg.Nodes {
		known[filepath.Clean(algod.ExpandHome(n.DataDir))] = true
	}
	var out []api.DiscoveredNode
	for _, f := range algod.Discover(sysstat.ListAlgod, sysstat.Alive) {
		if !known[filepath.Clean(f.DataDir)] {
			out = append(out, api.DiscoveredNode{Name: f.Name, DataDir: f.DataDir, Running: f.Running, Network: f.Network})
		}
	}
	return out
}

// ConfigJSON implements api.Hub. Secrets are redacted and channels are
// shown by name and type only.
func (d *Daemon) ConfigJSON() ([]byte, error) {
	cfg := *d.Config()
	cfg.Web.PasswordHash = redact(cfg.Web.PasswordHash)
	cfg.Web.ReadOnlyPasswordHash = redact(cfg.Web.ReadOnlyPasswordHash)
	cfg.Web.ReadOnlyTokenHashes = nil
	cfg.Agent.TokenHash = redact(cfg.Agent.TokenHash)
	nodes := make([]config.Node, len(cfg.Nodes))
	for i, n := range cfg.Nodes {
		n.Token, n.AdminToken = redact(n.Token), redact(n.AdminToken)
		nodes[i] = n
	}
	cfg.Nodes = nodes
	ch := map[string]config.Chan{}
	for k, v := range cfg.Channels {
		ch[k] = config.Chan{Type: v.Type}
	}
	cfg.Channels = ch
	b, err := json.MarshalIndent(map[string]any{"config": cfg, "editable": cfg.Web.AllowConfigEdits, "path": d.Config().Path}, "", "  ")
	return b, err
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "(set)"
}

// PutConfig implements api.Hub. It accepts nodes and retention only, and
// only when web.allow_config_edits is set. Alert channels are never
// editable through the API, because a command channel runs a program.
func (d *Daemon) PutConfig(ctx context.Context, b []byte) error {
	cur := d.Config()
	if !cur.Web.AllowConfigEdits {
		return errors.New("config edits from the web UI are off; set web.allow_config_edits in the config file")
	}
	var in struct {
		Nodes     []config.Node     `json:"nodes"`
		Retention *config.Retention `json:"retention"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return err
	}
	next := *cur
	if in.Nodes != nil {
		old := map[string]config.Node{}
		for _, n := range cur.Nodes {
			old[n.Name] = n
		}
		for i, n := range in.Nodes {
			// Redacted tokens keep their old values.
			if o, ok := old[n.Name]; ok {
				if n.Token == "(set)" {
					in.Nodes[i].Token = o.Token
				}
				if n.AdminToken == "(set)" {
					in.Nodes[i].AdminToken = o.AdminToken
				}
			}
		}
		next.Nodes = in.Nodes
	}
	if in.Retention != nil {
		next.Retention = *in.Retention
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.Save(cur.Path); err != nil {
		return err
	}
	return d.Reload()
}

// Reload re-reads the config: nodes, alerts and retention. A listen
// address change needs a restart.
func (d *Daemon) Reload() error {
	path := d.Config().Path
	if path == "" {
		return errors.New("no config file to reload")
	}
	next, err := config.Load(path)
	if err != nil {
		return err
	}
	cur := d.Config()
	if next.Web.ListenAddr() != cur.Web.ListenAddr() {
		d.logf("web.listen changed; it takes effect at the next restart")
	}
	rules, err := alert.Compile(next.Alerts)
	if err != nil {
		return err
	}
	// Retention: shortening a window deletes data, which is reported.
	old := d.db.Retention()
	nw := next.Retention.Windows()
	if d.o.Agent {
		nw = old
	}
	d.cfgMu.Lock()
	d.cfg = next
	d.cfgMu.Unlock()
	d.alerts.SetRules(rules, next.Channels)
	if d.auth != nil {
		d.auth.SetConfig(d.authConfig())
	}
	if nw != old {
		d.db.SetRetention(nw)
		go func() {
			rep := d.db.Maintain()
			d.logf("retention changed: %d blocks removed", len(rep.Deleted))
		}()
	}
	// Nodes: stop removed or changed, start new.
	want := map[string]config.Node{}
	for _, n := range next.Nodes {
		if !n.Disabled {
			want[n.Name] = n
		}
	}
	d.nodesMu.RLock()
	var stop []string
	for name, nr := range d.nodes {
		if w, ok := want[name]; !ok || !sameNode(w, nr.cfg) {
			stop = append(stop, name)
		}
	}
	d.nodesMu.RUnlock()
	for _, name := range stop {
		d.stopNode(name)
	}
	for _, n := range next.Nodes {
		d.nodesMu.RLock()
		_, running := d.nodes[n.Name]
		d.nodesMu.RUnlock()
		if !running && !n.Disabled {
			if err := d.startNode(d.runCtx, next, n); err != nil {
				d.logf("node %s: %v", n.Name, err)
			}
		}
	}
	d.remotes.reload(d.runCtx, next)
	d.emitEvent(model.Event{Kind: "config-reload", Severity: model.SevInfo, Msg: "algod.top config reloaded"})
	return nil
}

func sameNode(a, b config.Node) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// maintenanceLoop runs compaction, retention and the disk floor every 5
// minutes, and the scrub weekly, one job at a time.
func (d *Daemon) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	scrubPath := filepath.Join(d.db.Dir(), "state", "last-scrub")
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rep := d.db.Maintain()
		if len(rep.Deleted) > 0 || rep.FloorHit {
			d.noteFloor(rep)
		}
		if d.outbox != nil {
			d.pruneOutbox()
		}
		over := d.projectionOver()
		creep := d.memoryCreep(ctx)
		d.nodesMu.Lock()
		d.projOver, d.creep = over, creep
		d.nodesMu.Unlock()
		var last time.Time
		if b, err := os.ReadFile(scrubPath); err == nil {
			last, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
		}
		if time.Since(last) > 7*24*time.Hour {
			bad := d.db.Scrub()
			os.WriteFile(scrubPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
			for _, p := range bad {
				d.emitEvent(model.Event{Kind: "scrub-error", Severity: model.SevError, Msg: "block fails its checksum: " + p})
			}
		}
	}
}

// noteFloor records the shortened coverage in words for the node health.
func (d *Daemon) noteFloor(rep store.MaintenanceReport) {
	if !rep.FloorHit {
		return
	}
	now := time.Now().UnixNano()
	ret := d.db.Retention()
	d.nodesMu.Lock()
	defer d.nodesMu.Unlock()
	for _, n := range d.db.Nodes() {
		lo, _, ok := d.db.Coverage(n.ID, store.TierRaw)
		if !ok || ret[0] == 0 {
			continue
		}
		have := time.Duration(now - lo)
		if have < ret[0]-24*time.Hour {
			d.floor[n.Name] = fmt.Sprintf("raw history is %d days instead of %d", int(have.Hours()/24), int(ret[0].Hours()/24))
		}
	}
}

// selfLoop records algodtop's own CPU, RSS, heap, queue depth and store
// size as series under the node "@algodtop".
func (d *Daemon) selfLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	sys := sysstat.New()
	defer sys.Close()
	pid := os.Getpid()
	var cw clockWatch
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		d.noteClock(&cw, now)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		p := sys.Procs([]int{pid})[pid]
		d.selfMu.Lock()
		cpu := 0.0
		if d.selfPrev[1] > 0 {
			if dt := float64(now.UnixNano())/1e9 - d.selfPrev[1]; dt > 0 {
				cpu = (p.CPUSeconds - d.selfPrev[0]) / dt * 100
			}
		}
		d.selfPrev = [2]float64{p.CPUSeconds, float64(now.UnixNano()) / 1e9}
		d.selfStats = api.SelfStats{CPU: cpu, RSS: p.RSSBytes, Heap: float64(ms.HeapInuse), Goroutines: runtime.NumGoroutine(),
			WriteBPS: d.writeBPS, WriteNanos: d.db.Stats.WriteNanos.Load(), HeadBytes: d.db.HeadBytes(), Samples: d.db.Stats.Appended.Load(),
			Late: d.db.Stats.Late.Load(), Dropped: d.db.Stats.Dropped.Load()}
		st := d.selfStats
		d.selfMu.Unlock()
		samples := []model.Sample{
			{Name: "algodtop_cpu_seconds_total", Kind: model.KindCounter, Value: p.CPUSeconds},
			{Name: "algodtop_resident_bytes", Kind: model.KindGauge, Value: p.RSSBytes},
			{Name: "algodtop_heap_bytes", Kind: model.KindGauge, Value: st.Heap},
			{Name: "algodtop_goroutines", Kind: model.KindGauge, Value: float64(st.Goroutines)},
			{Name: "algodtop_queue_depth", Kind: model.KindGauge, Value: float64(len(d.in))},
			{Name: "algodtop_queue_drops_total", Kind: model.KindCounter, Value: float64(d.drops.Load())},
			{Name: "algodtop_write_latency_seconds", Kind: model.KindGauge, Value: float64(st.WriteNanos) / 1e9},
			{Name: "algodtop_samples_total", Kind: model.KindCounter, Value: float64(st.Samples)},
			{Name: "algodtop_block_bytes_written_total", Kind: model.KindCounter, Value: float64(d.db.Stats.BytesWritten.Load())},
			{Name: "algodtop_head_bytes", Kind: model.KindGauge, Value: float64(st.HeadBytes)},
		}
		for tier, sz := range d.db.Sizes() {
			samples = append(samples, model.Sample{Name: "algodtop_store_bytes", Labels: model.Labels{{Name: "tier", Value: tier}}, Kind: model.KindGauge, Value: float64(sz)})
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i].Name < samples[j].Name })
		d.enqueue(item{batch: &model.Batch{Node: "@algodtop", Group: model.GroupSelf, Time: now.UnixNano(), Samples: samples}})
	}
}

// noteMonitorGap records how long algodtop itself was down.
func (d *Daemon) noteMonitorGap() {
	p := filepath.Join(d.db.Dir(), "state", "last-alive")
	b, err := os.ReadFile(p)
	if err == nil {
		var last int64
		fmt.Sscan(strings.TrimSpace(string(b)), &last)
		if last > 0 {
			gap := time.Since(time.Unix(0, last))
			if gap > time.Minute {
				d.emitEvent(model.Event{Time: time.Now().UnixNano(), Kind: model.EvMonitorGap, Severity: model.SevWarn,
					Msg: fmt.Sprintf("algod.top was down for %s", gap.Round(time.Second)), Attrs: map[string]any{"from": last, "to": time.Now().UnixNano()}})
			}
		}
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			os.WriteFile(p, []byte(fmt.Sprint(time.Now().UnixNano())), 0o600)
			select {
			case <-d.runCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (d *Daemon) writeCleanStop() {
	os.WriteFile(filepath.Join(d.db.Dir(), "state", "last-alive"), []byte(fmt.Sprint(time.Now().UnixNano())), 0o600)
}
