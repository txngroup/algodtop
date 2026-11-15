package daemon

import (
	"context"
	"fmt"
	"math"
	"time"

	"algod.top/internal/disk"
	"algod.top/internal/model"
	"algod.top/internal/query"
)

// clockWatch records host clock jumps: wall time moving apart from the
// monotonic clock. A jump never reorders stored data; samples older than
// the current head go through the late-data path.
type clockWatch struct {
	started  bool
	lastWall int64         // unix ns
	lastMono time.Duration // since process start
}

var processStart = time.Now()

func (c *clockWatch) check(now time.Time) (time.Duration, bool) {
	return c.observe(now.UnixNano(), now.Sub(processStart))
}

// observe compares how far the wall clock and the monotonic clock moved.
func (c *clockWatch) observe(wall int64, mono time.Duration) (time.Duration, bool) {
	if !c.started {
		c.started, c.lastWall, c.lastMono = true, wall, mono
		return 0, false
	}
	jump := time.Duration(wall-c.lastWall) - (mono - c.lastMono)
	c.lastWall, c.lastMono = wall, mono
	if jump > 2*time.Second || jump < -2*time.Second {
		return jump, true
	}
	return 0, false
}

func (d *Daemon) noteClock(cw *clockWatch, now time.Time) {
	if jump, ok := cw.check(now); ok {
		dir := "forward"
		if jump < 0 {
			dir = "backward"
		}
		d.emitEvent(model.Event{Kind: model.EvClockJump, Severity: model.SevWarn,
			Msg: fmt.Sprintf("host clock jumped %s by %s", dir, jump.Abs().Round(time.Millisecond)), Attrs: map[string]any{"seconds": jump.Seconds()}})
	}
}

// projectionOver reports whether the projected use for the configured
// windows exceeds free space minus the disk floor.
func (d *Daemon) projectionOver() bool {
	u, err := disk.Stat(d.db.Dir())
	if err != nil || u.Total == 0 {
		return false
	}
	cfg := d.Config()
	in := d.db.Inspection()
	var need, have int64
	for _, p := range in.Project(d.db.Retention(), len(cfg.Nodes), cfg.Intervals.Scrape.D()) {
		if p.Settled >= 0 {
			need += p.Settled
		} else {
			need += p.PerYear // one year of a forever tier
		}
	}
	for _, sz := range d.db.Sizes() {
		have += sz
	}
	floor := cfg.Disk.FloorPercent / 100
	if floor == 0 {
		floor = 0.05
	}
	room := int64(u.Free) - int64(floor*float64(u.Total))
	return need-have > room
}

// creepDays is how many whole days memoryCreep reads.
const creepDays = 10

// memoryCreep flags nodes whose daily minimum RSS rose three days running.
// Plain RSS spikes are normal; a rising floor is a leak.
func (d *Daemon) memoryCreep(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	today := time.Now().Truncate(24 * time.Hour)
	for _, s := range d.states() {
		if s.Kind == "local" {
			out[s.Name] = floorRising(ctx, d.qe, s.Name, s.StartTime, today)
		}
	}
	return out
}

// floorRising reads a node's daily minimum RSS for the creepDays before
// today, a UTC midnight, and applies risingFloor. start is the process
// start, unix ns, or 0 when unknown.
func floorRising(ctx context.Context, qe *query.Engine, node string, start int64, today time.Time) bool {
	from := today.Add(-creepDays * 24 * time.Hour)
	r, err := qe.Run(ctx, query.Request{Node: node, Metric: "proc_resident_bytes", Fn: "min", From: from.UnixNano(), To: today.UnixNano(), Points: creepDays})
	if err != nil || len(r.Series) == 0 {
		return false
	}
	return risingFloor(r.Series[0].Min, firstWholeDay(from, start))
}

// firstWholeDay is the index of the first day, counted from from, that
// the process ran from start to end. Days before a restart belong to
// another process, and the day of the start holds its warm-up.
func firstWholeDay(from time.Time, start int64) int {
	st := time.Unix(0, start)
	if start == 0 || st.Before(from) {
		return 0
	}
	return int(st.Sub(from)/(24*time.Hour)) + 1
}

// risingFloor reports whether each of the last three daily minimums is
// higher than every earlier one from index first on. Comparing each day
// with only the day before also flagged the recovery from one low day:
// when the host runs short of memory, macOS compresses and pages out
// every process at once, and their minimums drop together for a day.
func risingFloor(mins []float64, first int) bool {
	n := len(mins)
	if first < 0 || n-first < 4 {
		return false
	}
	for _, v := range mins[n-4:] {
		if math.IsNaN(v) {
			return false
		}
	}
	for i := n - 3; i < n; i++ {
		for _, v := range mins[first:i] {
			if !math.IsNaN(v) && mins[i] <= v {
				return false
			}
		}
	}
	return true
}
