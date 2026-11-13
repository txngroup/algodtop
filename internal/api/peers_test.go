package api

import (
	"math"
	"testing"
	"time"

	"algod.top/internal/model"
)

// usagePolls builds polls every 30 s from start. Each peer moves rate bytes
// per second in both directions while it is in a poll, and its totals
// restart from zero when it comes back after missing a poll.
func usagePolls(start time.Time, polls int, rate map[string]int64, present func(key string, i int) bool) []model.PeerUsage {
	var out []model.PeerUsage
	tot := map[string]int64{}
	last := map[string]int{}
	for i := 0; i < polls; i++ {
		at := start.Add(time.Duration(i) * 30 * time.Second).UnixNano()
		for k, r := range rate {
			if !present(k, i) {
				continue
			}
			if l, ok := last[k]; ok && l == i-1 {
				tot[k] += 30 * r
			} else {
				tot[k] = 15 * r // connected half an interval before the poll
			}
			last[k] = i
			out = append(out, model.PeerUsage{Time: at, Key: k, Network: "p2p", Addr: k, BytesIn: tot[k], BytesOut: tot[k]})
		}
	}
	return out
}

func rateOf(t *testing.T, r PeerTrafficResult, key string) float64 {
	t.Helper()
	for _, p := range r.Top {
		if p.Key == key {
			return p.RateIn
		}
	}
	t.Fatalf("%s not in top: %+v", key, r.Top)
	return 0
}

func near(a, b float64) bool { return math.Abs(a-b) <= 0.02*b }

// A peer seen for 2 minutes of a 1 h range shows the rate it moved bytes
// at, rather than its bytes spread over the hour.
func TestTopTalkerRateIsOverConnectedTime(t *testing.T) {
	from := time.Unix(1_800_000_000, 0)
	to := from.Add(time.Hour)
	us := usagePolls(from, 120, map[string]int64{"steady": 1000, "brief": 8000}, func(k string, i int) bool {
		return k == "steady" || (i >= 100 && i <= 104)
	})
	r := PeerTraffic(us, from.UnixNano(), to.UnixNano(), 10, 5, "")
	if got := rateOf(t, r, "steady"); !near(got, 1000) {
		t.Fatalf("steady peer: %.0f B/s, want 1000", got)
	}
	if got := rateOf(t, r, "brief"); !near(got, 8000) {
		t.Fatalf("brief peer: %.0f B/s, want 8000", got)
	}
}

// A peer that leaves and comes back counts only the time it was connected.
// A gap with no polls at all, as when the daemon was stopped, keeps its
// length so the rate across it stays right.
func TestTopTalkerRateSkipsReconnectGaps(t *testing.T) {
	from := time.Unix(1_800_000_000, 0)
	to := from.Add(time.Hour)
	us := usagePolls(from, 120, map[string]int64{"steady": 1000, "flappy": 4000}, func(k string, i int) bool {
		return k == "steady" || i < 20 || i >= 60
	})
	r := PeerTraffic(us, from.UnixNano(), to.UnixNano(), 10, 5, "")
	// flappy's return poll carries 15 s of bytes, counted over the 30 s
	// since the poll before it, so allow a little under 4000.
	if got := rateOf(t, r, "flappy"); got < 3900 || got > 4000 {
		t.Fatalf("flappy peer: %.0f B/s, want about 4000", got)
	}

	// Drop every poll between 20 and 60: the daemon was down, and the
	// steady peer's totals kept growing across the gap.
	var gap []model.PeerUsage
	for _, u := range usagePolls(from, 120, map[string]int64{"steady": 1000}, func(string, int) bool { return true }) {
		if i := (u.Time - from.UnixNano()) / int64(30*time.Second); i < 20 || i >= 60 {
			gap = append(gap, u)
		}
	}
	r = PeerTraffic(gap, from.UnixNano(), to.UnixNano(), 10, 5, "")
	if got := rateOf(t, r, "steady"); !near(got, 1000) {
		t.Fatalf("steady peer across a monitoring gap: %.0f B/s, want 1000", got)
	}
}
