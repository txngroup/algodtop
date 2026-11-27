package collector

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"algod.top/internal/model"
)

// Two accounts, each with its old key, which stops at 6631356, and a
// renewal that has voted since.
func renewed() []PartKeyState {
	return []PartKeyState{
		{ID: "old-q", Address: "AAAA", First: 631356, Last: 6631356, VoteKey: "vq1", LastVote: 6483479},
		{ID: "old-u", Address: "BBBB", First: 631830, Last: 6631830, VoteKey: "vu1", LastVote: 6473505},
		{ID: "new-u", Address: "BBBB", First: 6472801, Last: 13320411, VoteKey: "vu2", LastVote: 6608411},
		{ID: "new-q", Address: "AAAA", First: 6481946, Last: 16481946, VoteKey: "vq2", LastVote: 6608411},
	}
}

func TestMarkKeysPicksTheVotingRenewal(t *testing.T) {
	keys := renewed()
	markKeys(keys, nil, nil, 6608504)
	want := map[string][2]bool{"old-q": {false, true}, "old-u": {false, true}, "new-u": {true, false}, "new-q": {true, false}}
	for _, k := range keys {
		if w := want[k.ID]; k.Active != w[0] || k.Superseded != w[1] {
			t.Errorf("%s: active %v superseded %v, want %v", k.ID, k.Active, k.Superseded, w)
		}
	}
	exp := ExpiryKeys(keys)
	if len(exp) != 2 || exp[0].ID != "new-q" || exp[1].ID != "new-u" {
		t.Fatalf("expiry keys %+v", exp)
	}
}

// A renewal that is installed but has not voted leaves the old key in
// charge, so its expiry still counts.
func TestMarkKeysUnvotedRenewal(t *testing.T) {
	keys := []PartKeyState{
		{ID: "old", Address: "A", First: 100, Last: 1000, LastVote: 900},
		{ID: "new", Address: "A", First: 950, Last: 5000},
	}
	markKeys(keys, nil, nil, 960)
	if !keys[0].Active || keys[1].Active || keys[0].Superseded {
		t.Fatalf("%+v", keys)
	}
	if exp := ExpiryKeys(keys); exp[0].ID != "old" || !LaterKey(keys, exp[0]) {
		t.Fatalf("%+v", exp)
	}
}

// The on-chain vote key decides when the account is listed.
func TestMarkKeysOnChainWins(t *testing.T) {
	keys := renewed()
	markKeys(keys, []AccountState{{Address: "AAAA", VoteKey: "vq1"}}, nil, 6608504)
	for _, k := range keys {
		if k.ID == "old-q" && !k.Active || k.ID == "new-q" && k.Active {
			t.Errorf("%s: active %v", k.ID, k.Active)
		}
	}
}

// Before the first round is known nothing is active, and the key that
// lasts longest stands for the account.
func TestExpiryKeysWithoutRound(t *testing.T) {
	keys := renewed()
	markKeys(keys, nil, nil, 0)
	for _, k := range ExpiryKeys(keys) {
		if k.ID != "new-q" && k.ID != "new-u" {
			t.Errorf("picked %s", k.ID)
		}
	}
}

