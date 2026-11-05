package query

import (
	"context"
	"math"
	"testing"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/store"
)

func fill(t *testing.T, hours int) (*store.DB, time.Time, time.Time) {
	t.Helper()
	start, _ := time.Parse(time.RFC3339, "2026-11-01T00:00:00Z")
	now := start
	db, err := store.Open(store.Options{Dir: t.TempDir(), Now: func() time.Time { return now }, Retention: store.DefaultRetention})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	les := []string{"0.1", "0.5", "1", "+Inf"}
	for i := 0; i < hours*360; i++ {
		b := &model.Batch{Node: "n", Group: model.GroupMetrics, Time: now.UnixNano()}
		b.Samples = append(b.Samples,
			model.Sample{Name: "g", Kind: model.KindGauge, Value: float64(i % 10)},
			model.Sample{Name: "bytes_total", Labels: model.Labels{{Name: "dir", Value: "in"}}, Kind: model.KindCounter, Value: float64(i) * 1000},
			model.Sample{Name: "bytes_total", Labels: model.Labels{{Name: "dir", Value: "out"}}, Kind: model.KindCounter, Value: float64(i) * 500},
		)
		// Every sample lands in the 0.5 bucket: p50 must be in (0.1, 0.5].
		for j, le := range les {
			v := float64(i)
			if j == 0 {
				v = 0
			}
			b.Samples = append(b.Samples, model.Sample{Name: "lat_bucket", Labels: model.Labels{{Name: "le", Value: le}}, Kind: model.KindHistPart, Value: v})
		}
		db.Append(b)
		now = now.Add(10 * time.Second)
	}
	db.Tick()
	return db, start, now
}

func TestRateRawAndRollup(t *testing.T) {
	db, start, end := fill(t, 30)
	e := NewEngine(db, 2, 5*time.Second)
	// One hour: raw tier.
	r, err := e.Run(context.Background(), Request{Node: "n", Metric: "bytes_total", Match: []Matcher{{Name: "dir", Value: "in"}},
		From: start.Add(2 * time.Hour).UnixNano(), To: start.Add(3 * time.Hour).UnixNano(), Points: 360})
	if err != nil {
		t.Fatal(err)
	}
	if r.Fn != "rate" || len(r.Parts) != 1 || r.Parts[0].Tier != "raw" || len(r.Series) != 1 {
		t.Fatalf("raw query: %+v", r)
	}
	for i, v := range r.Series[0].Avg {
		if math.Abs(v-100) > 1e-9 {
			t.Fatalf("raw rate[%d] = %v want 100", i, v)
		}
	}
	// 24 hours at 100 points: 10m tier for flushed windows plus raw for the
	// head.
	r, err = e.Run(context.Background(), Request{Node: "n", Metric: "bytes_total", Match: []Matcher{{Name: "dir", Value: "in"}},
		From: end.Add(-24 * time.Hour).UnixNano(), To: end.UnixNano(), Points: 100})
	if err != nil {
		t.Fatal(err)
	}
	if r.Parts[0].Tier != "10m" || r.Parts[len(r.Parts)-1].Tier != "raw" {
		t.Fatalf("parts %+v", r.Parts)
	}
	n := 0
	for i, v := range r.Series[0].Avg {
		if math.IsNaN(v) {
			continue
		}
		n++
		if math.Abs(v-100) > 0.5 {
			t.Fatalf("stitched rate[%d] = %v want 100 (parts %+v)", i, v, r.Parts)
		}
	}
	if n < 95 {
		t.Fatalf("only %d of 100 buckets have data", n)
	}
	// Sum by label.
	r, _ = e.Run(context.Background(), Request{Node: "n", Metric: "bytes_total", By: "dir",
		From: start.Add(2 * time.Hour).UnixNano(), To: start.Add(3 * time.Hour).UnixNano(), Points: 10})
	if len(r.Series) != 2 {
		t.Fatalf("by dir: %d series", len(r.Series))
	}
}

func TestGaugeMinMaxAndRawToggle(t *testing.T) {
	db, start, _ := fill(t, 6)
	e := NewEngine(db, 2, 5*time.Second)
	r, err := e.Run(context.Background(), Request{Node: "n", Metric: "g", From: start.UnixNano(), To: start.Add(4 * time.Hour).UnixNano(), Points: 24})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Series[0]
	for i := range s.Avg {
		if s.Min[i] != 0 || s.Max[i] != 9 || math.Abs(s.Avg[i]-4.5) > 1e-9 {
			t.Fatalf("bucket %d min %v max %v avg %v", i, s.Min[i], s.Max[i], s.Avg[i])
		}
	}
	r, _ = e.Run(context.Background(), Request{Node: "n", Metric: "g", From: start.UnixNano(), To: start.Add(4 * time.Hour).UnixNano(), Points: 24, Tier: "raw"})
	if !r.Raw || len(r.Series[0].T) != 4*360 {
		t.Fatalf("full resolution returned %d points", len(r.Series[0].T))
	}
}

