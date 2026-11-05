package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"algod.top/internal/api"
	"algod.top/internal/model"
)

// Options configure the interactive UI.
type Options struct {
	Source   Source
	Style    Style
	UTC      bool
	Interval time.Duration
	// WriteConfig renders and writes discovered nodes, for the W key.
	WriteConfig func() (text []string, write func() (string, error))
	Empty       bool // nothing found: show the help screen
}

// key events
const (
	kUp = iota + 1000
	kDown
	kLeft
	kRight
	kEnter
	kEsc
	kBackspace
	kCtrlC
)

// App is the running terminal UI.
type App struct {
	o        Options
	m        Model
	mu       sync.Mutex
	prev     []string
	out      *bufio.Writer
	rings    map[string]*ring
	sparkAt  time.Time
	sparkFor string
	writeFn  func() (string, error)
}

type ring struct{ v []float64 }

func (r *ring) add(x float64, n int) {
	r.v = append(r.v, x)
	if len(r.v) > n {
		r.v = r.v[len(r.v)-n:]
	}
}

// DetectStyle returns colour and Unicode support for the terminal.
func DetectStyle() Style {
	s := Style{Color: os.Getenv("NO_COLOR") == "", Unicode: true}
	if os.Getenv("TERM") == "dumb" {
		s.Color = false
	}
	lang := strings.ToUpper(os.Getenv("LC_ALL") + os.Getenv("LC_CTYPE") + os.Getenv("LANG"))
	if lang != "" && !strings.Contains(lang, "UTF-8") && !strings.Contains(lang, "UTF8") {
		s.Unicode = false
	}
	return s
}

// Run starts the interactive UI on the terminal and returns on quit.
func Run(o Options) (err error) {
	if o.Interval <= 0 {
		o.Interval = 2 * time.Second
	}
	in, out := os.Stdin, os.Stdout
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal; use -b for batch mode")
	}
	enableVT(out)
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	restore := func() {
		fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
		term.Restore(fd, state)
	}
	defer func() {
		restore()
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	// Restore the terminal on interrupt as well.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l\x1b[2J")
	a := &App{o: o, out: bufio.NewWriterSize(out, 64*1024), rings: map[string]*ring{}}
	a.m.Style = o.Style
	a.m.UTC = o.UTC
	a.m.Spark = map[string][]float64{}
	a.m.At = map[string]Point{}
	if o.Empty {
		a.m.Screen = ScreenEmpty
	}
	keys := make(chan int, 16)
	go readKeys(in, keys)
	resize, stopResize := watchResize()
	defer stopResize()
	tick := time.NewTicker(o.Interval)
	defer tick.Stop()
	a.refresh()
	a.draw(true)
	for {
		select {
		case <-sig:
			return nil
		case <-resize:
			a.draw(true)
		case <-tick.C:
			if !a.m.Paused && a.m.Cursor == 0 {
				a.refresh()
			}
			a.draw(false)
		case k := <-keys:
			if quit := a.key(k); quit {
				return nil
			}
			a.draw(false)
		}
	}
}

func readKeys(r io.Reader, ch chan<- int) {
	br := bufio.NewReader(r)
	for {
		b, err := br.ReadByte()
		if err != nil {
			close(ch)
			return
		}
		switch b {
		case 3:
			ch <- kCtrlC
		case '\r', '\n':
			ch <- kEnter
		case 127, 8:
			ch <- kBackspace
		case 27:
			// An escape sequence arrives in one read; a lone Esc does not.
			if br.Buffered() == 0 {
				time.Sleep(30 * time.Millisecond)
			}
			if br.Buffered() == 0 {
				ch <- kEsc
				continue
			}
			b2, _ := br.ReadByte()
			if b2 != '[' && b2 != 'O' {
				ch <- kEsc
				continue
			}
			b3, _ := br.ReadByte()
			switch b3 {
			case 'A':
				ch <- kUp
			case 'B':
				ch <- kDown
			case 'C':
				ch <- kRight
			case 'D':
				ch <- kLeft
			default:
				// Consume the rest of a longer sequence.
				for br.Buffered() > 0 {
					c, _ := br.ReadByte()
					if c >= 0x40 && c <= 0x7e {
						break
					}
				}
			}
		default:
			ch <- int(b)
		}
	}
}

