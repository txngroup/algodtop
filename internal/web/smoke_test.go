package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/health"
	"algod.top/internal/model"
	"algod.top/internal/query"
	"algod.top/internal/store"
	"algod.top/internal/web"
)

// testHub serves a fixed dataset: a temporary store with a few hours of
// synthetic samples for two nodes, and fixed live status.
type testHub struct {
	db    *store.DB
	q     *query.Engine
	nodes []api.NodeView
	start time.Time
	mu    sync.Mutex
	sils  []model.Silence
}

func (h *testHub) Nodes() []api.NodeView { return h.nodes }
func (h *testHub) Node(name string) (api.NodeView, bool) {
	for _, n := range h.nodes {
		if n.Name == name {
			return n, true
		}
	}
	return api.NodeView{}, false
}
func (h *testHub) Status() api.DaemonStatus {
	return api.DaemonStatus{Host: "smoke", Version: "test", Platform: "test/test", Started: h.start.UnixNano(),
		StoreBytes: 1 << 20, TierBytes: map[string]int64{"raw": 1 << 19}, Retention: map[string]string{"raw": "30d"}}
}
func (h *testHub) Store() *store.DB     { return h.db }
func (h *testHub) Query() *query.Engine { return h.q }
func (h *testHub) Reload() error        { return nil }
func (h *testHub) Snapshot() (string, error) {
	return "", nil
}
func (h *testHub) Silences() []model.Silence {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]model.Silence(nil), h.sils...)
}
func (h *testHub) AddSilence(s model.Silence) (model.Silence, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.ID = fmt.Sprintf("s%d", len(h.sils)+1)
	h.sils = append(h.sils, s)
	return s, nil
}
func (h *testHub) DeleteSilence(id string) error { return nil }
func (h *testHub) Discovered() []api.DiscoveredNode {
	return []api.DiscoveredNode{{Name: "spare", DataDir: "/tmp/spare", Network: "testnet"}}
}
func (h *testHub) ConfigJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"config":   map[string]any{"nodes": []map[string]any{{"name": "alpha", "datadir": "/data/alpha", "token": "(set)"}, {"name": "beta", "url": "http://10.0.0.2:8080"}}, "retention": map[string]string{"raw": "30d"}, "channels": map[string]any{"phone": map[string]string{"type": "webhook"}}},
		"editable": true,
		"path":     "/etc/algodtop.json",
	})
}
func (h *testHub) PutConfig(ctx context.Context, b []byte) error { return nil }
func (h *testHub) AgentBatches(ctx context.Context, since string, wait time.Duration) ([]byte, string, error) {
	return nil, "", nil
}

// Subscribe sends one status and one event, then ends the stream. The
// browser reconnects after the retry interval, so a headless run with a
// virtual time budget never waits on an open stream.
func (h *testHub) Subscribe(nodes map[string]bool, series []api.Sub) (<-chan api.Message, func()) {
	ch := make(chan api.Message, 2)
	ch <- api.Message{Event: "status", Data: map[string]any{"nodes": h.nodes, "status": h.Status(), "time": time.Now().UnixNano()}}
	ch <- api.Message{Event: "event", Data: model.Event{Time: time.Now().UnixNano(), Node: "alpha", Kind: model.EvRestart, Severity: model.SevWarn, Msg: "algod restarted"}}
	close(ch)
	return ch, func() {}
}

