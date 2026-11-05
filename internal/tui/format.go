package tui

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Style holds the terminal capabilities: colour and Unicode blocks.
type Style struct {
	Color   bool
	Unicode bool
}

// ANSI colours. The mapping matches the web UI: inbound one hue, outbound
// another, ws and p2p by lightness.
const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cRev    = "\x1b[7m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cBlue   = "\x1b[34m"
	cCyan   = "\x1b[36m"
	cOrange = "\x1b[38;5;208m"
)

func (s Style) c(code, text string) string {
	if !s.Color || text == "" {
		return text
	}
	return code + text + cReset
}

// Sparkline characters. Unicode blocks are written as escapes so the
// source stays ASCII.
var (
	sparkUnicode = []rune{'\u2581', '\u2582', '\u2583', '\u2584', '\u2585', '\u2586', '\u2587', '\u2588'}
	sparkASCII   = []rune{'_', '.', '-', '~', '=', '+', '*', '#'}
)

// Spark renders values as a sparkline of exactly width cells. Gaps are
// blank, never a drawn value.
func (s Style) Spark(vals []float64, width int, zero bool) string {
	if width <= 0 {
		return ""
	}
	set := sparkASCII
	if s.Unicode {
		set = sparkUnicode
	}
	// Resample to width buckets.
	b := make([]float64, width)
	for i := range b {
		b[i] = math.NaN()
	}
	if len(vals) > 0 {
		for i := range b {
			lo := i * len(vals) / width
			hi := max(lo+1, (i+1)*len(vals)/width)
			sum, n := 0.0, 0
			for _, v := range vals[lo:min(hi, len(vals))] {
				if !math.IsNaN(v) && !math.IsInf(v, 0) {
					sum += v
					n++
				}
			}
			if n > 0 {
				b[i] = sum / float64(n)
			}
		}
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range b {
		if !math.IsNaN(v) {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	if zero && lo > 0 {
		lo = 0
	}
	var sb strings.Builder
	for _, v := range b {
		if math.IsNaN(v) {
			sb.WriteByte(' ')
			continue
		}
		k := 0
		if hi > lo {
			k = int((v - lo) / (hi - lo) * float64(len(set)-1))
		}
		sb.WriteRune(set[max(0, min(k, len(set)-1))])
	}
	return sb.String()
}

// Bytes renders 2.1G, 410K.
func Bytes(v float64) string {
	if math.IsNaN(v) || v < 0 {
		return "-"
	}
	units := []string{"", "K", "M", "G", "T", "P"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f", v)
	}
	if v < 10 {
		return fmt.Sprintf("%.1f%s", v, units[i])
	}
	return fmt.Sprintf("%.0f%s", v, units[i])
}

// KBps renders a byte rate in KB/s.
func KBps(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.0f", v/1024)
}

// Num renders a number compactly.
func Num(v float64) string {
	switch {
	case math.IsNaN(v):
		return "-"
	case math.Abs(v) >= 1e9:
		return fmt.Sprintf("%.1fG", v/1e9)
	case math.Abs(v) >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case math.Abs(v) >= 1e4:
		return fmt.Sprintf("%.1fk", v/1e3)
	case math.Abs(v) >= 100 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// Dur renders 3d 4h, 5m, 0.6s.
func Dur(d time.Duration) string {
	switch {
	case d < 0:
		return "-"
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
}

// Secs renders seconds as 0.6s or 1m.
func Secs(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return Dur(time.Duration(v * float64(time.Second)))
}

// visibleLen counts runes outside ANSI escapes.
func visibleLen(s string) int {
	n := 0
	in := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			in = false
		case !in:
			n++
		}
	}
	return n
}

// pad pads or truncates to width visible cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := visibleLen(s)
	if n == w {
		return s
	}
	if n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return truncate(s, w)
}

func rpad(s string, w int) string {
	n := visibleLen(s)
	if n >= w {
		return truncate(s, w)
	}
	return strings.Repeat(" ", w-n) + s
}

// truncate cuts to w visible cells, keeping escapes and resetting colour.
func truncate(s string, w int) string {
	if visibleLen(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	in := false
	esc := false
	for _, r := range s {
		if r == '\x1b' {
			in, esc = true, true
			b.WriteRune(r)
			continue
		}
		if in {
			b.WriteRune(r)
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
				in = false
			}
			continue
		}
		if n >= w {
			break
		}
		b.WriteRune(r)
		n++
	}
	if esc {
		b.WriteString(cReset)
	}
	return b.String()
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
