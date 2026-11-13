package tui

import (
	"cmp"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/health"
	"algod.top/internal/model"
)

// Screen is which view is showing.
type Screen int

// Screens.
const (
	ScreenMain Screen = iota
	ScreenNode
	ScreenHelp
	ScreenEmpty
	ScreenWrite
)

// Pane is the lower pane of the node screen.
type Pane int

// Panes.
const (
	PanePeers Pane = iota
	PaneEvents
	PaneLogs
)

// Ranges cycles with r.
var Ranges = []struct {
	Name string
	D    time.Duration
}{{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour},
	{"1y", 365 * 24 * time.Hour}, {"all", 20 * 365 * 24 * time.Hour}}

// Point is a value at the time cursor.
type Point struct {
	Round, Lag, RTime, CPU, RSS, NetIn, NetOut float64
	Peers                                      collector.PeerCounts
	Up                                         float64
}

// Model is everything the renderer needs. Rendering is a pure function of
// it, so tests compare screens with golden files.
type Model struct {
	W, H      int
	Style     Style
	Now       time.Time
	UTC       bool
	Status    api.DaemonStatus
	Nodes     []api.NodeView
	Events    []model.Event
	Sel       int
	Screen    Screen
	Node      string
	Pane      Pane
	RangeIdx  int
	FullRes   bool
	Cursor    int64 // 0 = live, else unix ns
	Paused    bool
	Filter    string
	SortCol   int
	Spark     map[string][]float64
	At        map[string]Point // values at the cursor
	PeerHist  []api.PeerHistory
	Templ     []api.TemplateCount
	NodeEv    []model.Event
	PeerSort  int
	Prompt    string // active prompt label, "" when none
	Input     string
	Message   string
	Err       string
	WriteText []string // W: config contents to confirm
	// Scroll is the first row of the node screen's open pane. The last
	// draw sets paneRows and paneTotal, which bound it.
	Scroll    int
	paneRows  int
	paneTotal int
}

// window picks the rows of a pane's list that fit in room lines, from the
// scroll offset. A list that does not fit gets a footer line, so it shows
// room-1 rows.
func (m *Model) window(total, room int) (from, to int, footer string) {
	m.paneTotal = total
	if total <= room {
		m.paneRows = total
		return 0, total, ""
	}
	rows := max(room-1, 0)
	m.paneRows = max(rows, 1)
	from = max(0, min(m.Scroll, total-rows))
	to = min(total, from+rows)
	if rows == 0 {
		return from, to, m.Style.c(cDim, fmt.Sprintf(" %d rows; the window is too short to show them", total))
	}
	return from, to, m.Style.c(cDim, fmt.Sprintf(" rows %d-%d of %d   up/down, pgup/pgdn, home/end scroll", from+1, to, total))
}

func (m *Model) fmtTime(t time.Time) time.Time {
	if m.UTC {
		return t.UTC()
	}
	return t.Local()
}

// Columns of the node table.
var columns = []struct {
	Name  string
	Width int
	Right bool
}{
	{"NODE", 12, false}, {"NET", 9, false}, {"VER", 12, false}, {"ROUND", 10, true}, {"LAG", 6, true}, {"RTIME", 6, true},
	{"WS i/o", 8, true}, {"P2P i/o", 8, true}, {"CPU%", 6, true}, {"RSS", 6, true}, {"NET KB/s", 10, true}, {"HEALTH", 0, false},
}

// Render draws the whole screen as exactly H lines of W cells.
func Render(m *Model) []string {
	var lines []string
	switch m.Screen {
	case ScreenHelp:
		lines = renderHelp(m)
	case ScreenEmpty:
		lines = renderEmpty(m)
	case ScreenNode:
		lines = renderNode(m)
	case ScreenWrite:
		lines = renderWrite(m)
	default:
		lines = renderMain(m)
	}
	if m.Prompt != "" {
		p := m.Style.c(cRev, " "+m.Prompt+" ") + " " + m.Input + "_"
		if len(lines) >= m.H {
			lines = lines[:m.H-1]
		}
		for len(lines) < m.H-1 {
			lines = append(lines, "")
		}
		lines = append(lines, p)
	}
	for len(lines) < m.H {
		lines = append(lines, "")
	}
	lines = lines[:m.H]
	for i := range lines {
		lines[i] = pad(lines[i], m.W)
	}
	return lines
}

