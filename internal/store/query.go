package store

import (
	"encoding/json"
	"sort"

	"algod.top/internal/model"
)

// Points is one series' data for a time range from one tier. For raw data
// Cols has one column. For rollups the columns follow NumCols.
type Points struct {
	Meta SeriesMeta
	Tier Tier
	T    []int64
	Cols [][]float64
}

// Find returns the series of a node with a metric name.
func (db *DB) Find(node uint32, name string) []SeriesMeta { return db.ix.find(node, name) }

// Names returns the metric names of a node.
func (db *DB) Names(node uint32) []string { return db.ix.names(node) }

// Series returns metadata for an ID.
func (db *DB) Series(id uint32) (SeriesMeta, bool) { return db.ix.get(id) }

// Coverage returns the time range a tier holds for a node.
func (db *DB) Coverage(node uint32, tier Tier) (from, to int64, ok bool) {
	return db.cat.coverage(tier, node)
}

// HeadStart returns the start of a node's current head window, or 0.
func (db *DB) HeadStart(node uint32) int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if h := db.heads[node]; h != nil {
		return h.start
	}
	return 0
}

// HeadFirst returns the time of the first sample in a node's head, or 0.
func (db *DB) HeadFirst(node uint32) int64 {
	db.mu.RLock()
	h := db.heads[node]
	db.mu.RUnlock()
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.samples == 0 {
		return 0
	}
	return h.minT
}

// Read returns data for series ids of one node in [from, to) from a tier.
// For raw it merges blocks, supplementary blocks and the head. For rollup
// tiers it merges compacted blocks with partial blocks, where a partial
// overrides its window. It also returns the restart markers in range.
func (db *DB) Read(node uint32, ids []uint32, tier Tier, from, to int64) ([]Points, []int64, error) {
	want := map[uint32]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if tier == TierRaw {
		return db.readRaw(node, want, from, to)
	}
	return db.readRollup(node, want, tier, from, to)
}

func (db *DB) readRaw(node uint32, want map[uint32]bool, from, to int64) ([]Points, []int64, error) {
	parts := map[uint32][]*colSeries{}
	var resets []int64
	// Highest generation main block per window, then supplementary blocks.
	refs := db.cat.blocks(TierRaw, node, from, to)
	best := map[int64]blockRef{}
	for _, r := range refs {
		if r.Kind == bkMain {
			if b, ok := best[r.Start]; !ok || r.Gen > b.Gen {
				best[r.Start] = r
			}
		}
	}
	var ordered []blockRef
	for _, r := range best {
		ordered = append(ordered, r)
	}
	for _, r := range refs {
		if r.Kind == bkSupp {
			ordered = append(ordered, r)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Start != ordered[j].Start {
			return ordered[i].Start < ordered[j].Start
		}
		return ordered[i].Kind < ordered[j].Kind
	})
	for _, r := range ordered {
		br, err := openBlock(r.Path)
		if err != nil {
			db.logf("store: query open %s: %v", r.Path, err)
			continue
		}
		resets = append(resets, br.resets...)
		cache := map[model.Group][]int64{}
		for id := range want {
			s, err := br.readSeries(id, cache)
			if err != nil {
				db.logf("store: query %s: %v", r.Path, err)
				continue
			}
			if s != nil {
				parts[id] = append(parts[id], clip(s, from, to))
			}
		}
		br.close()
	}
	db.mu.RLock()
	h := db.heads[node]
	db.mu.RUnlock()
	if h != nil && h.end > from && h.start < to {
		snap := h.snapshot(want)
		groups, err := snap.decodeGroups()
		if err == nil {
			resets = append(resets, snap.resets...)
			for id := range want {
				s, err := snap.decodeSeries(id, groups)
				if err == nil && s != nil {
					// The head wins over any block for its window.
					parts[id] = append([]*colSeries{clip(s, from, to)}, parts[id]...)
				}
			}
		}
	}
	return db.collect(parts, TierRaw, mergeRaw), dedupSorted(resets), nil
}

