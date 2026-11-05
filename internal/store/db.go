// Package store is algodtop's chunk store: a WAL, per-node in-memory heads,
// immutable 2 h raw blocks, partial rollups at 1m, 10m and 1h, compaction,
// retention by guaranteed window, and append-only event and session logs.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"algod.top/internal/disk"
	"algod.top/internal/enc"
	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// FormatVersion is the on-disk format version in meta.json.
const FormatVersion = 1

// Forever marks a tier window that never expires.
const Forever = time.Duration(0)

// Options configure a DB.
type Options struct {
	Dir         string
	Caps        Caps
	Retention   [nTiers]time.Duration // 0 = forever
	GroupCommit time.Duration         // WAL fsync interval, default 1 s
	IdleGrace   time.Duration         // idle head flush, default 10 min
	BGRate      int64                 // background bytes/s, default 10 MB/s
	DiskFloor   float64               // free fraction, default 0.05
	AtFloor     string                // "delete" (default) or "pause"
	FloorMin    [nTiers]time.Duration // minimum windows kept at the floor
	Now         func() time.Time
	Logf        func(format string, args ...any)
	// NoRollups skips the 1m, 10m and 1h tiers, for agents whose store is
	// only a buffer.
	NoRollups bool
	// AddrRetention is how long peer sessions keep raw addresses, default
	// 1 year. After that only /24 aggregates remain.
	AddrRetention time.Duration
	// InfoLogDays keeps raw info log lines this long when a node asks for
	// them; warnings and errors are kept 90 days.
	InfoLogDays int
	// DiskUsage reports the volume holding the store; tests replace it.
	DiskUsage func(dir string) (disk.Usage, error)
}

// DefaultRetention is the default guaranteed window per tier.
var DefaultRetention = [nTiers]time.Duration{30 * 24 * time.Hour, 365 * 24 * time.Hour, 5 * 365 * 24 * time.Hour, Forever}

// DefaultFloorMin is the minimum history the disk floor keeps per tier.
var DefaultFloorMin = [nTiers]time.Duration{2 * 24 * time.Hour, 30 * 24 * time.Hour, 365 * 24 * time.Hour, 0}

// NodeInfo is one entry in the node registry.
type NodeInfo struct {
	ID          uint32 `json:"id"`
	Name        string `json:"name"`
	GenesisID   string `json:"genesis_id,omitempty"`
	GenesisHash string `json:"genesis_hash,omitempty"`
	Created     int64  `json:"created"`
	Retired     bool   `json:"retired,omitempty"`
}

type metaFile struct {
	Format  int        `json:"format"`
	Created int64      `json:"created"`
	Nodes   []NodeInfo `json:"nodes"`
}

type watermark struct {
	HeadSeq  uint64 `json:"head_seq"`
	LateSeq  uint64 `json:"late_seq"`
	UsageSeq uint64 `json:"usage_seq"`
	Newest   int64  `json:"newest"`
	// lastHead and lastLate are the newest sequence numbers applied.
	lastHead uint64
	lastLate uint64
}

type lateBatch struct {
	b   model.Batch
	ids []uint32
	seq uint64
}

// Stats are the store's self-monitoring counters.
type Stats struct {
	Appended     atomic.Int64
	Late         atomic.Int64
	Dropped      atomic.Int64 // samples dropped by caps
	Paused       atomic.Int64 // batches discarded while writes are paused
	Flushes      atomic.Int64
	WriteNanos   atomic.Int64 // last append latency
	BytesWritten atomic.Int64
}

// DB is the store. Append, AppendEvent, AppendUsage, Tick and Close are
// called only from the writer goroutine. Query methods are safe from any
// goroutine.
type DB struct {
	opts Options
	dir  string
	now  func() time.Time
	logf func(string, ...any)

	metaMu sync.Mutex
	meta   metaFile

	ix  *index
	cat *catalog
	wal *wal
	lim *limiter

	mu    sync.RWMutex // guards heads, wm, ctr maps
	heads map[uint32]*head
	late  map[uint32][]lateBatch
	wm    map[uint32]*watermark
	ctr   map[uint32]map[uint32]ctrState

	bgMu sync.Mutex // serialises late-data rewrites with raw folding

	events   *recLog
	sessions *recLog
	rawlogs  *recLog
	infologs *recLog
	ann      *annotations
	usage    *usageStore

	recentMu sync.Mutex
	recent   []model.Event

	paused     atomic.Bool
	pausedFrom int64
	lastCommit time.Time
	lastLate   time.Time
	closed     bool

	Stats Stats

	lock *os.File
}