func (a *App) size() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 120, 40
	}
	return w, h
}

// draw redraws only the lines that changed, so it stays cheap over a slow
// SSH link.
func (a *App) draw(full bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, h := a.size()
	if w != a.m.W || h != a.m.H {
		full = true
	}
	a.m.W, a.m.H = w, h
	a.m.Now = time.Now()
	lines := Render(&a.m)
	if full {
		a.prev = nil
		a.out.WriteString("\x1b[2J")
	}
	for i, l := range lines {
		if i < len(a.prev) && a.prev[i] == l {
			continue
		}
		fmt.Fprintf(a.out, "\x1b[%d;1H%s\x1b[K", i+1, l)
	}
	a.prev = lines
	a.out.Flush()
}

func (a *App) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// refresh polls the source for live status.
func (a *App) refresh() {
	ctx, cancel := a.ctx()
	defer cancel()
	nodes, st, err := a.o.Source.Nodes(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.m.Err = "source: " + err.Error()
		return
	}
	a.m.Err = ""
	a.m.Nodes, a.m.Status = nodes, st
	if a.m.Sel >= len(nodes) {
		a.m.Sel = max(0, len(nodes)-1)
	}
	now := time.Now()
	evs, err := a.o.Source.Events(ctx, "", now.Add(-24*time.Hour).UnixNano(), now.Add(time.Minute).UnixNano(), 200)
	if err == nil {
		var keep []model.Event
		for _, e := range evs {
			if e.Kind != model.EvConnect {
				keep = append(keep, e)
			}
		}
		a.m.Events = keep
	}
	// Live sparklines: rings filled from each poll.
	for _, n := range nodes {
		a.push(n.Name+"|rtime", n.RoundTime)
		a.push(n.Name+"|rtime_p99", n.RoundP99)
		a.push(n.Name+"|tpb", n.TxnsPerBlk)
		a.push(n.Name+"|pool", n.PoolSize)
		a.push(n.Name+"|lag", n.Lag)
		a.push(n.Name+"|peers_ws", float64(n.Peers.WSIn+n.Peers.WSOut))
		a.push(n.Name+"|peers_p2p", float64(n.Peers.P2PIn+n.Peers.P2POut))
		a.push(n.Name+"|idle", float64(n.IdlePeers))
		a.push(n.Name+"|bw_ws", n.WSIn+n.WSOut)
		a.push(n.Name+"|bw_p2p", n.P2PIn+n.P2POut)
		a.push(n.Name+"|bw_wire", n.NetIn+n.NetOut)
		a.push(n.Name+"|bw_gap", n.NetIn+n.NetOut-(n.WSIn+n.WSOut+n.P2PIn+n.P2POut))
		a.push(n.Name+"|cpu", n.CPU)
		a.push(n.Name+"|rss", n.RSS)
		a.push(n.Name+"|fds", n.Handles)
		a.push(n.Name+"|disk", n.DiskFree)
		a.push(n.Name+"|clock", n.ClockOff)
	}
	if st.Live {
		for k, r := range a.rings {
			a.m.Spark[k] = r.v
		}
	}
	if a.m.Screen == ScreenNode {
		a.refreshNodeLocked(ctx, false)
	}
}

func (a *App) push(k string, v float64) {
	r := a.rings[k]
	if r == nil {
		r = &ring{}
		a.rings[k] = r
	}
	r.add(v, 400)
}

// span returns the range and the sparkline bucket count.
func (a *App) span() (from, to int64, points int) {
	to = time.Now().UnixNano()
	if a.m.Cursor != 0 {
		to = a.m.Cursor
	}
	from = to - int64(Ranges[a.m.RangeIdx].D)
	points = max(10, a.sparkWidth())
	return
}

func (a *App) sparkWidth() int {
	w := a.m.W
	if w >= 110 {
		w /= 2
	}
	return max(4, w-26)
}

