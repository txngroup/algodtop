package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/query"
	"algod.top/internal/store"
)

func TestClockJump(t *testing.T) {
	var c clockWatch
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).UnixNano()
	c.observe(base, 0)
	if _, ok := c.observe(base+int64(10*time.Second), 10*time.Second); ok {
		t.Fatal("steady clock reported a jump")
	}
	// The wall clock moves back an hour while 10 s pass.
	jump, ok := c.observe(base+int64(20*time.Second)-int64(time.Hour), 20*time.Second)
	if !ok || jump > -59*time.Minute {
		t.Fatalf("missed a backward jump: %v %v", jump, ok)
	}
}

func TestRisingFloor(t *testing.T) {
	nan := math.NaN()
	mb := func(v ...float64) []float64 {
		for i := range v {
			v[i] *= 1 << 20
		}
		return v
	}
	cases := []struct {
		name  string
		mins  []float64
		first int
		want  bool
	}{
		{"steady leak", mb(900, 950, 1000, 1050, 1100, 1150, 1200, 1250, 1300, 1350), 0, true},
		{"flat with noise", mb(1300, 1310, 1290, 1305, 1295, 1300, 1302, 1304, 1306, 1308), 0, false},
		// Host memory pressure drops the minimum for one day, and the
		// next days climb back below where they were before.
		{"recovery from a low day", mb(1330, 1315, 1245, 1180, 1250, 1300, 380, 1230, 1260, 1290), 0, false},
		{"recovery that passes the old floor", mb(1330, 1315, 1245, 1180, 1250, 1300, 380, 1230, 1260, 1460), 0, false},
		// algod restarted on day 5: earlier days belong to the old process.
		{"leak after a restart", mb(1500, 1550, 1600, 1650, 1700, 300, 400, 500, 600, 700), 6, true},
		{"too few days since the restart", mb(1500, 1550, 1600, 1650, 1700, 1750, 300, 400, 500, 600), 8, false},
		{"missing day", mb(100, 200, 300, 400, 500, 600, 700, nan, 900, 1000), 0, false},
		{"gap before the window", mb(nan, nan, nan, nan, nan, nan, 700, 800, 900, 1000), 0, true},
	}
	for _, c := range cases {
		if got := risingFloor(c.mins, c.first); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFirstWholeDay(t *testing.T) {
	from := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		start time.Time
		want  int
	}{
		{time.Time{}, 0},
		{from.Add(-72 * time.Hour), 0},
		{from.Add(-time.Second), 0},
		{from, 1},
		{from.Add(5*24*time.Hour + 3*time.Hour), 6},
	} {
		var ns int64
		if !c.start.IsZero() {
			ns = c.start.UnixNano()
		}
		if got := firstWholeDay(from, ns); got != c.want {
			t.Errorf("start %v: got %d, want %d", c.start, got, c.want)
		}
	}
}

// floorRising reads whole UTC days from the store. Each day's minimum
// sits in its last sample, 10 minutes before midnight, so a bucket that
// crossed midnight would move it to the next day.
func TestFloorRisingFromStore(t *testing.T) {
	// Ten days of windows take about 15 s of fsyncs.
	t.Parallel()
	today := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	from := today.Add(-creepDays * 24 * time.Hour)
	now := from
	db, err := store.Open(store.Options{Dir: t.TempDir(), Now: func() time.Time { return now }, Retention: store.DefaultRetention})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const mb = 1 << 20
	nodes := map[string][]float64{
		"leak": {900, 950, 1000, 1050, 1100, 1150, 1200, 1250, 1300, 1350},
		// One low day under memory pressure, then the climb back.
		"dip": {1330, 1315, 1245, 1180, 1250, 1300, 380, 1230, 1260, 1290},
	}
	for ; now.Before(today); now = now.Add(10 * time.Minute) {
		day := int(now.Sub(from) / (24 * time.Hour))
		last := now.Add(10*time.Minute).Sub(from)%(24*time.Hour) == 0
		for name, mins := range nodes {
			v := (mins[day] + 400) * mb
			if last {
				v = mins[day] * mb
			}
			db.Append(&model.Batch{Node: name, Group: model.GroupMetrics, Time: now.UnixNano(),
				Samples: []model.Sample{{Name: "proc_resident_bytes", Kind: model.KindGauge, Value: v}}})
		}
	}
	db.Tick()
	qe := query.NewEngine(db, 2, 10*time.Second)
	ctx := context.Background()
	if !floorRising(ctx, qe, "leak", 0, today) {
		t.Error("missed a steady leak")
	}
	if floorRising(ctx, qe, "dip", 0, today) {
		t.Error("flagged the climb back from a low day")
	}
	// A restart 2 days ago leaves too few whole days to judge.
	if floorRising(ctx, qe, "leak", today.Add(-36*time.Hour).UnixNano(), today) {
		t.Error("judged a process that started 2 days ago")
	}
}
