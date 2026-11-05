// Package query plans and runs series queries: it picks a tier per part of
// the range, stitches tiers together, and aggregates to exactly the number
// of buckets the client can draw. The functions are fixed: rate, increase,
// avg, min, max, last, percentiles and heatmaps from histograms, and sum by
// label. There is no query language.
package query

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/promparse"
	"algod.top/internal/store"
)

// Aliases maps old metric names to new ones, so a chart that spans an algod
// upgrade stays continuous. The raw stored name is kept, so nothing is lost
// if an alias is wrong.
var Aliases = map[string]string{}

// Matcher filters series by a label.
type Matcher struct {
	Name   string
	Value  string
	Negate bool
	Prefix bool
}

// ParseMatcher parses label:value, label:!value or label:prefix*.
func ParseMatcher(s string) (Matcher, error) {
	name, val, ok := strings.Cut(s, ":")
	if !ok {
		name, val, ok = strings.Cut(s, "=")
	}
	if !ok || name == "" {
		return Matcher{}, fmt.Errorf("bad match %q, want label:value", s)
	}
	m := Matcher{Name: name}
	if strings.HasPrefix(val, "!") {
		m.Negate = true
		val = val[1:]
	}
	if strings.HasSuffix(val, "*") {
		m.Prefix = true
		val = strings.TrimSuffix(val, "*")
	}
	m.Value = val
	return m, nil
}

func (m Matcher) match(ls model.Labels) bool {
	v := ls.Get(m.Name)
	ok := v == m.Value
	if m.Prefix {
		ok = strings.HasPrefix(v, m.Value)
	}
	return ok != m.Negate
}

// Request is one series query.
type Request struct {
	Node   string
	Metric string
	Match  []Matcher
	From   int64
	To     int64
	Points int
	Tier   string // "" plans automatically, "raw" forces full resolution
	Fn     string
	By     string
	Limit  int // max series, default 50
}

// Part says which tier answered a stretch of the range.
type Part struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Tier string `json:"tier"`
}

// Series is one result line. Values are NaN where there is no data, and
// the API writes those as null.
type Series struct {
	Labels map[string]string `json:"labels"`
	Kind   string            `json:"kind"`
	T      []int64           `json:"t"`
	Avg    []float64         `json:"avg"`
	Min    []float64         `json:"min,omitempty"`
	Max    []float64         `json:"max,omitempty"`
}

// Heatmap is a histogram over time.
type Heatmap struct {
	Labels map[string]string `json:"labels"`
	LE     []float64         `json:"le"`
	T      []int64           `json:"t"`
	Counts [][]float64       `json:"counts"` // [bucket][le]
}