func TestHistogramPercentile(t *testing.T) {
	db, start, _ := fill(t, 3)
	e := NewEngine(db, 2, 5*time.Second)
	r, err := e.Run(context.Background(), Request{Node: "n", Metric: "lat_bucket", From: start.Add(time.Hour).UnixNano(), To: start.Add(2 * time.Hour).UnixNano(), Points: 6, Fn: "p50"})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range r.Series[0].Avg {
		if !(v > 0.1 && v <= 0.5) {
			t.Fatalf("p50[%d] = %v", i, v)
		}
	}
	r, _ = e.Run(context.Background(), Request{Node: "n", Metric: "lat_bucket", From: start.Add(time.Hour).UnixNano(), To: start.Add(2 * time.Hour).UnixNano(), Points: 6, Fn: "heatmap"})
	if len(r.Heatmaps) != 1 || r.Heatmaps[0].Counts[0][1] == 0 || r.Heatmaps[0].Counts[0][2] != 0 {
		t.Fatalf("heatmap %+v", r.Heatmaps)
	}
}

func TestQuantile(t *testing.T) {
	les := []float64{1, 2, 4, math.Inf(1)}
	cum := []float64{10, 20, 30, 30}
	if q := Quantile(0.5, les, cum); math.Abs(q-1.5) > 1e-9 {
		t.Fatalf("q50 %v", q)
	}
}

func TestAlias(t *testing.T) {
	db, start, _ := fill(t, 1)
	Aliases["g"] = "gauge_new_name"
	defer delete(Aliases, "g")
	e := NewEngine(db, 2, 5*time.Second)
	r, _ := e.Run(context.Background(), Request{Node: "n", Metric: "gauge_new_name", From: start.UnixNano(), To: start.Add(time.Hour).UnixNano(), Points: 6})
	if len(r.Series) != 1 {
		t.Fatal("alias did not resolve")
	}
}

// An increase over a range that does not start on a bucket boundary must
// match the raw total, whichever tier answers.
func TestIncreaseMatchesAcrossTiers(t *testing.T) {
	db, start, end := fill(t, 30)
	e := NewEngine(db, 2, 5*time.Second)
	from := end.Add(-25*time.Hour - 17*time.Minute).UnixNano()
	to := end.Add(-90 * time.Minute).UnixNano()
	_ = start
	total := func(points int, tier string) float64 {
		r, err := e.Run(context.Background(), Request{Node: "n", Metric: "bytes_total", Match: []Matcher{{Name: "dir", Value: "in"}}, From: from, To: to, Points: points, Fn: "increase", Tier: tier})
		if err != nil {
			t.Fatal(err)
		}
		s := 0.0
		for _, v := range r.Series[0].Avg {
			if !math.IsNaN(v) {
				s += v
			}
		}
		return s
	}
	want := total(100, "raw")
	for _, pts := range []int{1, 10, 100, 1000} {
		if got := total(pts, ""); math.Abs(got-want) > 1000 {
			t.Errorf("points=%d: increase %v, raw says %v", pts, got, want)
		}
	}
}

// A restart mid-window flushes a partial rollup for the head's window at
// close. After reopening, the head holds that window again and keeps
// growing, so a long-range query must read the window from raw rather than
// from the partial, which stops at the restart.
func TestRestartMidWindowKeepsRecentData(t *testing.T) {
	dir := t.TempDir()
	start, _ := time.Parse(time.RFC3339, "2026-11-01T00:00:00Z")
	now := start
	open := func() *store.DB {
		db, err := store.Open(store.Options{Dir: dir, Now: func() time.Time { return now }, Retention: store.DefaultRetention})
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	write := func(db *store.DB, d time.Duration) {
		for end := now.Add(d); now.Before(end); now = now.Add(10 * time.Second) {
			db.Append(&model.Batch{Node: "n", Group: model.GroupMetrics, Time: now.UnixNano(), Samples: []model.Sample{
				{Name: "g", Kind: model.KindGauge, Value: 7}}})
		}
		db.Tick()
	}
	db := open()
	write(db, 5*time.Hour+20*time.Minute) // into the 04:00-06:00 window
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	defer db.Close()
	write(db, 30*time.Minute) // 05:20-05:50, after the restart
	e := NewEngine(db, 2, 5*time.Second)
	// From the window start, as the fleet sparklines ask on a young store:
	// about 70 s per point, which the 1m tier answers.
	r, err := e.Run(context.Background(), Request{Node: "n", Metric: "g", From: start.Add(4 * time.Hour).UnixNano(), To: now.UnixNano(), Points: 96})
	if err != nil {
		t.Fatal(err)
	}
	restart := start.Add(5*time.Hour + 20*time.Minute).UnixNano()
	for i, ts := range r.Series[0].T {
		if ts > restart+int64(r.Step) && math.IsNaN(r.Series[0].Avg[i]) {
			t.Fatalf("bucket at %s after the restart has no data; parts %+v", time.Unix(0, ts).UTC().Format("15:04"), r.Parts)
		}
	}
}
