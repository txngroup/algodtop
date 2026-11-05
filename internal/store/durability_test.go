package store

import (
	"bufio"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"algod.top/internal/disk"
	"algod.top/internal/model"
)

// TestMain lets the test binary act as a writer that a crash test kills.
func TestMain(m *testing.M) {
	if dir := os.Getenv("ALGODTOP_CRASH_WRITER"); dir != "" {
		crashWriter(dir)
		return
	}
	os.Exit(m.Run())
}

// crashWriter appends samples forever and prints the newest sample time
// after each group commit. The parent kills it with SIGKILL.
func crashWriter(dir string) {
	start, _ := time.Parse(time.RFC3339, "2026-11-03T00:00:00Z")
	if s := os.Getenv("ALGODTOP_CRASH_START"); s != "" {
		n, _ := strconv.ParseInt(s, 10, 64)
		start = time.Unix(0, n)
	}
	now := start
	db, err := Open(Options{Dir: dir, Now: func() time.Time { return now }, GroupCommit: time.Second, Retention: DefaultRetention})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	i := 0
	for {
		db.Append(batchAt("n", now, i))
		i++
		now = now.Add(10 * time.Second)
		if i%5 == 0 {
			// A commit fsyncs everything up to now.
			db.commit()
			fmt.Println("committed", now.Add(-10*time.Second).UnixNano())
		}
		db.Tick()
	}
}

// TestCrashAtRandomPoints kills a writer with SIGKILL at random moments,
// including during head flushes and rollups, and checks that recovery
// keeps every sample up to the last commit, with no gap.
func TestCrashAtRandomPoints(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test")
	}
	dir := t.TempDir()
	self, _ := os.Executable()
	rnd := rand.New(rand.NewSource(7))
	var committed int64
	startAt := ""
	for round := 0; round < 6; round++ {
		cmd := exec.Command(self, "-test.run=XXX")
		cmd.Env = append(os.Environ(), "ALGODTOP_CRASH_WRITER="+dir)
		if startAt != "" {
			cmd.Env = append(cmd.Env, "ALGODTOP_CRASH_START="+startAt)
		}
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(out)
		deadline := time.Now().Add(time.Duration(200+rnd.Intn(1500)) * time.Millisecond)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 2 && f[0] == "committed" {
				committed, _ = strconv.ParseInt(f[1], 10, 64)
			} else if len(f) > 0 && f[0] == "error" {
				t.Fatalf("writer: %s", sc.Text())
			}
			if time.Now().After(deadline) {
				break
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
		// Recover and check everything up to the last commit is there.
		db, err := Open(Options{Dir: dir, Now: func() time.Time { return time.Unix(0, committed) }, Retention: DefaultRetention})
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		node, _ := db.NodeID("n")
		ms := db.Find(node, "c")
		if len(ms) != 1 {
			t.Fatalf("round %d: series %v", round, ms)
		}
		pts, _, err := db.Read(node, []uint32{ms[0].ID}, TierRaw, 0, committed+1)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if len(pts) > 0 {
			n = len(pts[0].T)
			for i := 1; i < n; i++ {
				if pts[0].T[i]-pts[0].T[i-1] != int64(10*time.Second) {
					t.Fatalf("round %d: gap at %d: %v", round, i, time.Duration(pts[0].T[i]-pts[0].T[i-1]))
				}
			}
		}
		first, _ := time.Parse(time.RFC3339, "2026-11-03T00:00:00Z")
		want := int((committed-first.UnixNano())/int64(10*time.Second)) + 1
		if n < want {
			t.Fatalf("round %d: %d samples after recovery, want at least %d (committed through %v)", round, n, want, time.Unix(0, committed).UTC())
		}
		var last int64
		if n > 0 {
			last = pts[0].T[n-1]
		}
		db.Close()
		// The next writer continues after the newest recovered sample.
		startAt = strconv.FormatInt(last+int64(10*time.Second), 10)
		committed = last
	}
}