func (m *Model) header() string {
	t := m.fmtTime(m.Now).Format("15:04:05")
	ok, warn, crit := 0, 0, 0
	for _, n := range m.Nodes {
		switch {
		case !n.Up:
			crit++
		case n.Health.Level == health.Critical:
			crit++
		case n.Health.Level == health.Warn:
			warn++
		default:
			ok++
		}
	}
	var parts []string
	parts = append(parts, m.Style.c(cBold, "algod.top"), "host "+m.Status.Host, t)
	if m.Status.Live {
		parts = append(parts, m.Style.c(cYellow, "no daemon: live only, no history"))
	} else {
		up := m.Now.Sub(time.Unix(0, m.Status.Started))
		parts = append(parts, "daemon up "+Dur(up), "store "+Bytes(float64(m.Status.StoreBytes))+"B")
		if m.Status.Paused {
			parts = append(parts, m.Style.c(cRed, "WRITES PAUSED"))
		}
		if m.Status.Latest != "" {
			parts = append(parts, m.Style.c(cYellow, "newer release: algod.top upgrade"))
		}
	}
	summary := fmt.Sprintf("nodes %d: %d OK", len(m.Nodes), ok)
	if warn > 0 {
		summary += fmt.Sprintf(", %s", m.Style.c(cYellow, fmt.Sprintf("%d Warn", warn)))
	}
	if crit > 0 {
		summary += fmt.Sprintf(", %s", m.Style.c(cRed, fmt.Sprintf("%d Critical", crit)))
	}
	parts = append(parts, summary)
	return strings.Join(parts, "  ")
}

func (m *Model) rangeLabel() string {
	s := "range " + Ranges[m.RangeIdx].Name
	if m.FullRes {
		s += " full-res"
	}
	switch {
	case m.Cursor != 0:
		s += "  " + m.Style.c(cOrange, "at "+m.fmtTime(time.Unix(0, m.Cursor)).Format("2006-01-02 15:04:05"))
	case m.Paused:
		s += "  " + m.Style.c(cYellow, "paused")
	default:
		s += "  live"
	}
	return s
}

// VisibleNodes applies the filter and sort.
func (m *Model) VisibleNodes() []api.NodeView {
	var out []api.NodeView
	f := strings.ToLower(m.Filter)
	for _, n := range m.Nodes {
		if f == "" || strings.Contains(strings.ToLower(n.Name+" "+n.Network+" "+n.Summary+" "+n.Version), f) {
			out = append(out, n)
		}
	}
	if m.SortCol > 0 {
		key := func(n api.NodeView) float64 {
			switch m.SortCol {
			case 1:
				return 0
			case 3:
				return float64(n.Round)
			case 4:
				return n.Lag
			case 5:
				return n.RoundTime
			case 6:
				return n.WSIn + n.WSOut
			case 7:
				return n.P2PIn + n.P2POut
			case 8:
				return n.CPU
			case 9:
				return n.RSS
			case 10:
				return n.NetIn + n.NetOut
			case 11:
				return float64(n.Health.Level)
			}
			return 0
		}
		sort.SliceStable(out, func(i, j int) bool {
			if m.SortCol == 1 {
				return out[i].Network < out[j].Network
			}
			if m.SortCol == 2 {
				return out[i].Version > out[j].Version
			}
			return key(out[i]) > key(out[j])
		})
	}
	return out
}

