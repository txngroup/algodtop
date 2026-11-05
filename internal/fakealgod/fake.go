// Package fakealgod is an algod stand-in for tests, benchmarks and the
// Docker remote tests. It serves a Mainnet-shaped /metrics fixture with
// counters that grow with uptime, produces rounds, churns peers, writes
// node.log lines and can stall, restart, rotate logs, change port and
// disable API auth.
package fakealgod

import (
	"bytes"
	"crypto/sha512"
	_ "embed"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed mainnet.metrics
var metricsFixture []byte

// Options configure a fake.
type Options struct {
	DataDir      string        // write algod.net, algod.pid, tokens, node.log
	Addr         string        // listen address, default 127.0.0.1:0
	RoundEvery   time.Duration // default 2.8 s
	Token        string
	AdminToken   string
	DisableAuth  bool
	PeerUsage    bool // report the per-peer usage fields
	Peers        int  // default 20
	Network      string
	Version      [3]int
	ChurnEvery   time.Duration // peer churn, default 20 s
	LogTraceRate int           // trace lines per round
	Seed         int64
	PID          int // reported in algod.pid, default os.Getpid()

	// The rest make a demo look like a real network.
	Round       uint64    // first round, default 52184330
	GenesisHash string    // base64, default a placeholder named after the network
	Branch      string    // build branch, default "fake"
	Channel     string    // build channel, default "stable"
	Accounts    []Account // online accounts whose participation keys this node holds
	DocIPs      bool      // peer addresses from the documentation ranges (RFC 5737)
	Load        float64   // scales how fast counters grow, default 1
	Jitter      bool      // vary each round by a few percent, with an occasional slow one
	// Uptime is how long the process seems to have run at start, which
	// sets how big its counters begin; default 24 h. A demo restart uses a
	// short one, as a real restart starts its counters from zero.
	Uptime time.Duration
	// Wave makes counter rates rise and fall over minutes and hours, as a
	// real node's traffic does, rather than grow at one steady rate. The
	// wave follows the wall clock and the data directory, so a restarted
	// fake picks it up where it was.
	Wave bool
	// WireTo is the address of a wire sink (see ServeWireSink). The fake
	// sends the bytes its counters say it sent there and has the sink send
	// back the bytes it received, so the OS sees that traffic on sockets
	// the fake owns.
	WireTo string
}

// Account is an online account whose participation key the fake holds.
type Account struct {
	Address string
	Amount  uint64  // microAlgos
	KeyDays float64 // days from start until the key's last valid round
}

// Fake is a running fake algod.
type Fake struct {
	mu      sync.Mutex
	o       Options
	ln      net.Listener
	srv     *http.Server
	started time.Time
	round   uint64
	roundAt time.Time
	txnCtr  uint64
	stalled bool
	down    bool
	peers   []*fpeer
	rnd     *rand.Rand
	cond    *sync.Cond
	logF    *os.File
	pid     int
	stop    chan struct{}
	wg      sync.WaitGroup
	lines   []metricLine
	keys    []fakeKey
	phase   [len(waves)]float64
	// appIn and appOut are the fixture's daily app-level bytes received
	// and sent, over WS and P2P.
	appIn, appOut float64
	// Paths records every request path.
	Paths map[string]int
	// AdminUsed records paths requested with the admin token.
	AdminUsed map[string]int
}

type fakeKey struct {
	acct          Account
	first, last   uint64
	id, vote, sel string
}

type fpeer struct {
	addr, dir, net, id string
	since              time.Time
	rx, tx             int64
}

type metricLine struct {
	text    string // name{labels}
	value   float64
	counter bool
}

// Start runs a fake algod.
func Start(o Options) (*Fake, error) {
	if o.RoundEvery == 0 {
		o.RoundEvery = 2800 * time.Millisecond
	}
	if o.Addr == "" {
		o.Addr = "127.0.0.1:0"
	}
	if o.Peers == 0 {
		o.Peers = 20
	}
	if o.Network == "" {
		o.Network = "mainnet"
	}
	if o.Version == ([3]int{}) {
		o.Version = [3]int{4, 4, 1}
	}
	if o.ChurnEvery == 0 {
		o.ChurnEvery = 20 * time.Second
	}
	if o.Seed == 0 {
		o.Seed = time.Now().UnixNano()
	}
	if o.Round == 0 {
		o.Round = 52184330
	}
	if o.GenesisHash == "" {
		o.GenesisHash = o.Network + "-genesis-hash="
	}
	if o.Branch == "" {
		o.Branch = "fake"
	}
	if o.Channel == "" {
		o.Channel = "stable"
	}
	if o.Load == 0 {
		o.Load = 1
	}
	if o.Uptime == 0 {
		o.Uptime = 24 * time.Hour
	}
	f := &Fake{o: o, started: time.Now().Add(-o.Uptime), round: o.Round, roundAt: time.Now(), txnCtr: 3761712520,
		rnd: rand.New(rand.NewSource(o.Seed)), stop: make(chan struct{}), Paths: map[string]int{}, AdminUsed: map[string]int{}}
	f.cond = sync.NewCond(&f.mu)
	f.pid = o.PID
	if f.pid == 0 {
		f.pid = os.Getpid()
	}
	f.parseFixture()
	h := fnv.New64a()
	h.Write([]byte(o.DataDir + "|" + o.Network))
	pr := rand.New(rand.NewSource(int64(h.Sum64())))
	for i := range f.phase {
		f.phase[i] = 2 * math.Pi * pr.Float64()
	}
	for _, a := range o.Accounts {
		perDay := uint64(86400 / o.RoundEvery.Seconds())
		k := fakeKey{acct: a, first: 1, last: o.Round + uint64(a.KeyDays*float64(perDay))}
		if o.Round > 30*perDay {
			k.first = o.Round - 30*perDay // the key was made a month ago
		}
		k.id = fmt.Sprintf("%016x%016x", f.rnd.Uint64(), f.rnd.Uint64())
		k.vote, k.sel = randKey(f.rnd), randKey(f.rnd)
		f.keys = append(f.keys, k)
	}
	out := false
	for i := 0; i < o.Peers; i++ {
		p := f.newPeer()
		out = out || p.dir == "outbound"
		f.peers = append(f.peers, p)
	}
	// algod always dials out; a node with no outgoing peers is broken.
	if !out && len(f.peers) > 0 {
		f.peers[0].dir = "outbound"
	}
	if err := f.listen(o.Addr); err != nil {
		return nil, err
	}
	if o.DataDir != "" {
		if err := f.writeDataDir(); err != nil {
			f.Close()
			return nil, err
		}
	}
	f.wg.Add(1)
	go f.loop()
	if o.WireTo != "" {
		f.wg.Add(1)
		go f.wireLoop(o.WireTo)
	}
	return f, nil
}

func (f *Fake) parseFixture() {
	kinds := map[string]bool{}
	for _, l := range strings.Split(string(metricsFixture), "\n") {
		l = strings.TrimSuffix(l, "\r") // a CRLF checkout of the fixture
		if strings.HasPrefix(l, "# TYPE ") {
			p := strings.Fields(l)
			if len(p) == 4 {
				kinds[p[2]] = p[3] == "counter"
			}
			continue
		}
		if l == "" || l[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(l[i+1:], 64)
		if err != nil {
			continue
		}
		name := l[:i]
		base := name
		if j := strings.IndexByte(base, '{'); j >= 0 {
			base = base[:j]
		}
		f.lines = append(f.lines, metricLine{text: name, value: v, counter: kinds[base]})
		switch name {
		case "algod_network_received_bytes_total", "algod_network_p2p_received_bytes_total":
			f.appIn += v
		case "algod_network_sent_bytes_total", "algod_network_p2p_sent_bytes_total":
			f.appOut += v
		}
	}
}

func (f *Fake) listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	f.ln = ln
	f.srv = &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go f.srv.Serve(ln)
	return nil
}

// Addr returns host:port.
func (f *Fake) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ln.Addr().String()
}