// TestRetentionLongHistory runs 40 simulated days and checks after every
// maintenance pass that each tier still holds every sample younger than
// its window.
func TestRetentionLongHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	dir := t.TempDir()
	c := newClock("2026-09-01T00:00:00Z")
	start := c.t
	ret := [nTiers]time.Duration{3 * 24 * time.Hour, 10 * 24 * time.Hour, 25 * 24 * time.Hour, Forever}
	db, err := Open(Options{Dir: dir, Now: c.Now, Retention: ret})
	if err != nil {
		t.Fatal(err)
	}
	node := uint32(0)
	for day := 0; day < 40; day++ {
		for m := 0; m < 1440; m++ {
			db.Append(&model.Batch{Node: "n", Group: model.GroupMetrics, Time: c.t.UnixNano(),
				Samples: []model.Sample{{Name: "g", Kind: model.KindGauge, Value: float64(m)}}})
			c.Add(time.Minute)
		}
		db.Tick()
		db.Maintain()
		node, _ = db.NodeID("n")
		id := db.Find(node, "g")[0].ID
		now := c.t
		for t2 := Tier(0); t2 < nTiers; t2++ {
			from := start
			if ret[t2] != Forever && now.Sub(start) > ret[t2] {
				from = now.Add(-ret[t2])
			}
			pts, _, _ := db.Read(node, []uint32{id}, t2, from.UnixNano(), now.UnixNano())
			if len(pts) == 0 {
				t.Fatalf("day %d tier %s: no data", day, t2)
			}
			step := int64(time.Minute)
			if t2 != TierRaw {
				step = t2.Res()
			}
			got := pts[0].T
			// The window's oldest sample must still be present; blocks of
			// the current head window are not in rollup tiers yet.
			if got[0] > from.UnixNano()+step {
				t.Fatalf("day %d tier %s: oldest %v, window starts %v", day, t2, time.Unix(0, got[0]).UTC(), from.UTC())
			}
			for i := 1; i < len(got); i++ {
				if got[i]-got[i-1] != step {
					t.Fatalf("day %d tier %s: hole at %v", day, t2, time.Unix(0, got[i-1]).UTC())
				}
			}
		}
	}
	db.Close()
}

// TestDiskFloorOrder fills a fake volume and checks the deletion order:
// raw first down to 2 days, then 1m down to 30 days, and so on, with
// events naming what was removed. Pause mode deletes nothing.
func TestDiskFloorOrder(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-09-01T00:00:00Z")
	const total = 100 << 20
	var base int64 = 50 << 20
	var gain int64 = 0 // bytes of free space per byte deleted
	var size0 int64
	var db *DB
	usage := func(string) (disk.Usage, error) {
		free := base
		if db != nil && gain > 0 {
			var s int64
			for _, v := range db.cat.size() {
				s += v
			}
			free += (size0 - s) * gain
		}
		return disk.Usage{Free: uint64(free), Total: total}, nil
	}
	var err error
	db, err = Open(Options{Dir: dir, Now: c.Now, Retention: [nTiers]time.Duration{}, DiskUsage: usage})
	if err != nil {
		t.Fatal(err)
	}
	for m := 0; m < 6*1440; m++ { // six days, one sample a minute
		db.Append(batchAt("n", c.t, m))
		c.Add(time.Minute)
	}
	db.Tick()
	db.Maintain() // compact first, so only the floor changes sizes below
	node, _ := db.NodeID("n")
	raw := db.cat.all(TierRaw, node)
	for _, v := range db.cat.size() {
		size0 += v
	}
	// Phase 1: below the 5% floor; deleting about ten raw blocks frees
	// enough. The oldest raw blocks go first and nothing else is touched.
	base = total*5/100 - 1
	gain = int64(total/100+2) / (10 * raw[0].Size)
	rep := db.Maintain()
	if !rep.FloorHit || len(rep.Deleted) == 0 {
		t.Fatalf("floor pass: %+v", rep)
	}
	for i, d := range rep.Deleted {
		if d.Tier != "raw" || d.From != raw[i].Start {
			t.Fatalf("deletion %d is %s from %v, want the oldest raw block", i, d.Tier, time.Unix(0, d.From).UTC())
		}
	}
	if n := len(rep.Deleted); n < 5 || n > 20 {
		t.Fatalf("%d blocks deleted, want about ten", n)
	}
	if db.IsPaused() {
		t.Fatal("paused although space was freed")
	}
	evs := db.Events(EventFilter{From: 0, To: math.MaxInt64, Kinds: []string{model.EvRetention}})
	if len(evs) == 0 || !strings.Contains(evs[0].Msg, "disk floor removed raw") {
		t.Fatalf("events %+v", evs)
	}
	// Phase 2: deleting never helps. Raw goes down to 2 days before any
	// other tier is touched; with nothing left, writes pause.
	gain = 0
	base = 1 << 20
	rep = db.Maintain()
	lastRaw := -1
	firstOther := -1
	for i, d := range rep.Deleted {
		if d.Tier == "raw" && firstOther < 0 {
			lastRaw = i
			if c.t.Sub(time.Unix(0, d.To)) < 2*24*time.Hour {
				t.Fatalf("raw deleted inside the 2-day minimum before other stages: %v", time.Unix(0, d.To).UTC())
			}
		} else if firstOther < 0 && d.Tier != "raw" {
			firstOther = i
		}
	}
	if lastRaw < 0 || firstOther < 0 {
		t.Fatalf("stages: last raw %d, first other %d", lastRaw, firstOther)
	}
	if !db.IsPaused() {
		t.Fatal("nothing left to delete, but writes are not paused")
	}
	base = 50 << 20
	db.Maintain()
	if db.IsPaused() {
		t.Fatal("writes stay paused after space returned")
	}
	if evs := db.Events(EventFilter{From: 0, To: math.MaxInt64, Kinds: []string{model.EvMonitorGap}}); len(evs) == 0 {
		t.Fatal("the pause was not recorded as a monitor gap")
	}
	db.Close()

	// Pause mode never deletes inside a window.
	dir2 := t.TempDir()
	free2 := uint64(50 << 20)
	db2, _ := Open(Options{Dir: dir2, Now: c.Now, AtFloor: "pause", DiskUsage: func(string) (disk.Usage, error) { return disk.Usage{Free: free2, Total: total}, nil }})
	for m := 0; m < 1440; m++ {
		db2.Append(batchAt("n", c.t, m))
		c.Add(time.Minute)
	}
	db2.Tick()
	before := len(db2.cat.all(TierRaw, 1))
	free2 = 1 << 20
	db2.Maintain()
	if !db2.IsPaused() || len(db2.cat.all(TierRaw, 1)) != before {
		t.Fatalf("pause mode: paused=%v blocks %d -> %d", db2.IsPaused(), before, len(db2.cat.all(TierRaw, 1)))
	}
	db2.Close()
}

