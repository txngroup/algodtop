package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Tier is a storage resolution.
type Tier uint8

// Tiers, finest first.
const (
	TierRaw Tier = 0
	Tier1m  Tier = 1
	Tier10m Tier = 2
	Tier1h  Tier = 3
	nTiers       = 4
)

// WindowNS is the head and partial block length: 2 h aligned to UTC.
const WindowNS = int64(2 * time.Hour)

// Res returns the bucket width of a rollup tier, or 0 for raw.
func (t Tier) Res() int64 {
	switch t {
	case Tier1m:
		return int64(time.Minute)
	case Tier10m:
		return int64(10 * time.Minute)
	case Tier1h:
		return int64(time.Hour)
	}
	return 0
}

func (t Tier) String() string {
	switch t {
	case TierRaw:
		return "raw"
	case Tier1m:
		return "1m"
	case Tier10m:
		return "10m"
	case Tier1h:
		return "1h"
	}
	return "?"
}

// ParseTier parses a tier name.
func ParseTier(s string) (Tier, bool) {
	for t := Tier(0); t < nTiers; t++ {
		if t.String() == s {
			return t, true
		}
	}
	return 0, false
}

// windowStart aligns t down to its 2 h window.
func windowStart(t int64) int64 {
	w := t / WindowNS * WindowNS
	if t < 0 && t%WindowNS != 0 {
		w -= WindowNS
	}
	return w
}

// periodStart returns the compaction period that holds t for a rollup tier:
// a UTC day for 1m, an ISO week (Monday) for 10m, a month for 1h.
func periodStart(tier Tier, t int64) int64 {
	u := time.Unix(0, t).UTC()
	switch tier {
	case Tier1m:
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).UnixNano()
	case Tier10m:
		d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		wd := (int(d.Weekday()) + 6) % 7
		return d.AddDate(0, 0, -wd).UnixNano()
	case Tier1h:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).UnixNano()
	}
	return windowStart(t)
}

func periodEnd(tier Tier, start int64) int64 {
	u := time.Unix(0, start).UTC()
	switch tier {
	case Tier1m:
		return u.AddDate(0, 0, 1).UnixNano()
	case Tier10m:
		return u.AddDate(0, 0, 7).UnixNano()
	case Tier1h:
		return u.AddDate(0, 1, 0).UnixNano()
	}
	return start + WindowNS
}

// blockRef describes one block file.
type blockRef struct {
	Path  string
	Tier  Tier
	Kind  uint8
	Node  uint32
	Start int64
	End   int64
	Gen   uint32
	Size  int64
}

var blockName = regexp.MustCompile(`^n(\d+)-(-?\d+)\.([gspc])(\d+)\.blk$`)

func kindLetter(k uint8) string {
	switch k {
	case bkMain:
		return "g"
	case bkSupp:
		return "s"
	case bkPartial:
		return "p"
	}
	return "c"
}

// blockPath returns where a block lives.
func blockPath(root string, tier Tier, kind uint8, node uint32, start int64, gen uint32) string {
	u := time.Unix(0, start).UTC()
	var sub string
	switch tier {
	case TierRaw:
		sub = u.Format("2006-01-02")
	case Tier1m:
		sub = u.Format("2006-01")
	default:
		sub = u.Format("2006")
	}
	name := fmt.Sprintf("n%d-%d.%s%d.blk", node, start/int64(time.Second), kindLetter(kind), gen)
	return filepath.Join(root, "chunks", tier.String(), sub, name)
}

func parseBlockPath(path string, tier Tier) (blockRef, bool) {
	m := blockName.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return blockRef{}, false
	}
	node, _ := strconv.ParseUint(m[1], 10, 32)
	secs, _ := strconv.ParseInt(m[2], 10, 64)
	gen, _ := strconv.ParseUint(m[4], 10, 32)
	start := secs * int64(time.Second)
	r := blockRef{Path: path, Tier: tier, Node: uint32(node), Start: start, Gen: uint32(gen)}
	switch m[3] {
	case "g":
		r.Kind = bkMain
	case "s":
		r.Kind = bkSupp
	case "p":
		r.Kind = bkPartial
	case "c":
		r.Kind = bkCompacted
	}
	if r.Kind == bkCompacted {
		r.End = periodEnd(tier, start)
	} else {
		r.End = start + WindowNS
	}
	return r, true
}