// URL returns the base URL.
func (f *Fake) URL() string { return "http://" + f.Addr() }

// Close stops the fake.
func (f *Fake) Close() {
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	f.mu.Lock()
	f.cond.Broadcast()
	srv := f.srv
	f.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	f.wg.Wait()
	f.mu.Lock()
	if f.logF != nil {
		f.logF.Close()
	}
	f.mu.Unlock()
	if f.o.DataDir != "" {
		os.Remove(filepath.Join(f.o.DataDir, "algod.pid"))
	}
}

func (f *Fake) writeDataDir() error {
	d := f.o.DataDir
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	w := func(name, s string) error { return os.WriteFile(filepath.Join(d, name), []byte(s), 0o600) }
	gen := fmt.Sprintf(`{"network":%q,"id":"v1.0","alloc":[]}`, f.o.Network)
	if err := w("genesis.json", gen); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(d, "config.json")); os.IsNotExist(err) {
		cfg := map[string]any{"EndpointAddress": f.ln.Addr().String(), "DisableAPIAuth": f.o.DisableAuth}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		w("config.json", string(b))
	}
	if f.o.Token != "" {
		w("algod.token", f.o.Token)
	}
	if f.o.AdminToken != "" {
		w("algod.admin.token", f.o.AdminToken)
	}
	w("algod.net", f.ln.Addr().String()+"\n")
	w("algod.pid", strconv.Itoa(f.pid)+"\n")
	lf, err := os.OpenFile(filepath.Join(d, "node.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	f.logF = lf
	return nil
}

func (f *Fake) newPeer() *fpeer {
	dir := "inbound"
	if f.rnd.Intn(2) == 0 {
		dir = "outbound"
	}
	nt := "p2p"
	if f.rnd.Intn(5) == 0 {
		nt = "ws"
	}
	return f.newPeerOn(nt, dir)
}

// newPeerOn makes a peer on network nt ("ws" or "p2p") in direction dir.
func (f *Fake) newPeerOn(nt, dir string) *fpeer {
	ip := fmt.Sprintf("%d.%d.%d.%d", 1+f.rnd.Intn(220), f.rnd.Intn(255), f.rnd.Intn(255), 1+f.rnd.Intn(254))
	if f.o.DocIPs {
		ip = fmt.Sprintf("%s.%d", []string{"192.0.2", "198.51.100", "203.0.113"}[f.rnd.Intn(3)], 1+f.rnd.Intn(254))
	}
	p := &fpeer{dir: dir, net: nt, since: time.Now()}
	if nt == "p2p" {
		p.addr = fmt.Sprintf("/ip4/%s/tcp/%d", ip, 4190+f.rnd.Intn(3))
		// An ed25519 libp2p peer ID: "12D3KooW" and 44 base58 characters.
		const b58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
		id := []byte("12D3KooW")
		for range 44 {
			id = append(id, b58[f.rnd.Intn(len(b58))])
		}
		p.id = string(id)
	} else {
		p.addr = fmt.Sprintf("%s:4160", ip)
	}
	return p
}

func (f *Fake) loop() {
	defer f.wg.Done()
	rt := time.NewTimer(f.o.RoundEvery)
	ct := time.NewTicker(f.o.ChurnEvery)
	defer rt.Stop()
	defer ct.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-rt.C:
			f.mu.Lock()
			rt.Reset(f.nextRound())
			if !f.stalled && !f.down {
				f.round++
				f.roundAt = time.Now()
				f.txnCtr += uint64(f.rnd.Intn(80))
				for _, p := range f.peers {
					p.rx += int64(2000 + f.rnd.Intn(30000))
					p.tx += int64(1000 + f.rnd.Intn(20000))
				}
				f.writeLogLocked(map[string]any{"Context": "Agreement", "Round": f.round, "Type": "RoundConcluded", "file": "actions.go", "level": "info",
					"msg": fmt.Sprintf("committed round %d with pre-validated block", f.round)})
				for i := 0; i < f.o.LogTraceRate; i++ {
					f.writeLogLocked(map[string]any{"Context": "Agreement", "Round": f.round, "Type": "VoteAccepted", "file": "trace.go", "level": "info", "msg": "vote accepted"})
				}
				f.cond.Broadcast()
			}
			f.mu.Unlock()
		case <-ct.C:
			f.mu.Lock()
			if !f.down && len(f.peers) > 0 {
				i := f.rnd.Intn(len(f.peers))
				old := f.peers[i]
				reason := []string{"ReadError", "LeastPerformingPeer", "IdleConnection", "PeerNotRespondingToPing"}[f.rnd.Intn(4)]
				f.writeLogLocked(map[string]any{"details": map[string]any{"Address": old.addr, "Incoming": old.dir == "inbound", "Reason": reason, "MessageDelay": 773255546},
					"file": "telemetry.go", "level": "info", "msg": "/Network/DisconnectPeer"})
				if reason == "ReadError" {
					f.writeLogLocked(map[string]any{"file": "wsPeer.go", "level": "warning",
						"msg": fmt.Sprintf("peer[%s] line=523 read err: stream reset: connection closed: read tcp4 192.0.2.10:4190->%s: read: connection reset by peer", old.addr, ipOf(old.addr))})
				}
				// algod keeps its outgoing count, so a lost peer is
				// replaced by one of the same direction and network.
				np := f.newPeerOn(old.net, old.dir)
				f.peers[i] = np
				f.writeLogLocked(map[string]any{"details": map[string]any{"Address": np.addr, "Incoming": np.dir == "inbound"},
					"file": "telemetry.go", "level": "info", "msg": "/Network/ConnectPeer"})
			}
			f.mu.Unlock()
		}
	}
}

