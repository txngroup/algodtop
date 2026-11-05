package store

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"algod.top/internal/model"
)

// MaintenanceReport says what one maintenance pass did.
type MaintenanceReport struct {
	Compacted int
	Folded    int
	Deleted   []Deletion
	Corrupt   []string
	FloorHit  bool
	Paused    bool
}

// Deletion is one block removed by retention or the disk floor.
type Deletion struct {
	Node   string `json:"node"`
	Tier   string `json:"tier"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"` // retention, floor
}

// Maintain runs one background pass: fold supplementary raw blocks,
// compact closed rollup periods, delete blocks past their window, and
// enforce the disk floor. Jobs run one at a time under the I/O limit. It
// runs on its own goroutine, never the writer's.
func (db *DB) Maintain() MaintenanceReport {
	var rep MaintenanceReport
	now := db.now().UnixNano()
	for _, node := range db.cat.nodes(TierRaw) {
		rep.Folded += db.foldSupps(node)
	}
	for t := Tier1m; t <= Tier1h; t++ {
		for _, node := range db.cat.nodes(t) {
			db.sweepPartials(node, t)
			rep.Compacted += db.compactTier(node, t)
		}
	}
	rep.Deleted = append(rep.Deleted, db.applyRetention(now)...)
	floorDel, hit, paused := db.enforceFloor(now)
	rep.Deleted = append(rep.Deleted, floorDel...)
	rep.FloorHit, rep.Paused = hit, paused
	if len(rep.Deleted) > 0 {
		db.reportDeletions(now, rep.Deleted)
	}
	return rep
}

// foldSupps merges supplementary raw blocks of closed windows into the main
// block for that window.
func (db *DB) foldSupps(node uint32) int {
	// The newest window may get a head again (after a restart), so it is
	// never folded.
	headStart := max(db.HeadStart(node), windowStart(db.Newest(node)))
	byWin := map[int64][]blockRef{}
	for _, r := range db.cat.all(TierRaw, node) {
		if headStart != 0 && r.Start >= headStart {
			continue
		}
		byWin[r.Start] = append(byWin[r.Start], r)
	}
	folded := 0
	for ws, refs := range byWin {
		hasSupp := false
		for _, r := range refs {
			if r.Kind == bkSupp {
				hasSupp = true
			}
		}
		if !hasSupp {
			continue
		}
		if err := db.foldWindow(node, ws, refs); err != nil {
			db.logf("store: fold node %d window %d: %v", node, ws, err)
			continue
		}
		folded++
	}
	return folded
}

func (db *DB) foldWindow(node uint32, ws int64, refs []blockRef) error {
	db.bgMu.Lock()
	defer db.bgMu.Unlock()
	var main *blockRef
	var supps []blockRef
	for _, r := range refs {
		r := r
		switch r.Kind {
		case bkMain:
			if main == nil || r.Gen > main.Gen {
				main = &r
			}
		case bkSupp:
			supps = append(supps, r)
		}
	}
	inputs := append([]blockRef(nil), supps...)
	if main != nil {
		inputs = append([]blockRef{*main}, inputs...)
	}
	var brs []*blockReader
	for _, r := range inputs {
		br, err := openBlock(r.Path)
		if err != nil {
			closeAll(brs)
			return err
		}
		brs = append(brs, br)
	}
	defer closeAll(brs)
	gen := uint32(1)
	if main != nil {
		gen = main.Gen + 1
	}
	bw, err := newBlockWriter(blockPath(db.dir, TierRaw, bkMain, node, ws, gen),
		blockHeader{Tier: TierRaw, Kind: bkMain, Node: node, Start: ws, End: ws + WindowNS, Gen: gen}, db.lim)
	if err != nil {
		return err
	}
	if err := mergeBlocksInto(bw, brs, nil); err != nil {
		bw.abort()
		return err
	}
	if err := bw.finish(); err != nil {
		return err
	}
	db.addBlock(bw)
	for _, r := range refs {
		if r.Path != bw.path {
			db.deleteBlock(r)
		}
	}
	return nil
}