// catalog is the in-memory list of block files per tier and node.
type catalog struct {
	mu    sync.RWMutex
	tiers [nTiers]map[uint32][]blockRef
}

func newCatalog() *catalog {
	c := &catalog{}
	for i := range c.tiers {
		c.tiers[i] = map[uint32][]blockRef{}
	}
	return c
}

// scan loads every block under root.
func (c *catalog) scan(root string) error {
	for t := Tier(0); t < nTiers; t++ {
		dir := filepath.Join(root, "chunks", t.String())
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if len(name) > 5 && name[:5] == ".tmp-" {
				os.Remove(p) // a write interrupted by a crash
				return nil
			}
			r, ok := parseBlockPath(p, t)
			if !ok {
				return nil
			}
			if fi, err := d.Info(); err == nil {
				r.Size = fi.Size()
			}
			c.addLocked(r)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *catalog) addLocked(r blockRef) {
	list := append(c.tiers[r.Tier][r.Node], r)
	sort.Slice(list, func(i, j int) bool {
		if list[i].Start != list[j].Start {
			return list[i].Start < list[j].Start
		}
		if list[i].Kind != list[j].Kind {
			return list[i].Kind < list[j].Kind
		}
		return list[i].Gen < list[j].Gen
	})
	c.tiers[r.Tier][r.Node] = list
}

func (c *catalog) add(r blockRef) {
	c.mu.Lock()
	c.addLocked(r)
	c.mu.Unlock()
}

func (c *catalog) remove(path string, tier Tier, node uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.tiers[tier][node]
	for i, r := range list {
		if r.Path == path {
			c.tiers[tier][node] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

// blocks returns a copy of the blocks of a tier and node that overlap
// [from, to).
func (c *catalog) blocks(tier Tier, node uint32, from, to int64) []blockRef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []blockRef
	for _, r := range c.tiers[tier][node] {
		if r.End > from && r.Start < to {
			out = append(out, r)
		}
	}
	return out
}

// all returns every block of a tier and node.
func (c *catalog) all(tier Tier, node uint32) []blockRef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]blockRef(nil), c.tiers[tier][node]...)
}

// nodes lists node IDs with blocks in a tier.
func (c *catalog) nodes(tier Tier) []uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []uint32
	for n := range c.tiers[tier] {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// coverage returns the earliest start and latest end held in a tier.
func (c *catalog) coverage(tier Tier, node uint32) (int64, int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := c.tiers[tier][node]
	if len(list) == 0 {
		return 0, 0, false
	}
	lo, hi := list[0].Start, list[0].End
	for _, r := range list {
		lo = min(lo, r.Start)
		hi = max(hi, r.End)
	}
	return lo, hi, true
}

// size returns total bytes per tier.
func (c *catalog) size() [nTiers]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out [nTiers]int64
	for t := range c.tiers {
		for _, list := range c.tiers[t] {
			for _, r := range list {
				out[t] += r.Size
			}
		}
	}
	return out
}

// limiter is a byte-rate limiter for background I/O.
type limiter struct {
	mu   sync.Mutex
	rate float64 // bytes per second, 0 = unlimited
	next time.Time
}

func newLimiter(bytesPerSec int64) *limiter { return &limiter{rate: float64(bytesPerSec)} }

func (l *limiter) wait(n int) {
	if l == nil || l.rate <= 0 || n <= 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	d := time.Until(l.next)
	l.mu.Unlock()
	if d > 5*time.Millisecond {
		time.Sleep(d)
	}
}
