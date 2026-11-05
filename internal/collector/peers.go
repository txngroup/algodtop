package collector

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/model"
)

// peersLoop polls /v2/node/peers with the admin token. Diffs between polls
// give peer sessions even when the log level hides connects. Builds with
// per-peer usage add byte totals and open streams.
func (n *Node) peersLoop(ctx context.Context) {
	// Poll as soon as the node first answers, then on the interval.
	for i := 0; i < 30 && !n.State().Up; i++ {
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
	t := time.NewTicker(n.o.PeersEvery)
	defer t.Stop()
	for {
		c := n.getClient()
		if c != nil && c.HasAdmin() && n.State().Up {
			n.pollPeers(ctx, c)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// NormalizeAddr strips a URL scheme and trailing slash so a log address
// and a peers address compare equal.
func NormalizeAddr(a string) string {
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	return strings.TrimSuffix(a, "/")
}

// IPOf extracts the remote IP from a multiaddr or host:port.
func IPOf(a string) string {
	a = NormalizeAddr(a)
	if strings.HasPrefix(a, "/ip4/") || strings.HasPrefix(a, "/ip6/") || strings.HasPrefix(a, "/dns") {
		p := strings.Split(a, "/")
		if len(p) >= 3 {
			return p[2]
		}
	}
	if h, _, err := net.SplitHostPort(a); err == nil {
		return h
	}
	return a
}

// NetOf guesses the network type from an address: multiaddrs are p2p.
func NetOf(a string) string {
	if strings.HasPrefix(NormalizeAddr(a), "/") {
		return "p2p"
	}
	return "ws"
}

func isGossip(stream string) bool {
	return strings.HasPrefix(stream, "/algorand-ws/") || strings.HasPrefix(stream, "ws-gossip/")
}

func (n *Node) pollPeers(ctx context.Context, c *algod.Client) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	peers, err := c.Peers(rctx)
	cancel()
	if err != nil {
		return
	}
	now := n.o.Now()
	var counts PeerCounts
	idle := 0
	gossipStreams := 0
	hasUsage := false
	list := make([]PeerInfo, 0, len(peers))
	var usage []model.PeerUsage
	// For p2p peers the totals cover every connection to the peer ID and
	// repeat on each connection, so each peer ID counts once.
	seenID := map[string]bool{}
	var sumIn, sumOut int64
	for _, p := range peers {
		dir := "in"
		if p.ConnectionType == "outbound" {
			dir = "out"
		}
		netType := p.NetworkType
		if netType == "" {
			netType = NetOf(p.Address)
		}
		switch {
		case netType == "ws" && dir == "in":
			counts.WSIn++
		case netType == "ws":
			counts.WSOut++
		case dir == "in":
			counts.P2PIn++
		default:
			counts.P2POut++
		}
		pi := PeerInfo{Addr: NormalizeAddr(p.Address), IP: IPOf(p.Address), Network: netType, Direction: dir, PeerID: p.PeerID, Protocols: p.SupportedProtocols}
		if p.HasUsage() {
			hasUsage = true
			pi.HasUsage = true
			pi.Streams = *p.ActiveStreams
			if p.TotalBytesReceived != nil {
				pi.BytesIn = *p.TotalBytesReceived
			}
			if p.TotalBytesSent != nil {
				pi.BytesOut = *p.TotalBytesSent
			}
			gossip := false
			for _, s := range pi.Streams {
				if isGossip(s) {
					gossip = true
					gossipStreams++
				}
			}
			if !gossip {
				idle++
			}
			key := pi.Addr
			if netType == "p2p" && pi.PeerID != "" {
				key = pi.PeerID
			}
			if !seenID[key] {
				seenID[key] = true
				sumIn += pi.BytesIn
				sumOut += pi.BytesOut
				usage = append(usage, model.PeerUsage{Time: now.UnixNano(), Key: key, Network: netType, Direction: dir, Addr: pi.Addr,
					BytesIn: pi.BytesIn, BytesOut: pi.BytesOut, Streams: pi.Streams})
			}
		}
		list = append(list, pi)
	}
	list = n.sess.observe(now, list)
	samples := []model.Sample{
		{Name: "algodtop_peers", Labels: model.Labels{{Name: "dir", Value: "in"}, {Name: "net", Value: "ws"}}, Kind: model.KindGauge, Value: float64(counts.WSIn)},
		{Name: "algodtop_peers", Labels: model.Labels{{Name: "dir", Value: "out"}, {Name: "net", Value: "ws"}}, Kind: model.KindGauge, Value: float64(counts.WSOut)},
		{Name: "algodtop_peers", Labels: model.Labels{{Name: "dir", Value: "in"}, {Name: "net", Value: "p2p"}}, Kind: model.KindGauge, Value: float64(counts.P2PIn)},
		{Name: "algodtop_peers", Labels: model.Labels{{Name: "dir", Value: "out"}, {Name: "net", Value: "p2p"}}, Kind: model.KindGauge, Value: float64(counts.P2POut)},
	}
	if hasUsage {
		in, out := n.sess.usageCounter(usage)
		samples = append(samples,
			model.Sample{Name: "algodtop_peers_idle", Kind: model.KindGauge, Value: float64(idle)},
			model.Sample{Name: "algodtop_gossip_streams", Kind: model.KindGauge, Value: float64(gossipStreams)},
			model.Sample{Name: "algodtop_peer_bytes_total", Labels: model.Labels{{Name: "dir", Value: "in"}}, Kind: model.KindCounter, Value: in},
			model.Sample{Name: "algodtop_peer_bytes_total", Labels: model.Labels{{Name: "dir", Value: "out"}}, Kind: model.KindCounter, Value: out},
		)
		n.o.Sink.Usage(n.o.Name, usage)
	}
	n.update(func(s *State) {
		s.Peers = counts
		s.IdlePeers = idle
		s.PeerList = list
		s.PeerUsage = hasUsage
	})
	n.batch(model.GroupPeers, now, samples)
}

// sessionTracker turns connect/disconnect log events and peers-poll diffs
// into peer sessions. Log events give exact times and reasons. Poll diffs
// fill in what the log level hides.
type sessionTracker struct {
	n    *Node
	mu   sync.Mutex
	live map[string]*model.Session // key: net|dir|addr
	// usage counter state: per key last totals, cumulative sums
	last     map[string][2]int64
	cumIn    float64
	cumOut   float64
	prevPoll map[string]PeerInfo
	pending  map[string]model.Session // log disconnects awaiting a poll
}

func newSessionTracker(n *Node) *sessionTracker {
	t := &sessionTracker{n: n, live: map[string]*model.Session{}, last: map[string][2]int64{}, pending: map[string]model.Session{}}
	for _, s := range n.persist.Sessions {
		s := s
		t.live[sessKey(s.Network, s.Direction, s.Addr)] = &s
	}
	return t
}

func sessKey(netw, dir, addr string) string { return netw + "|" + dir + "|" + NormalizeAddr(addr) }

func (t *sessionTracker) open() []model.Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]model.Session, 0, len(t.live))
	for _, s := range t.live {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Open returns the open sessions.
func (n *Node) OpenSessions() []model.Session { return n.sess.open() }

// observe reconciles a poll with the open sessions and returns the list
// with each peer's connected-since time.
func (t *sessionTracker) observe(now time.Time, peers []PeerInfo) []PeerInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	for i := range peers {
		p := &peers[i]
		k := sessKey(p.Network, p.Direction, p.Addr)
		seen[k] = true
		s, ok := t.live[k]
		if !ok {
			// A peer in the first poll was connected before algodtop looked.
			s = &model.Session{Node: t.n.o.Name, Network: p.Network, Direction: p.Direction, Addr: p.Addr, IP: p.IP,
				PeerID: p.PeerID, Start: now.UnixNano(), Source: "poll", OpenBefore: t.prevPoll == nil}
			t.live[k] = s
		}
		if p.PeerID != "" {
			s.PeerID = p.PeerID
		}
		if p.HasUsage {
			s.BytesIn, s.BytesOut = p.BytesIn, p.BytesOut
			if len(p.Protocols) > 0 {
				s.Protocols = p.Protocols
			}
			if prev, ok := t.prevPoll[k]; ok && t.n.o.PeersEvery > 0 {
				dt := t.n.o.PeersEvery.Seconds()
				if d := p.BytesIn - prev.BytesIn; d >= 0 {
					p.RateIn = float64(d) / dt
				}
				if d := p.BytesOut - prev.BytesOut; d >= 0 {
					p.RateOut = float64(d) / dt
				}
			}
		}
		p.Since = s.Start
		p.SinceBefore = s.OpenBefore
	}
	for k, s := range t.live {
		if seen[k] {
			continue
		}
		s.End = now.UnixNano()
		if s.Reason == "" {
			s.Reason = "gone"
		}
		t.n.o.Sink.Session(*s)
		delete(t.live, k)
	}
	t.prevPoll = map[string]PeerInfo{}
	for _, p := range peers {
		t.prevPoll[sessKey(p.Network, p.Direction, p.Addr)] = p
	}
	return peers
}

// logConnect records a connect from node.log.
func (t *sessionTracker) logConnect(at time.Time, addr, ip string, incoming bool) {
	if ip == "" {
		ip = IPOf(addr)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	dir := "out"
	if incoming {
		dir = "in"
	}
	netw := NetOf(addr)
	k := sessKey(netw, dir, addr)
	if s, ok := t.live[k]; ok {
		// A poll saw it first; the log has the exact start.
		if s.Source == "poll" && at.UnixNano() < s.Start {
			s.Start = at.UnixNano()
			s.OpenBefore = false
		}
		s.Source = "log"
		return
	}
	t.live[k] = &model.Session{Node: t.n.o.Name, Network: netw, Direction: dir, Addr: NormalizeAddr(addr), IP: ip, Start: at.UnixNano(), Source: "log"}
}

// logDisconnect ends a session with the exact time and reason.
func (t *sessionTracker) logDisconnect(at time.Time, addr, ip string, incoming bool, reason string) model.Session {
	if ip == "" {
		ip = IPOf(addr)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	dir := "out"
	if incoming {
		dir = "in"
	}
	netw := NetOf(addr)
	k := sessKey(netw, dir, addr)
	s, ok := t.live[k]
	if !ok {
		// Started before monitoring began: the start is unknown.
		s = &model.Session{Node: t.n.o.Name, Network: netw, Direction: dir, Addr: NormalizeAddr(addr), IP: ip, Source: "log"}
	}
	if s.IP == "" || s.IP == IPOf(addr) {
		s.IP = ip
	}
	s.End = at.UnixNano()
	s.Reason = reason
	if s.Start == 0 {
		s.Start = s.End
	}
	t.n.o.Sink.Session(*s)
	delete(t.live, k)
	delete(t.prevPoll, k)
	return *s
}

// closeAll ends every open session, when algod goes down.
func (t *sessionTracker) closeAll() {
	// Read the node state before taking t.mu, for the same lock order as
	// savePersisted.
	if t.n.State().Up {
		return // daemon shutdown: sessions stay open in the state file
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.n.o.Now().UnixNano()
	for k, s := range t.live {
		s.End = now
		if s.Reason == "" {
			s.Reason = "node down"
		}
		t.n.o.Sink.Session(*s)
		delete(t.live, k)
	}
}

// usageCounter turns per-key totals into a monotonic counter. A key's
// total that drops is a reset: a new session starts from zero.
func (t *sessionTracker) usageCounter(us []model.PeerUsage) (float64, float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := map[string][2]int64{}
	for _, u := range us {
		prev, ok := t.last[u.Key]
		din, dout := u.BytesIn, u.BytesOut
		if ok && u.BytesIn >= prev[0] {
			din = u.BytesIn - prev[0]
		}
		if ok && u.BytesOut >= prev[1] {
			dout = u.BytesOut - prev[1]
		}
		t.cumIn += float64(din)
		t.cumOut += float64(dout)
		next[u.Key] = [2]int64{u.BytesIn, u.BytesOut}
	}
	t.last = next
	return t.cumIn, t.cumOut
}
