package api

import (
	"testing"
	"time"
)

func TestTimeParam(t *testing.T) {
	def := time.Unix(0, 42)
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"", 42},
		{"1794094676", 1794094676e9},
		{"1794094676942", 1794094676942e6},
		{"1794094676942000000", 1794094676942000000},
		// The fleet page divides a nanosecond time by 1e6.
		{"1794094676942.322", 1794094676942322000},
		{"1794094676942.", 1794094676942000000},
		{"1794094676.5", 1794094676500000000},
		{"2026-11-07T23:37:56Z", time.Date(2026, 11, 7, 23, 37, 56, 0, time.UTC).UnixNano()},
	} {
		got, err := timeParam(c.in, def)
		if err != nil {
			t.Errorf("timeParam(%q): %v", c.in, err)
			continue
		}
		// A float64 holds about 16 digits, so a fractional millisecond
		// lands within a few hundred nanoseconds.
		if d := got - c.want; d < -1000 || d > 1000 {
			t.Errorf("timeParam(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"1e12", "NaN", "Inf", "0x1p40", "1.2.3", ".5", "+1.5", "12.5x", "99999999999999999999.5"} {
		if v, err := timeParam(bad, def); err == nil {
			t.Errorf("timeParam(%q) = %d, want an error", bad, v)
		}
	}
}