func ipOf(addr string) string {
	if strings.HasPrefix(addr, "/ip4/") {
		p := strings.Split(addr, "/")
		if len(p) >= 5 {
			return p[2] + ":" + p[4]
		}
	}
	return addr
}

func (f *Fake) writeLogLocked(m map[string]any) {
	if f.logF == nil {
		return
	}
	m["time"] = time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	b, _ := json.Marshal(m)
	f.logF.Write(append(b, '\n'))
}

// WriteLog appends a raw line to node.log.
func (f *Fake) WriteLog(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logF != nil {
		f.logF.WriteString(strings.TrimRight(line, "\n") + "\n")
	}
}

// Stall stops or restarts round production.
func (f *Fake) Stall(on bool) {
	f.mu.Lock()
	f.stalled = on
	f.mu.Unlock()
}

// SetDown makes every endpoint fail, as if algod were stopped.
func (f *Fake) SetDown(on bool) {
	f.mu.Lock()
	f.down = on
	f.mu.Unlock()
	if f.o.DataDir != "" {
		if on {
			os.Remove(filepath.Join(f.o.DataDir, "algod.pid"))
		} else {
			os.WriteFile(filepath.Join(f.o.DataDir, "algod.pid"), []byte(strconv.Itoa(f.pid)+"\n"), 0o600)
		}
	}
}