func newHub(t *testing.T) *testHub {
	t.Helper()
	real := time.Now()
	start := real.Add(-4 * time.Hour).Truncate(time.Minute)
	var cur time.Time
	var filled bool
	clock := func() time.Time {
		if filled {
			return time.Now()
		}
		return cur
	}
	db, err := store.Open(store.Options{Dir: t.TempDir(), Now: clock, Retention: store.DefaultRetention})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	gapFrom, gapTo := start.Add(150*time.Minute), start.Add(160*time.Minute)
	les := []string{"0.01", "0.1", "1", "+Inf"}
	i := 0
	for cur = start; cur.Before(real); cur = cur.Add(10 * time.Second) {
		i++
		if !cur.Before(gapFrom) && cur.Before(gapTo) {
			continue // algodtop was down
		}
		for ni, node := range []string{"alpha", "beta"} {
			f := float64(i)
			k := float64(ni + 1)
			ts := cur.UnixNano()
			wave := math.Sin(f / 40)
			batch := func(g model.Group, ss ...model.Sample) {
				if err := db.Append(&model.Batch{Node: node, Group: g, Time: ts, Samples: ss}); err != nil {
					t.Fatal(err)
				}
			}
			lbl := func(kv ...string) model.Labels {
				var l model.Labels
				for j := 0; j+1 < len(kv); j += 2 {
					l = append(l, model.Label{Name: kv[j], Value: kv[j+1]})
				}
				l.Sort()
				return l
			}
			g := func(name string, v float64, l ...string) model.Sample {
				return model.Sample{Name: name, Labels: lbl(l...), Kind: model.KindGauge, Value: v}
			}
			c := func(name string, v float64, l ...string) model.Sample {
				return model.Sample{Name: name, Labels: lbl(l...), Kind: model.KindCounter, Value: v}
			}
			batch(model.GroupRound,
				g("algodtop_round", 1000+f*3.6),
				g("algodtop_round_time_seconds", 2.75+0.1*wave),
				g("algodtop_block_txns", 20+10*wave*k))
			batch(model.GroupREST,
				g("algodtop_up", 1),
				g("algodtop_sync_lag_seconds", math.Mod(f, 3)),
				g("algodtop_txpool_size", 50+20*wave))
			batch(model.GroupPeers,
				g("algodtop_peers", 4, "dir", "out", "net", "ws"),
				g("algodtop_peers", 2*k, "dir", "in", "net", "ws"),
				g("algodtop_peers", 40+math.Round(3*wave), "dir", "in", "net", "p2p"),
				g("algodtop_peers", 48, "dir", "out", "net", "p2p"))
			ms := []model.Sample{
				c("algod_network_received_bytes_total", f*40e3*k),
				c("algod_network_sent_bytes_total", f*30e3*k),
				c("algod_network_p2p_received_bytes_total", f*300e3*k),
				c("algod_network_p2p_sent_bytes_total", f*700e3*k),
				c("algod_agreement_handled_vote", f*15000),
				c("algod_agreement_handled_proposal", f*90),
				c("algod_network_p2p_inbound_evictions_total", math.Floor(f/30), "state", "busy"),
				g("libp2p_io_dht_kad_network_size", 100),
				g("libp2p_rcmgr_streams", 85, "dir", "inbound", "protocol", "/meshsub/1.3.0", "scope", "protocol"),
				g("libp2p_rcmgr_streams", 84, "dir", "outbound", "protocol", "/meshsub/1.3.0", "scope", "protocol"),
			}
			for j, le := range les {
				ms = append(ms, model.Sample{Name: "libp2p_swarm_dial_latency_seconds_bucket", Labels: lbl("le", le), Kind: model.KindHistPart, Value: f * float64(j+1)})
			}
			batch(model.GroupMetrics, ms...)
			batch(model.GroupProc,
				c("proc_cpu_seconds_total", f*2.2, "method", "test"),
				g("proc_resident_bytes", 1.1e9+5e7*wave, "method", "test"),
				g("proc_open_handles", 150, "method", "test"),
				g("proc_threads", 30, "method", "test"))
			batch(model.GroupNet,
				c("proc_net_bytes_total", f*400e3*k, "dir", "in", "method", "test"),
				c("proc_net_bytes_total", f*800e3*k, "dir", "out", "method", "test"))
			batch(model.GroupHost,
				g("host_disk_free_bytes", 3e11-f*1e5),
				g("host_disk_total_bytes", 1e12),
				g("host_datadir_bytes", 3e10, "sub", "mainnet-v1.0"),
				g("host_datadir_bytes", 8e9, "sub", "(files)"),
				g("host_load1", 4+wave))
			batch(model.GroupLog,
				c("log_template_lines_total", f*3, "level", "info", "template", "finished round <n>"),
				c("log_template_lines_total", math.Floor(f/50), "level", "warning", "template", "peer <addr> stale"),
				c("log_lines_total", f*20, "level", "info"),
				c("log_lines_total", math.Floor(f/50), "level", "warning"))
			if ni == 0 && i%3 == 0 {
				var us []model.PeerUsage
				for p := 0; p < 7; p++ {
					us = append(us, model.PeerUsage{Time: ts, Key: fmt.Sprintf("peer%d", p), Network: "p2p", Direction: "in",
						Addr: fmt.Sprintf("/ip4/10.0.0.%d/tcp/4190", p), BytesIn: int64(f * float64(1000*(p+1))), BytesOut: int64(f * 500)})
				}
				if err := db.AppendUsage(node, us); err != nil {
					t.Fatal(err)
				}
			}
		}
		if i%60 == 0 {
			db.Tick()
		}
	}
	db.Tick()
	filled = true

	ev := func(e model.Event) {
		if err := db.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	ev(model.Event{Time: gapTo.UnixNano(), Kind: model.EvMonitorGap, Severity: model.SevWarn, Msg: "algodtop was down for 10m0s",
		Attrs: map[string]any{"from": gapFrom.UnixNano(), "to": gapTo.UnixNano()}})
	down := real.Add(-50 * time.Minute)
	ev(model.Event{Time: down.UnixNano(), Node: "beta", Kind: model.EvNodeDown, Severity: model.SevCritical, Msg: "algod is down: test"})
	ev(model.Event{Time: down.Add(4 * time.Minute).UnixNano(), Node: "beta", Kind: model.EvNodeUp, Severity: model.SevInfo, Msg: "algod is up"})
	ev(model.Event{Time: real.Add(-30 * time.Minute).UnixNano(), Node: "alpha", Kind: model.EvRestart, Severity: model.SevWarn, Msg: "algod restarted"})
	ev(model.Event{Time: real.Add(-20 * time.Minute).UnixNano(), Node: "alpha", Kind: model.EvLogWarn, Severity: model.SevWarn, Msg: "peer stale"})
	clusterAt := real.Add(-40 * time.Minute)
	for p := 0; p < 8; p++ {
		ip := fmt.Sprintf("192.0.2.%d", p+1)
		end := clusterAt.Add(time.Duration(p) * time.Second)
		ev(model.Event{Time: end.UnixNano(), Node: "alpha", Kind: model.EvDisconnect, Severity: model.SevInfo, Msg: "disconnect in p2p " + ip + " reason=gone",
			Attrs: map[string]any{"ip": ip, "network": "p2p", "direction": "in", "reason": "gone"}})
		if err := db.AppendSession(model.Session{Node: "alpha", Network: "p2p", Direction: "in", Addr: "/ip4/" + ip + "/tcp/4190", IP: ip,
			Start: end.Add(-20 * time.Minute).UnixNano(), End: end.UnixNano(), Reason: "gone", Source: "log"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.PutAnnotation(model.Annotation{Node: "alpha", Start: real.Add(-90 * time.Minute).UnixNano(), End: real.Add(-80 * time.Minute).UnixNano(), Text: "run 2", Created: real.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendLogLine(store.LogLine{Time: real.Add(-20 * time.Minute).UnixNano(), Node: "alpha", Level: "warning", Template: "peer <addr> stale", Line: `{"level":"warning","msg":"peer 1.2.3.4 stale"}`}); err != nil {
		t.Fatal(err)
	}

	st := func(name string, up bool) collector.State {
		return collector.State{Name: name, Kind: "local", Network: "testnet", Up: up, Healthy: up, Ready: up, Reachable: up,
			Version: "5.1.0", Round: 1000 + uint64(i)*4, RoundAt: real.UnixNano(), RoundTime: 2.75, RoundP99: 2.9,
			Peers: collector.PeerCounts{WSIn: 2, WSOut: 4, P2PIn: 40, P2POut: 48}, CPU: 22, RSS: 1.1e9,
			WSIn: 4e3, WSOut: 3e3, P2PIn: 3e4, P2POut: 7e4, DiskFree: 3e11, DiskTotal: 1e12,
			StartTime: real.Add(-2 * time.Hour).UnixNano(), UpSince: real.Add(-2 * time.Hour).UnixNano()}
	}
	alpha := st("alpha", true)
	alpha.PeerUsage = true
	alpha.Keys = []collector.PartKeyState{{ID: "k1", Address: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", First: 1, Last: 2000000, RoundsLeft: 1500000, DaysLeft: 6.2, LastVote: 990, Active: true},
		{ID: "k0", Address: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", First: 1, Last: 1100, RoundsLeft: 100, DaysLeft: 0.3, LastVote: 800, Superseded: true}}
	alpha.Accounts = []collector.AccountState{{Address: "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", Status: "Online", KeyMatches: true, IncentiveEligible: true, ExpectedInterval: 400, RoundsSinceVote: 10}}
	alpha.PeerList = []collector.PeerInfo{{Addr: "/ip4/10.0.0.1/tcp/4190", IP: "10.0.0.1", Network: "p2p", Direction: "in", Since: real.Add(-time.Hour).UnixNano(), PeerID: "12D3KooWtest", RateIn: 2000, RateOut: 900, HasUsage: true, Streams: []string{"/meshsub/1.3.0"}}}
	beta := st("beta", true)
	nodes := []api.NodeView{
		{State: alpha, Health: health.Status{Level: health.Warn, Reasons: []health.Reason{{Rule: "key-expiry", Level: health.Warn, Text: "participation key for ABCDEF... expires in 6 days"}}}, Summary: "Warn: participation key for ABCDEF... expires in 6 days", Lag: 0.4, History: true},
		// Several reasons at mixed levels, which the UI lists one per line.
		{State: beta, Health: health.Status{Level: health.Critical, Reasons: []health.Reason{
			{Rule: "key_expiry", Level: health.Critical, Text: "participation key for AAAAAA... expires in 20 hours"},
			{Rule: "key_expiry", Level: health.Critical, Text: "participation key for BBBBBB... expires in 22 hours"},
			{Rule: "version_behind", Level: health.Warn, Text: "version 5.1.0 is behind 5.1.2 on alpha, another testnet stable node"},
		}}, Summary: "Critical: participation key for AAAAAA... expires in 20 hours; participation key for BBBBBB... expires in 22 hours", Lag: 1.1, History: true},
	}
	return &testHub{db: db, q: query.NewEngine(db, 2, 10*time.Second), nodes: nodes, start: start}
}

func findChrome() string {
	for _, p := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// consoleErrors returns the lines of Chrome's log that report a script
// error, or an SVG attribute the page set to NaN or undefined.
func consoleErrors(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		switch {
		case strings.Contains(l, "CONSOLE") && strings.Contains(l, "Uncaught"),
			strings.Contains(l, "ERROR:CONSOLE"),
			strings.Contains(l, "CONSOLE") && strings.Contains(l, "Error: <"):
			out = append(out, l)
		}
	}
	return out
}

func TestSmokePages(t *testing.T) {
	hub := newHub(t)
	srv := httptest.NewServer(api.New(api.Options{Hub: hub, Static: web.FS}))
	// Cleanup, not defer: the parallel subtests run after this function
	// returns.
	t.Cleanup(srv.Close)

	// The API answers for the fixed dataset before any browser runs.
	for _, p := range []string{"/", "/node/alpha", "/js/app.js", "/css/app.css", "/api/nodes", "/api/series?node=alpha&metric=algodtop_peers&from=-1h&points=50"} {
		res, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: %s", p, res.Status)
		}
	}

	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chrome or Chromium found; skipping the headless page check")
	}
	// A listener with a password shows the login form to a new browser.
	hash, err := api.HashPassword("smoke")
	if err != nil {
		t.Fatal(err)
	}
	authSrv := httptest.NewServer(api.New(api.Options{Hub: hub, Auth: api.NewAuth(api.AuthConfig{PasswordHash: hash}), Static: web.FS}))
	t.Cleanup(authSrv.Close)

	pages := []struct{ path, id string }{
		{authSrv.URL + "/node/alpha", "login"},
		{"/", "fleet"},
		{"/?view=table", "fleet"},
		{"/node/alpha", "node"},
		{"/node/alpha?r=6h&raw=1&closed=consensus", "node"},
		{"/node/beta?r=24h&closed=consensus,ws,p2p", "node"},
		{"/peers/alpha?cl=1", "peers"},
		{"/events?kind=disconnect", "events"},
		{"/compare", "compare"},
		{"/compare?mode=nodes&node=alpha&node2=beta&r=6h", "compare"},
		{"/explorer?node=alpha&metric=algodtop_peers", "explorer"},
		{"/explorer?node=alpha&metric=libp2p_swarm_dial_latency_seconds_bucket&fn=heatmap", "explorer"},
		{"/settings", "settings"},
	}
	sem := make(chan struct{}, 4)
	for _, pg := range pages {
		url := pg.path
		if strings.HasPrefix(url, "/") {
			url = srv.URL + url
		}
		name := strings.TrimPrefix(pg.path, "/")
		if !strings.HasPrefix(pg.path, "/") {
			name = "login"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
				"--user-data-dir="+profileDir(t), "--window-size=1280,2000", "--enable-logging=stderr", "--v=0",
				"--virtual-time-budget=5000", "--dump-dom", url)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("chrome: %v\n%s", err, stderr.String())
			}
			dom := stdout.String()
			if dir := os.Getenv("ALGODTOP_SMOKE_DUMP"); dir != "" {
				// For debugging: keep each page's DOM and Chrome log.
				file := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "-", ",", "-").Replace(name)
				os.WriteFile(dir+"/"+file+".html", stdout.Bytes(), 0o644)
				os.WriteFile(dir+"/"+file+".log", stderr.Bytes(), 0o644)
			}
			if dir := os.Getenv("ALGODTOP_SMOKE_SHOT"); dir != "" {
				// For a visual check: a screenshot of each page.
				file := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "-", ",", "-").Replace(name)
				exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--hide-scrollbars",
					"--user-data-dir="+profileDir(t), "--window-size=1280,1600", "--virtual-time-budget=5000",
					"--screenshot="+dir+"/"+file+".png", url).Run()
			}
			if errs := consoleErrors(stderr.String()); len(errs) > 0 {
				t.Errorf("console errors on %s:\n%s", pg.path, strings.Join(errs, "\n"))
			}
			if !strings.Contains(dom, `id="`+pg.id+`"`) {
				t.Errorf("%s: DOM lacks the main element #%s:\n%.2000s\nchrome log:\n%s", pg.path, pg.id, dom, tail(stderr.String(), 3000))
			}
			if strings.Contains(dom, "This page failed to load") {
				t.Errorf("%s: the page reported a load failure", pg.path)
			}
			if pg.id == "fleet" && strings.Count(dom, `<li class="critical">`) < 2 {
				t.Errorf("%s: health reasons are not listed one per line", pg.path)
			}
		})
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// profileDir returns a short temporary directory for a Chrome profile.
// Chrome puts a unix socket in it, and macOS limits socket paths to 104
// bytes, which a t.TempDir path can exceed.
func profileDir(t *testing.T) string {
	base := ""
	if len(os.TempDir()) > 40 {
		if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
			base = "/tmp"
		}
	}
	d, err := os.MkdirTemp(base, "algodtop-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