// Open opens or creates a store in opts.Dir and replays the WAL.
func Open(opts Options) (*DB, error) {
	if opts.Dir == "" {
		return nil, errors.New("store: no directory")
	}
	if opts.GroupCommit <= 0 {
		opts.GroupCommit = time.Second
	}
	if opts.IdleGrace <= 0 {
		opts.IdleGrace = 10 * time.Minute
	}
	if opts.BGRate == 0 {
		opts.BGRate = 10 << 20
	}
	if opts.DiskFloor == 0 {
		opts.DiskFloor = 0.05
	}
	if opts.AtFloor == "" {
		opts.AtFloor = "delete"
	}
	if opts.FloorMin == ([nTiers]time.Duration{}) {
		opts.FloorMin = DefaultFloorMin
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DiskUsage == nil {
		opts.DiskUsage = disk.Stat
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	db := &DB{opts: opts, dir: opts.Dir, now: opts.Now, logf: opts.Logf, cat: newCatalog(), lim: newLimiter(opts.BGRate),
		heads: map[uint32]*head{}, late: map[uint32][]lateBatch{}, wm: map[uint32]*watermark{}, ctr: map[uint32]map[uint32]ctrState{}}
	for _, d := range []string{"wal", "series", "chunks", "events", "sessions", "peers", "state", "logs"} {
		if err := os.MkdirAll(filepath.Join(opts.Dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	lock, err := fsx.Lock(filepath.Join(opts.Dir, "LOCK"))
	if err != nil {
		if errors.Is(err, fsx.ErrLocked) {
			return nil, ErrInUse
		}
		return nil, err
	}
	db.lock = lock
	if err := db.loadMeta(); err != nil {
		return nil, err
	}
	if db.ix, err = openIndex(opts.Dir, opts.Caps); err != nil {
		return nil, err
	}
	if err := db.cat.scan(opts.Dir); err != nil {
		return nil, err
	}
	if err := db.loadState(); err != nil {
		return nil, err
	}
	if db.events, err = newRecLog(filepath.Join(opts.Dir, "events"), false); err != nil {
		return nil, err
	}
	if db.sessions, err = newRecLog(filepath.Join(opts.Dir, "sessions"), false); err != nil {
		return nil, err
	}
	if db.rawlogs, err = newRecLog(filepath.Join(opts.Dir, "logs"), true); err != nil {
		return nil, err
	}
	if db.infologs, err = newRecLog(filepath.Join(opts.Dir, "logs-info"), true); err != nil {
		return nil, err
	}
	if db.ann, err = openAnnotations(filepath.Join(opts.Dir, "annotations.json")); err != nil {
		return nil, err
	}
	db.usage = newUsageStore(db)
	if err := db.usage.scan(); err != nil {
		return nil, err
	}
	db.loadRecent()
	segs, maxSeq, err := replayWAL(filepath.Join(opts.Dir, "wal"), db.replay)
	if err != nil {
		return nil, err
	}
	// Sequence numbers never go back. After a clean restart every segment
	// may be pruned or empty, so the next number also follows each
	// segment's first number and every saved watermark. Reusing numbers
	// would let replay skip new records as already flushed.
	for _, s := range segs {
		if s.first > 0 {
			maxSeq = max(maxSeq, s.first-1)
		}
	}
	for _, w := range db.wm {
		maxSeq = max(maxSeq, w.HeadSeq, w.LateSeq, w.UsageSeq)
	}
	if db.wal, err = openWAL(filepath.Join(opts.Dir, "wal"), segs, maxSeq); err != nil {
		return nil, err
	}
	db.lastCommit = db.now()
	return db, nil
}

// Dir returns the store directory.
func (db *DB) Dir() string { return db.dir }

func (db *DB) loadMeta() error {
	path := filepath.Join(db.dir, "meta.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		db.meta = metaFile{Format: FormatVersion, Created: db.now().UnixNano()}
		return db.saveMetaLocked()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &db.meta); err != nil {
		return fmt.Errorf("store: meta.json: %w", err)
	}
	if db.meta.Format > FormatVersion {
		return fmt.Errorf("store: format %d is newer than this algodtop (%d)", db.meta.Format, FormatVersion)
	}
	return nil
}

func (db *DB) saveMetaLocked() error {
	b, err := json.MarshalIndent(db.meta, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(db.dir, "meta.json"), b, 0o600)
}

// RegisterNode returns the ID for a node name and genesis. A name that
// reappears with a different genesis hash gets a new ID, so two networks
// never merge into one history.
func (db *DB) RegisterNode(name, genesisID, genesisHash string) (uint32, error) {
	db.metaMu.Lock()
	defer db.metaMu.Unlock()
	for i := len(db.meta.Nodes) - 1; i >= 0; i-- {
		n := &db.meta.Nodes[i]
		if n.Name != name || n.Retired {
			continue
		}
		if n.GenesisHash == genesisHash || genesisHash == "" {
			return n.ID, nil
		}
		if n.GenesisHash == "" {
			n.GenesisHash, n.GenesisID = genesisHash, genesisID
			return n.ID, db.saveMetaLocked()
		}
		n.Retired = true
	}
	id := uint32(1)
	for _, n := range db.meta.Nodes {
		id = max(id, n.ID+1)
	}
	db.meta.Nodes = append(db.meta.Nodes, NodeInfo{ID: id, Name: name, GenesisID: genesisID, GenesisHash: genesisHash, Created: db.now().UnixNano()})
	return id, db.saveMetaLocked()
}

// NodeID returns the current ID for a node name.
func (db *DB) NodeID(name string) (uint32, bool) {
	db.metaMu.Lock()
	defer db.metaMu.Unlock()
	for i := len(db.meta.Nodes) - 1; i >= 0; i-- {
		if n := db.meta.Nodes[i]; n.Name == name && !n.Retired {
			return n.ID, true
		}
	}
	for i := len(db.meta.Nodes) - 1; i >= 0; i-- {
		if n := db.meta.Nodes[i]; n.Name == name {
			return n.ID, true
		}
	}
	return 0, false
}

// Nodes returns the node registry.
func (db *DB) Nodes() []NodeInfo {
	db.metaMu.Lock()
	defer db.metaMu.Unlock()
	return append([]NodeInfo(nil), db.meta.Nodes...)
}

// NodeName returns the name of a node ID.
func (db *DB) NodeName(id uint32) string {
	db.metaMu.Lock()
	defer db.metaMu.Unlock()
	for _, n := range db.meta.Nodes {
		if n.ID == id {
			return n.Name
		}
	}
	return ""
}

// state files

type stateFile struct {
	Watermarks map[uint32]*watermark `json:"watermarks"`
}

func (db *DB) loadState() error {
	b, err := os.ReadFile(filepath.Join(db.dir, "state", "watermarks.json"))
	if err == nil {
		var sf stateFile
		if err := json.Unmarshal(b, &sf); err != nil {
			return fmt.Errorf("store: watermarks: %w", err)
		}
		for k, v := range sf.Watermarks {
			v.lastHead, v.lastLate = v.HeadSeq, v.LateSeq
			db.wm[k] = v
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	ents, _ := os.ReadDir(filepath.Join(db.dir, "state"))
	for _, e := range ents {
		var node uint32
		if _, err := fmt.Sscanf(e.Name(), "ctr-n%d.bin", &node); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(db.dir, "state", e.Name()))
		if err != nil {
			continue
		}
		m := map[uint32]ctrState{}
		rd := &byteReader{b: b}
		n := rd.uvarint()
		for i := uint64(0); i < n && rd.err == nil; i++ {
			id := rd.u32()
			st := ctrState{Offset: math.Float64frombits(rd.u64()), LastRaw: math.Float64frombits(rd.u64()), LastT: int64(rd.u64())}
			m[id] = st
		}
		if rd.err == nil {
			db.ctr[node] = m
		}
	}
	return nil
}

func (db *DB) saveWatermarks() error {
	db.mu.RLock()
	sf := stateFile{Watermarks: map[uint32]*watermark{}}
	for k, v := range db.wm {
		c := *v
		sf.Watermarks[k] = &c
	}
	db.mu.RUnlock()
	b, err := json.Marshal(sf)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(db.dir, "state", "watermarks.json"), b, 0o600)
}

func (db *DB) saveCtr(node uint32) error {
	db.mu.RLock()
	m := db.ctr[node]
	ids := make([]uint32, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b := binary.AppendUvarint(nil, uint64(len(ids)))
	for _, id := range ids {
		st := m[id]
		b = binary.LittleEndian.AppendUint32(b, id)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(st.Offset))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(st.LastRaw))
		b = binary.LittleEndian.AppendUint64(b, uint64(st.LastT))
	}
	db.mu.RUnlock()
	return fsx.WriteAtomic(filepath.Join(db.dir, "state", fmt.Sprintf("ctr-n%d.bin", node)), b, 0o600)
}

func (db *DB) watermark(node uint32) *watermark {
	w := db.wm[node]
	if w == nil {
		w = &watermark{}
		db.wm[node] = w
	}
	return w
}

// replay applies one WAL record during Open.
func (db *DB) replay(rec *walRecord) {
	w := db.watermark(rec.node)
	switch rec.category() {
	case catLate:
		if rec.seq <= w.LateSeq {
			return
		}
	case catUsage:
		if rec.seq <= w.UsageSeq {
			return
		}
	default:
		if rec.seq <= w.HeadSeq {
			return
		}
	}
	switch rec.typ {
	case walBatch:
		b := model.Batch{Node: db.NodeName(rec.node), Group: rec.group, Time: rec.time, Reset: rec.flags&walFlagReset != 0}
		b.Samples = make([]model.Sample, len(rec.ids))
		for i, id := range rec.ids {
			m, _ := db.ix.get(id)
			b.Samples[i] = model.Sample{Name: m.Name, Labels: m.Labels, Kind: m.Kind, Value: rec.vals[i]}
		}
		if rec.flags&walFlagLate != 0 {
			db.late[rec.node] = append(db.late[rec.node], lateBatch{b: b, ids: rec.ids, seq: rec.seq})
			w.lastLate = max(w.lastLate, rec.seq)
		} else {
			// A record replayed into the late buffer keeps a head sequence
			// number; flushHead writes the late buffer before it moves the
			// head watermark.
			w.lastHead = max(w.lastHead, rec.seq)
			if late, h := db.route(rec.node, &b); late || !h.append(&b, rec.ids, db.now().UnixNano()) {
				db.late[rec.node] = append(db.late[rec.node], lateBatch{b: b, ids: rec.ids, seq: rec.seq})
			}
		}
		w.Newest = max(w.Newest, rec.time)
	case walPeerUsage:
		db.usage.add(rec.node, rec.seq, rec.usage)
	}
}

// route finds the head for a batch. It flushes the current head first when
// the batch starts a newer window. With no head, a window that already has
// a block (after a restart or an idle flush) is reloaded into a head, so
// the window keeps growing in order rather than through the late path.
// Data older than the node's newest window, or out of order within the
// head, is late.
func (db *DB) route(node uint32, b *model.Batch) (bool, *head) {
	ws := windowStart(b.Time)
	db.mu.RLock()
	h := db.heads[node]
	newest := int64(0)
	if w := db.wm[node]; w != nil {
		newest = w.Newest
	}
	db.mu.RUnlock()
	if h != nil && ws > h.start {
		if err := db.flushHead(h); err != nil {
			db.logf("store: flush node %d window %d: %v", node, h.start, err)
		}
		h = nil
	}
	if h == nil {
		if newest != 0 && ws < windowStart(newest) {
			return true, nil
		}
		h = db.loadHead(node, ws)
		db.mu.Lock()
		db.heads[node] = h
		db.mu.Unlock()
	}
	if ws < h.start {
		return true, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ts := h.groups[b.Group]; ts != nil && ts.Count() > 0 && b.Time <= ts.Last() {
		return true, nil
	}
	return false, h
}

func (db *DB) replaying() bool { return db.wal == nil }

// mainBlock returns the newest main block of a window.
func (db *DB) mainBlock(node uint32, ws int64) (blockRef, bool) {
	var best blockRef
	found := false
	for _, r := range db.cat.blocks(TierRaw, node, ws, ws+WindowNS) {
		if r.Kind == bkMain && r.Start == ws && (!found || r.Gen > best.Gen) {
			best, found = r, true
		}
	}
	return best, found
}

func (db *DB) hasMainBlock(node uint32, ws int64) bool {
	_, ok := db.mainBlock(node, ws)
	return ok
}

// loadHead returns a head for a window, filled from the window's main
// block when one exists. The counter adjustment state goes back to the
// block's start state, because the flush that wrote the block advanced
// it to the end of the window.
func (db *DB) loadHead(node uint32, ws int64) *head {
	h := newHead(node, ws)
	ref, ok := db.mainBlock(node, ws)
	if !ok {
		return h
	}
	br, err := openBlock(ref.Path)
	if err != nil {
		db.logf("store: reload %s: %v", ref.Path, err)
		return h
	}
	defer br.close()
	groups := map[model.Group][]int64{}
	for g := range br.groups {
		ts, err := br.timestamps(g)
		if err != nil {
			db.logf("store: reload %s: %v", ref.Path, err)
			return newHead(node, ws)
		}
		groups[g] = ts
		e := &enc.TSEncoder{}
		for _, t := range ts {
			e.Append(t)
		}
		h.groups[g] = e
		if len(ts) > 0 {
			if h.minT == 0 || ts[0] < h.minT {
				h.minT = ts[0]
			}
			h.maxT = max(h.maxT, ts[len(ts)-1])
		}
	}
	db.mu.Lock()
	ctr := db.ctr[node]
	if ctr == nil {
		ctr = map[uint32]ctrState{}
		db.ctr[node] = ctr
	}
	db.mu.Unlock()
	for id := range br.series {
		s, err := br.readSeries(id, groups)
		if err != nil || s == nil {
			continue
		}
		hs := &headSeries{group: s.Group, kind: s.Kind, pres: presenceFor(groups[s.Group], s.T)}
		for _, v := range s.Cols[0] {
			hs.vals.Append(v)
		}
		h.series[id] = hs
		h.samples += len(s.T)
		if s.Start != nil {
			db.mu.Lock()
			ctr[id] = *s.Start
			db.mu.Unlock()
		}
	}
	h.resets = append(h.resets, br.resets...)
	h.gen = ref.Gen
	return h
}

// Append stores one batch. It never blocks on queries.
func (db *DB) Append(b *model.Batch) error {
	if db.closed {
		return errors.New("store: closed")
	}
	start := time.Now()
	node, ok := db.NodeID(b.Node)
	if !ok {
		var err error
		if node, err = db.RegisterNode(b.Node, "", ""); err != nil {
			return err
		}
	}
	if db.paused.Load() {
		db.Stats.Paused.Add(1)
		return nil
	}
	ids := make([]uint32, len(b.Samples))
	for i := range b.Samples {
		id, ok := db.ix.lookup(node, &b.Samples[i], b.Group)
		if !ok {
			db.Stats.Dropped.Add(1)
		}
		ids[i] = id
	}
	// Routing may flush the previous window first; that flush must not
	// claim this record, so the WAL write comes after it.
	late, h := db.route(node, b)
	seq, err := db.wal.appendBatch(node, b, ids, late)
	if err != nil {
		return err
	}
	db.mu.Lock()
	w := db.watermark(node)
	db.mu.Unlock()
	if !late && h.append(b, ids, db.now().UnixNano()) {
		db.mu.Lock()
		w.lastHead = seq
		db.mu.Unlock()
	} else {
		db.Stats.Late.Add(1)
		cp := *b
		cp.Samples = append([]model.Sample(nil), b.Samples...)
		db.mu.Lock()
		db.late[node] = append(db.late[node], lateBatch{b: cp, ids: ids, seq: seq})
		if late {
			w.lastLate = seq
		} else {
			w.lastHead = seq
		}
		db.mu.Unlock()
	}
	db.mu.Lock()
	w.Newest = max(w.Newest, b.Time)
	db.mu.Unlock()
	db.Stats.Appended.Add(int64(len(b.Samples)))
	db.Stats.WriteNanos.Store(int64(time.Since(start)))
	return nil
}

// AppendUsage stores per-peer usage samples for a node.
func (db *DB) AppendUsage(nodeName string, us []model.PeerUsage) error {
	if len(us) == 0 || db.paused.Load() {
		return nil
	}
	node, ok := db.NodeID(nodeName)
	if !ok {
		return fmt.Errorf("store: unknown node %q", nodeName)
	}
	seq, err := db.wal.appendUsage(node, us)
	if err != nil {
		return err
	}
	db.usage.add(node, seq, us)
	return nil
}

// AppendEvent stores an event.
func (db *DB) AppendEvent(e model.Event) error {
	if e.Time == 0 {
		e.Time = db.now().UnixNano()
	}
	db.recentMu.Lock()
	db.recent = append(db.recent, e)
	if len(db.recent) > 4000 {
		db.recent = append([]model.Event(nil), db.recent[len(db.recent)-2000:]...)
	}
	db.recentMu.Unlock()
	return db.events.append(e.Time, e)
}

// AppendSession stores a finished peer session.
func (db *DB) AppendSession(s model.Session) error {
	t := s.End
	if t == 0 {
		t = s.Start
	}
	return db.sessions.append(t, s)
}

// AppendLogLine keeps one raw log line for drill-down.
func (db *DB) AppendLogLine(l LogLine) error {
	if l.Level == "info" || l.Level == "debug" {
		return db.infologs.append(l.Time, l)
	}
	return db.rawlogs.append(l.Time, l)
}

// LogLine is a kept raw log line.
type LogLine struct {
	Time     int64  `json:"time"`
	Node     string `json:"node"`
	Level    string `json:"level"`
	Template string `json:"template"`
	Line     string `json:"line"`
}

func (db *DB) loadRecent() {
	now := db.now().UnixNano()
	from := now - int64(48*time.Hour)
	var evs []model.Event
	db.events.scan(from, now+int64(time.Hour), func(b []byte) bool {
		var e model.Event
		if json.Unmarshal(b, &e) == nil && e.Time >= from {
			evs = append(evs, e)
		}
		return true
	})
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time < evs[j].Time })
	if len(evs) > 2000 {
		evs = evs[len(evs)-2000:]
	}
	db.recent = evs
}

// Tick runs the writer's periodic work: group commit, idle head flushes and
// late-data flushes. The writer goroutine calls it about once a second.
func (db *DB) Tick() error {
	now := db.now()
	if now.Sub(db.lastCommit) >= db.opts.GroupCommit {
		if err := db.commit(); err != nil {
			return err
		}
		db.lastCommit = now
	}
	db.mu.RLock()
	var idle []*head
	for _, h := range db.heads {
		if h.end+int64(db.opts.IdleGrace) < now.UnixNano() {
			idle = append(idle, h)
		}
	}
	hasLate := false
	lateSamples := 0
	for _, l := range db.late {
		if len(l) > 0 {
			hasLate = true
		}
		for _, lb := range l {
			lateSamples += len(lb.b.Samples)
		}
	}
	db.mu.RUnlock()
	for _, h := range idle {
		if err := db.flushHead(h); err != nil {
			db.logf("store: idle flush: %v", err)
		}
	}
	// Late data waits up to 30 s so a catch-up or a header backfill
	// rewrites each window's partial rollups a few times, not every tick.
	if hasLate && db.lastLate.IsZero() {
		db.lastLate = now
	}
	if hasLate && (now.Sub(db.lastLate) >= 30*time.Second || lateSamples > 200000) {
		db.lastLate = time.Time{}
		if err := db.flushLate(); err != nil {
			db.logf("store: late flush: %v", err)
		}
	}
	return nil
}

// commit fsyncs the index, then the WAL, then the logs.
func (db *DB) commit() error {
	if err := db.ix.sync(); err != nil {
		return err
	}
	if err := db.wal.sync(); err != nil {
		return err
	}
	db.events.sync()
	db.sessions.sync()
	db.rawlogs.sync()
	db.infologs.sync()
	return nil
}

// Close flushes every head and late buffer, fsyncs and closes.
func (db *DB) Close() error {
	if db.closed {
		return nil
	}
	var errs []error
	db.mu.RLock()
	heads := make([]*head, 0, len(db.heads))
	for _, h := range db.heads {
		heads = append(heads, h)
	}
	db.mu.RUnlock()
	for _, h := range heads {
		if err := db.flushHead(h); err != nil {
			errs = append(errs, err)
		}
	}
	if err := db.flushLate(); err != nil {
		errs = append(errs, err)
	}
	if err := db.usage.flushAll(); err != nil {
		errs = append(errs, err)
	}
	if err := db.saveWatermarks(); err != nil {
		errs = append(errs, err)
	}
	db.wal.prune(db.flushedSeqs)
	errs = append(errs, db.wal.close(), db.ix.close(), db.events.close(), db.sessions.close(), db.rawlogs.close(), db.infologs.close())
	db.closed = true
	db.lock.Close()
	return errors.Join(errs...)
}

func (db *DB) flushedSeqs(node uint32) [nCats]uint64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	w := db.wm[node]
	if w == nil {
		return [nCats]uint64{}
	}
	return [nCats]uint64{w.HeadSeq, w.LateSeq, w.UsageSeq}
}

// flushHead writes a head to a raw block plus its partial rollups, fsyncs
// them, advances the watermark and drops the head.
func (db *DB) flushHead(h *head) error {
	// Batches diverted to the late buffer during replay carry head sequence
	// numbers, so they must be on disk before the head watermark moves.
	db.mu.RLock()
	pending := len(db.late[h.node]) > 0
	db.mu.RUnlock()
	if pending {
		if err := db.flushLate(); err != nil {
			return err
		}
	}
	if !h.empty() {
		if err := db.writeWindow(h); err != nil {
			return err
		}
	}
	if err := db.usage.flushBefore(h.node, h.end); err != nil {
		db.logf("store: peer usage flush: %v", err)
	}
	db.mu.Lock()
	if db.heads[h.node] == h {
		delete(db.heads, h.node)
	}
	w := db.watermark(h.node)
	// Late batches still buffered keep their own watermark.
	w.HeadSeq = w.lastHead
	db.mu.Unlock()
	db.Stats.Flushes.Add(1)
	if err := db.saveCtr(h.node); err != nil {
		return err
	}
	if err := db.saveWatermarks(); err != nil {
		return err
	}
	if db.wal != nil {
		db.wal.prune(db.flushedSeqs)
	}
	return nil
}

// writeWindow writes the raw block and partial rollups for a head.
func (db *DB) writeWindow(h *head) error {
	snap := h.snapshot(nil)
	groups, err := snap.decodeGroups()
	if err != nil {
		return err
	}
	node := h.node
	// Supplementary blocks for this window join the rollups.
	supps := db.openSupps(node, h.start)
	defer closeAll(supps)
	ids := make([]uint32, 0, len(snap.series))
	for id := range snap.series {
		ids = append(ids, id)
	}
	for _, br := range supps {
		for id := range br.series {
			if _, ok := snap.series[id]; !ok {
				ids = append(ids, id)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// A reloaded head rewrites the block it came from.
	raw, err := newBlockWriter(blockPath(db.dir, TierRaw, bkMain, node, h.start, h.gen),
		blockHeader{Tier: TierRaw, Kind: bkMain, Node: node, Start: h.start, End: h.end, Gen: h.gen}, nil)
	if err != nil {
		return err
	}
	gs := sortedGroups(groups)
	for _, g := range gs {
		if err := raw.writeGroup(g, groups[g]); err != nil {
			raw.abort()
			return err
		}
	}
	raw.resets = snap.resets
	resets := append([]int64(nil), snap.resets...)
	suppTS := map[*blockReader]map[model.Group][]int64{}
	for _, br := range supps {
		resets = append(resets, br.resets...)
		suppTS[br] = map[model.Group][]int64{}
	}
	sort.Slice(resets, func(i, j int) bool { return resets[i] < resets[j] })

	// Rollup group columns are the bucket starts of the raw group columns.
	var rw [nTiers]*blockWriter
	gen := db.nextPartialGen(node, h.start)
	for t := Tier1m; t <= Tier1h && !db.opts.NoRollups; t++ {
		rw[t], err = newBlockWriter(blockPath(db.dir, t, bkPartial, node, h.start, gen),
			blockHeader{Tier: t, Kind: bkPartial, Node: node, Start: h.start, End: h.end, Gen: gen}, nil)
		if err != nil {
			raw.abort()
			for k := Tier1m; k < t; k++ {
				rw[k].abort()
			}
			return err
		}
	}
	abort := func() {
		raw.abort()
		for t := Tier1m; t <= Tier1h; t++ {
			rw[t].abort()
		}
	}
	allTS := map[model.Group][]int64{}
	for g, ts := range groups {
		allTS[g] = ts
	}
	for _, br := range supps {
		for g := range br.groups {
			ts, err := br.timestamps(g)
			if err != nil {
				continue
			}
			suppTS[br][g] = ts
			allTS[g] = unionTS(allTS[g], ts)
		}
	}
	bucketTS := [nTiers]map[model.Group][]int64{}
	for t := Tier1m; t <= Tier1h; t++ {
		bucketTS[t] = map[model.Group][]int64{}
		for _, g := range sortedGroups(allTS) {
			b := bucketize(allTS[g], t.Res())
			bucketTS[t][g] = b
			if err := rw[t].writeGroup(g, b); err != nil {
				abort()
				return err
			}
		}
		rw[t].setResets(resets)
	}
	db.mu.Lock()
	ctr := db.ctr[node]
	if ctr == nil {
		ctr = map[uint32]ctrState{}
		db.ctr[node] = ctr
	}
	db.mu.Unlock()
	for _, id := range ids {
		parts := []*colSeries{}
		s, err := snap.decodeSeries(id, groups)
		if err != nil {
			abort()
			return err
		}
		db.mu.RLock()
		st := ctr[id]
		db.mu.RUnlock()
		if s != nil {
			parts = append(parts, s)
			ss := snap.series[id]
			var start *ctrStart
			if ss.kind.IsCumulative() {
				cp := st
				start = &cp
			}
			if err := raw.writeSeries(id, ss.group, ss.kind, &ss.pres, [][]byte{ss.vals}, start); err != nil {
				abort()
				return err
			}
		}
		for _, br := range supps {
			ps, err := br.readSeries(id, suppTS[br])
			if err == nil && ps != nil {
				parts = append(parts, ps)
			}
		}
		merged := mergeRaw(parts)
		if merged == nil {
			continue
		}
		ru := rollupAll(merged, &st, resets)
		if merged.Kind.IsCumulative() {
			db.mu.Lock()
			ctr[id] = st
			db.mu.Unlock()
		}
		for t := Tier1m; t <= Tier1h; t++ {
			if err := rw[t].writeColSeries(ru[t], bucketTS[t][merged.Group]); err != nil {
				abort()
				return err
			}
		}
	}
	if err := raw.finish(); err != nil {
		for t := Tier1m; t <= Tier1h; t++ {
			rw[t].abort()
		}
		return err
	}
	db.addBlock(raw)
	// Older main generations of this window are superseded.
	for _, r := range db.cat.blocks(TierRaw, node, h.start, h.end) {
		if r.Kind == bkMain && r.Start == h.start && r.Gen != h.gen {
			db.deleteBlock(r)
		}
	}
	for t := Tier1m; t <= Tier1h; t++ {
		if err := rw[t].finish(); err != nil {
			return err
		}
		db.addBlock(rw[t])
	}
	return nil
}

func (db *DB) addBlock(bw *blockWriter) {
	if bw == nil {
		return
	}
	r, ok := parseBlockPath(bw.path, bw.hdr.Tier)
	if !ok {
		return
	}
	r.Size = bw.off + trailerSize
	db.Stats.BytesWritten.Add(r.Size)
	// A rewrite of the same path replaces the old catalog entry.
	db.cat.remove(r.Path, r.Tier, r.Node)
	db.cat.add(r)
	// A new partial generation supersedes the older ones for its window.
	if r.Kind == bkPartial {
		for _, old := range db.cat.blocks(r.Tier, r.Node, r.Start, r.End) {
			if old.Kind == bkPartial && old.Start == r.Start && old.Gen < r.Gen {
				db.deleteBlock(old)
			}
		}
	}
}

func (db *DB) nextPartialGen(node uint32, start int64) uint32 {
	var gen uint32
	found := false
	for _, r := range db.cat.blocks(Tier1m, node, start, start+WindowNS) {
		if r.Kind == bkPartial && r.Start == start {
			gen = max(gen, r.Gen+1)
			found = true
		}
	}
	if !found {
		return 0
	}
	return gen
}

func (db *DB) openSupps(node uint32, start int64) []*blockReader {
	var out []*blockReader
	for _, r := range db.cat.blocks(TierRaw, node, start, start+WindowNS) {
		if r.Kind == bkSupp && r.Start == start {
			br, err := openBlock(r.Path)
			if err != nil {
				db.logf("store: open %s: %v", r.Path, err)
				continue
			}
			out = append(out, br)
		}
	}
	return out
}

func closeAll(brs []*blockReader) {
	for _, br := range brs {
		br.close()
	}
}

func sortedGroups[V any](m map[model.Group]V) []model.Group {
	out := make([]model.Group, 0, len(m))
	for g := range m {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func bucketize(ts []int64, res int64) []int64 {
	var out []int64
	for _, t := range ts {
		b := floorDiv(t, res) * res
		if len(out) == 0 || out[len(out)-1] != b {
			out = append(out, b)
		}
	}
	return out
}

func unionTS(a, b []int64) []int64 {
	out := make([]int64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i] < b[j]):
			out = append(out, a[i])
			i++
		case i >= len(a) || b[j] < a[i]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// flushLate writes buffered late batches to supplementary raw blocks and
// rewrites the partial rollups of each affected window.
func (db *DB) flushLate() error {
	db.mu.Lock()
	late := db.late
	db.late = map[uint32][]lateBatch{}
	db.mu.Unlock()
	if len(late) == 0 {
		return nil
	}
	db.bgMu.Lock()
	defer db.bgMu.Unlock()
	var errs []error
	for node, batches := range late {
		sort.SliceStable(batches, func(i, j int) bool { return batches[i].b.Time < batches[j].b.Time })
		byWin := map[int64][]lateBatch{}
		for _, lb := range batches {
			ws := windowStart(lb.b.Time)
			byWin[ws] = append(byWin[ws], lb)
		}
		var maxSeq uint64
		for ws, lbs := range byWin {
			h := newHead(node, ws)
			for i := range lbs {
				h.append(&lbs[i].b, lbs[i].ids, 0)
				maxSeq = max(maxSeq, lbs[i].seq)
			}
			if err := db.writeSupp(h); err != nil {
				errs = append(errs, err)
				continue
			}
			db.mu.RLock()
			cur := db.heads[node]
			db.mu.RUnlock()
			if cur != nil && cur.start == ws {
				continue // the head flush includes supplementary blocks
			}
			if db.opts.NoRollups {
				continue
			}
			if err := db.rewritePartials(node, ws); err != nil {
				errs = append(errs, err)
			}
		}
		db.mu.Lock()
		w := db.watermark(node)
		w.LateSeq = max(w.LateSeq, maxSeq)
		db.mu.Unlock()
	}
	if err := db.saveWatermarks(); err != nil {
		errs = append(errs, err)
	}
	if db.wal != nil {
		db.wal.prune(db.flushedSeqs)
	}
	return errors.Join(errs...)
}

func (db *DB) writeSupp(h *head) error {
	snap := h.snapshot(nil)
	groups, err := snap.decodeGroups()
	if err != nil {
		return err
	}
	var gen uint32
	for _, r := range db.cat.blocks(TierRaw, h.node, h.start, h.end) {
		if r.Kind == bkSupp && r.Start == h.start {
			gen = max(gen, r.Gen+1)
		}
	}
	bw, err := newBlockWriter(blockPath(db.dir, TierRaw, bkSupp, h.node, h.start, gen),
		blockHeader{Tier: TierRaw, Kind: bkSupp, Node: h.node, Start: h.start, End: h.end, Gen: gen}, nil)
	if err != nil {
		return err
	}
	for _, g := range sortedGroups(groups) {
		if err := bw.writeGroup(g, groups[g]); err != nil {
			bw.abort()
			return err
		}
	}
	bw.resets = snap.resets
	ids := make([]uint32, 0, len(snap.series))
	for id := range snap.series {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		ss := snap.series[id]
		if err := bw.writeSeries(id, ss.group, ss.kind, &ss.pres, [][]byte{ss.vals}, nil); err != nil {
			bw.abort()
			return err
		}
	}
	if err := bw.finish(); err != nil {
		return err
	}
	db.addBlock(bw)
	return nil
}

// rewritePartials recomputes the partial rollups of a past window from its
// raw blocks and writes them as a new generation. A new partial overrides
// any compacted block for its window until the compactor folds it in.
func (db *DB) rewritePartials(node uint32, ws int64) error {
	var brs []*blockReader
	var main *blockReader
	var mainGen uint32
	for _, r := range db.cat.blocks(TierRaw, node, ws, ws+WindowNS) {
		if r.Start != ws {
			continue
		}
		br, err := openBlock(r.Path)
		if err != nil {
			continue
		}
		if r.Kind == bkMain {
			if main != nil && r.Gen < mainGen {
				br.close()
				continue
			}
			if main != nil {
				main.close()
			}
			main, mainGen = br, r.Gen
			continue
		}
		brs = append(brs, br)
	}
	if main != nil {
		brs = append([]*blockReader{main}, brs...)
	}
	defer closeAll(brs)
	if len(brs) == 0 {
		return nil
	}
	tsCache := map[*blockReader]map[model.Group][]int64{}
	allTS := map[model.Group][]int64{}
	idset := map[uint32]bool{}
	var resets []int64
	for _, br := range brs {
		tsCache[br] = map[model.Group][]int64{}
		for g := range br.groups {
			ts, err := br.timestamps(g)
			if err != nil {
				continue
			}
			tsCache[br][g] = ts
			allTS[g] = unionTS(allTS[g], ts)
		}
		for id := range br.series {
			idset[id] = true
		}
		resets = append(resets, br.resets...)
	}
	sort.Slice(resets, func(i, j int) bool { return resets[i] < resets[j] })
	ids := make([]uint32, 0, len(idset))
	for id := range idset {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	gen := db.nextPartialGen(node, ws)
	if gen == 0 {
		gen = 1
	}
	var rw [nTiers]*blockWriter
	bucketTS := [nTiers]map[model.Group][]int64{}
	for t := Tier1m; t <= Tier1h; t++ {
		var err error
		rw[t], err = newBlockWriter(blockPath(db.dir, t, bkPartial, node, ws, gen),
			blockHeader{Tier: t, Kind: bkPartial, Node: node, Start: ws, End: ws + WindowNS, Gen: gen}, db.lim)
		if err != nil {
			for k := Tier1m; k < t; k++ {
				rw[k].abort()
			}
			return err
		}
		bucketTS[t] = map[model.Group][]int64{}
		for _, g := range sortedGroups(allTS) {
			b := bucketize(allTS[g], t.Res())
			bucketTS[t][g] = b
			rw[t].writeGroup(g, b)
		}
		rw[t].setResets(resets)
	}
	starts := db.startStates(node, ws, main)
	for _, id := range ids {
		var parts []*colSeries
		for _, br := range brs {
			if s, err := br.readSeries(id, tsCache[br]); err == nil && s != nil {
				parts = append(parts, s)
			}
		}
		merged := mergeRaw(parts)
		if merged == nil {
			continue
		}
		st := starts[id]
		ru := rollupAll(merged, &st, resets)
		for t := Tier1m; t <= Tier1h; t++ {
			rw[t].writeColSeries(ru[t], bucketTS[t][merged.Group])
		}
	}
	for t := Tier1m; t <= Tier1h; t++ {
		if err := rw[t].finish(); err != nil {
			return err
		}
		db.addBlock(rw[t])
	}
	return nil
}

// startStates returns the counter adjustment state at the start of window
// ws: from the main block when it has one, else by replaying the newest
// earlier main block.
func (db *DB) startStates(node uint32, ws int64, main *blockReader) map[uint32]ctrState {
	out := map[uint32]ctrState{}
	if main != nil {
		cache := map[model.Group][]int64{}
		for id := range main.series {
			s, err := main.readSeries(id, cache)
			if err == nil && s != nil && s.Start != nil {
				out[id] = *s.Start
			}
		}
		return out
	}
	var prev *blockRef
	for _, r := range db.cat.blocks(TierRaw, node, ws-30*24*int64(time.Hour), ws) {
		if r.Kind == bkMain && r.Start < ws {
			rr := r
			prev = &rr
		}
	}
	if prev == nil {
		return out
	}
	br, err := openBlock(prev.Path)
	if err != nil {
		return out
	}
	defer br.close()
	cache := map[model.Group][]int64{}
	for id := range br.series {
		s, err := br.readSeries(id, cache)
		if err != nil || s == nil || !s.Kind.IsCumulative() {
			continue
		}
		st := ctrState{}
		if s.Start != nil {
			st = *s.Start
		}
		_, _, end := adjust(s, st, br.resets)
		out[id] = end
	}
	return out
}

// Pause stops writing new data (the disk floor in pause mode).
func (db *DB) Pause(now int64) {
	if !db.paused.Swap(true) {
		db.pausedFrom = now
	}
}

// Resume restarts writes after a pause and records the gap.
func (db *DB) Resume(now int64) {
	if db.paused.Swap(false) {
		db.AppendEvent(model.Event{Time: now, Kind: model.EvMonitorGap, Severity: model.SevWarn,
			Msg: "writes paused at the disk floor", Attrs: map[string]any{"from": db.pausedFrom, "to": now}})
	}
}

// IsPaused reports whether writes are paused.
func (db *DB) IsPaused() bool { return db.paused.Load() }

// SeriesCount returns the number of series of a node.
func (db *DB) SeriesCount(node uint32) int { return db.ix.seriesCount(node) }

// Dropped returns cap drops by metric name.
func (db *DB) Dropped(node uint32) map[string]int64 { return db.ix.dropped(node) }

// Sizes returns bytes stored per tier.
func (db *DB) Sizes() map[string]int64 {
	s := db.cat.size()
	out := map[string]int64{}
	for t := Tier(0); t < nTiers; t++ {
		out[t.String()] = s[t]
	}
	return out
}

// TotalSize returns the size of the whole store directory.
func (db *DB) TotalSize() int64 { return fsx.DirSize(db.dir) }

// HeadBytes returns the in-memory size of every head.
func (db *DB) HeadBytes() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var n int64
	for _, h := range db.heads {
		h.mu.Lock()
		for _, ts := range h.groups {
			n += int64(len(ts.Bytes()))
		}
		for _, s := range h.series {
			n += int64(len(s.vals.Bytes())) + 64
		}
		h.mu.Unlock()
	}
	return n
}

// Newest returns the newest sample time stored for a node.
func (db *DB) Newest(node uint32) int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if w := db.wm[node]; w != nil {
		return w.Newest
	}
	return 0
}

// Retention returns the configured window per tier.
func (db *DB) Retention() [nTiers]time.Duration { return db.opts.Retention }

// SetRetention changes the windows. The next maintenance pass deletes what
// falls outside them.
func (db *DB) SetRetention(r [nTiers]time.Duration) { db.opts.Retention = r }

var _ = enc.ErrShort

// ErrInUse is returned when another process (a running daemon) has the
// store open.
var ErrInUse = errors.New("store: in use by another algodtop process")