// Restart resets every counter and, with newPort, moves to a new port and
// rewrites algod.net as algod does.
func (f *Fake) Restart(newPort bool, newPID int) error {
	f.mu.Lock()
	f.started = time.Now()
	if newPID != 0 {
		f.pid = newPID
	}
	srv := f.srv
	f.mu.Unlock()
	if newPort {
		srv.Close()
		if err := f.listen("127.0.0.1:0"); err != nil {
			return err
		}
	}
	if f.o.DataDir != "" {
		os.WriteFile(filepath.Join(f.o.DataDir, "algod.net"), []byte(f.Addr()+"\n"), 0o600)
		os.WriteFile(filepath.Join(f.o.DataDir, "algod.pid"), []byte(strconv.Itoa(f.pid)+"\n"), 0o600)
	}
	return nil
}

// RotateLog renames node.log to node.archive.log and starts a new file.
func (f *Fake) RotateLog() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logF == nil {
		return nil
	}
	f.logF.Close()
	d := f.o.DataDir
	if err := os.Rename(filepath.Join(d, "node.log"), filepath.Join(d, "node.archive.log")); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(d, "node.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	f.logF = lf
	return nil
}

// Round returns the current round.
func (f *Fake) Round() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.round
}

func (f *Fake) authorized(r *http.Request, admin bool) bool {
	if f.o.DisableAuth && !admin {
		return true
	}
	tok := r.Header.Get("X-Algo-API-Token")
	if admin {
		return f.o.AdminToken == "" || tok == f.o.AdminToken
	}
	return f.o.Token == "" || tok == f.o.Token || tok == f.o.AdminToken
}

