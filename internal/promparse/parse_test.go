package promparse

import (
	"os"
	"strings"
	"testing"

	"algod.top/internal/model"
)

func TestParseMainnetFixture(t *testing.T) {
	f, err := os.Open("../fakealgod/mainnet.metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if r.Families != 407 {
		t.Errorf("families = %d, want 407", r.Families)
	}
	if len(r.Series) < 1000 {
		t.Errorf("series = %d, want over 1000", len(r.Series))
	}
	kinds := map[model.Kind]int{}
	for _, s := range r.Series {
		kinds[s.Kind]++
	}
	if kinds[model.KindHistPart] < 600 {
		t.Errorf("histogram parts = %d, want at least 600", kinds[model.KindHistPart])
	}
	if kinds[model.KindGauge] == 0 || kinds[model.KindCounter] == 0 {
		t.Errorf("kinds = %v", kinds)
	}
	found := false
	for _, s := range r.Series {
		if s.Name == "algod_network_p2p_inbound_evictions_total" && s.Labels.Get("state") == "busy" {
			found = true
			if s.Kind != model.KindCounter {
				t.Errorf("evictions kind = %v", s.Kind)
			}
		}
	}
	if !found {
		t.Error("evictions series not found")
	}
}

func TestParseHistogramGrouping(t *testing.T) {
	in := `# TYPE lat_bucket counter
lat_bucket{le="0.1"} 1
lat_bucket{le="+Inf"} 3
# TYPE lat_sum counter
lat_sum 0.7
# TYPE lat_count counter
lat_count 3
# TYPE lone_count counter
lone_count 5
# TYPE g gauge
g{a="x\"y",b="2"} NaN
`
	r, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]model.Kind{"lat_bucket": model.KindHistPart, "lat_sum": model.KindHistPart, "lat_count": model.KindHistPart, "lone_count": model.KindCounter, "g": model.KindGauge}
	for _, s := range r.Series {
		if want[s.Name] != s.Kind {
			t.Errorf("%s kind %v want %v", s.Name, s.Kind, want[s.Name])
		}
		if s.Name == "g" && s.Labels.Get("a") != `x"y` {
			t.Errorf("label a = %q", s.Labels.Get("a"))
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add("a{b=\"c\"} 1\n# TYPE a counter\n")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = Parse(strings.NewReader(s))
	})
}

func TestParserCacheReusesLabels(t *testing.T) {
	p := NewParser()
	a, _ := p.Parse(strings.NewReader("# TYPE x counter\nx{a=\"1\",b=\"q z\"} 5\ny 1\n"))
	b, _ := p.Parse(strings.NewReader("# TYPE x counter\nx{a=\"1\",b=\"q z\"} 7\ny 2\nz{c=\"3\"} 9\n"))
	if b.Series[0].Value != 7 || b.Series[0].Kind != model.KindCounter || b.Series[0].Labels.Get("b") != "q z" {
		t.Fatalf("second scrape %+v", b.Series[0])
	}
	if &a.Series[0].Labels[0] != &b.Series[0].Labels[0] {
		t.Fatal("labels were parsed again instead of reused")
	}
	if b.Series[0].Full != `x{a=1,b=q z}` || b.Series[2].Name != "z" {
		t.Fatalf("keys %q %+v", b.Series[0].Full, b.Series[2])
	}
}

func BenchmarkParseMainnetCached(b *testing.B) {
	data, _ := os.ReadFile("../fakealgod/mainnet.metrics")
	p := NewParser()
	p.Parse(strings.NewReader(string(data)))
	b.ReportAllocs()
	for b.Loop() {
		p.Parse(strings.NewReader(string(data)))
	}
}

func TestParserKindsStableAcrossScrapes(t *testing.T) {
	data, _ := os.ReadFile("../fakealgod/mainnet.metrics")
	p := NewParser()
	first, _ := p.Parse(strings.NewReader(string(data)))
	second, _ := p.Parse(strings.NewReader(string(data)))
	for i := range first.Series {
		if first.Series[i].Kind == 0 || first.Series[i].Kind != second.Series[i].Kind {
			t.Fatalf("%s: kind %v then %v", first.Series[i].Name, first.Series[i].Kind, second.Series[i].Kind)
		}
	}
	// A new series in the third scrape is classified, and the rest keep
	// their kinds.
	third, _ := p.Parse(strings.NewReader(string(data) + "# TYPE brand_new gauge\nbrand_new 1\n"))
	last := third.Series[len(third.Series)-1]
	if last.Name != "brand_new" || last.Kind != model.KindGauge || third.Series[0].Kind != first.Series[0].Kind {
		t.Fatalf("third scrape: %+v", last)
	}
}
