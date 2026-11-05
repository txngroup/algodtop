package fakealgod

import (
	"net"
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