// ServeHTTP implements the algod endpoints algodtop uses.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Paths[r.Method+" "+r.URL.Path]++
	if t := r.Header.Get("X-Algo-API-Token"); t != "" && t == f.o.AdminToken {
		f.AdminUsed[r.URL.Path]++
	}
	down := f.down
	f.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "fake algod is read-only", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	admin := p == "/v2/node/peers" || p == "/v2/participation"
	if p != "/metrics" && p != "/health" && p != "/genesis" && !f.authorized(r, admin) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Invalid API Token"}`))
		return
	}
	switch {
	case p == "/metrics":
		f.serveMetrics(w)
	case p == "/health":
		w.Write([]byte("null\n"))
	case p == "/ready":
		f.mu.Lock()
		stalled := f.stalled
		f.mu.Unlock()
		if stalled {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	case p == "/versions":
		f.json(w, map[string]any{"versions": []string{"v2"}, "genesis_id": f.o.Network + "-v1.0",
			"genesis_hash_b64": f.o.GenesisHash,
			"build":            map[string]any{"major": f.o.Version[0], "minor": f.o.Version[1], "build_number": f.o.Version[2], "commit_hash": "fake0000", "branch": f.o.Branch, "channel": f.o.Channel}})
	case p == "/v2/status":
		f.json(w, f.status())
	case strings.HasPrefix(p, "/v2/status/wait-for-block-after/"):
		r0, _ := strconv.ParseUint(strings.TrimPrefix(p, "/v2/status/wait-for-block-after/"), 10, 64)
		deadline := time.Now().Add(time.Minute)
		f.mu.Lock()
		for f.round <= r0 && time.Now().Before(deadline) {
			select {
			case <-f.stop:
				f.mu.Unlock()
				return
			default:
			}
			done := make(chan struct{})
			go func() {
				select {
				case <-time.After(time.Until(deadline)):
				case <-done:
				case <-r.Context().Done():
				}
				f.mu.Lock()
				f.cond.Broadcast()
				f.mu.Unlock()
			}()
			f.cond.Wait()
			close(done)
			if r.Context().Err() != nil {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		f.json(w, f.status())
	case strings.HasPrefix(p, "/v2/blocks/"):
		rn, _ := strconv.ParseUint(strings.TrimPrefix(p, "/v2/blocks/"), 10, 64)
		f.mu.Lock()
		tc := f.txnCtr - (f.round-rn)*40
		ts := f.roundAt.Unix() - int64(f.round-rn)*3
		f.mu.Unlock()
		f.json(w, map[string]any{"block": map[string]any{"rnd": rn, "ts": ts, "tc": tc, "gen": f.o.Network + "-v1.0"}})
	case p == "/v2/transactions/pending":
		f.mu.Lock()
		pool := 100 + f.rnd.Intn(300)
		f.mu.Unlock()
		f.json(w, map[string]any{"top-transactions": []any{}, "total-transactions": pool})
	case p == "/v2/ledger/supply":
		f.json(w, map[string]any{"current_round": f.Round(), "online-money": 2011157800833460, "total-money": 9816694088529129})
	case p == "/v2/node/peers":
		f.json(w, map[string]any{"Peers": f.peerList()})
	case p == "/v2/participation":
		if len(f.keys) == 0 {
			w.Write([]byte("null\n"))
			return
		}
		f.json(w, f.partKeys())
	case strings.HasPrefix(p, "/v2/accounts/"):
		addr := strings.TrimPrefix(p, "/v2/accounts/")
		for _, k := range f.keys {
			if k.acct.Address == addr {
				f.mu.Lock()
				rnd := f.round
				f.mu.Unlock()
				f.json(w, map[string]any{"address": addr, "amount": k.acct.Amount, "status": "Online", "round": rnd, "incentive-eligible": true,
					"last-proposed": rnd - 1200, "participation": map[string]any{"vote-first-valid": k.first, "vote-last-valid": k.last, "vote-participation-key": k.vote}})
				return
			}
		}
		f.json(w, map[string]any{"address": addr, "amount": 1000000, "status": "Offline", "round": f.Round()})
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) status() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"last-round": f.round, "time-since-last-round": time.Since(f.roundAt).Nanoseconds(), "catchup-time": 0,
		"last-version": "v40", "next-version": "v40", "next-version-round": f.round + 1, "next-version-supported": true}
}

func (f *Fake) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (f *Fake) serveMetrics(w http.ResponseWriter) {
	scale := f.counterScale(time.Now())
	var b bytes.Buffer
	b.Grow(len(metricsFixture))
	lastType := ""
	for _, l := range f.lines {
		base := l.text
		if j := strings.IndexByte(base, '{'); j >= 0 {
			base = base[:j]
		}
		if base != lastType {
			kind := "gauge"
			if l.counter {
				kind = "counter"
			}
			fmt.Fprintf(&b, "# TYPE %s %s\n", base, kind)
			lastType = base
		}
		v := l.value
		if l.counter {
			v = float64(int64(v * scale))
		}
		fmt.Fprintf(&b, "%s %s\n", l.text, strconv.FormatFloat(v, 'g', -1, 64))
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write(b.Bytes())
}

func (f *Fake) peerList() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.peers))
	for _, p := range f.peers {
		m := map[string]any{"network-address": p.addr, "connection-type": p.dir, "network-type": p.net}
		if f.o.PeerUsage {
			streams := []string{"/algorand-ws/2.2.0"}
			if p.net == "ws" {
				streams = []string{"ws-gossip/2.2"}
			}
			m["active-streams"] = streams
			m["total-bytes-received"] = p.rx
			m["total-bytes-sent"] = p.tx
			if p.net == "p2p" {
				m["peer-id"] = p.id
				m["supported-protocols"] = []string{"/algorand-ws/2.2.0", "/ipfs/id/1.0.0", "/meshsub/1.1.0"}
			}
		}
		out = append(out, m)
	}
	return out
}