// algod logs a closed account that still has a key installed several
// times a round. Its keys stop being active and stop counting toward an
// expiry until the reports stop. A report replayed from an old part of
// the log does not count.
func TestClosedAccountKeys(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	n := &Node{o: Options{Name: "n", Sink: &recSink{}, Now: func() time.Time { return now }}}
	n.st.Round = 6608504
	n.st.Keys = renewed()
	markKeys(n.st.Keys, nil, nil, n.st.Round)
	tl := newTailer(n, nil)
	report := func(at time.Time) {
		tl.line([]byte(`{"file":"node.go","level":"info","msg":"node.VotingKeys: Address: AAAA - Account was closed but still has a participation key active.","time":"` + at.UTC().Format(time.RFC3339Nano) + `"}`))
	}
	report(now.Add(-time.Hour))
	if exp := ExpiryKeys(n.State().Keys); len(exp) != 2 {
		t.Fatalf("an hour-old report closed the account: %+v", exp)
	}
	report(now)
	for _, k := range n.State().Keys {
		if k.Closed != (k.Address == "AAAA") || k.Closed && (k.Active || k.Superseded) {
			t.Errorf("%s: closed %v active %v superseded %v", k.ID, k.Closed, k.Active, k.Superseded)
		}
	}
	if exp := ExpiryKeys(n.State().Keys); len(exp) != 1 || exp[0].ID != "new-u" {
		t.Fatalf("expiry keys %+v", exp)
	}
	// The reports stop, as when the account is funded again.
	now = now.Add(closedFor + time.Minute)
	n.mu.Lock()
	closed := n.closedLocked()
	n.mu.Unlock()
	keys := n.State().Keys
	markKeys(keys, nil, closed, n.st.Round)
	if exp := ExpiryKeys(keys); len(exp) != 2 || exp[0].ID != "new-q" || !exp[0].Active {
		t.Fatalf("after the reports stopped: %+v", exp)
	}
}

// Peers in the first poll were connected before algodtop looked, so their
// connected-since time is a lower bound. A peer that appears later, or
// whose connect the log records, has a real start.
func TestFirstPollPeersAreOpenBefore(t *testing.T) {
	n := &Node{o: Options{Name: "n", Sink: &recSink{}, PeersEvery: 30 * time.Second}}
	tr := newSessionTracker(n)
	now := time.Unix(1_800_000_000, 0)
	a := PeerInfo{Addr: "1.2.3.4:4160", IP: "1.2.3.4", Network: "ws", Direction: "out"}
	b := PeerInfo{Addr: "5.6.7.8:4160", IP: "5.6.7.8", Network: "ws", Direction: "out"}
	got := tr.observe(now, []PeerInfo{a})
	if !got[0].SinceBefore {
		t.Fatal("a peer in the first poll has a known start")
	}
	got = tr.observe(now.Add(30*time.Second), []PeerInfo{a, b})
	if !got[0].SinceBefore || got[1].SinceBefore {
		t.Fatalf("second poll: %+v", got)
	}
	// The log has a's real connect time.
	tr.logConnect(now.Add(-time.Hour), a.Addr, a.IP, false)
	got = tr.observe(now.Add(60*time.Second), []PeerInfo{a, b})
	if got[0].SinceBefore || got[0].Since != now.Add(-time.Hour).UnixNano() {
		t.Fatalf("after the log connect: %+v", got[0])
	}
}

// A disconnect with no connect before it ends a session that began before
// algodtop looked. Its start is unknown, so it is marked open before
// rather than stored as a session of zero length.
func TestLogDisconnectWithoutConnectIsOpenBefore(t *testing.T) {
	sink := &recSink{}
	n := &Node{o: Options{Name: "n", Sink: sink, PeersEvery: 30 * time.Second}}
	tr := newSessionTracker(n)
	now := time.Unix(1_800_000_000, 0)
	s, _ := tr.logDisconnect(now, "/ip4/1.2.3.4/tcp/4190", "", false, "ReadError")
	if !s.OpenBefore || s.Start != s.End || s.End != now.UnixNano() {
		t.Fatalf("unknown start: %+v", s)
	}
	// A session whose connect the log saw has a real start.
	tr.logConnect(now, "/ip4/5.6.7.8/tcp/4190", "", true)
	s, _ = tr.logDisconnect(now.Add(time.Minute), "/ip4/5.6.7.8/tcp/4190", "", true, "IdleConnection")
	if s.OpenBefore || s.Start != now.UnixNano() {
		t.Fatalf("known start: %+v", s)
	}
	if len(sink.sessions) != 2 {
		t.Fatalf("%d sessions stored, want 2", len(sink.sessions))
	}
}

