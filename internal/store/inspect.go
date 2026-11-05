package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"algod.top/internal/model"
)

// Inspection is a read-only look at a store directory: it never replays
// the WAL or takes the lock, so it is safe while a daemon runs.
type Inspection struct {
	Nodes  []NodeInfo
	Kinds  map[uint32]map[model.Kind]int // node -> kind -> series
	Tiers  [nTiers]map[uint32]TierInfo
	Other  int64 // bytes outside block files
	Exists bool
	cat    *catalog
}

// TierInfo is one tier's holdings for a node.
type TierInfo struct {
	From, To int64
	Bytes    int64
	Blocks   int
}

// Inspect reads a store directory without opening it for writing.
func Inspect(dir string) (*Inspection, error) {
	in := &Inspection{Kinds: map[uint32]map[model.Kind]int{}}
	for i := range in.Tiers {
		in.Tiers[i] = map[uint32]TierInfo{}
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if os.IsNotExist(err) {
		return in, nil
	}
	if err != nil {
		return nil, err
	}
	in.Exists = true
	var mf metaFile
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, err
	}
	in.Nodes = mf.Nodes
	if f, err := os.Open(filepath.Join(dir, "series", "index.bin")); err == nil {
		ix := &index{byKey: map[seriesKeyT]uint32{}, meta: []SeriesMeta{{}}, byNode: map[uint32]map[string][]uint32{}, count: map[uint32]int{}}
		ix.load(f)
		f.Close()
		for _, m := range ix.meta[1:] {
			if in.Kinds[m.Node] == nil {
				in.Kinds[m.Node] = map[model.Kind]int{}
			}
			in.Kinds[m.Node][m.Kind]++
		}
	}
	in.cat = newCatalog()
	in.cat.scan(dir)
	var blockBytes int64
	for t := Tier(0); t < nTiers; t++ {
		for _, n := range in.cat.nodes(t) {
			ti := TierInfo{}
			for i, r := range in.cat.all(t, n) {
				if i == 0 || r.Start < ti.From {
					ti.From = r.Start
				}
				ti.To = max(ti.To, r.End)
				ti.Bytes += r.Size
				ti.Blocks++
			}
			blockBytes += ti.Bytes
			in.Tiers[t][n] = ti
		}
	}
	return in, nil
}

// Projection is projected disk use per tier for the configured windows.
type Projection struct {
	Tier     string
	PerDay   int64 // bytes per day for all nodes
	Window   time.Duration
	Settled  int64 // bytes once the window is full, or -1 if forever
	PerYear  int64 // growth per year for forever tiers
	Measured bool
}

// Project estimates disk use. It uses measured bytes per day where a tier
// holds at least a day of data, and otherwise 1.5 bytes per stored value,
// the design's pessimistic figure.
func (in *Inspection) Project(ret [nTiers]time.Duration, nodes int, scrape time.Duration) []Projection {
	if scrape <= 0 {
		scrape = 10 * time.Second
	}
	// Values per bucket from the series mix, or Mainnet's when unknown.
	gauges, counters, hists := 116.0, 287.0, 676.0
	var g, c, h float64
	for _, k := range in.Kinds {
		g += float64(k[model.KindGauge])
		c += float64(k[model.KindCounter])
		h += float64(k[model.KindHistPart])
	}
	if n := float64(len(in.Kinds)); n > 0 {
		gauges, counters, hists = g/n, c/n, h/n
	}
	if nodes <= 0 {
		nodes = max(1, len(in.Kinds))
	}
	perRaw := (gauges + counters + hists) * float64(24*time.Hour/scrape) * 1.5
	perBucket := (gauges*5 + counters*2 + hists) * 1.5
	estimates := [nTiers]float64{perRaw, perBucket * 1440, perBucket * 144, perBucket * 24}
	var out []Projection
	for t := Tier(0); t < nTiers; t++ {
		p := Projection{Tier: t.String(), Window: ret[t]}
		perDay := estimates[t] * float64(nodes)
		var bytes int64
		var span time.Duration
		for _, ti := range in.Tiers[t] {
			bytes += ti.Bytes
			span = max(span, time.Duration(ti.To-ti.From))
		}
		if span >= 24*time.Hour && bytes > 0 {
			measuredNodes := max(1, len(in.Tiers[t]))
			perDay = float64(bytes) / span.Hours() * 24 / float64(measuredNodes) * float64(nodes)
			p.Measured = true
		}
		p.PerDay = int64(perDay)
		if ret[t] == 0 {
			p.Settled = -1
			p.PerYear = int64(perDay * 365)
		} else {
			p.Settled = int64(perDay * ret[t].Hours() / 24)
		}
		out = append(out, p)
	}
	return out
}

// Preview lists blocks a set of windows would delete now.
func (in *Inspection) Preview(ret [nTiers]time.Duration, now time.Time) []Deletion {
	if in.cat == nil {
		return nil
	}
	names := map[uint32]string{}
	for _, n := range in.Nodes {
		names[n.ID] = n.Name
	}
	var out []Deletion
	for t := Tier(0); t < nTiers; t++ {
		if ret[t] == 0 {
			continue
		}
		cut := now.Add(-ret[t]).UnixNano()
		for _, n := range in.cat.nodes(t) {
			for _, r := range in.cat.all(t, n) {
				if r.End <= cut {
					out = append(out, Deletion{Node: names[n], Tier: t.String(), From: r.Start, To: r.End, Bytes: r.Size, Reason: "retention"})
				}
			}
		}
	}
	return out
}

// Inspection returns an inspection of the open store from memory.
func (db *DB) Inspection() *Inspection {
	in := &Inspection{Kinds: map[uint32]map[model.Kind]int{}, Nodes: db.Nodes(), Exists: true, cat: db.cat}
	for i := range in.Tiers {
		in.Tiers[i] = map[uint32]TierInfo{}
	}
	db.ix.mu.RLock()
	for _, m := range db.ix.meta[1:] {
		if in.Kinds[m.Node] == nil {
			in.Kinds[m.Node] = map[model.Kind]int{}
		}
		in.Kinds[m.Node][m.Kind]++
	}
	db.ix.mu.RUnlock()
	for t := Tier(0); t < nTiers; t++ {
		for _, n := range db.cat.nodes(t) {
			ti := TierInfo{}
			for i, r := range db.cat.all(t, n) {
				if i == 0 || r.Start < ti.From {
					ti.From = r.Start
				}
				ti.To = max(ti.To, r.End)
				ti.Bytes += r.Size
				ti.Blocks++
			}
			in.Tiers[t][n] = ti
		}
	}
	return in
}
