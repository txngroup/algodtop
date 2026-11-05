package store

import (
	"math"
	"sort"

	"algod.top/internal/model"
)

// Rollup columns per kind.
const (
	// Gauges.
	ColMin   = 0
	ColMax   = 1
	ColSum   = 2
	ColCount = 3
	ColLast  = 4
	// Counters and histogram parts.
	ColCtrLast    = 0
	ColCtrMaxRate = 1
)

// NumCols returns the column count of a series kind in a tier.
func NumCols(t Tier, k model.Kind) int {
	if t == TierRaw {
		return 1
	}
	switch k {
	case model.KindGauge:
		return 5
	case model.KindCounter:
		return 2
	}
	return 1
}

// ctrState carries a cumulative series' reset adjustment across windows.
type ctrState = ctrStart

// adjust returns reset-adjusted cumulative values and per-sample rates for a
// raw cumulative series, starting from st, and the state after the last
// sample. A reset is a value that goes down, or a restart marker between two
// samples. The rate across a reset is the new value divided by the interval.
func adjust(s *colSeries, st ctrState, resets []int64) (adj, rates []float64, end ctrState) {
	vals := s.Cols[0]
	adj = make([]float64, len(vals))
	rates = make([]float64, len(vals))
	end = st
	ri := sort.Search(len(resets), func(i int) bool { return resets[i] > st.LastT })
	for i, v := range vals {
		t := s.T[i]
		has := end.LastT != 0
		reset := false
		for ri < len(resets) && resets[ri] <= t {
			if has && resets[ri] > end.LastT {
				reset = true
			}
			ri++
		}
		if has && v < end.LastRaw {
			reset = true
		}
		rate := math.NaN()
		if reset {
			end.Offset += end.LastRaw
			if dt := float64(t-end.LastT) / 1e9; dt > 0 {
				rate = v / dt
			}
		} else if has {
			if dt := float64(t-end.LastT) / 1e9; dt > 0 {
				rate = (v - end.LastRaw) / dt
			}
		}
		adj[i] = v + end.Offset
		rates[i] = rate
		end.LastRaw = v
		end.LastT = t
	}
	return adj, rates, end
}