// Result is a query answer.
type Result struct {
	Node     string    `json:"node"`
	Metric   string    `json:"metric"`
	Fn       string    `json:"fn"`
	From     int64     `json:"from"`
	To       int64     `json:"to"`
	Step     int64     `json:"step"`
	Parts    []Part    `json:"parts"`
	Series   []Series  `json:"series"`
	Heatmaps []Heatmap `json:"heatmaps,omitempty"`
	Resets   []int64   `json:"resets,omitempty"`
	Raw      bool      `json:"raw,omitempty"`
	Kind     string    `json:"kind"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Engine runs queries against a store with a concurrency limit and a
// timeout, so a heavy query waits or fails but never delays the writer.
type Engine struct {
	DB      *store.DB
	sem     chan struct{}
	Timeout time.Duration
	Now     func() time.Time
}

// NewEngine returns an engine allowing n concurrent queries.
func NewEngine(db *store.DB, n int, timeout time.Duration) *Engine {
	if n <= 0 {
		n = 2
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Engine{DB: db, sem: make(chan struct{}, n), Timeout: timeout, Now: time.Now}
}

// ErrBusy is returned when the query queue is full for too long.
var ErrBusy = errors.New("query: too many concurrent queries, try again")

// Run executes a request.
func (e *Engine) Run(ctx context.Context, r Request) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ErrBusy
	}
	type out struct {
		r   *Result
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := e.run(r)
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		return o.r, o.err
	case <-ctx.Done():
		return nil, fmt.Errorf("query: timed out after %v", e.Timeout)
	}
}

func (e *Engine) run(r Request) (*Result, error) {
	db := e.DB
	node, ok := db.NodeID(r.Node)
	if !ok {
		return nil, fmt.Errorf("unknown node %q", r.Node)
	}
	if r.To <= r.From {
		return nil, errors.New("query: to must be after from")
	}
	if r.Points <= 0 {
		r.Points = 300
	}
	r.Points = min(r.Points, 5000)
	if r.Limit <= 0 {
		r.Limit = 50
	}
	metas := e.selectSeries(node, r)
	if len(metas) == 0 {
		return &Result{Node: r.Node, Metric: r.Metric, Fn: r.Fn, From: r.From, To: r.To, Step: (r.To - r.From) / int64(r.Points)}, nil
	}
	kind := metas[0].Kind
	isHist := false
	if base := promparse.HistBase(r.Metric); base != "" && kind == model.KindHistPart && strings.HasSuffix(r.Metric, "_bucket") {
		isHist = true
	}
	fn := r.Fn
	if fn == "" {
		switch {
		case isHist:
			fn = "p99"
		case kind.IsCumulative():
			fn = "rate"
		default:
			fn = "avg"
		}
	}
	res := &Result{Node: r.Node, Metric: r.Metric, Fn: fn, From: r.From, To: r.To, Kind: kind.String()}
	if len(metas) > r.Limit && r.By == "" && !isHist {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d series match; showing the first %d. Use match= or by= to narrow.", len(metas), r.Limit))
		metas = metas[:r.Limit]
	}
	ids := make([]uint32, len(metas))
	for i, m := range metas {
		ids[i] = m.ID
	}
	raw := r.Tier == "raw"
	parts := e.plan(node, r, raw)
	res.Parts = parts
	res.Step = (r.To - r.From) / int64(r.Points)
	if raw {
		res.Raw = true
		res.Step = 0
	}
	// Read each part.
	type partData struct {
		part   Part
		tier   store.Tier
		points map[uint32]store.Points
	}
	var pds []partData
	for _, p := range parts {
		tier, _ := store.ParseTier(p.Tier)
		// Read one extra bucket before the part so rates have a base.
		pad := int64(10 * time.Minute)
		if tier != store.TierRaw {
			pad = 2 * tier.Res()
		}
		pts, resets, err := db.Read(node, ids, tier, p.From-pad, p.To)
		if err != nil {
			return nil, err
		}
		m := map[uint32]store.Points{}
		for _, x := range pts {
			m[x.Meta.ID] = x
		}
		res.Resets = append(res.Resets, resets...)
		pds = append(pds, partData{p, tier, m})
	}
	sort.Slice(res.Resets, func(i, j int) bool { return res.Resets[i] < res.Resets[j] })
	// Per series: build per-bucket aggregates across parts.
	nb := r.Points
	step := float64(r.To-r.From) / float64(nb)
	bucketOf := func(t int64) int {
		if t < r.From || t >= r.To {
			return -1
		}
		return min(int(float64(t-r.From)/step), nb-1)
	}
	if isHist {
		return e.histogram(res, metas, r, fn, pds2(pds, func(p partData) (Part, store.Tier, map[uint32]store.Points) { return p.part, p.tier, p.points }), bucketOf, nb, step)
	}
	type acc struct {
		min, max, sum, wsum float64
		n                   int
		last                float64
	}
	groups := map[string]*Series{}
	var order []string
	for _, meta := range metas {
		var out []Sample
		for _, pd := range pds {
			pts, ok := pd.points[meta.ID]
			if !ok {
				continue
			}
			out = append(out, samplesFor(pts, pd.part, pd.tier, fn, meta.Kind, res.Resets)...)
		}
		labels := meta.Labels.Map()
		key := meta.Labels.String()
		if r.By != "" {
			labels = map[string]string{r.By: meta.Labels.Get(r.By)}
			key = r.By + "=" + meta.Labels.Get(r.By)
		}
		if raw {
			s := groups[key]
			if s == nil {
				s = &Series{Labels: labels, Kind: meta.Kind.String()}
				groups[key] = s
				order = append(order, key)
			}
			mergeRawInto(s, out, r.By != "")
			continue
		}
		accs := make([]acc, nb)
		for i := range accs {
			accs[i] = acc{min: math.Inf(1), max: math.Inf(-1)}
		}
		for _, sm := range out {
			b := bucketOf(sm.T)
			if b < 0 || math.IsNaN(sm.V) {
				continue
			}
			a := &accs[b]
			w := sm.W
			if w <= 0 {
				w = 1
			}
			a.sum += sm.V * w
			a.wsum += w
			a.n++
			lo, hi := sm.Lo, sm.Hi
			if math.IsNaN(lo) {
				lo = sm.V
			}
			if math.IsNaN(hi) {
				hi = sm.V
			}
			a.min = math.Min(a.min, lo)
			a.max = math.Max(a.max, hi)
			a.last = sm.V
		}
		s := groups[key]
		if s == nil {
			s = &Series{Labels: labels, Kind: meta.Kind.String(), T: make([]int64, nb), Avg: nanSlice(nb), Min: nanSlice(nb), Max: nanSlice(nb)}
			for i := range s.T {
				s.T[i] = r.From + int64(float64(i)*step)
			}
			groups[key] = s
			order = append(order, key)
		}
		for i, a := range accs {
			if a.n == 0 {
				continue
			}
			v := a.sum / a.wsum
			if fn == "last" {
				v = a.last
			}
			if fn == "increase" {
				v = a.sum
			}
			if r.By != "" && !math.IsNaN(s.Avg[i]) {
				s.Avg[i] += v
				s.Min[i] += a.min
				s.Max[i] += a.max
			} else {
				s.Avg[i], s.Min[i], s.Max[i] = v, a.min, a.max
			}
			if fn == "min" {
				s.Avg[i] = s.Min[i]
			}
			if fn == "max" {
				s.Avg[i] = s.Max[i]
			}
		}
	}
	for _, k := range order {
		res.Series = append(res.Series, *groups[k])
	}
	return res, nil
}

func pds2[T any](in []T, f func(T) (Part, store.Tier, map[uint32]store.Points)) []histPart {
	out := make([]histPart, len(in))
	for i, x := range in {
		p, t, m := f(x)
		out[i] = histPart{p, t, m}
	}
	return out
}

func nanSlice(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = math.NaN()
	}
	return s
}

// selectSeries finds the series for the metric and its aliases.
func (e *Engine) selectSeries(node uint32, r Request) []store.SeriesMeta {
	names := []string{r.Metric}
	for old, cur := range Aliases {
		if cur == r.Metric {
			names = append(names, old)
		}
	}
	var out []store.SeriesMeta
	for _, n := range names {
		for _, m := range e.DB.Find(node, n) {
			ok := true
			for _, mt := range r.Match {
				if !mt.match(m.Labels) {
					ok = false
					break
				}
			}
			if ok {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Labels.String() < out[j].Labels.String() })
	return out
}

// plan picks a tier for each part of the range.
//
//  1. The coarsest tier whose resolution is at or below range / points.
//  2. Any part not yet in that tier (the head, or late data still being
//     rolled up) comes from the next finer tier, down to raw and the head.
//  3. Any part older than the tier holds comes from the finest tier that
//     still holds it.
func (e *Engine) plan(node uint32, r Request, raw bool) []Part {
	if raw {
		return []Part{{r.From, r.To, "raw"}}
	}
	step := (r.To - r.From) / int64(r.Points)
	tier := store.TierRaw
	for t := store.Tier1h; t >= store.Tier1m; t-- {
		if t.Res() <= step {
			tier = t
			break
		}
	}
	if tier == store.TierRaw {
		return []Part{{r.From, r.To, "raw"}}
	}
	lo, hi, ok := e.DB.Coverage(node, tier)
	if !ok {
		// Nothing rolled up yet: the head and raw blocks answer.
		return []Part{{r.From, r.To, "raw"}}
	}
	// The head's window answers from raw. A restart flushes a partial
	// rollup for it at close, and that partial stops at the restart while
	// the head goes on.
	if hs := e.DB.HeadStart(node); hs != 0 && hs < hi {
		hi = max(hs, lo)
	}
	var parts []Part
	from := r.From
	if from < lo {
		// Older than this tier holds: the finest coarser tier with data.
		older := tier
		for t := tier + 1; t <= store.Tier1h; t++ {
			if l2, _, ok := e.DB.Coverage(node, t); ok && l2 < lo {
				older = t
				break
			}
		}
		if older != tier {
			end := min(lo, r.To)
			parts = append(parts, Part{from, end, older.String()})
		}
		from = lo
	}
	if from < r.To && from < hi {
		parts = append(parts, Part{from, min(hi, r.To), tier.String()})
		from = min(hi, r.To)
	}
	if from < r.To {
		parts = append(parts, Part{from, r.To, "raw"})
	}
	if len(parts) == 0 {
		parts = []Part{{r.From, r.To, tier.String()}}
	}
	return e.alignParts(node, parts)
}

// alignParts gives a rollup tier only the whole buckets inside its part.
// A bucket that starts before the part, or runs past it, would be counted
// whole or not at all, so the partial-bucket edges come from the finest
// tier that still holds them.
func (e *Engine) alignParts(node uint32, parts []Part) []Part {
	var out []Part
	add := func(p Part) {
		if p.To <= p.From {
			return
		}
		if n := len(out); n > 0 && out[n-1].Tier == p.Tier && out[n-1].To == p.From {
			out[n-1].To = p.To
			return
		}
		out = append(out, p)
	}
	finer := func(t0 int64, coarse store.Tier) string {
		for t := store.TierRaw; t < coarse; t++ {
			if lo, _, ok := e.DB.Coverage(node, t); ok && lo <= t0 {
				return t.String()
			}
			if t == store.TierRaw && e.DB.HeadStart(node) != 0 && e.DB.HeadStart(node) <= t0 {
				return "raw"
			}
		}
		return coarse.String()
	}
	for _, p := range parts {
		tier, _ := store.ParseTier(p.Tier)
		res := tier.Res()
		if res == 0 {
			add(p)
			continue
		}
		a := (p.From + res - 1) / res * res
		b := p.To / res * res
		if a >= b {
			add(Part{p.From, p.To, finer(p.From, tier)})
			continue
		}
		add(Part{p.From, a, finer(p.From, tier)})
		add(Part{a, b, p.Tier})
		add(Part{b, p.To, finer(b, tier)})
	}
	return out
}

// Sample is one value placed in time, with an optional min/max and a
// weight (seconds covered) for averaging rates.
type Sample struct {
	T  int64
	V  float64
	Lo float64
	Hi float64
	W  float64
}

// samplesFor turns stored points into values for fn.
func samplesFor(p store.Points, part Part, tier store.Tier, fn string, kind model.Kind, resets []int64) []Sample {
	var out []Sample
	inPart := func(t int64) bool { return t >= part.From && t < part.To }
	if tier == store.TierRaw {
		vals := p.Cols[0]
		if kind.IsCumulative() && (fn == "rate" || fn == "increase" || fn == "avg" || fn == "max" || fn == "min") {
			ri := 0
			for i := 1; i < len(p.T); i++ {
				t0, t1 := p.T[i-1], p.T[i]
				for ri < len(resets) && resets[ri] <= t0 {
					ri++
				}
				reset := vals[i] < vals[i-1] || (ri < len(resets) && resets[ri] <= t1)
				dt := float64(t1-t0) / 1e9
				if dt <= 0 || !inPart(t1) {
					continue
				}
				d := vals[i] - vals[i-1]
				if reset {
					d = vals[i]
				}
				if fn == "increase" {
					out = append(out, Sample{T: t1, V: d, Lo: math.NaN(), Hi: math.NaN(), W: 1})
					continue
				}
				out = append(out, Sample{T: t1, V: d / dt, Lo: math.NaN(), Hi: math.NaN(), W: dt})
			}
			return out
		}
		for i, t := range p.T {
			if inPart(t) {
				out = append(out, Sample{T: t, V: vals[i], Lo: math.NaN(), Hi: math.NaN(), W: 1})
			}
		}
		return out
	}
	res := tier.Res()
	if kind == model.KindGauge {
		for i, t := range p.T {
			if !inPart(t) {
				continue
			}
			cnt := p.Cols[store.ColCount][i]
			avg := math.NaN()
			if cnt > 0 {
				avg = p.Cols[store.ColSum][i] / cnt
			}
			v := avg
			if fn == "last" {
				v = p.Cols[store.ColLast][i]
			}
			out = append(out, Sample{T: t, V: v, Lo: p.Cols[store.ColMin][i], Hi: p.Cols[store.ColMax][i], W: cnt})
		}
		return out
	}
	// Counters: rates between consecutive reset-adjusted lasts.
	last := p.Cols[store.ColCtrLast]
	for i := 1; i < len(p.T); i++ {
		t := p.T[i]
		if !inPart(t) {
			continue
		}
		dt := float64(t-p.T[i-1]) / 1e9
		if dt <= 0 {
			continue
		}
		d := last[i] - last[i-1]
		if d < 0 {
			d = last[i] - last[i-1] // should not happen with adjusted values
			d = math.Max(0, d)
		}
		hi := math.NaN()
		if len(p.Cols) > store.ColCtrMaxRate {
			hi = p.Cols[store.ColCtrMaxRate][i]
		}
		// The bucket starts at t; its rate is placed at the bucket start.
		_ = res
		if fn == "increase" {
			out = append(out, Sample{T: t, V: d, Lo: math.NaN(), Hi: math.NaN(), W: 1})
			continue
		}
		out = append(out, Sample{T: t, V: d / dt, Lo: math.NaN(), Hi: hi, W: dt})
	}
	return out
}

func mergeRawInto(s *Series, smp []Sample, sum bool) {
	if !sum || len(s.T) == 0 {
		for _, x := range smp {
			s.T = append(s.T, x.T)
			s.Avg = append(s.Avg, x.V)
		}
		return
	}
	idx := map[int64]int{}
	for i, t := range s.T {
		idx[t] = i
	}
	for _, x := range smp {
		if i, ok := idx[x.T]; ok {
			s.Avg[i] += x.V
		} else {
			s.T = append(s.T, x.T)
			s.Avg = append(s.Avg, x.V)
		}
	}
}

type histPart struct {
	part   Part
	tier   store.Tier
	points map[uint32]store.Points
}

// histogram computes percentiles or a heatmap from bucket deltas.
func (e *Engine) histogram(res *Result, metas []store.SeriesMeta, r Request, fn string, pds []histPart, bucketOf func(int64) int, nb int, step float64) (*Result, error) {
	// Group bucket series by their labels minus le.
	type hgroup struct {
		labels map[string]string
		les    []float64
		ids    []uint32
	}
	groups := map[string]*hgroup{}
	var order []string
	for _, m := range metas {
		leS := m.Labels.Get("le")
		if leS == "" {
			continue
		}
		le, err := parseLE(leS)
		if err != nil {
			continue
		}
		var rest model.Labels
		for _, l := range m.Labels {
			if l.Name != "le" && (r.By == "" || l.Name == r.By) {
				rest = append(rest, l)
			}
		}
		k := rest.String()
		g := groups[k]
		if g == nil {
			g = &hgroup{labels: rest.Map()}
			groups[k] = g
			order = append(order, k)
		}
		g.les = append(g.les, le)
		g.ids = append(g.ids, m.ID)
	}
	for _, k := range order {
		g := groups[k]
		idx := make([]int, len(g.les))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(a, b int) bool { return g.les[idx[a]] < g.les[idx[b]] })
		les := make([]float64, len(idx))
		ids := make([]uint32, len(idx))
		for i, j := range idx {
			les[i], ids[i] = g.les[j], g.ids[j]
		}
		// counts[bucket][le] = cumulative increase in the bucket
		counts := make([][]float64, nb)
		for i := range counts {
			counts[i] = make([]float64, len(les))
		}
		for li, id := range ids {
			for _, pd := range pds {
				p, ok := pd.points[id]
				if !ok {
					continue
				}
				for _, s := range samplesFor(p, pd.part, pd.tier, "increase", model.KindHistPart, res.Resets) {
					if b := bucketOf(s.T); b >= 0 && !math.IsNaN(s.V) {
						counts[b][li] += s.V
					}
				}
			}
		}
		ts := make([]int64, nb)
		for i := range ts {
			ts[i] = r.From + int64(float64(i)*step)
		}
		if fn == "heatmap" {
			// Convert cumulative le counts to per-bin counts.
			bins := make([][]float64, nb)
			for i := range counts {
				bins[i] = make([]float64, len(les))
				prev := 0.0
				for j, c := range counts[i] {
					bins[i][j] = math.Max(0, c-prev)
					prev = c
				}
			}
			res.Heatmaps = append(res.Heatmaps, Heatmap{Labels: g.labels, LE: les, T: ts, Counts: bins})
			continue
		}
		q := 0.99
		switch fn {
		case "p50":
			q = 0.5
		case "p90":
			q = 0.9
		case "p99":
			q = 0.99
		case "p999":
			q = 0.999
		}
		s := Series{Labels: g.labels, Kind: "histogram", T: ts, Avg: nanSlice(nb)}
		for i := range counts {
			s.Avg[i] = Quantile(q, les, counts[i])
		}
		res.Series = append(res.Series, s)
	}
	res.Kind = "histogram"
	return res, nil
}

func parseLE(s string) (float64, error) {
	if s == "+Inf" || s == "Inf" {
		return math.Inf(1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// Quantile interpolates a quantile from cumulative bucket counts, as
// Prometheus histogram_quantile does.
func Quantile(q float64, les, cum []float64) float64 {
	if len(les) == 0 {
		return math.NaN()
	}
	total := cum[len(cum)-1]
	if total <= 0 || math.IsNaN(total) {
		return math.NaN()
	}
	rank := q * total
	for i, c := range cum {
		if c >= rank {
			if math.IsInf(les[i], 1) {
				if i > 0 {
					return les[i-1]
				}
				return math.NaN()
			}
			lo, clo := 0.0, 0.0
			if i > 0 {
				lo, clo = les[i-1], cum[i-1]
			}
			if c == clo {
				return les[i]
			}
			return lo + (les[i]-lo)*(rank-clo)/(c-clo)
		}
	}
	return les[len(les)-1]
}
