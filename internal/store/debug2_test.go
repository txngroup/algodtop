package store

import (
	"testing"
	"time"
)

// Crash, recover, close cleanly, and continue, several times. A clean
// close leaves an empty WAL segment whose name the next writer reuses; an
// earlier version deleted that open file and lost everything after it.
func TestRecoveryAcrossCleanRestartCycles(t *testing.T) {
	dir := t.TempDir()
	c := newClock("2026-11-03T00:00:00Z")
	total := 0
	for round, n := range []int{150, 300, 200, 250, 400} {
		db := openTest(t, dir, c)
		for i := 0; i < n; i++ {
			db.Append(batchAt("n", c.t, total))
			total++
			c.Add(10 * time.Second)
			if i%5 == 0 {
				db.commit()
			}
			db.Tick()
		}
		db.commit()
		crash(db)
		db2 := openTest(t, dir, c)
		node, _ := db2.NodeID("n")
		pts, _, _ := db2.Read(node, []uint32{idOf(t, db2, node, "c")}, TierRaw, 0, c.t.UnixNano())
		if len(pts[0].T) != total {
			t.Fatalf("round %d: recovered %d of %d samples", round, len(pts[0].T), total)
		}
		db2.Close()
	}
}
