package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"algod.top/internal/fsx"
	"algod.top/internal/logparse"
	"algod.top/internal/model"
)

// LogOptions configure log ingestion.
type LogOptions struct {
	Disable      bool
	Backfill     bool  // read the existing node.log and archive on first start
	KeepRawInfo  bool  // keep info lines raw too
	TemplateCap  int   // templates per node, default 2000
	FloodCap     int   // app IDs per flood counter, default 100
	BackfillRate int64 // bytes per second, default 10 MB/s
}

// tailState is persisted so a restart resumes where it stopped.
type tailState struct {
	Path      string `json:"path"`
	Offset    int64  `json:"offset"`
	FirstHash string `json:"first_hash"`
	Started   bool   `json:"started"`
}

// tailer follows node.log. The stdlib has no inotify, kqueue or
// ReadDirectoryChangesW wrapper, so it polls: new bytes every 250 ms and a
// stat of the path every second.
type tailer struct {
	mu     sync.Mutex // the driver ticks and the node saves state concurrently
	n      *Node
	st     tailState
	f      *os.File
	fi     os.FileInfo
	buf    []byte // a partial line carried to the next read
	read   []byte // reused read buffer
	counts *logCounts
	resets map[string]time.Time // address -> last "connection reset" line
}

func newTailer(n *Node, saved *tailState) *tailer {
	t := &tailer{n: n, resets: map[string]time.Time{}}
	if saved != nil {
		t.st = *saved
	}
	capT, capF := n.o.Logs.TemplateCap, n.o.Logs.FloodCap
	if capT <= 0 {
		capT = 2000
	}
	if capF <= 0 {
		capF = 100
	}
	t.counts = newLogCounts(capT, capF)
	return t
}

func (t *tailer) state() tailState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st
}

func firstLineHash(f *os.File) string {
	b := make([]byte, 512)
	n, _ := f.ReadAt(b, 0)
	b = b[:n]
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

func (t *tailer) paths() (live, archive string) {
	t.n.mu.RLock()
	f := t.n.files
	t.n.mu.RUnlock()
	if f == nil {
		return "", ""
	}
	return f.LogPaths.Live, f.LogPaths.NewestArchive()
}

func (t *tailer) run(ctx context.Context) {
	// Resume, and backfill on a first start, on this node's goroutine, so a
	// long backfill never holds up the other nodes.
	t.mu.Lock()
	t.resume()
	t.mu.Unlock()
	tails.add(t)
	<-ctx.Done()
	tails.remove(t)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.drain()
	t.flushCounts(t.n.o.Now(), true)
	if t.f != nil {
		t.f.Close()
	}
}

// tick reads new bytes every 250 ms, stats the path every second, and
// closes the minute every 15 s when no lines arrive.
func (t *tailer) tick(i int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.drain()
	if i%4 == 0 {
		t.checkRotation()
	}
	if i%60 == 0 {
		t.flushCounts(t.n.o.Now(), false)
	}
}

// tailDriver runs every node's tailer from one 250 ms ticker. Five
// tailers with their own tickers woke the process 20 times a second.
type tailDriver struct {
	mu   sync.Mutex
	set  map[*tailer]bool
	stop chan struct{}
}

var tails = &tailDriver{set: map[*tailer]bool{}}

func (d *tailDriver) add(t *tailer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.set[t] = true
	if d.stop == nil {
		d.stop = make(chan struct{})
		go d.loop(d.stop)
	}
}

func (d *tailDriver) remove(t *tailer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.set, t)
	if len(d.set) == 0 && d.stop != nil {
		close(d.stop)
		d.stop = nil
	}
}

func (d *tailDriver) loop(stop chan struct{}) {
	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	for i := 1; ; i++ {
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		d.mu.Lock()
		list := make([]*tailer, 0, len(d.set))
		for t := range d.set {
			list = append(list, t)
		}
		d.mu.Unlock()
		for _, t := range list {
			t.tick(i)
		}
	}
}