func (db *DB) readRollup(node uint32, want map[uint32]bool, tier Tier, from, to int64) ([]Points, []int64, error) {
	refs := db.cat.blocks(tier, node, from, to)
	// Compacted blocks first, then the newest partial per window.
	var comp []blockRef
	partials := map[int64]blockRef{}
	for _, r := range refs {
		switch r.Kind {
		case bkCompacted:
			comp = append(comp, r)
		case bkPartial:
			if b, ok := partials[r.Start]; !ok || r.Gen > b.Gen {
				partials[r.Start] = r
			}
		}
	}
	// The newest compacted block per period wins.
	bestComp := map[int64]blockRef{}
	for _, r := range comp {
		if b, ok := bestComp[r.Start]; !ok || r.Gen > b.Gen {
			bestComp[r.Start] = r
		}
	}
	var ordered []blockRef
	for _, r := range bestComp {
		ordered = append(ordered, r)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })
	var ps []blockRef
	for _, r := range partials {
		ps = append(ps, r)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Start < ps[j].Start })
	ordered = append(ordered, ps...)

	parts := map[uint32][]authPart{}
	var resets []int64
	for _, r := range ordered {
		br, err := openBlock(r.Path)
		if err != nil {
			continue
		}
		resets = append(resets, br.resets...)
		cache := map[model.Group][]int64{}
		for id := range want {
			s, err := br.readSeries(id, cache)
			if err != nil || s == nil {
				continue
			}
			parts[id] = append(parts[id], authPart{s: s, from: r.Start, to: r.End})
		}
		br.close()
	}
	merged := map[uint32][]*colSeries{}
	for id, p := range parts {
		if s := overrideMerge(p); s != nil {
			merged[id] = []*colSeries{clip(s, from, to)}
		}
	}
	return db.collect(merged, tier, func(p []*colSeries) *colSeries { return p[0] }), dedupSorted(resets), nil
}

func (db *DB) collect(parts map[uint32][]*colSeries, tier Tier, merge func([]*colSeries) *colSeries) []Points {
	ids := make([]uint32, 0, len(parts))
	for id := range parts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Points, 0, len(ids))
	for _, id := range ids {
		s := merge(parts[id])
		if s == nil || len(s.T) == 0 {
			continue
		}
		meta, _ := db.ix.get(id)
		out = append(out, Points{Meta: meta, Tier: tier, T: s.T, Cols: s.Cols})
	}
	return out
}

func clip(s *colSeries, from, to int64) *colSeries {
	lo := sort.Search(len(s.T), func(i int) bool { return s.T[i] >= from })
	hi := sort.Search(len(s.T), func(i int) bool { return s.T[i] >= to })
	if lo == 0 && hi == len(s.T) {
		return s
	}
	out := &colSeries{ID: s.ID, Group: s.Group, Kind: s.Kind, Start: s.Start, T: s.T[lo:hi], Cols: make([][]float64, len(s.Cols))}
	for c := range s.Cols {
		out.Cols[c] = s.Cols[c][lo:hi]
	}
	return out
}

func dedupSorted(v []int64) []int64 {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	out := v[:0]
	for i, x := range v {
		if i == 0 || x != v[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// Events returns events matching f, oldest first. Past f.Limit it keeps
// the newest.
func (db *DB) Events(f EventFilter) []model.Event {
	out, _ := db.EventsCounted(f)
	return out
}

// EventsCounted is Events plus the number of events of each kind in the
// node and range of f. The counts ignore f.Kinds and f.Limit, so a caller
// can tell what the limit left out.
func (db *DB) EventsCounted(f EventFilter) ([]model.Event, map[string]int) {
	var out []model.Event
	counts := map[string]int{}
	anyKind := f
	anyKind.Kinds = nil
	db.events.scan(f.From, f.To, func(b []byte) bool {
		var e model.Event
		if json.Unmarshal(b, &e) != nil || !anyKind.match(&e) {
			return true
		}
		counts[e.Kind]++
		if f.match(&e) {
			out = append(out, e)
		}
		return true
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, counts
}

// RecentEvents returns the newest n events from memory, oldest first.
func (db *DB) RecentEvents(n int, node string) []model.Event {
	db.recentMu.Lock()
	defer db.recentMu.Unlock()
	var out []model.Event
	for i := len(db.recent) - 1; i >= 0 && len(out) < n; i-- {
		e := db.recent[i]
		if node == "" || e.Node == node || e.Node == "" {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Sessions returns finished sessions of a node in [from, to). An empty
// node matches all, an empty addr matches every address.
func (db *DB) Sessions(node, addr string, from, to int64) []model.Session {
	var out []model.Session
	db.sessions.scan(from, to, func(b []byte) bool {
		var s model.Session
		if json.Unmarshal(b, &s) != nil {
			return true
		}
		end := s.End
		if end == 0 {
			end = s.Start
		}
		if end < from || s.Start >= to {
			return true
		}
		if node != "" && s.Node != node {
			return true
		}
		if addr != "" && s.IP != addr && s.Addr != addr {
			return true
		}
		out = append(out, s)
		return true
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// LogLines returns kept raw log lines.
func (db *DB) LogLines(node, template string, from, to int64, limit int) []LogLine {
	var out []LogLine
	for _, l := range []*recLog{db.rawlogs, db.infologs} {
		l.scan(from, to, func(b []byte) bool {
			var ll LogLine
			if json.Unmarshal(b, &ll) != nil {
				return true
			}
			if ll.Time < from || ll.Time >= to || (node != "" && ll.Node != node) || (template != "" && ll.Template != template) {
				return true
			}
			out = append(out, ll)
			return true
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
