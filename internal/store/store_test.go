package store

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"algod.top/internal/model"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time      { return c.t }
func (c *fakeClock) Add(d time.Duration) { c.t = c.t.Add(d) }
func newClock(s string) *fakeClock       { t, _ := time.Parse(time.RFC3339, s); return &fakeClock{t} }
func openTest(t *testing.T, dir string, c *fakeClock) *DB {
	t.Helper()
	db, err := Open(Options{Dir: dir, Now: c.Now, Retention: DefaultRetention, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// gen produces a gauge and a counter sample every 10 s.
func batchAt(node string, t time.Time, i int) *model.Batch {
	return &model.Batch{Node: node, Group: model.GroupMetrics, Time: t.UnixNano(), Samples: []model.Sample{
		{Name: "g", Kind: model.KindGauge, Value: float64(i % 7)},
		{Name: "c", Kind: model.KindCounter, Value: float64(i * 100)},
		{Name: "lab", Labels: model.Labels{{Name: "x", Value: "1"}}, Kind: model.KindGauge, Value: float64(i)},
	}}
}

func idOf(t *testing.T, db *DB, node uint32, name string) uint32 {
	t.Helper()
	s := db.Find(node, name)
	if len(s) != 1 {
		t.Fatalf("find %s: %d series", name, len(s))
	}
	return s[0].ID
}

func TestWriteReadEveryTier(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	start := c.t
	n := 6 * 360 // 6 hours of 10 s samples
	for i := 0; i < n; i++ {
		if err := db.Append(batchAt("mainnet", c.t, i)); err != nil {
			t.Fatal(err)
		}
		c.Add(10 * time.Second)
		if i%10 == 0 {
			db.Tick()
		}
	}
	node, _ := db.NodeID("mainnet")
	gid, cid := idOf(t, db, node, "g"), idOf(t, db, node, "c")
	pts, _, err := db.Read(node, []uint32{gid, cid}, TierRaw, start.UnixNano(), c.t.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("raw series %d", len(pts))
	}
	for _, p := range pts {
		if len(p.T) != n {
			t.Fatalf("%s raw points %d want %d", p.Meta.Name, len(p.T), n)
		}
		for i := range p.T {
			want := float64(i % 7)
			if p.Meta.Name == "c" {
				want = float64(i * 100)
			}
			if p.Cols[0][i] != want {
				t.Fatalf("%s[%d] = %v want %v", p.Meta.Name, i, p.Cols[0][i], want)
			}
		}
	}
	// Two full windows are flushed, so rollups cover the first 4 hours.
	for _, tier := range []Tier{Tier1m, Tier10m, Tier1h} {
		pts, _, err := db.Read(node, []uint32{gid, cid}, tier, start.UnixNano(), start.Add(4*time.Hour).UnixNano())
		if err != nil {
			t.Fatal(err)
		}
		want := int(4 * time.Hour / time.Duration(tier.Res()))
		for _, p := range pts {
			if len(p.T) != want {
				t.Fatalf("%s %s buckets %d want %d", tier, p.Meta.Name, len(p.T), want)
			}
			if p.Meta.Name == "g" {
				for i := range p.T {
					if tier == Tier1m {
						break
					}
					if p.Cols[ColMin][i] != 0 || p.Cols[ColMax][i] != 6 {
						t.Fatalf("%s gauge bucket %d min %v max %v", tier, i, p.Cols[ColMin][i], p.Cols[ColMax][i])
					}
				}
			} else {
				for i := range p.T {
					if r := p.Cols[ColCtrMaxRate][i]; i > 0 && r != 10 {
						t.Fatalf("%s counter max rate %v at %d last %v", tier, r, i, p.Cols[ColCtrLast][i-1:i+1])
					}
				}
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCrashRecoveryFromWAL(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	start := c.t
	for i := 0; i < 100; i++ {
		db.Append(batchAt("n1", c.t, i))
		c.Add(10 * time.Second)
	}
	db.Tick()
	c.Add(time.Second)
	db.Tick() // group commit
	// Simulate a crash: drop the DB without Close.
	db.wal.sync()
	db.ix.sync()
	crash(db)
	db2 := openTest(t, dir, c)
	node, _ := db2.NodeID("n1")
	pts, _, err := db2.Read(node, []uint32{idOf(t, db2, node, "c")}, TierRaw, start.UnixNano(), c.t.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || len(pts[0].T) != 100 {
		t.Fatalf("after replay got %v points", len(pts[0].T))
	}
	db2.Close()
}

func TestTornWALRecord(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	for i := 0; i < 10; i++ {
		db.Append(batchAt("n1", c.t, i))
		c.Add(10 * time.Second)
	}
	db.wal.sync()
	db.ix.sync()
	crash(db)
	// Tear the last record.
	segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*.wal"))
	last := segs[len(segs)-1]
	fi, _ := os.Stat(last)
	os.Truncate(last, fi.Size()-5)
	db2 := openTest(t, dir, c)
	node, _ := db2.NodeID("n1")
	pts, _, _ := db2.Read(node, []uint32{idOf(t, db2, node, "g")}, TierRaw, 0, c.t.UnixNano())
	if len(pts[0].T) != 9 {
		t.Fatalf("got %d points after torn record, want 9", len(pts[0].T))
	}
	db2.Close()
}

func TestCleanRestartKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T01:00:00Z")
	db := openTest(t, dir, c)
	start := c.t
	for i := 0; i < 1000; i++ {
		db.Append(batchAt("n1", c.t, i))
		c.Add(10 * time.Second)
		db.Tick()
	}
	db.Close()
	db = openTest(t, dir, c)
	for i := 1000; i < 2000; i++ {
		db.Append(batchAt("n1", c.t, i))
		c.Add(10 * time.Second)
		db.Tick()
	}
	node, _ := db.NodeID("n1")
	pts, _, _ := db.Read(node, []uint32{idOf(t, db, node, "c")}, TierRaw, start.UnixNano(), c.t.UnixNano())
	if len(pts[0].T) != 2000 {
		t.Fatalf("got %d points, want 2000", len(pts[0].T))
	}
	db.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*.wal"))
	if len(segs) > 2 {
		t.Fatalf("%d WAL segments left after clean close", len(segs))
	}
}

func TestLateData(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	start := c.t
	// Write 5 hours, skipping minute 30-40 of the first window.
	var skipped []int
	for i := 0; i < 5*360; i++ {
		if i >= 180 && i < 240 {
			skipped = append(skipped, i)
		} else {
			db.Append(batchAt("n1", start.Add(time.Duration(i)*10*time.Second), i))
		}
	}
	c.t = start.Add(5 * time.Hour)
	for _, i := range skipped {
		db.Append(batchAt("n1", start.Add(time.Duration(i)*10*time.Second), i))
	}
	c.Add(10 * time.Second)
	db.Tick()
	c.Add(31 * time.Second)
	db.Tick()
	node, _ := db.NodeID("n1")
	gid := idOf(t, db, node, "lab")
	pts, _, _ := db.Read(node, []uint32{gid}, TierRaw, start.UnixNano(), start.Add(2*time.Hour).UnixNano())
	if len(pts[0].T) != 720 {
		t.Fatalf("raw after late data: %d points, want 720", len(pts[0].T))
	}
	for i := range pts[0].T {
		if pts[0].Cols[0][i] != float64(i) {
			t.Fatalf("late merge wrong at %d: %v", i, pts[0].Cols[0][i])
		}
	}
	pts, _, _ = db.Read(node, []uint32{gid}, Tier1m, start.UnixNano(), start.Add(2*time.Hour).UnixNano())
	if len(pts[0].T) != 120 {
		t.Fatalf("1m after late data: %d buckets", len(pts[0].T))
	}
	// Bucket 30 holds samples 180..185.
	if got := pts[0].Cols[ColCount][30]; got != 6 {
		t.Fatalf("1m bucket 30 count %v want 6", got)
	}
	// Fold and compact, then check again.
	c.t = start.Add(50 * time.Hour)
	db.Append(batchAt("n1", c.t, 0))
	rep := db.Maintain()
	if rep.Folded == 0 || rep.Compacted == 0 {
		t.Fatalf("maintenance: %+v", rep)
	}
	pts, _, _ = db.Read(node, []uint32{gid}, TierRaw, start.UnixNano(), start.Add(2*time.Hour).UnixNano())
	if len(pts[0].T) != 720 {
		t.Fatalf("raw after fold: %d points", len(pts[0].T))
	}
	pts, _, _ = db.Read(node, []uint32{gid}, Tier1m, start.UnixNano(), start.Add(2*time.Hour).UnixNano())
	if len(pts[0].T) != 120 || pts[0].Cols[ColCount][30] != 6 {
		t.Fatalf("1m after compaction: %d buckets", len(pts[0].T))
	}
	db.Close()
}

func TestCounterResetAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	start := c.t
	v := 0.0
	for i := 0; i < 2*720+10; i++ {
		b := &model.Batch{Node: "n1", Group: model.GroupMetrics, Time: c.t.UnixNano()}
		if i == 700 {
			v = 0 // restart
			b.Reset = true
		}
		v += 50
		b.Samples = []model.Sample{{Name: "bytes", Kind: model.KindCounter, Value: v}}
		db.Append(b)
		c.Add(10 * time.Second)
	}
	node, _ := db.NodeID("n1")
	id := idOf(t, db, node, "bytes")
	pts, resets, _ := db.Read(node, []uint32{id}, Tier1h, start.UnixNano(), start.Add(4*time.Hour).UnixNano())
	if len(resets) != 1 {
		t.Fatalf("resets %v", resets)
	}
	last := pts[0].Cols[ColCtrLast]
	for i := 1; i < len(last); i++ {
		if last[i] < last[i-1] {
			t.Fatalf("adjusted counter went down: %v", last)
		}
	}
	// 3600 s / 10 s = 360 samples of 50 each per hour.
	if d := last[2] - last[1]; d != 360*50 {
		t.Fatalf("hour 2 increase %v want %v", d, 360*50)
	}
	db.Close()
}

func TestRetentionDeletesOnlyOutsideWindow(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-10-01T00:00:00Z")
	db, err := Open(Options{Dir: dir, Now: c.Now, Logf: t.Logf,
		Retention: [nTiers]time.Duration{24 * time.Hour, 48 * time.Hour, Forever, Forever}})
	if err != nil {
		t.Fatal(err)
	}
	start := c.t
	// Three days at one sample a minute.
	for i := 0; i < 3*1440; i++ {
		db.Append(&model.Batch{Node: "n", Group: model.GroupMetrics, Time: c.t.UnixNano(),
			Samples: []model.Sample{{Name: "g", Kind: model.KindGauge, Value: float64(i)}}})
		c.Add(time.Minute)
	}
	db.Tick()
	db.Maintain()
	node, _ := db.NodeID("n")
	id := idOf(t, db, node, "g")
	now := c.t
	pts, _, _ := db.Read(node, []uint32{id}, TierRaw, start.UnixNano(), now.UnixNano())
	if len(pts) == 0 {
		t.Fatal("no raw data left")
	}
	oldest := time.Unix(0, pts[0].T[0])
	if now.Sub(oldest) < 24*time.Hour {
		t.Fatalf("raw kept only %v, window is 24h", now.Sub(oldest))
	}
	if now.Sub(oldest) > 24*time.Hour+2*time.Hour+time.Minute {
		t.Fatalf("raw kept %v, more than window plus one block", now.Sub(oldest))
	}
	pts, _, _ = db.Read(node, []uint32{id}, Tier10m, start.UnixNano(), now.UnixNano())
	if !time.Unix(0, pts[0].T[0]).Equal(start) {
		t.Fatalf("forever tier lost its start: %v", time.Unix(0, pts[0].T[0]))
	}
	evs := db.Events(EventFilter{From: 0, To: math.MaxInt64, Kinds: []string{model.EvRetention}})
	if len(evs) == 0 {
		t.Fatal("no retention event recorded")
	}
	db.Close()
}

func TestGenesisChangeGetsNewNode(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	a, _ := db.RegisterNode("x", "mainnet-v1.0", "hashA")
	a2, _ := db.RegisterNode("x", "mainnet-v1.0", "hashA")
	b, _ := db.RegisterNode("x", "testnet-v1.0", "hashB")
	if a != a2 || a == b {
		t.Fatalf("ids a=%d a2=%d b=%d", a, a2, b)
	}
	if id, _ := db.NodeID("x"); id != b {
		t.Fatalf("current id %d want %d", id, b)
	}
	db.Close()
}

func TestCaps(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db, _ := Open(Options{Dir: dir, Now: c.Now, Caps: Caps{PerName: 3, PerNode: 10}})
	b := &model.Batch{Node: "n", Group: model.GroupMetrics, Time: c.t.UnixNano()}
	for i := 0; i < 5; i++ {
		b.Samples = append(b.Samples, model.Sample{Name: "m", Labels: model.Labels{{Name: "i", Value: string(rune('a' + i))}}, Kind: model.KindGauge, Value: 1})
	}
	db.Append(b)
	node, _ := db.NodeID("n")
	if got := len(db.Find(node, "m")); got != 3 {
		t.Fatalf("series %d want 3", got)
	}
	if db.Dropped(node)["m"] != 2 {
		t.Fatalf("drops %v", db.Dropped(node))
	}
	db.Close()
}

func TestPeerUsageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	db.RegisterNode("n", "", "")
	for i := 0; i < 500; i++ {
		ts := c.t.UnixNano()
		db.AppendUsage("n", []model.PeerUsage{{Time: ts, Key: "peerA", Network: "p2p", Direction: "in", Addr: "1.2.3.4", BytesIn: int64(i * 1000), BytesOut: int64(i * 10), Streams: []string{"/algorand-ws/2.2.0"}}})
		db.Append(batchAt("n", c.t, i))
		c.Add(30 * time.Second)
	}
	got := db.PeerUsage("n", "peerA", 0, c.t.UnixNano())
	if len(got) != 500 {
		t.Fatalf("usage samples %d", len(got))
	}
	if got[499].BytesIn != 499000 || len(got[10].Streams) != 1 {
		t.Fatalf("bad sample %+v", got[499])
	}
	db.Close()
	db = openTest(t, dir, c)
	if got := db.PeerUsage("n", "", 0, c.t.UnixNano()); len(got) != 500 {
		t.Fatalf("after reopen usage samples %d", len(got))
	}
	db.Close()
}

func TestBlockCorruptionDetected(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	for i := 0; i < 800; i++ {
		db.Append(batchAt("n", c.t, i))
		c.Add(10 * time.Second)
	}
	db.Tick()
	if bad := db.Scrub(); len(bad) != 0 {
		t.Fatalf("clean store reports %v", bad)
	}
	blocks, _ := filepath.Glob(filepath.Join(dir, "chunks", "raw", "*", "*.blk"))
	b, _ := os.ReadFile(blocks[0])
	b[100] ^= 0xff
	os.WriteFile(blocks[0], b, 0o600)
	if bad := db.Scrub(); len(bad) != 1 {
		t.Fatalf("scrub found %v", bad)
	}
	db.Close()
}

// A restart inside a window reloads the window's block into a head, so
// new samples do not go through the late path, and counter rollups stay
// continuous across the restart.
func TestRestartMidWindowReloadsHead(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db := openTest(t, dir, c)
	start := c.t
	for i := 0; i < 300; i++ { // 50 minutes
		db.Append(batchAt("n", c.t, i))
		c.Add(10 * time.Second)
	}
	db.Close()
	db = openTest(t, dir, c)
	for i := 300; i < 720+60; i++ { // into the next window
		db.Append(batchAt("n", c.t, i))
		c.Add(10 * time.Second)
		db.Tick()
	}
	supps, _ := filepath.Glob(filepath.Join(dir, "chunks", "raw", "*", "*.s*.blk"))
	if len(supps) != 0 {
		t.Fatalf("restart produced %d supplementary blocks", len(supps))
	}
	if st := db.Stats.Late.Load(); st != 0 {
		t.Fatalf("%d late batches after a restart", st)
	}
	node, _ := db.NodeID("n")
	id := idOf(t, db, node, "c")
	pts, _, _ := db.Read(node, []uint32{id}, TierRaw, start.UnixNano(), start.Add(2*time.Hour).UnixNano())
	if len(pts[0].T) != 720 {
		t.Fatalf("raw points %d want 720", len(pts[0].T))
	}
	pts, _, _ = db.Read(node, []uint32{id}, Tier1m, start.UnixNano(), start.Add(2*time.Hour).UnixNano())
	for i, r := range pts[0].Cols[ColCtrMaxRate] {
		if i > 0 && r != 10 {
			t.Fatalf("1m max rate %v at bucket %d after a restart", r, i)
		}
	}
	last := pts[0].Cols[ColCtrLast]
	for i := 1; i < len(last); i++ {
		if last[i]-last[i-1] != 600 {
			t.Fatalf("1m increase %v at bucket %d", last[i]-last[i-1], i)
		}
	}
	db.Close()
}

func TestSessionAddressesAggregateAfterRetention(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2025-01-15T00:00:00Z")
	db, _ := Open(Options{Dir: dir, Now: c.Now, AddrRetention: 30 * 24 * time.Hour})
	db.AppendSession(model.Session{Node: "n", IP: "203.0.113.77", Addr: "/ip4/203.0.113.77/tcp/4190", PeerID: "12D3KooWX", Start: c.t.UnixNano(), End: c.t.Add(time.Hour).UnixNano(), Reason: "ReadError"})
	db.events.sync()
	db.sessions.sync()
	c.t = c.t.Add(90 * 24 * time.Hour)
	db.Maintain()
	ss := db.Sessions("n", "", 0, c.t.UnixNano())
	if len(ss) != 1 || ss[0].IP != "203.0.113.0/24" || ss[0].Addr != "" || ss[0].PeerID != "" || ss[0].Reason != "ReadError" {
		t.Fatalf("sessions %+v", ss)
	}
	if aggregateIP("2001:db8:1234:5678::1") != "2001:db8:1234::/48" {
		t.Fatal(aggregateIP("2001:db8:1234:5678::1"))
	}
	db.Close()
}

// crash drops a DB the way a killed process does: the OS closes its files
// and releases its lock, and nothing more is written. Windows cannot
// truncate or delete a file another handle still has open.
func crash(db *DB) {
	db.lock.Close()
	db.wal.f.Close()
	db.ix.f.Close()
}

// Peer churn fills the limit with the newest events. The counts still
// cover the whole range and every kind, so a caller can see the restart
// the limit left out and ask for it by kind.
func TestEventsCountedSeesPastTheLimit(t *testing.T) {
	c := newClock("2026-11-04T00:00:00Z")
	db := openTest(t, t.TempDir(), c)
	defer db.Close()
	base := c.Now()
	at := func(d time.Duration) int64 { return base.Add(d).UnixNano() }
	db.AppendEvent(model.Event{Time: at(time.Minute), Node: "relay", Kind: model.EvRestart, Msg: "algod restarted"})
	for i := range 50 {
		db.AppendEvent(model.Event{Time: at(2*time.Minute + time.Duration(i)*time.Second), Node: "relay", Kind: model.EvDisconnect})
	}
	db.AppendEvent(model.Event{Time: at(3 * time.Minute), Node: "other", Kind: model.EvRestart})
	f := EventFilter{Node: "relay", From: at(0), To: at(time.Hour), Limit: 10}
	evs, counts := db.EventsCounted(f)
	if len(evs) != 10 || evs[9].Kind != model.EvDisconnect {
		t.Fatalf("got %d events, last %+v", len(evs), evs[len(evs)-1])
	}
	if counts[model.EvRestart] != 1 || counts[model.EvDisconnect] != 50 || len(counts) != 2 {
		t.Fatalf("counts %v", counts)
	}
	f.Kinds = []string{model.EvRestart}
	evs, counts = db.EventsCounted(f)
	if len(evs) != 1 || evs[0].Msg != "algod restarted" || counts[model.EvDisconnect] != 50 {
		t.Fatalf("by kind: %+v, counts %v", evs, counts)
	}
}