// resume opens node.log at the saved offset. If the file rotated while the
// daemon was down, it first reads the tail of the archive.
func (t *tailer) resume() {
	live, archive := t.paths()
	if live == "" {
		return
	}
	f, err := fsx.OpenShared(live)
	if err != nil {
		return
	}
	fi, _ := f.Stat()
	h := firstLineHash(f)
	switch {
	case t.st.Started && t.st.FirstHash == h && h != "":
		// Same file: resume, unless it was truncated.
		if fi.Size() < t.st.Offset {
			t.st.Offset = 0
		}
	case t.st.Started:
		// node.log rotated while we were down. Finish the archive first.
		if archive != "" {
			if af, err := fsx.OpenShared(archive); err == nil {
				if firstLineHash(af) == t.st.FirstHash {
					af.Seek(t.st.Offset, io.SeekStart)
					t.f = af
					t.drainAll(false)
					t.n.event(model.EvLogRotation, model.SevInfo, "node.log rotated while algod.top was down; read the rest of the archive", nil)
				}
				af.Close()
				t.f = nil
			}
		}
		t.st.Offset = 0
	default:
		// First start.
		if t.n.o.Logs.Backfill {
			if archive != "" {
				if af, err := fsx.OpenShared(archive); err == nil {
					t.f = af
					t.drainAll(true)
					af.Close()
					t.f = nil
				}
			}
			t.st.Offset = 0
		} else {
			t.st.Offset = fi.Size()
		}
	}
	f.Seek(t.st.Offset, io.SeekStart)
	t.f, t.fi = f, fi
	t.st.Path = live
	t.st.FirstHash = h
	t.st.Started = true
	t.buf = t.buf[:0]
}

// drainAll reads a whole file to EOF, rate limited when backfilling.
func (t *tailer) drainAll(limited bool) {
	rate := t.n.o.Logs.BackfillRate
	if rate <= 0 {
		rate = 10 << 20
	}
	for {
		n := t.readChunk(4 << 20)
		if n == 0 {
			break
		}
		if limited {
			time.Sleep(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
		}
	}
	t.buf = t.buf[:0]
}

func (t *tailer) drain() {
	if t.f == nil {
		t.resume()
		if t.f == nil {
			return
		}
	}
	// A short read means the end of the file, so the usual tick costs one
	// read rather than a second one that returns nothing.
	for t.readChunk(1<<20) == 1<<20 {
	}
}

// readChunk reads up to max bytes and parses complete lines.
func (t *tailer) readChunk(maxBytes int) int {
	// One buffer is reused for every read: a new one each 250 ms per
	// node was most of the daemon's garbage.
	if cap(t.read) < maxBytes {
		t.read = make([]byte, maxBytes)
	}
	chunk := t.read[:maxBytes]
	n, _ := t.f.Read(chunk)
	if n <= 0 {
		return 0
	}
	t.st.Offset += int64(n)
	data := chunk[:n]
	// Finish the line carried from the last read, then parse lines in place
	// in the read buffer. Only the trailing partial line is copied.
	if len(t.buf) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.buf = append(t.buf, data...)
			if len(t.buf) > 1<<20 {
				t.buf = t.buf[:0] // a runaway line without newline
			}
			return n
		}
		t.buf = append(t.buf, data[:i]...)
		t.line(t.buf)
		t.buf = t.buf[:0]
		data = data[i+1:]
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		t.line(data[:i])
		data = data[i+1:]
	}
	if len(data) <= 1<<20 {
		t.buf = append(t.buf[:0], data...)
	}
	return n
}

// checkRotation notices when the path points to a different file. It
// reads the old file to EOF, then opens the new node.log from offset 0.
func (t *tailer) checkRotation() {
	live, _ := t.paths()
	if live == "" || t.f == nil {
		return
	}
	pfi, err := os.Stat(live)
	if err != nil {
		return
	}
	cur, err := t.f.Stat()
	if err != nil {
		return
	}
	if os.SameFile(cur, pfi) && live == t.st.Path {
		if pfi.Size() < t.st.Offset {
			t.n.event(model.EvLogRotation, model.SevInfo, "node.log was truncated", nil)
			t.f.Seek(0, io.SeekStart)
			t.st.Offset = 0
			t.buf = t.buf[:0]
		}
		return
	}
	t.drain()
	t.f.Close()
	f, err := fsx.OpenShared(live)
	if err != nil {
		t.f = nil
		return
	}
	t.f = f
	t.fi = pfi
	t.st = tailState{Path: live, Offset: 0, FirstHash: firstLineHash(f), Started: true}
	t.buf = t.buf[:0]
	t.n.event(model.EvLogRotation, model.SevInfo, "node.log rotated", nil)
}

func (t *tailer) line(b []byte) {
	l := logparse.Parse(b)
	if l.Time.IsZero() {
		l.Time = t.n.o.Now()
	}
	// A new hash for the first line keeps resume correct after the file
	// gets its first bytes.
	if t.st.FirstHash == "" && t.f != nil {
		t.st.FirstHash = firstLineHash(t.f)
	}
	t.counts.add(&l)
	t.flushCounts(l.Time, false)
	switch l.Kind {
	case logparse.KindTemplate:
		if l.Level == "warning" || l.Level == "error" || l.Level == "fatal" || l.Level == "panic" || t.n.o.Logs.KeepRawInfo {
			t.n.o.Sink.LogLine(LogLine{Time: l.Time.UnixNano(), Node: t.n.o.Name, Level: l.Level, Template: l.Template, Line: string(b)})
		}
		if l.Extract != nil {
			t.extract(&l)
		}
	}
}

