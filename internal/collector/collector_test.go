package collector

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/fakealgod"
	"algod.top/internal/logparse"
	"algod.top/internal/model"
)

type recSink struct {
	mu       sync.Mutex
	batches  []*model.Batch
	events   []model.Event
	sessions []model.Session
	usage    []model.PeerUsage
	lines    []LogLine
	ann      []model.Annotation
}

func (s *recSink) Batch(b *model.Batch) { s.mu.Lock(); s.batches = append(s.batches, b); s.mu.Unlock() }
func (s *recSink) Event(e model.Event)  { s.mu.Lock(); s.events = append(s.events, e); s.mu.Unlock() }
func (s *recSink) Session(x model.Session) {
	s.mu.Lock()
	s.sessions = append(s.sessions, x)
	s.mu.Unlock()
}
func (s *recSink) Usage(_ string, us []model.PeerUsage) {
	s.mu.Lock()
	s.usage = append(s.usage, us...)
	s.mu.Unlock()
}
func (s *recSink) LogLine(l LogLine)           { s.mu.Lock(); s.lines = append(s.lines, l); s.mu.Unlock() }
func (s *recSink) Annotate(a model.Annotation) { s.mu.Lock(); s.ann = append(s.ann, a); s.mu.Unlock() }

func (s *recSink) count(g model.Group) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.batches {
		if b.Group == g {
			n++
		}
	}
	return n
}

func (s *recSink) hasEvent(kind string) *model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.events {
		if s.events[i].Kind == kind {
			e := s.events[i]
			return &e
		}
	}
	return nil
}