// algod removes a slow ws peer twice and logs the same disconnect each
// time, under a millisecond apart and not always in time order. The
// repeat is one disconnect: one event and one session with its real start.
func TestRepeatedDisconnectLineCountsOnce(t *testing.T) {
	sink := &recSink{}
	n := &Node{o: Options{Name: "n", Sink: sink, PeersEvery: 30 * time.Second}}
	n.sess = newSessionTracker(n)
	tl := newTailer(n, nil)
	at := func(base time.Time, us int) string {
		return base.Add(time.Duration(us) * time.Microsecond).UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	conn := func(ts string) {
		tl.line([]byte(`{"details":{"Address":"192.0.2.7","Incoming":false,"Endpoint":"relay.example.net:4160"},"file":"telemetry.go","level":"info","msg":"/Network/ConnectPeer","time":"` + ts + `"}`))
	}
	disc := func(ts, reason string) {
		tl.line([]byte(`{"details":{"Address":"192.0.2.7","Incoming":false,"Endpoint":"relay.example.net:4160","MessageDelay":233255577,"Reason":"` + reason + `"},"file":"telemetry.go","level":"info","msg":"/Network/DisconnectPeer","time":"` + ts + `"}`))
	}
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(time.Minute)
	conn(at(t0, 0))
	disc(at(t1, 0), "SlowConnection")
	disc(at(t1, 68), "SlowConnection")
	// Out of order: the later line comes first.
	conn(at(t1, 500_000))
	disc(at(t2, 120), "SlowConnection")
	disc(at(t2, 0), "SlowConnection")
	// A real reconnect and disconnect soon after still counts.
	conn(at(t2, 300_000))
	disc(at(t2, 600_000), "LeastPerformingPeer")

	var discs []model.Event
	for _, e := range sink.events {
		if e.Kind == model.EvDisconnect {
			discs = append(discs, e)
		}
	}
	if len(discs) != 3 {
		t.Fatalf("%d disconnect events, want 3: %+v", len(discs), discs)
	}
	want := [][2]time.Time{{t0, t1}, {t1.Add(500 * time.Millisecond), t2.Add(120 * time.Microsecond)}, {t2.Add(300 * time.Millisecond), t2.Add(600 * time.Millisecond)}}
	if len(sink.sessions) != len(want) {
		t.Fatalf("%d sessions, want %d: %+v", len(sink.sessions), len(want), sink.sessions)
	}
	for i, s := range sink.sessions {
		if s.OpenBefore || s.Start != want[i][0].UnixNano() || s.End != want[i][1].UnixNano() {
			t.Errorf("session %d: %+v, want %v to %v", i, s, want[i][0], want[i][1])
		}
	}
}

// A node still marked down after a restart must not log the restart again
// on the next check under the same pid.
func TestRestartLoggedOnce(t *testing.T) {
	sink := &recSink{}
	n := NewNode(Options{Name: "n", Source: &LocalSource{Dir: t.TempDir()}, Sink: sink})
	prev := processAlive.Load()
	SetProcessAlive(func(int) bool { return true })
	defer processAlive.Store(prev)
	now := time.Now()
	n.checkPID(24, 0, now, true)
	n.markDown(now, "connection refused")
	n.checkPID(164, 0, now, false)
	n.checkPID(164, 0, now.Add(10*time.Second), false)
	n.checkPID(164, 0, now.Add(20*time.Second), false)
	restarts := 0
	for _, e := range sink.events {
		if e.Kind == model.EvRestart {
			restarts++
			if e.Msg != "algod restarted (pid 24 -> 164)" {
				t.Errorf("restart %q", e.Msg)
			}
		}
	}
	if restarts != 1 {
		t.Fatalf("%d restart events, want 1", restarts)
	}
}

// A local algod that stops answering while its process runs is hung, not
// crashed. The down reason says the process is still there.
func TestDownReasonNamesARunningPID(t *testing.T) {
	// A listener that never accepts: connects succeed and requests hang,
	// as they do for an algod stopped with SIGSTOP.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	for name, body := range map[string]string{"algod.net": ln.Addr().String() + "\n", "algod.pid": fmt.Sprint(os.Getpid()) + "\n", "algod.token": "tok\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev := processAlive.Load()
	SetProcessAlive(func(int) bool { return true })
	defer processAlive.Store(prev)
	n := NewNode(Options{Name: "n", Source: &LocalSource{Dir: dir}, Sink: &recSink{}, Scrape: 100 * time.Millisecond})
	ctx := context.Background()
	n.refreshFiles(ctx, true)
	n.mu.Lock()
	n.st.Up = true
	n.mu.Unlock()
	n.scrape(ctx)
	n.scrape(ctx)
	st := n.State()
	want := fmt.Sprintf("; algod pid %d is still running", os.Getpid())
	if st.Up || !strings.HasPrefix(st.LastError, "REST unreachable: ") || !strings.HasSuffix(st.LastError, want) {
		t.Fatalf("up %v, last error %q", st.Up, st.LastError)
	}
}

// A peer's rate is its bytes over the time since the last poll that
// answered. A failed poll in between must not double the rate, and a new
// peer or a reset total has no rate yet.
func TestPeerRateUsesThePollGap(t *testing.T) {
	n := &Node{o: Options{Name: "n", Sink: &recSink{}, PeersEvery: 30 * time.Second}}
	tr := newSessionTracker(n)
	now := time.Unix(1_800_000_000, 0)
	p := PeerInfo{Addr: "/ip4/1.2.3.4/tcp/4190", IP: "1.2.3.4", Network: "p2p", Direction: "in", HasUsage: true}
	at := func(sec int, in, out int64) PeerInfo {
		q := p
		q.BytesIn, q.BytesOut = in, out
		return tr.observe(now.Add(time.Duration(sec)*time.Second), []PeerInfo{q})[0]
	}
	if got := at(0, 1000, 500); !got.RateUnknown || got.RateIn != 0 {
		t.Fatalf("first poll: %+v", got)
	}
	if got := at(30, 31000, 15500); got.RateUnknown || got.RateIn != 1000 || got.RateOut != 500 {
		t.Fatalf("after 30 s: %+v", got)
	}
	// The poll at 60 s failed, so the next answer comes 60 s later.
	if got := at(90, 91000, 45500); got.RateUnknown || got.RateIn != 1000 || got.RateOut != 500 {
		t.Fatalf("after a missed poll: %+v", got)
	}
	if got := at(120, 100, 45600); !got.RateUnknown {
		t.Fatalf("a total that went down: %+v", got)
	}
}

// A node that stays down across a restart of algod.top keeps the time it
// went down. It used to count from the restart, so a node down for hours
// read as down for seconds.
func TestDownSinceSurvivesARestart(t *testing.T) {
	dir, stateDir := t.TempDir(), t.TempDir()
	open := func(sink *recSink) *Node {
		return NewNode(Options{Name: "n", Source: &LocalSource{Dir: dir}, Sink: sink, StateDir: stateDir})
	}
	went := time.Now().Add(-3 * time.Hour)
	first := open(&recSink{})
	first.update(func(s *State) { s.Up = true })
	first.markDown(went, "process not running, no live algod.pid")
	first.savePersisted()

	sink := &recSink{}
	second := open(sink)
	second.checkPID(0, 0, time.Now(), true)
	if st := second.State(); st.Up || st.DownSince != went.UnixNano() {
		t.Fatalf("after a restart: up %v, down since %v, want %v", st.Up, time.Unix(0, st.DownSince), went)
	}
	if len(sink.events) != 0 {
		t.Fatalf("a node still down logged %+v", sink.events)
	}
	// Coming back records the whole stretch, and the state file forgets it.
	second.markUp(time.Now())
	if len(sink.events) != 1 || sink.events[0].Kind != model.EvNodeUp || sink.events[0].Attrs["down_since"] != went.UnixNano() {
		t.Fatalf("events %+v", sink.events)
	}
	second.savePersisted()
	if st := open(&recSink{}).State(); st.DownSince != 0 {
		t.Fatalf("down since %v after algod came back", time.Unix(0, st.DownSince))
	}
}