func (m *Model) selected() *api.NodeView {
	vis := m.VisibleNodes()
	if len(vis) == 0 {
		return nil
	}
	i := max(0, min(m.Sel, len(vis)-1))
	return &vis[i]
}

func dash(v float64, f func(float64) string) string {
	if math.IsNaN(v) {
		return "-"
	}
	return f(v)
}

// roundText shows an unknown round as "-". It is 0 until /v2/status
// answers, for example while algod rejects the API token.
func roundText(v float64) string {
	if math.IsNaN(v) || v == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

func (m *Model) summaryLine(n *api.NodeView) string {
	if n == nil {
		return ""
	}
	p, hist := m.At[n.Name]
	round, rt, tpb := float64(n.Round), n.RoundTime, n.TxnsPerBlk
	if m.Cursor != 0 && hist {
		round, rt, tpb = p.Round, p.RTime, math.NaN()
	}
	parts := []string{m.Style.c(cBold, n.Name), "round " + roundText(round),
		"rtime " + dash(rt, func(v float64) string { return fmt.Sprintf("%.2fs", v) }), "tpb " + Num(tpb), "pool " + Num(n.PoolSize)}
	return strings.Join(parts, "  ") + "     " + m.rangeLabel()
}

func healthCell(s Style, n api.NodeView) string {
	if !n.Up {
		return s.c(cRed, "DOWN")
	}
	switch n.Health.Level {
	case health.Critical:
		return s.c(cRed, "CRIT"+moreReasons(n.Health)+" "+firstReason(n.Health, health.Critical))
	case health.Warn:
		return s.c(cYellow, "WARN"+moreReasons(n.Health)+" "+firstReason(n.Health, health.Warn))
	}
	if n.Silenced {
		return s.c(cGreen, "OK") + " (silenced)"
	}
	return s.c(cGreen, "OK")
}

// moreReasons counts the reasons a table cell leaves out. It goes next to
// the level, because a narrow terminal cuts the end of the row.
func moreReasons(h health.Status) string {
	if len(h.Reasons) < 2 {
		return ""
	}
	return fmt.Sprintf(" (+%d)", len(h.Reasons)-1)
}

// healthLines lists a node's reasons one per line, worst first.
func healthLines(s Style, n api.NodeView) []string {
	word := map[health.Level]string{health.OK: s.c(cGreen, "OK"), health.Warn: s.c(cYellow, "WARN"), health.Critical: s.c(cRed, "CRIT")}
	head := word[n.Health.Level]
	if !n.Up {
		head = s.c(cRed, "DOWN")
	} else if n.Silenced {
		head += " (silenced)"
	}
	lines := []string{"  " + head}
	rs := append([]health.Reason(nil), n.Health.Reasons...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Level > rs[j].Level })
	for _, r := range rs {
		mark := s.c(cYellow, "warn")
		if r.Level == health.Critical {
			mark = s.c(cRed, "crit")
		}
		lines = append(lines, "    "+mark+"  "+r.Text)
	}
	return lines
}

func firstReason(h health.Status, l health.Level) string {
	for _, r := range h.Reasons {
		if r.Level == l {
			t := r.Text
			t = strings.Replace(t, "participation key for ", "key ", 1)
			t = strings.Replace(t, " expires in ", " expires ", 1)
			t = strings.Replace(t, " days", "d", 1)
			return t
		}
	}
	return ""
}