// refreshNodeLocked loads the node screen's history: sparklines, peers
// history, events and templates. Sparklines refresh every 15 s.
func (a *App) refreshNodeLocked(ctx context.Context, force bool) {
	n := a.m.Node
	// The peer list is not part of the node list; fetch it for this node.
	if peers, err := a.o.Source.Peers(ctx, n); err == nil {
		for i := range a.m.Nodes {
			if a.m.Nodes[i].Name == n {
				a.m.Nodes[i].PeerList = peers
			}
		}
	}
	from, to, pts := a.span()
	evs, err := a.o.Source.Events(ctx, n, from, to, 200)
	if err == nil {
		a.m.NodeEv = evs
	}
	if a.o.Source.Live() {
		return
	}
	if !force && time.Since(a.sparkAt) < 15*time.Second && a.sparkFor == n {
		return
	}
	a.sparkAt, a.sparkFor = time.Now(), n
	q := func(key, metric, fn string, match ...string) []float64 {
		s, err := a.o.Source.Series(ctx, Query{Node: n, Metric: metric, Fn: fn, Match: match, From: from, To: to, Points: pts, Raw: a.m.FullRes})
		if err != nil {
			return nil
		}
		v := s.Sum()
		a.m.Spark[n+"|"+key] = v
		return v
	}
	q("rtime", "algodtop_round_time_seconds", "avg")
	q("rtime_p99", "algodtop_round_time_seconds", "max")
	q("tpb", "algodtop_block_txns", "avg")
	q("pool", "algodtop_txpool_size", "avg")
	q("lag", "algodtop_sync_lag_seconds", "max")
	q("peers_ws", "algodtop_peers", "avg", "net:ws")
	q("peers_p2p", "algodtop_peers", "avg", "net:p2p")
	q("idle", "algodtop_peers_idle", "avg")
	q("evict", "algod_network_p2p_inbound_evictions_total", "rate")
	q("timeouts", "algod_network_p2p_stream_setup_timeouts_total", "rate")
	ws := addVec(q("x_wsin", "algod_network_received_bytes_total", "rate"), q("x_wsout", "algod_network_sent_bytes_total", "rate"))
	p2p := addVec(q("x_p2pin", "algod_network_p2p_received_bytes_total", "rate"), q("x_p2pout", "algod_network_p2p_sent_bytes_total", "rate"))
	wire := q("bw_wire", "proc_net_bytes_total", "rate")
	a.m.Spark[n+"|bw_ws"] = ws
	a.m.Spark[n+"|bw_p2p"] = p2p
	if wire != nil {
		a.m.Spark[n+"|bw_gap"] = subVec(wire, addVec(ws, p2p))
	}
	if cpu := q("cpu", "proc_cpu_seconds_total", "rate"); cpu != nil {
		for i := range cpu {
			cpu[i] *= 100
		}
	}
	q("rss", "proc_resident_bytes", "avg")
	q("fds", "proc_open_handles", "avg")
	q("disk", "host_disk_free_bytes", "min")
	q("clock", "host_clock_offset_seconds", "avg")
	if hist, err := a.o.Source.PeerHistory(ctx, n, time.Now().Add(-24*time.Hour).UnixNano(), time.Now().UnixNano()); err == nil {
		a.m.PeerHist = hist
	}
	if t, err := a.o.Source.Templates(ctx, n, from, to); err == nil {
		a.m.Templ = t
	}
}

func addVec(a, b []float64) []float64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := make([]float64, max(len(a), len(b)))
	for i := range out {
		x, y := math.NaN(), math.NaN()
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		switch {
		case math.IsNaN(x):
			out[i] = y
		case math.IsNaN(y):
			out[i] = x
		default:
			out[i] = x + y
		}
	}
	return out
}

func subVec(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = math.NaN()
		if i < len(b) && !math.IsNaN(a[i]) && !math.IsNaN(b[i]) {
			out[i] = a[i] - b[i]
		}
	}
	return out
}

