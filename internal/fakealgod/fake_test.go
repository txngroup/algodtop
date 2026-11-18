package fakealgod

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// With Wave the counters never go back, their rate swings, and over hours
// it averages Load.
func TestWaveCounters(t *testing.T) {
	f, err := Start(Options{Wave: true, Load: 2, Uptime: time.Minute, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t0 := time.Now()
	prev := f.counterScale(t0)
	lo, hi := 1e9, 0.0
	for m := 1; m <= 6*60; m++ {
		s := f.counterScale(t0.Add(time.Duration(m) * time.Minute))
		rate := (s - prev) * 86400 / 60 // days per day over the minute
		if rate <= 0 {
			t.Fatalf("minute %d: rate %v", m, rate)
		}
		lo, hi = min(lo, rate), max(hi, rate)
		prev = s
	}
	if hi/lo < 1.5 {
		t.Errorf("rate swings only from %.2f to %.2f", lo, hi)
	}
	mean := (f.counterScale(t0.Add(48*time.Hour)) - f.counterScale(t0)) / 2
	if mean < 1.95 || mean > 2.05 {
		t.Errorf("mean %.3f days per day, want 2", mean)
	}
}

type countConn struct {
	net.Conn
	rd, wr *atomic.Int64
}

func (c countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.rd.Add(int64(n))
	return n, err
}

func (c countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.wr.Add(int64(n))
	return n, err
}

type countLn struct {
	net.Listener
	rd, wr *atomic.Int64
}

func (l countLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countConn{c, l.rd, l.wr}, nil
}

// The sink receives what the fake sent and sends back what it received,
// at the rates the counters report.
func TestWireSink(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var rd, wr atomic.Int64
	go ServeWireSink(countLn{ln, &rd, &wr})
	f, err := Start(Options{WireTo: ln.Addr().String(), Uptime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	time.Sleep(3500 * time.Millisecond)
	// Three ticks of the fixture's mean rate, with the wire overhead.
	in, out := f.appIn/86400*3*wireOverhead, f.appOut/86400*3*wireOverhead
	if got := float64(rd.Load()); got < out*0.9 || got > out*1.1 {
		t.Errorf("sink read %.0f bytes, want about %.0f", got, out)
	}
	if got := float64(wr.Load()); got < in*0.9 || got > in*1.1 {
		t.Errorf("sink wrote %.0f bytes, want about %.0f", got, in)
	}
}

// Churn replaces a peer with one of the same direction and network, so a
// node keeps its outgoing count, and a node always has an outgoing peer.
func TestChurnKeepsDirections(t *testing.T) {
	count := func(f *Fake) map[string]int {
		c := map[string]int{}
		for _, p := range f.peerList() {
			c[p["connection-type"].(string)+" "+p["network-type"].(string)]++
		}
		return c
	}
	for seed := int64(1); seed <= 20; seed++ {
		f, err := Start(Options{Peers: 3, ChurnEvery: 5 * time.Millisecond, Seed: seed})
		if err != nil {
			t.Fatal(err)
		}
		before := count(f)
		if before["outbound ws"]+before["outbound p2p"] == 0 {
			t.Errorf("seed %d: no outgoing peer: %v", seed, before)
		}
		time.Sleep(60 * time.Millisecond)
		after := count(f)
		f.Close()
		for k, v := range before {
			if after[k] != v {
				t.Errorf("seed %d: %v became %v", seed, before, after)
				break
			}
		}
	}
}

// During an outage the round holds, catchup attempts come and go in
// /v2/status, and each ends with algod's log line. Afterwards the round
// jumps to where the network got to.
func TestOutage(t *testing.T) {
	dir := t.TempDir()
	f, err := Start(Options{DataDir: dir, RoundEvery: 20 * time.Millisecond, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Outage(true)
	r0 := f.Round()
	attempts, inAttempt := 0, false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		on := f.status()["catchup-time"].(int64) > 0
		if on && !inAttempt {
			attempts++
		}
		inAttempt = on
	}
	if attempts < 3 {
		t.Errorf("%d catchup attempts in 1 s, want several", attempts)
	}
	if f.Round() != r0 {
		t.Errorf("round moved from %d to %d during the outage", r0, f.Round())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "node.log"))
	if n := strings.Count(string(b), "Catchup Service: finished catching up"); n < 3 {
		t.Errorf("%d catchup log lines", n)
	}
	f.Outage(false)
	if f.Round() < r0+40 {
		t.Errorf("round %d after a 1 s outage at 20 ms rounds, from %d", f.Round(), r0)
	}
	if c := f.status()["catchup-time"].(int64); c != 0 {
		t.Errorf("catchup time %d after the outage", c)
	}

	f.CatchpointCatchup("1000#ABC")
	r1 := f.Round()
	time.Sleep(100 * time.Millisecond)
	st := f.status()
	if st["catchpoint"] != "1000#ABC" || st["catchup-time"].(int64) == 0 || f.Round() != r1 {
		t.Errorf("fast catchup: status %v, round %d from %d", st, f.Round(), r1)
	}
	f.CatchpointCatchup("")
	if _, ok := f.status()["catchpoint"]; ok {
		t.Error("catchpoint still set")
	}
}

// libp2p's blocked resources is typed gauge but counts up from the process
// start, so the fake grows it with uptime as it does a counter.
func TestBlockedResourcesGrow(t *testing.T) {
	read := func(uptime time.Duration) (string, float64) {
		t.Helper()
		f, err := Start(Options{Uptime: uptime, Seed: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		res, err := http.Get(f.URL() + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		typ, val := "", -1.0
		for _, l := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(l, "# TYPE libp2p_rcmgr_blocked_resources ") {
				typ = strings.Fields(l)[3]
			}
			if strings.HasPrefix(l, `libp2p_rcmgr_blocked_resources{resource="fd"`) {
				val, _ = strconv.ParseFloat(l[strings.LastIndexByte(l, ' ')+1:], 64)
			}
		}
		return typ, val
	}
	typ, day := read(24 * time.Hour)
	_, two := read(48 * time.Hour)
	if typ != "gauge" {
		t.Errorf("TYPE %q, want gauge as libp2p sends it", typ)
	}
	if day <= 0 || two < 1.9*day || two > 2.1*day {
		t.Errorf("blocked fd resources %v after a day and %v after two, want twice as many", day, two)
	}
}