func (m *Model) row(n api.NodeView, selected bool) string {
	m.fitColumns()
	cells := make([]string, len(columns))
	p, hist := m.At[n.Name]
	useHist := m.Cursor != 0 && hist
	if !n.Up && !useHist {
		cells[0], cells[1] = n.Name, n.Network
		for i := 2; i < len(cells)-1; i++ {
			cells[i] = "-"
		}
		cells[len(cells)-1] = healthCell(m.Style, n)
	} else {
		round, lag, rt := float64(n.Round), n.Lag, n.RoundTime
		peers, cpu, rss, nin, nout := n.Peers, n.CPU, n.RSS, n.NetIn, n.NetOut
		if useHist {
			round, lag, rt, peers, cpu, rss, nin, nout = p.Round, p.Lag, p.RTime, p.Peers, p.CPU, p.RSS, p.NetIn, p.NetOut
		}
		cells[0] = n.Name
		cells[1] = n.Network
		cells[2] = n.Version
		cells[3] = roundText(round)
		cells[4] = Secs(lag)
		cells[5] = dash(rt, func(v float64) string {
			if v == 0 {
				return "-"
			}
			return fmt.Sprintf("%.2fs", v)
		})
		if n.HasAdmin || useHist {
			cells[6] = fmt.Sprintf("%d/%d", peers.WSIn, peers.WSOut)
			cells[7] = fmt.Sprintf("%d/%d", peers.P2PIn, peers.P2POut)
		} else {
			cells[6], cells[7] = "n/a", "n/a"
		}
		if n.Kind == "rest" || n.Kind == "ssh" {
			cells[8], cells[9], cells[10] = "n/a", "n/a", "n/a"
		} else {
			cells[8] = dash(cpu, func(v float64) string { return fmt.Sprintf("%.1f", v) })
			cells[9] = Bytes(rss)
			if n.NetMethod != "" || useHist {
				cells[10] = KBps(nin) + "/" + KBps(nout)
			} else {
				cells[10] = "-"
			}
		}
		if useHist {
			cells[11] = m.Style.c(cOrange, "history")
			if p.Up == 0 {
				cells[11] = m.Style.c(cRed, "DOWN")
			}
		} else {
			cells[11] = healthCell(m.Style, n)
		}
	}
	var b strings.Builder
	for i, c := range columns {
		w := c.Width
		if w == 0 {
			b.WriteString(cells[i])
			continue
		}
		if c.Right {
			b.WriteString(rpad(cells[i], w-1) + " ")
		} else {
			b.WriteString(pad(cells[i], w))
		}
	}
	s := b.String()
	if selected {
		if m.Style.Color {
			return m.Style.c(cRev, pad(" "+s, m.W))
		}
		return ">" + s
	}
	return " " + s
}

func (m *Model) tableHeader() string {
	m.fitColumns()
	var b strings.Builder
	for i, c := range columns {
		name := c.Name
		if i == m.SortCol && m.SortCol > 0 {
			name += "*"
		}
		if c.Width == 0 {
			b.WriteString(name)
		} else if c.Right {
			b.WriteString(rpad(name, c.Width-1) + " ")
		} else {
			b.WriteString(pad(name, c.Width))
		}
	}
	return m.Style.c(cBold, " "+b.String())
}

func renderMain(m *Model) []string {
	lines := []string{m.header(), m.summaryLine(m.selected())}
	if m.Err != "" {
		lines = append(lines, m.Style.c(cRed, m.Err))
	} else if m.Message != "" {
		lines = append(lines, m.Style.c(cYellow, m.Message))
	} else {
		lines = append(lines, "")
	}
	lines = append(lines, m.tableHeader())
	vis := m.VisibleNodes()
	for i, n := range vis {
		lines = append(lines, m.row(n, i == m.Sel))
	}
	if len(vis) == 0 {
		lines = append(lines, "  (no nodes match "+fmt.Sprintf("%q", m.Filter)+")")
	}
	lines = append(lines, "", m.Style.c(cBold, "EVENTS"))
	room := m.H - len(lines)
	evs := m.Events
	for i := len(evs) - 1; i >= 0 && room > 0; i-- {
		lines = append(lines, m.eventLine(evs[i], true))
		room--
	}
	return lines
}