// loadAt fetches every node's values at the time cursor.
func (a *App) loadAt() {
	if a.m.Cursor == 0 || a.o.Source.Live() {
		return
	}
	ctx, cancel := a.ctx()
	defer cancel()
	_, _, pts := a.span()
	step := int64(Ranges[a.m.RangeIdx].D) / int64(pts)
	to := a.m.Cursor
	from := to - max(step, int64(30*time.Second))
	at := map[string]Point{}
	for _, n := range a.m.Nodes {
		last := func(metric, fn string, match ...string) float64 {
			s, err := a.o.Source.Series(ctx, Query{Node: n.Name, Metric: metric, Fn: fn, Match: match, From: from, To: to, Points: 1})
			if err != nil {
				return math.NaN()
			}
			v := s.Sum()
			if len(v) == 0 {
				return math.NaN()
			}
			return v[len(v)-1]
		}
		p := Point{
			Round:  last("algodtop_round", "last"),
			Lag:    last("algodtop_sync_lag_seconds", "avg"),
			RTime:  last("algodtop_round_time_seconds", "avg"),
			CPU:    last("proc_cpu_seconds_total", "rate") * 100,
			RSS:    last("proc_resident_bytes", "avg"),
			NetIn:  last("proc_net_bytes_total", "rate", "dir:in"),
			NetOut: last("proc_net_bytes_total", "rate", "dir:out"),
			Up:     last("algodtop_up", "min"),
		}
		ci := func(v float64) int {
			if math.IsNaN(v) {
				return 0
			}
			return int(math.Round(v))
		}
		p.Peers.WSIn = ci(last("algodtop_peers", "avg", "net:ws", "dir:in"))
		p.Peers.WSOut = ci(last("algodtop_peers", "avg", "net:ws", "dir:out"))
		p.Peers.P2PIn = ci(last("algodtop_peers", "avg", "net:p2p", "dir:in"))
		p.Peers.P2POut = ci(last("algodtop_peers", "avg", "net:p2p", "dir:out"))
		at[n.Name] = p
	}
	evs, err := a.o.Source.Events(ctx, "", to-int64(24*time.Hour), to, 200)
	a.mu.Lock()
	a.m.At = at
	if err == nil {
		a.m.Events = evs
	}
	a.mu.Unlock()
}

// key handles one key press. It returns true to quit.
func (a *App) key(k int) bool {
	m := &a.m
	m.Message = ""
	if m.Prompt != "" {
		return a.promptKey(k)
	}
	switch m.Screen {
	case ScreenHelp:
		m.Screen = ScreenMain
		return false
	case ScreenEmpty:
		return k == 'q' || k == kCtrlC || k == kEsc
	case ScreenWrite:
		if k == 'y' && a.writeFn != nil {
			p, err := a.writeFn()
			if err != nil {
				m.Err = err.Error()
			} else {
				m.Message = "wrote " + p + "; start the daemon with: algodtop daemon"
			}
		}
		m.Screen = ScreenMain
		return false
	}
	switch k {
	case 'q', kCtrlC:
		return true
	case '?', 'h':
		m.Screen = ScreenHelp
	case kUp, 'k':
		if m.Sel > 0 {
			m.Sel--
		}
	case kDown, 'j':
		if m.Sel < len(m.VisibleNodes())-1 {
			m.Sel++
		}
	case kEnter:
		if n := m.selected(); n != nil && m.Screen == ScreenMain {
			m.Screen, m.Node = ScreenNode, n.Name
			ctx, cancel := a.ctx()
			a.refreshNodeLocked(ctx, true)
			cancel()
		}
	case kEsc:
		m.Screen = ScreenMain
	case 'p':
		m.Pane = PanePeers
	case 'e':
		m.Pane = PaneEvents
	case 'l':
		m.Pane = PaneLogs
	case '<', kLeft:
		if m.Screen == ScreenNode && m.Pane == PanePeers {
			m.PeerSort = (m.PeerSort + len(peerCols) - 1) % len(peerCols)
		} else {
			m.SortCol = (m.SortCol + len(columns) - 1) % len(columns)
		}
	case '>', kRight:
		if m.Screen == ScreenNode && m.Pane == PanePeers {
			m.PeerSort = (m.PeerSort + 1) % len(peerCols)
		} else {
			m.SortCol = (m.SortCol + 1) % len(columns)
		}
	case 'r':
		m.RangeIdx = (m.RangeIdx + 1) % len(Ranges)
		a.rangeChanged()
	case 'f':
		m.FullRes = !m.FullRes
		a.rangeChanged()
	case ' ':
		m.Paused = !m.Paused
	case '[', ']':
		if a.o.Source.Live() {
			m.Message = "the time cursor needs the daemon (no history in live-only mode)"
			break
		}
		_, _, pts := a.span()
		step := int64(Ranges[m.RangeIdx].D) / int64(pts)
		now := time.Now().UnixNano()
		if m.Cursor == 0 {
			m.Cursor = now
		}
		if k == '[' {
			m.Cursor -= step
		} else {
			m.Cursor += step
		}
		if m.Cursor >= now {
			m.Cursor = 0
			a.refresh()
		} else {
			a.loadAt()
		}
		a.rangeChanged()
	case 't':
		m.Prompt, m.Input = "jump to time (YYYY-MM-DD HH:MM, or empty for live):", ""
	case 'a':
		m.Prompt, m.Input = "annotation for "+a.target()+":", ""
	case 's':
		m.Prompt, m.Input = "silence "+a.target()+" for (e.g. 10m restart for run 4):", ""
	case '/':
		m.Prompt, m.Input = "filter:", m.Filter
	case 'W':
		if a.o.WriteConfig == nil {
			m.Message = "nothing to write: the nodes already come from a daemon or config file"
			break
		}
		text, fn := a.o.WriteConfig()
		m.WriteText, a.writeFn = text, fn
		m.Screen = ScreenWrite
	}
	return false
}