// mergeBlocksInto streams the union of several blocks into bw one series
// at a time. With auth set, block i is authoritative over auth[i] and
// replaces earlier blocks there (rollups). Without it, earlier blocks win
// on equal timestamps (raw).
func mergeBlocksInto(bw *blockWriter, brs []*blockReader, auth [][2]int64) error {
	tsCache := make([]map[model.Group][]int64, len(brs))
	allTS := map[model.Group][]int64{}
	idset := map[uint32]bool{}
	var resets []int64
	for i, br := range brs {
		tsCache[i] = map[model.Group][]int64{}
		for g := range br.groups {
			ts, err := br.timestamps(g)
			if err != nil {
				return err
			}
			tsCache[i][g] = ts
		}
		for id := range br.series {
			idset[id] = true
		}
		resets = append(resets, br.resets...)
	}
	ids := make([]uint32, 0, len(idset))
	for id := range idset {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// Group columns must precede series, so merge every series once to
	// learn the output timestamps, then again to write. Series are read
	// twice rather than held in memory.
	mergeOne := func(id uint32) (*colSeries, error) {
		if auth != nil {
			var parts []authPart
			for i, br := range brs {
				s, err := br.readSeries(id, tsCache[i])
				if err != nil {
					return nil, err
				}
				if s != nil {
					parts = append(parts, authPart{s: s, from: auth[i][0], to: auth[i][1]})
				}
			}
			return overrideMerge(parts), nil
		}
		var parts []*colSeries
		for i, br := range brs {
			s, err := br.readSeries(id, tsCache[i])
			if err != nil {
				return nil, err
			}
			if s != nil {
				parts = append(parts, s)
			}
		}
		return mergeRaw(parts), nil
	}
	for _, id := range ids {
		s, err := mergeOne(id)
		if err != nil {
			return err
		}
		if s != nil {
			allTS[s.Group] = unionTS(allTS[s.Group], s.T)
		}
	}
	for _, g := range sortedGroups(allTS) {
		if err := bw.writeGroup(g, allTS[g]); err != nil {
			return err
		}
	}
	bw.resets = dedupSorted(resets)
	for _, id := range ids {
		s, err := mergeOne(id)
		if err != nil {
			return err
		}
		if s == nil {
			continue
		}
		if err := bw.writeColSeries(s, allTS[s.Group]); err != nil {
			return err
		}
	}
	return nil
}

// compactTier merges the partial blocks of each closed period into one
// compacted block. Partials are removed only after the merged block is
// fsynced.
func (db *DB) compactTier(node uint32, tier Tier) int {
	headStart := db.HeadStart(node)
	if headStart == 0 {
		headStart = windowStart(db.now().UnixNano())
	}
	byPeriod := map[int64][]blockRef{}
	for _, r := range db.cat.all(tier, node) {
		ps := r.Start
		if r.Kind == bkPartial {
			ps = periodStart(tier, r.Start)
		}
		byPeriod[ps] = append(byPeriod[ps], r)
	}
	n := 0
	for ps, refs := range byPeriod {
		pe := periodEnd(tier, ps)
		if pe > headStart {
			continue // still open
		}
		var comp *blockRef
		partials := map[int64]blockRef{}
		for _, r := range refs {
			r := r
			switch r.Kind {
			case bkCompacted:
				if comp == nil || r.Gen > comp.Gen {
					comp = &r
				}
			case bkPartial:
				if b, ok := partials[r.Start]; !ok || r.Gen > b.Gen {
					partials[r.Start] = r
				}
			}
		}
		if len(partials) == 0 {
			continue
		}
		if err := db.compactPeriod(node, tier, ps, pe, comp, partials, refs); err != nil {
			db.logf("store: compact node %d tier %s period %d: %v", node, tier, ps, err)
			continue
		}
		n++
	}
	return n
}

func (db *DB) compactPeriod(node uint32, tier Tier, ps, pe int64, comp *blockRef, partials map[int64]blockRef, all []blockRef) error {
	var inputs []blockRef
	var auth [][2]int64
	if comp != nil {
		inputs = append(inputs, *comp)
		auth = append(auth, [2]int64{ps, pe})
	}
	var starts []int64
	for s := range partials {
		starts = append(starts, s)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, s := range starts {
		r := partials[s]
		inputs = append(inputs, r)
		auth = append(auth, [2]int64{r.Start, r.End})
	}
	var brs []*blockReader
	for _, r := range inputs {
		br, err := openBlock(r.Path)
		if err != nil {
			closeAll(brs)
			return err
		}
		brs = append(brs, br)
	}
	defer closeAll(brs)
	gen := uint32(0)
	if comp != nil {
		gen = comp.Gen + 1
	}
	bw, err := newBlockWriter(blockPath(db.dir, tier, bkCompacted, node, ps, gen),
		blockHeader{Tier: tier, Kind: bkCompacted, Node: node, Start: ps, End: pe, Gen: gen}, db.lim)
	if err != nil {
		return err
	}
	if err := mergeBlocksInto(bw, brs, auth); err != nil {
		bw.abort()
		return err
	}
	if err := bw.finish(); err != nil {
		return err
	}
	db.addBlock(bw)
	// Remove every input, plus older generations the merge superseded.
	for _, r := range all {
		if r.Path == bw.path {
			continue
		}
		if r.Kind == bkCompacted || containsPath(inputs, r.Path) || (r.Kind == bkPartial && r.Gen <= partials[r.Start].Gen) {
			db.deleteBlock(r)
		}
	}
	return nil
}

func containsPath(refs []blockRef, p string) bool {
	for _, r := range refs {
		if r.Path == p {
			return true
		}
	}
	return false
}

func (db *DB) deleteBlock(r blockRef) bool {
	if err := os.Remove(r.Path); err != nil && !os.IsNotExist(err) {
		// On Windows an open block cannot be deleted until its reader
		// closes. The next pass retries.
		return false
	}
	db.cat.remove(r.Path, r.Tier, r.Node)
	return true
}

// applyRetention deletes blocks whose whole range is older than the tier's
// window. A block can outlive its window by up to one block length.
func (db *DB) applyRetention(now int64) []Deletion {
	var out []Deletion
	ret := db.opts.Retention
	for t := Tier(0); t < nTiers; t++ {
		if ret[t] == Forever {
			continue
		}
		cutoff := now - int64(ret[t])
		for _, node := range db.cat.nodes(t) {
			for _, r := range db.cat.all(t, node) {
				if r.End <= cutoff && db.deleteBlock(r) {
					out = append(out, Deletion{Node: db.NodeName(node), Tier: t.String(), From: r.Start, To: r.End, Bytes: r.Size, Reason: "retention"})
				}
			}
		}
	}
	if ret[TierRaw] != Forever {
		cutoff := now - int64(ret[TierRaw])
		if n, b := db.usage.pruneBefore(cutoff); n > 0 {
			out = append(out, Deletion{Tier: "peer-usage", To: cutoff, Bytes: b, Reason: "retention"})
		}
	}
	addrRet := db.opts.AddrRetention
	if addrRet == 0 {
		addrRet = 365 * 24 * time.Hour
	}
	if n := db.aggregateSessions(now - int64(addrRet)); n > 0 {
		out = append(out, Deletion{Tier: "peer-addresses", Reason: fmt.Sprintf("retention: %d session files reduced to /24 aggregates", n)})
	}
	if db.opts.InfoLogDays > 0 {
		for _, f := range db.infologs.removeBefore(now - int64(db.opts.InfoLogDays)*int64(24*time.Hour)) {
			out = append(out, Deletion{Tier: "info-log-lines", Reason: "retention " + f})
		}
	}
	// Warning and error lines are kept 90 days.
	for _, f := range db.rawlogs.removeBefore(now - int64(90*24*time.Hour)) {
		out = append(out, Deletion{Tier: "log-lines", Reason: "retention " + f})
	}
	return out
}

// floorPlan is the order in which the disk floor deletes data.
func (db *DB) floorCandidates(now int64) [][]blockRef {
	var stages [][]blockRef
	minW := db.opts.FloorMin
	for _, t := range []Tier{TierRaw, Tier1m, Tier10m} {
		cut := now - int64(minW[t])
		var stage []blockRef
		for _, node := range db.cat.nodes(t) {
			for _, r := range db.cat.all(t, node) {
				if r.End <= cut {
					stage = append(stage, r)
				}
			}
		}
		sort.Slice(stage, func(i, j int) bool { return stage[i].Start < stage[j].Start })
		stages = append(stages, stage)
	}
	// Finally the oldest block of any tier, so the newest data goes last.
	var any []blockRef
	for t := Tier(0); t < nTiers; t++ {
		for _, node := range db.cat.nodes(t) {
			any = append(any, db.cat.all(t, node)...)
		}
	}
	sort.Slice(any, func(i, j int) bool { return any[i].Start < any[j].Start })
	stages = append(stages, any)
	return stages
}

// enforceFloor deletes the oldest data when the volume is nearly full.
func (db *DB) enforceFloor(now int64) ([]Deletion, bool, bool) {
	u, err := db.opts.DiskUsage(db.dir)
	if err != nil || u.Total == 0 {
		return nil, false, false
	}
	floor := db.opts.DiskFloor
	if u.FreeFraction() >= floor {
		if db.IsPaused() {
			db.Resume(now)
		}
		return nil, false, false
	}
	if db.opts.AtFloor == "pause" {
		db.Pause(now)
		return nil, true, true
	}
	target := floor + 0.01
	var out []Deletion
	// Optional data first: kept log lines.
	for _, f := range db.rawlogs.removeBefore(now) {
		out = append(out, Deletion{Tier: "log-lines", Reason: "floor " + f})
	}
	for _, stage := range db.floorCandidates(now) {
		for _, r := range stage {
			u, err = db.opts.DiskUsage(db.dir)
			if err == nil && u.FreeFraction() >= target {
				return out, true, false
			}
			if db.deleteBlock(r) {
				out = append(out, Deletion{Node: db.NodeName(r.Node), Tier: r.Tier.String(), From: r.Start, To: r.End, Bytes: r.Size, Reason: "floor"})
			}
		}
	}
	u, err = db.opts.DiskUsage(db.dir)
	if err == nil && u.FreeFraction() < floor {
		db.Pause(now)
		return out, true, true
	}
	return out, true, false
}

func (db *DB) reportDeletions(now int64, ds []Deletion) {
	type key struct{ node, tier, reason string }
	agg := map[key]*Deletion{}
	var order []key
	for _, d := range ds {
		reason := d.Reason
		if strings.HasPrefix(reason, "retention") {
			reason = "retention"
		} else if strings.HasPrefix(reason, "floor") {
			reason = "floor"
		}
		k := key{d.Node, d.Tier, reason}
		a := agg[k]
		if a == nil {
			c := d
			c.Reason = reason
			agg[k] = &c
			order = append(order, k)
			continue
		}
		if d.From != 0 && (a.From == 0 || d.From < a.From) {
			a.From = d.From
		}
		a.To = max(a.To, d.To)
		a.Bytes += d.Bytes
	}
	for _, k := range order {
		d := agg[k]
		sev := model.SevInfo
		msg := fmt.Sprintf("retention removed %s data", d.Tier)
		if d.Reason == "floor" {
			sev = model.SevWarn
			msg = fmt.Sprintf("disk floor removed %s data", d.Tier)
		}
		if d.From != 0 {
			msg += fmt.Sprintf(" from %s to %s", time.Unix(0, d.From).UTC().Format(time.DateTime), time.Unix(0, d.To).UTC().Format(time.DateTime))
		}
		db.AppendEvent(model.Event{Time: now, Node: d.Node, Kind: model.EvRetention, Severity: sev, Msg: msg,
			Attrs: map[string]any{"tier": d.Tier, "from": d.From, "to": d.To, "bytes": d.Bytes, "reason": d.Reason}})
	}
}

// Scrub verifies every block's CRCs under the I/O limit and returns the
// paths that fail.
func (db *DB) Scrub() []string {
	var bad []string
	for t := Tier(0); t < nTiers; t++ {
		for _, node := range db.cat.nodes(t) {
			for _, r := range db.cat.all(t, node) {
				br, err := openBlock(r.Path)
				if err != nil {
					if !os.IsNotExist(err) {
						bad = append(bad, r.Path)
					}
					continue
				}
				db.lim.wait(int(r.Size))
				if err := br.verify(); err != nil {
					bad = append(bad, r.Path)
				}
				br.close()
			}
		}
	}
	return bad
}

// PreviewRetention lists what a set of windows would delete, without
// deleting anything.
func (db *DB) PreviewRetention(ret [nTiers]time.Duration) []Deletion {
	now := db.now().UnixNano()
	var out []Deletion
	for t := Tier(0); t < nTiers; t++ {
		if ret[t] == Forever {
			continue
		}
		cutoff := now - int64(ret[t])
		for _, node := range db.cat.nodes(t) {
			for _, r := range db.cat.all(t, node) {
				if r.End <= cutoff {
					out = append(out, Deletion{Node: db.NodeName(node), Tier: t.String(), From: r.Start, To: r.End, Bytes: r.Size, Reason: "retention"})
				}
			}
		}
	}
	return out
}

// sweepPartials deletes partial blocks superseded by a newer generation of
// the same window.
func (db *DB) sweepPartials(node uint32, tier Tier) {
	best := map[int64]uint32{}
	refs := db.cat.all(tier, node)
	for _, r := range refs {
		if r.Kind == bkPartial && r.Gen >= best[r.Start] {
			best[r.Start] = r.Gen
		}
	}
	for _, r := range refs {
		if r.Kind == bkPartial && r.Gen < best[r.Start] {
			db.deleteBlock(r)
		}
	}
}