func (m *Model) eventLine(e model.Event, withNode bool) string {
	t := m.fmtTime(time.Unix(0, e.Time))
	ts := t.Format("15:04:05")
	if m.Now.Sub(t) > 24*time.Hour {
		ts = t.Format("01-02 15:04")
	}
	kind := e.Kind
	msg := e.Msg
	if e.Kind == model.EvDisconnect || e.Kind == model.EvConnect {
		// "disconnect  in  p2p  1.2.3.4  reason=reset"
		msg = fmt.Sprintf("%-3s %-4s %-16s", attr(e, "direction"), attr(e, "network"), attr(e, "ip"))
		if e.Kind == model.EvDisconnect {
			msg += " reason=" + attr(e, "reason")
			if e.Attrs["reset"] == true {
				msg += " reset"
			}
		}
	}
	col := ""
	switch e.Severity {
	case model.SevWarn:
		col = cYellow
	case model.SevError, model.SevCritical:
		col = cRed
	}
	if strings.HasPrefix(kind, "log-") {
		kind = strings.TrimPrefix(kind, "log-")
	}
	s := ts + "  "
	if withNode {
		s += pad(e.Node, m.nodeColWidth())
	}
	s += kindCol(kind) + msg
	if col != "" {
		return m.Style.c(col, s)
	}
	return s
}

// kindCol pads an event kind to its column, which fits config-effective
// and a gap. A longer kind keeps its full name and a space, so it never
// runs into the message.
func kindCol(k string) string {
	const w = 18
	if visibleLen(k) >= w {
		return k + " "
	}
	return pad(k, w)
}

// nodeColWidth is the node column in event lines: the longest node name,
// up to 24, and a gap, so a long name never runs into the event kind.
func (m *Model) nodeColWidth() int {
	w := 8
	for _, n := range m.Nodes {
		w = max(w, visibleLen(n.Name))
	}
	return min(w, 24) + 2
}