func (a *App) target() string {
	if a.m.Screen == ScreenNode {
		return a.m.Node
	}
	if n := a.m.selected(); n != nil {
		return n.Name
	}
	return "all nodes"
}

func (a *App) rangeChanged() {
	if a.m.Screen == ScreenNode {
		ctx, cancel := a.ctx()
		a.refreshNodeLocked(ctx, true)
		cancel()
	}
}

func (a *App) promptKey(k int) bool {
	m := &a.m
	switch k {
	case kEsc, kCtrlC:
		m.Prompt = ""
	case kBackspace:
		if len(m.Input) > 0 {
			m.Input = m.Input[:len(m.Input)-1]
		}
	case kEnter:
		label, in := m.Prompt, strings.TrimSpace(m.Input)
		m.Prompt = ""
		a.submit(label, in)
	default:
		if k >= 32 && k < 127 {
			m.Input += string(rune(k))
		}
	}
	return false
}

func (a *App) submit(label, in string) {
	m := &a.m
	ctx, cancel := a.ctx()
	defer cancel()
	switch {
	case strings.HasPrefix(label, "filter"):
		m.Filter = in
		m.Sel = 0
	case strings.HasPrefix(label, "jump"):
		if in == "" {
			m.Cursor = 0
			a.refresh()
			return
		}
		t, err := parseLocalTime(in, m.UTC)
		if err != nil {
			m.Err = err.Error()
			return
		}
		m.Cursor = t.UnixNano()
		a.loadAt()
		a.rangeChanged()
	case strings.HasPrefix(label, "annotation"):
		if in == "" {
			return
		}
		at := time.Now().UnixNano()
		if m.Cursor != 0 {
			at = m.Cursor
		}
		node := a.target()
		if node == "all nodes" {
			node = ""
		}
		if err := a.o.Source.AddAnnotation(ctx, model.Annotation{Node: node, Start: at, Text: in}, ""); err != nil {
			m.Err = "annotation: " + err.Error()
			return
		}
		m.Message = "annotation added"
	case strings.HasPrefix(label, "silence"):
		f := strings.Fields(in)
		if len(f) == 0 {
			return
		}
		d, err := time.ParseDuration(f[0])
		if err != nil {
			m.Err = "silence: " + err.Error()
			return
		}
		node := a.target()
		var nodes []string
		if node != "all nodes" {
			nodes = []string{node}
		}
		if err := a.o.Source.AddSilence(ctx, nodes, d, strings.Join(f[1:], " ")); err != nil {
			m.Err = "silence: " + err.Error()
			return
		}
		m.Message = fmt.Sprintf("silenced %s for %s", node, d)
	}
}

func parseLocalTime(s string, utc bool) (time.Time, error) {
	loc := time.Local
	if utc {
		loc = time.UTC
	}
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02", "15:04"} {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			if l == "15:04" {
				now := time.Now().In(loc)
				t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q; use YYYY-MM-DD HH:MM", s)
}

var _ = api.NodeView{}