func (s *recSink) resetBatches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.batches {
		if b.Reset {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLocalNodeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: dir, RoundEvery: 200 * time.Millisecond, ChurnEvery: 300 * time.Millisecond,
		Token: "tok", AdminToken: "admintok", PeerUsage: true, Peers: 8, LogTraceRate: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	sink := &recSink{}
	n := NewNode(Options{Name: "fake", Source: &LocalSource{Dir: dir}, Sink: sink, Scrape: 200 * time.Millisecond,
		PeersEvery: 200 * time.Millisecond, StateDir: t.TempDir(), Logs: LogOptions{Backfill: true}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "metrics", 5*time.Second, func() bool { return sink.count(model.GroupMetrics) >= 2 })
	waitFor(t, "rounds", 5*time.Second, func() bool { return sink.count(model.GroupRound) >= 3 })
	waitFor(t, "peers", 5*time.Second, func() bool { return sink.count(model.GroupPeers) >= 2 })
	st := n.State()
	if !st.Up || st.Version != "4.4.1" || st.Round == 0 || st.Peers.Total() != 8 || !st.PeerUsage {
		t.Fatalf("state %+v", st)
	}
	if st.RoundTime <= 0 || st.RoundTime > 1 {
		t.Fatalf("round time %v", st.RoundTime)
	}
	// Log-derived connect and disconnect events from peer churn.
	waitFor(t, "disconnect events", 5*time.Second, func() bool { return sink.hasEvent(model.EvDisconnect) != nil })
	waitFor(t, "sessions", 5*time.Second, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.sessions) > 0 })

	// Restart with a new port and PID: a reset marker on the next scrape.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"P2PHybridIncomingConnectionsLimit": 40}`), 0o600)
	waitFor(t, "config pending", 5*time.Second, func() bool { return sink.hasEvent(model.EvConfigPending) != nil })
	if err := fake.Restart(true, 424242); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restart", 5*time.Second, func() bool { return sink.hasEvent(model.EvRestart) != nil })
	waitFor(t, "reset batch", 5*time.Second, func() bool { return sink.resetBatches() >= 1 })
	waitFor(t, "config effective", 5*time.Second, func() bool { return sink.hasEvent(model.EvConfigEffective) != nil })
	ce := sink.hasEvent(model.EvConfigEffective)
	if !strings.Contains(ce.Msg, "P2PHybridIncomingConnectionsLimit") {
		t.Fatalf("config effective msg %q", ce.Msg)
	}
	waitFor(t, "metrics after restart", 5*time.Second, func() bool { return n.State().Up && n.State().PID == 424242 })

	// Log rotation.
	before := sink.count(model.GroupLog)
	if err := fake.RotateLog(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rotation event", 5*time.Second, func() bool { return sink.hasEvent(model.EvLogRotation) != nil })
	_ = before

	// Down: algod.pid disappears.
	fake.SetDown(true)
	waitFor(t, "down", 5*time.Second, func() bool { return !n.State().Up })
	if sink.hasEvent(model.EvNodeDown) == nil {
		t.Fatal("no node-down event")
	}
	fake.SetDown(false)
	waitFor(t, "up again", 5*time.Second, func() bool { return n.State().Up })

	// Read-only: every request is a GET to an allowlisted path, and the
	// admin token reaches only the admin endpoints.
	paths, admin := fake.Requests()
	for p := range paths {
		method, path, _ := strings.Cut(p, " ")
		if method != "GET" || !algod.Allowed(path) {
			t.Errorf("request %s is not an allowlisted GET", p)
		}
	}
	for p := range admin {
		if !algod.IsAdminPath(p) {
			t.Errorf("admin token sent to %s", p)
		}
	}
	if admin["/v2/node/peers"] == 0 {
		t.Error("peers never polled with the admin token")
	}
}

func TestRESTNodeRestartHeuristic(t *testing.T) {
	fake, err := fakealgod.Start(fakealgod.Options{RoundEvery: 200 * time.Millisecond, DisableAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	sink := &recSink{}
	n := NewNode(Options{Name: "r", Source: &RESTSource{URL: fake.URL()}, Sink: sink, Scrape: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	// Let counters grow, then restart: most counters drop at once.
	time.Sleep(1500 * time.Millisecond)
	fake.Restart(false, 0)
	waitFor(t, "restart by counters", 5*time.Second, func() bool { return sink.hasEvent(model.EvRestart) != nil })
	if sink.resetBatches() == 0 {
		t.Fatal("no reset batch")
	}
	// Stall: lag grows, readiness drops.
	fake.Stall(true)
	waitFor(t, "not ready", 5*time.Second, func() bool { return !n.State().Ready })
}

func TestAllowlist(t *testing.T) {
	for _, p := range []string{"/v2/accounts/X/transactions", "/v2/shutdown", "/v2/catchup/x", "/v2/participation/generate/x", "/v2/transactions"} {
		if algod.Allowed(p) && !strings.HasPrefix(p, "/v2/accounts/") {
			t.Errorf("%s allowed", p)
		}
	}
	c, _ := algod.NewClient("127.0.0.1:1", algod.Options{})
	if _, err := c.Get(context.Background(), "/v2/shutdown", nil); err == nil {
		t.Fatal("shutdown allowed")
	}
}

func TestSNTPEncoding(t *testing.T) {
	now := time.Now()
	b := make([]byte, 8)
	putNTP(b, now)
	got := getNTP(b)
	if d := got.Sub(now); d > time.Microsecond || d < -time.Microsecond {
		t.Fatalf("ntp round trip off by %v", d)
	}
}

// Saving node state while the tailer closes minutes and records sessions
// must not deadlock: both paths take the node lock and their own lock.
func TestSaveWhileTailingDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: dir, RoundEvery: 50 * time.Millisecond, ChurnEvery: 30 * time.Millisecond,
		AdminToken: "a", DisableAuth: true, Peers: 10, LogTraceRate: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	n := NewNode(Options{Name: "busy", Source: &LocalSource{Dir: dir}, Sink: &recSink{}, Scrape: 100 * time.Millisecond,
		PeersEvery: 100 * time.Millisecond, StateDir: t.TempDir(), Logs: LogOptions{Backfill: true}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()
	stop := time.After(3 * time.Second)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				n.savePersisted()
				n.State()
			}
		}
	}()
	time.Sleep(3500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("node did not stop: a lock order deadlock")
	}
}

type roundRec struct {
	round uint64
	gap   float64 // round time, 0 when none was recorded
}

func (s *recSink) rounds() []roundRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []roundRec
	for _, b := range s.batches {
		if b.Group != model.GroupRound {
			continue
		}
		var r roundRec
		for _, x := range b.Samples {
			switch x.Name {
			case "algodtop_round":
				r.round = uint64(x.Value)
			case "algodtop_round_time_seconds":
				r.gap = x.Value
			}
		}
		out = append(out, r)
	}
	return out
}

// A restarted daemon restores the last round from its state file. Its
// first long-poll asks about a round that is already past, so the answer
// comes back at once and is not an arrival time. The next round must not
// get a round time measured from it.
func TestRestartSkipsTheFirstGap(t *testing.T) {
	fake, err := fakealgod.Start(fakealgod.Options{RoundEvery: 200 * time.Millisecond, DisableAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	stateDir := t.TempDir()
	run := func(sink *recSink, what string, n int) {
		t.Helper()
		node := NewNode(Options{Name: "r", Source: &RESTSource{URL: fake.URL()}, Sink: sink, Scrape: 200 * time.Millisecond, StateDir: stateDir})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { node.Run(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		waitFor(t, what, 10*time.Second, func() bool { return len(sink.rounds()) >= n })
	}
	first := &recSink{}
	run(first, "rounds", 3)
	rs := first.rounds()
	saved := rs[len(rs)-1].round
	waitFor(t, "rounds while stopped", 5*time.Second, func() bool { return fake.Round() > saved+1 })

	second := &recSink{}
	run(second, "rounds after restart", 4)
	rs = second.rounds()
	if rs[0].round <= saved+1 {
		t.Fatalf("first round after restart is %d, saved %d", rs[0].round, saved)
	}
	for _, r := range rs[:2] {
		if r.gap != 0 {
			t.Errorf("round %d has a round time of %.3fs after the restart", r.round, r.gap)
		}
	}
	if rs[2].round != rs[1].round+1 || rs[2].gap < 0.1 || rs[2].gap > 0.4 {
		t.Errorf("round %d after %d has round time %.3fs, want about 0.2s", rs[2].round, rs[1].round, rs[2].gap)
	}
}

// algod answers /metrics and /health without a token, so a node whose
// token is missing or wrong looks up and well. Its round is unknown: the
// state says why, and the lag runs from the first failure.
func TestRejectedTokenIsNotOK(t *testing.T) {
	dir := t.TempDir()
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: dir, RoundEvery: 200 * time.Millisecond, Token: "tok", AdminToken: "admintok"})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	tokenFile := filepath.Join(dir, "algod.token")
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		src  Source
		want string
	}{
		{"wrong", &RESTSource{URL: fake.URL(), Token: "nope"}, "algod rejects the API token (HTTP 401)"},
		{"unset", &RESTSource{URL: fake.URL()}, "no API token is set (HTTP 401)"},
		{"datadir", &LocalSource{Dir: dir}, "algod.token is missing or unreadable (HTTP 401)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := NewNode(Options{Name: c.name, Source: c.src, Sink: &recSink{}, Scrape: 200 * time.Millisecond})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { n.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			waitFor(t, "status error", 5*time.Second, func() bool { return n.State().StatusErr != "" })
			st := n.State()
			if !st.Up || st.Round != 0 || st.StatusErr != c.want {
				t.Fatalf("up %v, round %d, status error %q, want %q", st.Up, st.Round, st.StatusErr, c.want)
			}
			if lag := st.Lag(time.Unix(0, st.StatusErrAt).Add(31 * time.Second)); lag < 30 {
				t.Fatalf("lag %.1fs 31 s after the first failure", lag)
			}
			if c.name != "datadir" {
				return
			}
			// The data directory is read again every 5 s.
			if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "a round with the token back", 10*time.Second, func() bool {
				st := n.State()
				return st.StatusErr == "" && st.StatusErrAt == 0 && st.Round != 0
			})
		})
	}
}

// The log caps start from what the store holds. Before, the counts started
// empty at each restart, so new app IDs took the free slots, the store's
// own cap dropped them, and their lines were in no series at all.
func TestLogCapsStartFromStore(t *testing.T) {
	stored := func(metric string) []model.Labels {
		switch metric {
		case "log_txpool_invalid_total":
			return []model.Labels{{{Name: "app", Value: "1"}}, {{Name: "app", Value: "2"}}, {{Name: "app", Value: "other"}}}
		case "log_template_lines_total":
			return []model.Labels{{{Name: "level", Value: "warn"}, {Name: "template", Value: "a"}}}
		}
		return nil
	}
	c := newLogCounts(2, 3)
	c.seed(stored)
	for _, app := range []string{"3", "4", "1", "4"} {
		c.add(&logparse.Line{Kind: logparse.KindFlood, Level: "info", AppID: app})
	}
	for _, tm := range []string{"b", "c", "a"} {
		c.add(&logparse.Line{Kind: logparse.KindTemplate, Level: "warn", Template: tm})
	}
	got := map[string]float64{}
	for _, s := range c.samples() {
		switch s.Name {
		case "log_txpool_invalid_total":
			got["app="+s.Labels.Get("app")] = s.Value
		case "log_template_lines_total":
			got["template="+s.Labels.Get("template")] = s.Value
		case "log_template_drops_total":
			got["drops"] = s.Value
		}
	}
	want := map[string]float64{"app=1": 1, "app=3": 1, "app=other": 2, "template=a": 1, "template=b": 1, "drops": 1}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