// rollup buckets one raw series at resolution res. For cumulative kinds,
// adj and rates come from adjust.
func rollup(s *colSeries, tier Tier, adj, rates []float64) *colSeries {
	res := tier.Res()
	out := &colSeries{ID: s.ID, Group: s.Group, Kind: s.Kind}
	n := NumCols(tier, s.Kind)
	out.Cols = make([][]float64, n)
	cur := int64(math.MinInt64)
	for i, t := range s.T {
		b := floorDiv(t, res) * res
		if b != cur {
			cur = b
			out.T = append(out.T, b)
			switch s.Kind {
			case model.KindGauge:
				v := s.Cols[0][i]
				out.Cols[ColMin] = append(out.Cols[ColMin], v)
				out.Cols[ColMax] = append(out.Cols[ColMax], v)
				out.Cols[ColSum] = append(out.Cols[ColSum], 0)
				out.Cols[ColCount] = append(out.Cols[ColCount], 0)
				out.Cols[ColLast] = append(out.Cols[ColLast], v)
			case model.KindCounter:
				out.Cols[ColCtrLast] = append(out.Cols[ColCtrLast], 0)
				out.Cols[ColCtrMaxRate] = append(out.Cols[ColCtrMaxRate], math.NaN())
			default:
				out.Cols[ColCtrLast] = append(out.Cols[ColCtrLast], 0)
			}
		}
		k := len(out.T) - 1
		switch s.Kind {
		case model.KindGauge:
			v := s.Cols[0][i]
			if v < out.Cols[ColMin][k] || math.IsNaN(out.Cols[ColMin][k]) {
				out.Cols[ColMin][k] = v
			}
			if v > out.Cols[ColMax][k] || math.IsNaN(out.Cols[ColMax][k]) {
				out.Cols[ColMax][k] = v
			}
			if !math.IsNaN(v) {
				out.Cols[ColSum][k] += v
				out.Cols[ColCount][k]++
			}
			out.Cols[ColLast][k] = v
		case model.KindCounter:
			out.Cols[ColCtrLast][k] = adj[i]
			if r := rates[i]; !math.IsNaN(r) {
				if m := out.Cols[ColCtrMaxRate][k]; math.IsNaN(m) || r > m {
					out.Cols[ColCtrMaxRate][k] = r
				}
			}
		default:
			out.Cols[ColCtrLast][k] = adj[i]
		}
	}
	return out
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// rollupAll computes the 1m, 10m and 1h rollups of a raw series. For
// cumulative kinds it advances st.
func rollupAll(s *colSeries, st *ctrState, resets []int64) [nTiers]*colSeries {
	var out [nTiers]*colSeries
	var adj, rates []float64
	if s.Kind.IsCumulative() {
		var end ctrState
		adj, rates, end = adjust(s, *st, resets)
		*st = end
	}
	for t := Tier1m; t <= Tier1h; t++ {
		out[t] = rollup(s, t, adj, rates)
	}
	return out
}

// mergeRaw unions raw series parts by timestamp. Earlier parts win on equal
// timestamps, so the main block is passed first.
func mergeRaw(parts []*colSeries) *colSeries {
	var base *colSeries
	for _, p := range parts {
		if p != nil {
			base = p
			break
		}
	}
	if base == nil {
		return nil
	}
	ncols := len(base.Cols)
	type pt struct {
		t    int64
		vals []float64
	}
	var pts []pt
	seen := map[int64]bool{}
	for _, p := range parts {
		if p == nil {
			continue
		}
		for i, t := range p.T {
			if seen[t] {
				continue
			}
			seen[t] = true
			v := make([]float64, ncols)
			for c := 0; c < ncols && c < len(p.Cols); c++ {
				v[c] = p.Cols[c][i]
			}
			pts = append(pts, pt{t, v})
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].t < pts[j].t })
	out := &colSeries{ID: base.ID, Group: base.Group, Kind: base.Kind, Start: base.Start, Cols: make([][]float64, ncols)}
	out.T = make([]int64, len(pts))
	for c := range out.Cols {
		out.Cols[c] = make([]float64, len(pts))
	}
	for i, p := range pts {
		out.T[i] = p.t
		for c := range p.vals {
			out.Cols[c][i] = p.vals[c]
		}
	}
	return out
}

// overrideWindows merges rollup parts where each later part replaces the
// earlier ones inside its own authority range [from, to).
type authPart struct {
	s        *colSeries
	from, to int64
}

func overrideMerge(parts []authPart) *colSeries {
	var out *colSeries
	for _, p := range parts {
		if p.s == nil {
			continue
		}
		if out == nil {
			out = &colSeries{ID: p.s.ID, Group: p.s.Group, Kind: p.s.Kind, Cols: make([][]float64, len(p.s.Cols))}
		}
		// Drop existing points inside the new part's authority range.
		keep := 0
		for i, t := range out.T {
			if t >= p.from && t < p.to {
				continue
			}
			out.T[keep] = t
			for c := range out.Cols {
				out.Cols[c][keep] = out.Cols[c][i]
			}
			keep++
		}
		out.T = out.T[:keep]
		for c := range out.Cols {
			out.Cols[c] = out.Cols[c][:keep]
		}
		for i, t := range p.s.T {
			if t < p.from || t >= p.to {
				continue
			}
			out.T = append(out.T, t)
			for c := range out.Cols {
				v := math.NaN()
				if c < len(p.s.Cols) {
					v = p.s.Cols[c][i]
				}
				out.Cols[c] = append(out.Cols[c], v)
			}
		}
	}
	if out == nil {
		return nil
	}
	sortCols(out)
	return out
}

func sortCols(s *colSeries) {
	if sort.SliceIsSorted(s.T, func(i, j int) bool { return s.T[i] < s.T[j] }) {
		return
	}
	idx := make([]int, len(s.T))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return s.T[idx[a]] < s.T[idx[b]] })
	t := make([]int64, len(idx))
	for i, k := range idx {
		t[i] = s.T[k]
	}
	s.T = t
	for c := range s.Cols {
		col := make([]float64, len(idx))
		for i, k := range idx {
			col[i] = s.Cols[c][k]
		}
		s.Cols[c] = col
	}
}
