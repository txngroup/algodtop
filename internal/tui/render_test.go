package tui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/health"
	"algod.top/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixture() *Model {
	now := time.Date(2026, 11, 3, 10, 52, 14, 0, time.UTC)
	ok := health.Status{Level: health.OK}
	warn := health.Status{Level: health.Warn, Reasons: []health.Reason{{Rule: "key_expiry_warn", Level: health.Warn, Text: "participation key for ABCDEF... expires in 6 days"},
		{Rule: "version_behind", Level: health.Warn, Text: "version 4.4.0 is behind 4.4.1 on mainnet, another mainnet stable node"}}}
	down := health.Status{Level: health.Critical, Reasons: []health.Reason{{Rule: "down", Level: health.Critical, Text: "algod is down (no live algod.pid)"}}}
	mk := func(name, netw, ver string, round uint64, lag, rt float64, p collector.PeerCounts, cpu, rss, in, out float64, h health.Status, up bool) api.NodeView {
		st := collector.State{Name: name, Network: netw, Version: ver, Round: round, RoundTime: rt, Peers: p, CPU: cpu, RSS: rss,
			NetIn: in, NetOut: out, NetMethod: "nettop", Up: up, Healthy: up, HasAdmin: true, Kind: "local", TxnsPerBlk: 41, PoolSize: 312,
			RoundAt: now.Add(-time.Duration(lag * float64(time.Second))).UnixNano(), UpSince: now.Add(-50 * time.Hour).UnixNano()}
		return api.NodeView{State: st, Health: h, Summary: h.Summary(), Lag: lag}
	}
	nodes := []api.NodeView{
		mk("mainnet", "mainnet", "4.4.1", 52184330, 0.6, 2.79, collector.PeerCounts{WSIn: 12, WSOut: 4, P2PIn: 38, P2POut: 4}, 6.1, 2.1e9, 410*1024, 380*1024, ok, true),
		mk("testnet", "testnet", "4.4.1", 61022817, 0.4, 2.80, collector.PeerCounts{WSIn: 8, WSOut: 4, P2PIn: 11, P2POut: 4}, 2.3, 1.4e9, 95*1024, 90*1024, ok, true),
		mk("localnet", "localnet", "5.1.0", 9330871, 0.5, 2.01, collector.PeerCounts{WSIn: 2, WSOut: 4, P2PIn: 6, P2POut: 4}, 1.9, 1.1e9, 40*1024, 38*1024, ok, true),
		mk("mainnet-2", "mainnet", "4.4.0", 52184329, 0.8, 2.79, collector.PeerCounts{WSIn: 4, WSOut: 4}, 1.2, 0.9e9, 30*1024, 28*1024, warn, true),
		mk("betanet", "betanet", "", 0, 0, 0, collector.PeerCounts{}, 0, 0, 0, 0, down, false),
	}
	nodes[0].PeerList = []collector.PeerInfo{
		{Addr: "/ip4/203.0.113.4/tcp/4190", IP: "203.0.113.4", Network: "p2p", Direction: "in", Since: now.Add(-3 * time.Hour).UnixNano(), HasUsage: true, RateIn: 20480, RateOut: 10240, Streams: []string{"/algorand-ws/2.2.0"}},
		{Addr: "relay.example.net:4160", IP: "relay.example.net", Network: "ws", Direction: "out", Since: now.Add(-26 * time.Hour).UnixNano()},
	}
	nodes[0].PeerUsage = true
	evs := []model.Event{
		{Time: now.Add(-20 * time.Minute).UnixNano(), Node: "mainnet-2", Kind: "log-warn", Severity: model.SevWarn, Msg: "participation key for ABC... expires in 6 days"},
		{Time: now.Add(-5 * time.Minute).UnixNano(), Node: "mainnet", Kind: model.EvDisconnect, Severity: model.SevInfo, Msg: "",
			Attrs: map[string]any{"direction": "in", "network": "p2p", "ip": "1.2.3.4", "reason": "reset"}},
	}
	return &Model{W: 120, H: 20, Now: now, UTC: true, Style: Style{}, Nodes: nodes, Events: evs, Sel: 0,
		Status: api.DaemonStatus{Host: "algod-host", Started: now.Add(-76 * time.Hour).UnixNano(), StoreBytes: 2100 << 20, Platform: "darwin/arm64"},
		Spark:  map[string][]float64{"mainnet|rtime": {2.7, 2.8, 2.9, 2.7, 3.1, 2.8, 2.6, 2.7}}, At: map[string]Point{}}
}