// TestLateDataMatchesInOrder replays an agent outage and a clock jump and
// checks that queries and rollups match an in-order run.
func TestLateDataMatchesInOrder(t *testing.T) {
	mk := func(order []int) *DB {
		c := newClock("2026-11-03T00:00:00Z")
		db, _ := Open(Options{Dir: t.TempDir(), Now: c.Now})
		start := c.t
		for _, i := range order {
			db.Append(batchAt("n", start.Add(time.Duration(i)*10*time.Second), i))
		}
		c.t = start.Add(6 * time.Hour)
		db.Tick()
		c.Add(40 * time.Second)
		db.Tick()
		db.Maintain()
		return db
	}
	var inOrder, late []int
	for i := 0; i < 3*720; i++ {
		inOrder = append(inOrder, i)
	}
	// Hours 1 and 2 arrive after hour 3: an outage, then a backward jump.
	for i := 0; i < 3*720; i++ {
		if i < 360 || i >= 1080 {
			late = append(late, i)
		}
	}
	for i := 360; i < 1080; i++ {
		late = append(late, i)
	}
	a, b := mk(inOrder), mk(late)
	defer a.Close()
	defer b.Close()
	na, _ := a.NodeID("n")
	nb, _ := b.NodeID("n")
	for _, name := range []string{"g", "c", "lab"} {
		ia, ib := a.Find(na, name)[0].ID, b.Find(nb, name)[0].ID
		for tier := Tier(0); tier < nTiers; tier++ {
			from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC).UnixNano()
			pa, _, _ := a.Read(na, []uint32{ia}, tier, from, from+int64(6*time.Hour))
			pb, _, _ := b.Read(nb, []uint32{ib}, tier, from, from+int64(6*time.Hour))
			if len(pa) != 1 || len(pb) != 1 || len(pa[0].T) != len(pb[0].T) {
				t.Fatalf("%s %s: lengths differ", name, tier)
			}
			for i := range pa[0].T {
				for col := range pa[0].Cols {
					x, y := pa[0].Cols[col][i], pb[0].Cols[col][i]
					if x != y && !(math.IsNaN(x) && math.IsNaN(y)) {
						t.Fatalf("%s %s bucket %d col %d: in order %v, late %v", name, tier, i, col, x, y)
					}
				}
			}
		}
	}
}

func FuzzOpenBlock(f *testing.F) {
	dir := f.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	db, _ := Open(Options{Dir: dir, Now: c.Now})
	for i := 0; i < 50; i++ {
		db.Append(batchAt("n", c.t, i))
		c.Add(10 * time.Second)
	}
	db.Close()
	blocks, _ := filepath.Glob(filepath.Join(dir, "chunks", "raw", "*", "*.blk"))
	seed, _ := os.ReadFile(blocks[0])
	f.Add(seed)
	f.Fuzz(func(t *testing.T, b []byte) {
		p := filepath.Join(t.TempDir(), "n1-0.g0.blk")
		os.WriteFile(p, b, 0o600)
		br, err := openBlock(p)
		if err != nil {
			return
		}
		defer br.close()
		cache := map[model.Group][]int64{}
		for id := range br.series {
			br.readSeries(id, cache)
		}
		br.verify()
	})
}

func FuzzDecodeWAL(f *testing.F) {
	w := &wal{}
	_ = w
	f.Add([]byte{walBatch, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		decodeWAL(b)
	})
}
