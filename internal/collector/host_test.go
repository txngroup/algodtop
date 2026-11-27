package collector

import (
	"math"
	"testing"
	"time"
)

func TestNetRateNeedsOneMethod(t *testing.T) {
	sock := netPoint{in: 1000, out: 2000, t: 100, method: "sock_diag"}
	for _, c := range []struct {
		name    string
		prev    netPoint
		cur     netPoint
		in, out float64
		ok      bool
	}{
		{"steady", sock, netPoint{in: 4000, out: 8000, t: 130, method: "sock_diag"}, 100, 200, true},
		{"first reading", netPoint{}, sock, 0, 0, false},
		{"counter went down", sock, netPoint{in: 10, out: 9000, t: 130, method: "sock_diag"}, 0, 0, false},
		// A switch to interface totals jumps from one process's bytes to
		// a whole namespace's, which is not traffic.
		{"method changed", sock, netPoint{in: 9e9, out: 9e9, t: 130, method: "netns interface totals"}, 0, 0, false},
	} {
		in, out, ok := netRate(c.prev, c.cur)
		if in != c.in || out != c.out || ok != c.ok {
			t.Errorf("%s: got %v %v %v, want %v %v %v", c.name, in, out, ok, c.in, c.out, c.ok)
		}
	}
}

// A restart keeps the disk fill forecast: the stored readings seed the
// fit, and a reading of a disk another size stays out of it.
func TestDiskForecastSeed(t *testing.T) {
	const gb = 1e9
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var calls int
	h := &Host{diskHist: map[string][]diskPoint{}, o: HostOptions{
		DiskHistory: func(node string, from, to time.Time) []DiskReading {
			calls++
			if node != "n1" || !to.Equal(now) || !from.Equal(now.Add(-7*24*time.Hour)) {
				t.Errorf("history for %s from %v to %v", node, from, to)
			}
			var out []DiskReading
			// Two days of use growing 10 GB a day on a 1000 GB disk.
			for i := 0; i <= 48; i++ {
				ts := now.Add(time.Duration(i-48) * time.Hour)
				used := 400*gb + float64(i)*10*gb/24
				out = append(out, DiskReading{T: ts, Free: 1000*gb - used, Total: 1000 * gb})
			}
			// The data dir's old disk was full, which would flatten the fit.
			out = append(out, DiskReading{T: now.Add(-50 * time.Hour), Free: 0, Total: 500 * gb})
			return out
		}}}
	h.seedDisk("n1", "/data", now, 1000*gb)
	h.seedDisk("n1", "/data", now, 1000*gb)
	if calls != 1 {
		t.Errorf("read the store %d times, want once", calls)
	}
	days := h.daysUntilFull("/data", now, 420*gb, 1000*gb)
	if math.Abs(days-58) > 0.5 {
		t.Errorf("days until full: got %.2f, want 58", days)
	}
	// Without a store, a new dir waits for an hour of its own readings.
	h.o.DiskHistory = nil
	h.seedDisk("n2", "/other", now, 1000*gb)
	if d := h.daysUntilFull("/other", now, 420*gb, 1000*gb); d != 0 {
		t.Errorf("unseeded dir: got %.2f days, want 0", d)
	}
}