func (t *tailer) extract(l *logparse.Line) {
	e := l.Extract
	n := t.n
	switch e.Type {
	case "connect":
		ip := firstNonEmpty(e.IP, IPOf(e.Addr))
		n.sess.logConnect(l.Time, e.Addr, ip, e.Incoming)
		n.o.Sink.Event(model.Event{Time: l.Time.UnixNano(), Node: n.o.Name, Kind: model.EvConnect, Severity: model.SevInfo,
			Msg:   fmt.Sprintf("connect %s %s %s", dirWord(e.Incoming), NetOf(e.Addr), ip),
			Attrs: map[string]any{"addr": NormalizeAddr(e.Addr), "ip": ip, "direction": dirWord(e.Incoming), "network": NetOf(e.Addr)}})
	case "disconnect":
		ip := firstNonEmpty(e.IP, IPOf(e.Addr))
		s := n.sess.logDisconnect(l.Time, e.Addr, ip, e.Incoming, e.Reason)
		attrs := map[string]any{"addr": NormalizeAddr(e.Addr), "ip": ip, "direction": dirWord(e.Incoming), "network": NetOf(e.Addr),
			"reason": e.Reason, "message_delay": e.Delay, "start": s.Start}
		msg := fmt.Sprintf("disconnect %s %s %s reason=%s", dirWord(e.Incoming), NetOf(e.Addr), ip, e.Reason)
		if at, ok := t.resets[NormalizeAddr(e.Addr)]; ok && l.Time.Sub(at) < 5*time.Second {
			attrs["reset"] = true
			msg += " (connection reset)"
			delete(t.resets, NormalizeAddr(e.Addr))
		}
		n.o.Sink.Event(model.Event{Time: l.Time.UnixNano(), Node: n.o.Name, Kind: model.EvDisconnect, Severity: model.SevInfo, Msg: msg, Attrs: attrs})
	case "outgoing", "incoming":
		if e.Remote != "" {
			n.sess.logConnect(l.Time, e.Remote, "", e.Incoming)
		}
	case "conn-reset":
		if e.Addr != "" {
			t.resets[NormalizeAddr(e.Addr)] = l.Time
			if len(t.resets) > 1000 {
				for k, v := range t.resets {
					if l.Time.Sub(v) > time.Minute {
						delete(t.resets, k)
					}
				}
			}
		}
	case "catchup-start":
		n.noteCatchup(true)
	case "catchup-end":
		n.noteCatchup(false)
	case "panic", "fatal":
		n.o.Sink.Event(model.Event{Time: l.Time.UnixNano(), Node: n.o.Name, Kind: model.EvLogError, Severity: model.SevCritical, Msg: e.Type + ": " + trunc(e.Message, 200)})
	case "ledger-error":
		n.o.Sink.Event(model.Event{Time: l.Time.UnixNano(), Node: n.o.Name, Kind: model.EvLogError, Severity: model.SevError, Msg: "ledger error: " + trunc(e.Message, 200)})
	}
}

