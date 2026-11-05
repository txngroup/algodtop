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

// memoryCreep flags nodes whose daily minimum RSS rose three days running.
// Plain RSS spikes are normal; a rising floor is a leak.
func (d *Daemon) memoryCreep(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	now := time.Now().Truncate(24 * time.Hour)
	for _, s := range d.states() {
		if s.Kind != "local" {
			continue
		}
		r, err := d.qe.Run(ctx, query.Request{Node: s.Name, Metric: "proc_resident_bytes", Fn: "min", From: now.Add(-4 * 24 * time.Hour).UnixNano(), To: now.UnixNano(), Points: 4})
		if err != nil || len(r.Series) == 0 {
			continue
		}
		mins := r.Series[0].Min
		rising := len(mins) == 4
		for i := 1; i < len(mins) && rising; i++ {
			if math.IsNaN(mins[i]) || math.IsNaN(mins[i-1]) || mins[i] <= mins[i-1] {
				rising = false
			}
		}
		out[s.Name] = rising
	}
	return out
}