func golden(t *testing.T, name string, lines []string) {
	t.Helper()
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/tui -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from golden file:\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

func TestMainScreen(t *testing.T) {
	m := fixture()
	golden(t, "main", Render(m))
}

// A node with several reasons lists them one per line.
func TestNodeScreenReasons(t *testing.T) {
	m := fixture()
	m.Screen, m.Node, m.H = ScreenNode, "mainnet-2", 8
	golden(t, "node-reasons", Render(m))
}

func TestNodeScreen(t *testing.T) {
	m := fixture()
	m.Screen, m.Node, m.H = ScreenNode, "mainnet", 30
	golden(t, "node", Render(m))
}

func TestTimeCursor(t *testing.T) {
	m := fixture()
	m.Cursor = time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC).UnixNano()
	m.At["mainnet"] = Point{Round: 52181000, Lag: 0.5, RTime: 2.75, CPU: 5, RSS: 2e9, NetIn: 300 * 1024, NetOut: 280 * 1024, Up: 1,
		Peers: collector.PeerCounts{WSIn: 10, WSOut: 4, P2PIn: 30, P2POut: 4}}
	lines := Render(m)
	if !strings.Contains(lines[1], "at 2026-11-03 09:30:00") {
		t.Fatalf("header does not show the cursor: %q", lines[1])
	}
	golden(t, "cursor", lines)
}

func TestHelpAndEmpty(t *testing.T) {
	m := fixture()
	m.Screen = ScreenHelp
	golden(t, "help", Render(m))
	m.Screen = ScreenEmpty
	golden(t, "empty", Render(m))
}

func TestSparkASCIIAndGaps(t *testing.T) {
	s := Style{}
	got := s.Spark([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 8, true)
	if got != "_.-~=+*#" {
		t.Fatalf("ascii spark %q", got)
	}
	nan := []float64{1, 2}
	nan = append(nan, nanVal(), nanVal(), 3, 4)
	if g := s.Spark(nan, 6, true); g[2] != ' ' || g[3] != ' ' {
		t.Fatalf("gap drawn as a value: %q", g)
	}
	u := Style{Unicode: true}
	if r := []rune(u.Spark([]float64{0, 10}, 2, true)); r[1] != '\u2588' {
		t.Fatalf("unicode spark %q", string(r))
	}
}

func nanVal() float64 { var z float64; return z / z }

func TestBatchJSONAndTable(t *testing.T) {
	m := fixture()
	var b strings.Builder
	src := &fakeSource{nodes: m.Nodes, st: m.Status}
	if err := Batch(&b, BatchOptions{Source: src, Width: 120}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "mainnet") || !strings.Contains(b.String(), "DOWN") {
		t.Fatalf("batch output:\n%s", b.String())
	}
	b.Reset()
	Batch(&b, BatchOptions{Source: src, JSON: true})
	if !strings.HasPrefix(b.String(), `{"time":`) {
		t.Fatalf("json output %q", b.String())
	}
}

// A node name longer than the old fixed column must not run into the
// event kind.
func TestEventLineLongNodeName(t *testing.T) {
	m := fixture()
	m.Nodes = append(m.Nodes, api.NodeView{State: collector.State{Name: "mainnet-archive"}})
	e := model.Event{Time: m.Now.UnixNano(), Node: "mainnet-archive", Kind: model.EvDisconnect,
		Attrs: map[string]any{"direction": "in", "network": "p2p", "ip": "203.0.113.7", "reason": "ReadError"}}
	if line := m.eventLine(e, true); !strings.Contains(line, "mainnet-archive  disconnect") {
		t.Fatalf("%q", line)
	}
}

// An event kind longer than the old 12-cell column must keep its name and
// a gap before the message.
func TestEventLineLongKind(t *testing.T) {
	m := fixture()
	for _, k := range []string{model.EvAlertRepeated, model.EvConfigEffective, model.EvRetention} {
		e := model.Event{Time: m.Now.UnixNano(), Node: "mainnet-1", Kind: k, Msg: "key-expiry: soon"}
		if line := m.eventLine(e, true); !strings.Contains(line, k+"  ") && !strings.Contains(line, k+" key-expiry") {
			t.Fatalf("%q", line)
		}
	}
}