func attr(e model.Event, k string) string {
	if v, ok := e.Attrs[k]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

// sparkRow renders " label  value  sparkline" in width w.
func (m *Model) sparkRow(label, value, key string, w int, zero bool) string {
	sw := w - 26
	if sw < 4 {
		sw = 4
	}
	vals := m.Spark[key]
	sp := ""
	if vals == nil && m.Status.Live {
		sp = m.Style.c(cDim, "")
	} else {
		sp = m.Style.c(cCyan, m.Style.Spark(vals, sw, zero))
	}
	return " " + pad(label, 13) + rpad(value, 10) + "  " + sp
}

func renderNode(m *Model) []string {
	var n *api.NodeView
	for i := range m.Nodes {
		if m.Nodes[i].Name == m.Node {
			n = &m.Nodes[i]
		}
	}
	lines := []string{m.header()}
	if n == nil {
		return append(lines, "node "+m.Node+" not found (Esc to go back)")
	}
	// algod's process start when known, as the web UI shows; else the first
	// good scrape.
	up := "-"
	if st := cmp.Or(n.StartTime, n.UpSince); st != 0 {
		up = Dur(m.Now.Sub(time.Unix(0, st)))
	}
	cfgChange := ""
	if n.ConfigChanged != nil {
		cfgChange = "  config: " + truncate(n.ConfigChanged.Msg, 40) + " (" + Dur(m.Now.Sub(time.Unix(0, n.ConfigChanged.Time))) + " ago)"
	}
	head := fmt.Sprintf("%s  round %s  lag %s  ver %s  up %s%s", m.Style.c(cBold, n.Name), roundText(float64(n.Round)), Secs(n.Lag), n.Version, up, cfgChange)
	lines = append(lines, head+"     "+m.rangeLabel())
	if m.Err != "" {
		lines = append(lines, m.Style.c(cRed, m.Err))
	} else if m.Message != "" {
		lines = append(lines, m.Style.c(cYellow, m.Message))
	} else {
		lines = append(lines, healthLines(m.Style, *n)...)
	}
	key := func(metric string) string { return n.Name + "|" + metric }
	colW := m.W
	two := m.W >= 110
	if two {
		colW = m.W / 2
	}
	cons := []string{m.Style.c(cBold, "CONSENSUS"),
		m.sparkRow("round time", fmt.Sprintf("%.2fs", n.RoundTime), key("rtime"), colW, false),
		m.sparkRow("p99 round", fmt.Sprintf("%.2fs", n.RoundP99), key("rtime_p99"), colW, false),
		m.sparkRow("txn/block", Num(n.TxnsPerBlk), key("tpb"), colW, true),
		m.sparkRow("pool", Num(n.PoolSize), key("pool"), colW, true),
		m.sparkRow("lag", Secs(n.Lag), key("lag"), colW, true),
	}
	netw := []string{m.Style.c(cBold, "NETWORK"),
		m.sparkRow("ws in/out", fmt.Sprintf("%d/%d", n.Peers.WSIn, n.Peers.WSOut), key("peers_ws"), colW, true),
		m.sparkRow("p2p in/out", fmt.Sprintf("%d/%d", n.Peers.P2PIn, n.Peers.P2POut), key("peers_p2p"), colW, true),
		m.sparkRow("idle peers", fmt.Sprint(n.IdlePeers), key("idle"), colW, true),
		m.sparkRow("evictions/s", "", key("evict"), colW, true),
		m.sparkRow("timeouts/s", "", key("timeouts"), colW, true),
	}
	// The method that measured wire bytes, such as sock_diag, is too long
	// for a row label, so the header names it.
	bwHead := m.Style.c(cBold, "BANDWIDTH (KB/s)")
	if n.NetMethod != "" {
		bwHead += m.Style.c(cDim, "  wire bytes from "+n.NetMethod)
	}
	bw := []string{bwHead,
		m.sparkRow("ws in/out", KBps(n.WSIn)+"/"+KBps(n.WSOut), key("bw_ws"), colW, true),
		m.sparkRow("p2p in/out", KBps(n.P2PIn)+"/"+KBps(n.P2POut), key("bw_p2p"), colW, true),
		m.sparkRow("wire in/out", KBps(n.NetIn)+"/"+KBps(n.NetOut), key("bw_wire"), colW, true),
		m.sparkRow("wire-app gap", KBps(n.NetIn+n.NetOut-(n.WSIn+n.WSOut+n.P2PIn+n.P2POut)), key("bw_gap"), colW, false),
	}
	diskFree := "-"
	if n.DiskTotal > 0 {
		diskFree = fmt.Sprintf("%s (%.0f%%)", Bytes(n.DiskFree), n.DiskFree/n.DiskTotal*100)
	}
	res := []string{m.Style.c(cBold, "RESOURCES"),
		m.sparkRow("cpu %", fmt.Sprintf("%.1f", n.CPU), key("cpu"), colW, true),
		m.sparkRow("rss", Bytes(n.RSS), key("rss"), colW, true),
		m.sparkRow("fds", handles(n.Handles), key("fds"), colW, true),
		m.sparkRow("disk free", diskFree, key("disk"), colW, false),
	}
	if n.ClockKnown {
		res = append(res, m.sparkRow("clock offset", fmt.Sprintf("%+.3fs", n.ClockOff), key("clock"), colW, false))
	}
	for _, u := range n.Unavailable {
		res = append(res, m.Style.c(cDim, " "+u+": not available on "+strings.SplitN(m.Status.Platform, "/", 2)[0]))
	}
	if two {
		lines = append(lines, joinCols(cons, netw, colW)...)
		lines = append(lines, joinCols(bw, res, colW)...)
	} else {
		lines = append(lines, cons...)
		lines = append(lines, netw...)
		lines = append(lines, bw...)
		lines = append(lines, res...)
	}
	lines = append(lines, "")
	room := m.H - len(lines) - 1
	m.paneTotal, m.paneRows = 0, 0
	switch m.Pane {
	case PanePeers:
		lines = append(lines, m.peersPane(n, room)...)
	case PaneEvents:
		lines = append(lines, m.Style.c(cBold, "EVENTS (e)")+"   p peers  l log templates")
		from, to, footer := m.window(len(m.NodeEv), room-1)
		// Newest first.
		for i := from; i < to; i++ {
			lines = append(lines, m.eventLine(m.NodeEv[len(m.NodeEv)-1-i], false))
		}
		if footer != "" {
			lines = append(lines, footer)
		}
	case PaneLogs:
		lines = append(lines, m.Style.c(cBold, "LOG TEMPLATES (l)")+"   p peers  e events")
		if m.Status.Live {
			lines = append(lines, m.Style.c(cDim, " log templates need the daemon"))
		}
		from, to, footer := m.window(len(m.Templ), room-1)
		for _, t := range m.Templ[from:to] {
			lvl := t.Level
			if lvl == "warning" {
				lvl = m.Style.c(cYellow, "warn")
			} else if lvl == "error" {
				lvl = m.Style.c(cRed, "error")
			}
			lines = append(lines, fmt.Sprintf(" %9s  %-6s %s", Num(t.Count), lvl, t.Template))
		}
		if footer != "" {
			lines = append(lines, footer)
		}
	}
	return lines
}

func handles(v float64) string {
	if v < 0 {
		return "n/a"
	}
	return Num(v)
}

func joinCols(a, b []string, w int) []string {
	n := max(len(a), len(b))
	out := make([]string, n)
	for i := range n {
		l, r := "", ""
		if i < len(a) {
			l = a[i]
		}
		if i < len(b) {
			r = b[i]
		}
		out[i] = pad(l, w) + r
	}
	return out
}

// Peer pane columns, sortable with < and >.
var peerCols = []string{"ADDR", "DIR", "NET", "CONNECTED", "RESETS 24h", "CLUSTER", "IN KB/s", "OUT KB/s", "TOTAL", "STREAMS"}

func (m *Model) peersPane(n *api.NodeView, room int) []string {
	hdr := m.Style.c(cBold, "PEERS (p)") + fmt.Sprintf("  %d connected", len(n.PeerList))
	if n.PeerUsage {
		hdr += ", per-peer usage on"
	}
	hdr += "   e events  l log templates  < > sort"
	lines := []string{hdr}
	if !n.HasAdmin {
		return append(lines, m.Style.c(cDim, " the peer list needs the admin token"))
	}
	hist := map[string]api.PeerHistory{}
	for _, h := range m.PeerHist {
		hist[h.IP] = h
	}
	peers := append([]collector.PeerInfo(nil), n.PeerList...)
	sort.SliceStable(peers, func(i, j int) bool {
		a, b := peers[i], peers[j]
		switch m.PeerSort {
		case 1:
			return a.Direction < b.Direction
		case 2:
			return a.Network < b.Network
		case 3:
			return a.Since < b.Since
		case 4:
			return hist[a.IP].Resets > hist[b.IP].Resets
		case 5:
			return len(hist[a.IP].Clusters) > len(hist[b.IP].Clusters)
		case 6:
			return a.RateIn > b.RateIn
		case 7:
			return a.RateOut > b.RateOut
		case 8:
			return a.BytesIn+a.BytesOut > b.BytesIn+b.BytesOut
		}
		return a.Addr < b.Addr
	})
	widths := []int{38, 4, 5, 11, 11, 8, 9, 9, 12, 0}
	var h strings.Builder
	for i, c := range peerCols {
		if i == m.PeerSort {
			c += "*"
		}
		if widths[i] == 0 {
			h.WriteString(c)
		} else {
			h.WriteString(pad(c, widths[i]))
		}
	}
	lines = append(lines, m.Style.c(cBold, h.String()))
	from, to, footer := m.window(len(peers), room+1-len(lines))
	for _, p := range peers[from:to] {
		ph := hist[p.IP]
		cl := "-"
		if len(ph.Clusters) > 0 {
			cl = fmt.Sprint(len(ph.Clusters))
		}
		in, out, total := "-", "-", "-"
		if p.HasUsage {
			if !p.RateUnknown {
				in, out = KBps(p.RateIn), KBps(p.RateOut)
			}
			total = Bytes(float64(p.BytesIn)) + "/" + Bytes(float64(p.BytesOut))
		}
		col := cCyan
		if p.Direction == "out" {
			col = cOrange
		}
		since := Dur(m.Now.Sub(time.Unix(0, p.Since)))
		if p.SinceBefore {
			since += "+"
		}
		cells := []string{p.Addr, m.Style.c(col, p.Direction), p.Network, since, fmt.Sprint(ph.Resets), cl, in, out, total, strings.Join(p.Streams, ",")}
		var b strings.Builder
		for i, c := range cells {
			if widths[i] == 0 {
				b.WriteString(c)
			} else {
				b.WriteString(pad(c, widths[i]))
			}
		}
		lines = append(lines, b.String())
	}
	if footer != "" {
		lines = append(lines, footer)
	}
	return lines
}

// HelpKeys is the key table, shared with ? and --help.
var HelpKeys = [][2]string{
	{"Up, Down, Enter, Esc", "Select a node, open it, go back"},
	{"PgUp, PgDn, Home, End", "On a node, scroll the open pane; Up and Down scroll too"},
	{"<, >", "Change the sort column"},
	{"p, e, l", "Peers, events or log templates pane"},
	{"r", "Cycle the range: 1h, 6h, 24h, 7d, 30d, 1y, all"},
	{"f", "Toggle full resolution (tier=raw)"},
	{"[, ]", "Move the time cursor back or forward one bucket"},
	{"t", "Jump to a time, for example 2026-11-03 09:30"},
	{"Space", "Pause or resume live updates"},
	{"a", "Add an annotation at the cursor"},
	{"s", "Add a silence for the selected node"},
	{"/", "Filter rows"},
	{"W", "Write discovered nodes to a config file, after showing it"},
	{"?", "Help"},
	{"q", "Quit"},
}

func renderHelp(m *Model) []string {
	lines := []string{m.header(), "", m.Style.c(cBold, "KEYS")}
	for _, k := range HelpKeys {
		lines = append(lines, "  "+pad(k[0], 24)+k[1])
	}
	lines = append(lines, "", "The time cursor turns the screen into a view of the past: every value shows the state at that moment.",
		"Press any key to go back.")
	return lines
}

// EmptyHelp is shown when no daemon, config or node was found.
var EmptyHelp = []string{
	"algod.top found no daemon, no config file and no algod data directory.",
	"",
	"Point it at a node:",
	"  algod.top -d ~/.mainnet                 a local data directory",
	"  algod.top -u http://127.0.0.1:8080      an algod REST URL (token from $ALGOD_TOKEN)",
	"  algod.top -u ssh://relay1/~/.mainnet    a remote node over SSH, nothing installed",
	"  algod.top -H ssh://hub                  the terminal UI of a daemon on another host",
	"",
	"Keep history with the daemon:",
	"  algod.top daemon --discover             find local nodes and write them to the config",
	"  algod.top daemon                        run the collector, store and web UI",
	"",
	"Press q to quit.",
}

func renderEmpty(m *Model) []string {
	lines := []string{m.header(), ""}
	return append(lines, EmptyHelp...)
}

func renderWrite(m *Model) []string {
	lines := []string{m.header(), "", m.Style.c(cBold, "Write these nodes to the config file? (y/n)"), ""}
	return append(lines, m.WriteText...)
}

// fitColumns widens NODE for long names such as relay1/mainnet.
func (m *Model) fitColumns() {
	w := 12
	for _, n := range m.Nodes {
		w = max(w, min(runeLen(n.Name)+2, 28))
	}
	columns[0].Width = w
}