func dirWord(incoming bool) string {
	if incoming {
		return "in"
	}
	return "out"
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

// flushCounts emits the per-minute log counters once the minute of the
// line clock has moved on, plus warning and error events grouped by
// template.
func (t *tailer) flushCounts(now time.Time, force bool) {
	c := t.counts
	minute := now.Truncate(time.Minute)
	if c.minute.IsZero() {
		c.minute = minute
		return
	}
	if !force && !minute.After(c.minute) {
		return
	}
	at := c.minute.Add(time.Minute - time.Millisecond)
	if at.After(now) {
		at = now
	}
	samples := c.samples()
	if len(samples) > 0 {
		t.n.o.Sink.Batch(&model.Batch{Node: t.n.o.Name, Group: model.GroupLog, Time: at.UnixNano(), Samples: samples})
	}
	for tmpl, w := range c.warn {
		sev := model.SevWarn
		kind := model.EvLogWarn
		if w.level == "error" {
			sev, kind = model.SevError, model.EvLogError
		}
		msg := tmpl
		if w.n > 1 {
			msg = fmt.Sprintf("%s (x%d)", tmpl, w.n)
		}
		t.n.o.Sink.Event(model.Event{Time: w.first.UnixNano(), Node: t.n.o.Name, Kind: kind, Severity: sev, Msg: trunc(msg, 300),
			Attrs: map[string]any{"template": tmpl, "count": w.n, "level": w.level}})
	}
	// Error rate and its baseline for the health check.
	errs := float64(c.errMinute)
	t.n.update(func(s *State) {
		s.ErrorRate = 0.7*s.ErrorRate + 0.3*errs
		if s.ErrorBaseline == 0 {
			s.ErrorBaseline = errs
		} else {
			s.ErrorBaseline = 0.995*s.ErrorBaseline + 0.005*errs
		}
	})
	c.warn = map[string]*warnAgg{}
	c.errMinute = 0
	c.minute = minute
}

// logCounts holds cumulative log-derived counters, bounded by caps.
type logCounts struct {
	capT, capF int
	minute     time.Time
	templates  map[string]float64 // level|template
	trace      map[string]float64
	flood      map[string]float64
	http       map[string]float64
	extracts   map[string]float64
	levels     map[string]float64
	bad        float64
	templDrops float64
	warn       map[string]*warnAgg
	errMinute  int
}

type warnAgg struct {
	n     int
	first time.Time
	level string
}

func newLogCounts(capT, capF int) *logCounts {
	return &logCounts{capT: capT, capF: capF, templates: map[string]float64{}, trace: map[string]float64{}, flood: map[string]float64{},
		http: map[string]float64{}, extracts: map[string]float64{}, levels: map[string]float64{}, warn: map[string]*warnAgg{}}
}

func (c *logCounts) add(l *logparse.Line) {
	// algod logs every REST request, so algodtop's own polling would
	// otherwise lead the template and level counts.
	if l.Self {
		return
	}
	c.levels[l.Level]++
	switch l.Kind {
	case logparse.KindTrace:
		ty := l.TraceType
		if ty == "" {
			ty = "unknown"
		}
		if _, ok := c.trace[ty]; ok || len(c.trace) < 64 {
			c.trace[ty]++
		} else {
			c.trace["other"]++
		}
	case logparse.KindFlood:
		app := l.AppID
		if app == "" {
			app = "none"
		}
		if _, ok := c.flood[app]; ok || len(c.flood) < c.capF {
			c.flood[app]++
		} else {
			c.flood["other"]++
		}
	case logparse.KindHTTP:
		k := l.HTTPPath + "|" + l.HTTPCode
		if _, ok := c.http[k]; ok || len(c.http) < 200 {
			c.http[k]++
		}
	case logparse.KindTemplate:
		k := l.Level + "|" + l.Template
		if _, ok := c.templates[k]; ok || len(c.templates) < c.capT {
			c.templates[k]++
		} else {
			c.templDrops++
		}
		if l.Extract != nil {
			c.extracts[l.Extract.Type]++
		}
		if l.Level == "warning" || l.Level == "error" {
			w := c.warn[l.Template]
			if w == nil {
				if len(c.warn) < 50 {
					c.warn[l.Template] = &warnAgg{n: 1, first: l.Time, level: l.Level}
				}
			} else {
				w.n++
			}
			if l.Level == "error" {
				c.errMinute++
			}
		}
	case logparse.KindBad:
		c.bad++
	}
}

func (c *logCounts) samples() []model.Sample {
	var out []model.Sample
	add := func(name string, m map[string]float64, label string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, model.Sample{Name: name, Labels: model.Labels{{Name: label, Value: k}}, Kind: model.KindCounter, Value: m[k]})
		}
	}
	for k, v := range c.templates {
		level, tmpl, _ := strings.Cut(k, "|")
		out = append(out, model.Sample{Name: "log_template_lines_total", Labels: model.Labels{{Name: "level", Value: level}, {Name: "template", Value: tmpl}}, Kind: model.KindCounter, Value: v})
	}
	for k, v := range c.http {
		path, code, _ := strings.Cut(k, "|")
		out = append(out, model.Sample{Name: "log_http_requests_total", Labels: model.Labels{{Name: "path", Value: path}, {Name: "status", Value: code}}, Kind: model.KindCounter, Value: v})
	}
	add("log_trace_lines_total", c.trace, "type")
	add("log_txpool_invalid_total", c.flood, "app")
	add("log_extract_total", c.extracts, "type")
	add("log_lines_total", c.levels, "level")
	out = append(out, model.Sample{Name: "log_unparsed_lines_total", Kind: model.KindCounter, Value: c.bad},
		model.Sample{Name: "log_template_drops_total", Kind: model.KindCounter, Value: c.templDrops})
	return out
}