// Requests returns copies of the request counters.
func (f *Fake) Requests() (paths, admin map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths, admin = map[string]int{}, map[string]int{}
	for k, v := range f.Paths {
		paths[k] = v
	}
	for k, v := range f.AdminUsed {
		admin[k] = v
	}
	return paths, admin
}

// partKeys is the /v2/participation answer: one key per account, voting
// in the last few rounds.
func (f *Fake) partKeys() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, k := range f.keys {
		out = append(out, map[string]any{"id": k.id, "address": k.acct.Address,
			"last-vote": f.round - uint64(1+f.rnd.Intn(3)), "last-block-proposal": f.round - 1200,
			"effective-first-valid": k.first, "effective-last-valid": k.last,
			"key": map[string]any{"vote-first-valid": k.first, "vote-last-valid": k.last, "vote-key-dilution": 10000,
				"vote-participation-key": k.vote, "selection-participation-key": k.sel}})
	}
	return out
}

func randKey(r *rand.Rand) string {
	b := make([]byte, 32)
	r.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// Address returns a well-formed Algorand address for a seed: the 32 bytes
// of the seed's SHA-512/256, then the last 4 bytes of their SHA-512/256 as
// the checksum, in unpadded base32.
func Address(seed string) string {
	pk := sha512.Sum512_256([]byte(seed))
	sum := sha512.Sum512_256(pk[:])
	b := append(pk[:], sum[len(sum)-4:]...)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

// nextRound is the time until the next round. With Jitter, rounds vary by
// a few percent and one in 40 is 20 to 70 percent slower, as on Mainnet.
// Call it with f.mu held.
func (f *Fake) nextRound() time.Duration {
	if !f.o.Jitter {
		return f.o.RoundEvery
	}
	d := float64(f.o.RoundEvery) * (0.97 + 0.06*f.rnd.Float64())
	if f.rnd.Intn(40) == 0 {
		d *= 1.2 + 0.5*f.rnd.Float64()
	}
	return time.Duration(d)
}
