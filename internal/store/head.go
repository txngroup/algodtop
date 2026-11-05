package store

import (
	"sync"

	"algod.top/internal/enc"
	"algod.top/internal/model"
)

// head holds the newest 2 h window of raw samples for one node, in the same
// compressed encoding a block uses. The writer appends under mu, held for
// microseconds. Readers copy the encoded bytes under mu and decode outside.
type head struct {
	mu      sync.Mutex
	node    uint32
	start   int64
	end     int64
	groups  map[model.Group]*enc.TSEncoder
	series  map[uint32]*headSeries
	resets  []int64
	minT    int64
	maxT    int64
	touched int64 // hub wall clock of the last append
	samples int
	gen     uint32 // main block generation to write, set when reloaded from a block
}

type headSeries struct {
	group model.Group
	kind  model.Kind
	pres  enc.Presence
	vals  enc.XOREncoder
}

func newHead(node uint32, start int64) *head {
	return &head{node: node, start: start, end: start + WindowNS,
		groups: map[model.Group]*enc.TSEncoder{}, series: map[uint32]*headSeries{}}
}

// append adds a batch whose series IDs are already resolved (0 = dropped).
// It returns false when the batch is out of order for its group, which the
// caller routes to the late path.
func (h *head) append(b *model.Batch, ids []uint32, wall int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.groups[b.Group]
	if ts == nil {
		ts = &enc.TSEncoder{}
		h.groups[b.Group] = ts
	}
	if ts.Count() > 0 && b.Time <= ts.Last() {
		return false
	}
	ts.Append(b.Time)
	idx := ts.Count() - 1
	for i := range b.Samples {
		id := ids[i]
		if id == 0 {
			continue
		}
		s := &b.Samples[i]
		hs := h.series[id]
		if hs == nil {
			hs = &headSeries{group: b.Group, kind: s.Kind}
			h.series[id] = hs
		}
		if hs.pres.Len() > idx {
			continue // duplicate series in one batch
		}
		hs.pres.AppendN(false, idx-hs.pres.Len())
		hs.pres.Append(true)
		hs.vals.Append(s.Value)
		h.samples++
	}
	if b.Reset {
		h.resets = append(h.resets, b.Time)
	}
	if h.minT == 0 || b.Time < h.minT {
		h.minT = b.Time
	}
	h.maxT = max(h.maxT, b.Time)
	h.touched = wall
	return true
}

func (h *head) empty() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.groups) == 0
}

// headSnap is a consistent copy of a head's encoded bytes.
type headSnap struct {
	node   uint32
	start  int64
	end    int64
	groups map[model.Group]tsSnap
	series map[uint32]serSnap
	resets []int64
}

type tsSnap struct {
	b []byte
	n int
}

type serSnap struct {
	group model.Group
	kind  model.Kind
	pres  enc.Presence
	vals  []byte
}

// snapshot copies the head. With ids nil it copies every series.
func (h *head) snapshot(ids map[uint32]bool) *headSnap {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &headSnap{node: h.node, start: h.start, end: h.end, groups: make(map[model.Group]tsSnap, len(h.groups)),
		series: make(map[uint32]serSnap), resets: append([]int64(nil), h.resets...)}
	for g, ts := range h.groups {
		s.groups[g] = tsSnap{b: append([]byte(nil), ts.Bytes()...), n: ts.Count()}
	}
	for id, hs := range h.series {
		if ids != nil && !ids[id] {
			continue
		}
		s.series[id] = serSnap{group: hs.group, kind: hs.kind, pres: hs.pres.Clone(), vals: append([]byte(nil), hs.vals.Bytes()...)}
	}
	return s
}

// decodeGroups decodes every timestamp column in a snapshot.
func (s *headSnap) decodeGroups() (map[model.Group][]int64, error) {
	out := make(map[model.Group][]int64, len(s.groups))
	for g, t := range s.groups {
		ts, err := enc.DecodeTS(t.b, t.n)
		if err != nil {
			return nil, err
		}
		out[g] = ts
	}
	return out, nil
}

// decodeSeries decodes one series of a snapshot into a raw colSeries.
func (s *headSnap) decodeSeries(id uint32, groups map[model.Group][]int64) (*colSeries, error) {
	ss, ok := s.series[id]
	if !ok {
		return nil, nil
	}
	ts := groups[ss.group]
	vals, err := enc.DecodeXOR(ss.vals, ss.pres.Count())
	if err != nil {
		return nil, err
	}
	out := &colSeries{ID: id, Group: ss.group, Kind: ss.kind, T: make([]int64, 0, len(vals)), Cols: [][]float64{vals}}
	for i, p := range ss.pres.Bits() {
		if p && i < len(ts) {
			out.T = append(out.T, ts[i])
		}
	}
	if len(out.T) != len(vals) {
		out.Cols[0] = vals[:len(out.T)]
	}
	return out, nil
}
