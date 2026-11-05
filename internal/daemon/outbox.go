package daemon

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"algod.top/internal/disk"
	"algod.top/internal/fsx"
	"algod.top/internal/model"
	"algod.top/internal/store"
)

// OutItem is one thing an agent hands to the hub.
type OutItem struct {
	Seq        uint64            `json:"s"`
	Batch      *model.Batch      `json:"-"`
	B          *wireBatch        `json:"b,omitempty"`
	Event      *model.Event      `json:"e,omitempty"`
	Session    *model.Session    `json:"x,omitempty"`
	UsageNode  string            `json:"un,omitempty"`
	Usage      []model.PeerUsage `json:"u,omitempty"`
	Line       *store.LogLine    `json:"l,omitempty"`
	Annotation *model.Annotation `json:"a,omitempty"`
}

// wireBatch is a compact batch: samples as [name, labels, kind, value].
type wireBatch struct {
	Node    string  `json:"n"`
	Group   uint8   `json:"g"`
	Time    int64   `json:"t"`
	Reset   bool    `json:"r,omitempty"`
	Samples [][]any `json:"x"`
}

func toWire(b *model.Batch) *wireBatch {
	w := &wireBatch{Node: b.Node, Group: uint8(b.Group), Time: b.Time, Reset: b.Reset, Samples: make([][]any, len(b.Samples))}
	for i, s := range b.Samples {
		w.Samples[i] = []any{s.Name, s.Labels.String(), uint8(s.Kind), s.Value}
	}
	return w
}

func fromWire(w *wireBatch) (*model.Batch, error) {
	b := &model.Batch{Node: w.Node, Group: model.Group(w.Group), Time: w.Time, Reset: w.Reset, Samples: make([]model.Sample, 0, len(w.Samples))}
	for _, x := range w.Samples {
		if len(x) != 4 {
			return nil, errors.New("outbox: bad sample")
		}
		name, _ := x[0].(string)
		ls, _ := x[1].(string)
		kind, _ := x[2].(float64)
		v, ok := x[3].(float64)
		if !ok {
			continue // NaN and Inf are not valid JSON and were sent as null
		}
		b.Samples = append(b.Samples, model.Sample{Name: name, Labels: model.ParseLabels(ls), Kind: model.Kind(kind), Value: v})
	}
	return b, nil
}

// Outbox is the agent's buffer of everything not yet pulled by the hub.
// It is a sequence of segment files of length-prefixed JSON records with a
// CRC32 each. Closed segments are gzipped.
type Outbox struct {
	mu      sync.Mutex
	dir     string
	window  time.Duration
	cap     int64
	epoch   string
	seq     uint64
	acked   uint64
	segs    []*obSeg
	cur     *obSeg
	f       *os.File
	w       *bufio.Writer
	dirty   bool
	notify  chan struct{}
	stop    chan struct{}
	deleted []string
}

type obSeg struct {
	path   string
	first  uint64
	last   uint64
	size   int64
	newest int64 // unix ns of the newest record
	gz     bool
}

const obSegMax = 8 << 20

// OpenOutbox opens or creates an outbox.
func OpenOutbox(dir string, window time.Duration, capBytes int64) (*Outbox, error) {
	if capBytes <= 0 {
		capBytes = 1 << 30
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	o := &Outbox{dir: dir, window: window, cap: capBytes, notify: make(chan struct{}), stop: make(chan struct{})}
	if b, err := os.ReadFile(filepath.Join(dir, "epoch")); err == nil {
		o.epoch = strings.TrimSpace(string(b))
	} else {
		o.epoch = store.NewID()
		if err := fsx.WriteAtomic(filepath.Join(dir, "epoch"), []byte(o.epoch), 0o600); err != nil {
			return nil, err
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "acked")); err == nil {
		o.acked, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".ob") && !strings.HasSuffix(name, ".ob.gz") {
			continue
		}
		first, err := strconv.ParseUint(strings.SplitN(name, ".", 2)[0], 10, 64)
		if err != nil {
			continue
		}
		s := &obSeg{path: filepath.Join(dir, name), first: first, gz: strings.HasSuffix(name, ".gz")}
		if fi, err := e.Info(); err == nil {
			s.size = fi.Size()
		}
		// Find the last sequence and newest time by reading it once.
		o.scanSeg(s, 0, func(it *OutItem, t int64) bool {
			s.last = max(s.last, it.Seq)
			s.newest = max(s.newest, t)
			return true
		})
		if s.last == 0 {
			s.last = first - 1
		}
		o.segs = append(o.segs, s)
		o.seq = max(o.seq, s.last)
	}
	sort.Slice(o.segs, func(i, j int) bool { return o.segs[i].first < o.segs[j].first })
	// Close any plain segment left from a previous run, then open a new one.
	for _, s := range o.segs {
		if !s.gz {
			o.compress(s)
		}
	}
	if err := o.rotate(); err != nil {
		return nil, err
	}
	go o.loop()
	return o, nil
}

func (o *Outbox) rotate() error {
	if o.f != nil {
		o.w.Flush()
		o.f.Sync()
		o.f.Close()
		o.compress(o.cur)
	}
	first := o.seq + 1
	path := filepath.Join(o.dir, fmt.Sprintf("%020d.ob", first))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	o.f, o.w = f, bufio.NewWriterSize(f, 64*1024)
	o.cur = &obSeg{path: path, first: first, last: first - 1}
	o.segs = append(o.segs, o.cur)
	return nil
}

func (o *Outbox) compress(s *obSeg) {
	if s.gz {
		return
	}
	in, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(in)
	zw.Close()
	gzPath := s.path + ".gz"
	if fsx.WriteAtomic(gzPath, b.Bytes(), 0o600) == nil {
		os.Remove(s.path)
		s.path, s.gz, s.size = gzPath, true, int64(b.Len())
	}
}

// Add appends an item. It never blocks on the hub.
func (o *Outbox) Add(it OutItem) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	it.Seq = o.seq
	var t int64
	if it.Batch != nil {
		it.B = toWire(it.Batch)
		t = it.Batch.Time
	}
	if it.Event != nil {
		t = it.Event.Time
	}
	body, err := json.Marshal(it)
	if err != nil {
		// NaN values: drop them from the batch rather than lose it.
		if it.B != nil {
			keep := it.B.Samples[:0]
			for _, s := range it.B.Samples {
				if v, ok := s[3].(float64); ok && v == v && v < 1e308 && v > -1e308 {
					keep = append(keep, s)
				}
			}
			it.B.Samples = keep
			body, err = json.Marshal(it)
		}
		if err != nil {
			return
		}
	}
	rec := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint32(rec[0:], uint32(len(body)))
	binary.LittleEndian.PutUint32(rec[4:], crc32.ChecksumIEEE(body))
	rec = append(rec, body...)
	o.w.Write(rec)
	o.cur.size += int64(len(rec))
	o.cur.last = it.Seq
	if t == 0 {
		t = time.Now().UnixNano()
	}
	o.cur.newest = max(o.cur.newest, t)
	o.dirty = true
	if o.cur.size >= obSegMax {
		o.rotate()
	}
	close(o.notify)
	o.notify = make(chan struct{})
}

func (o *Outbox) loop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	prune := time.NewTicker(time.Minute)
	defer prune.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-t.C:
			o.mu.Lock()
			if o.dirty {
				o.w.Flush()
				o.f.Sync()
				o.dirty = false
			}
			o.mu.Unlock()
		case <-prune.C:
			o.prune()
		}
	}
}

// prune deletes pulled segments older than the buffer window, and the
// oldest data of any kind past the size cap.
func (o *Outbox) prune() {
	o.mu.Lock()
	defer o.mu.Unlock()
	cut := time.Now().Add(-o.window).UnixNano()
	var total int64
	for _, s := range o.segs {
		total += s.size
	}
	keep := o.segs[:0]
	for _, s := range o.segs {
		if s == o.cur {
			keep = append(keep, s)
			continue
		}
		pulled := s.last <= o.acked
		old := s.newest < cut
		overCap := total > o.cap
		if (pulled && old) || overCap {
			if os.Remove(s.path) == nil {
				total -= s.size
				if !pulled {
					o.deleted = append(o.deleted, fmt.Sprintf("seq %d-%d", s.first, s.last))
				}
				continue
			}
		}
		keep = append(keep, s)
	}
	o.segs = keep
}

// TakeDeleted returns unpulled ranges the cap removed, for the retention
// deletion event.
func (o *Outbox) TakeDeleted() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.deleted
	o.deleted = nil
	return d
}

// Close flushes and closes.
func (o *Outbox) Close() {
	close(o.stop)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.w.Flush()
	o.f.Sync()
	o.f.Close()
}

func (o *Outbox) scanSeg(s *obSeg, since uint64, fn func(*OutItem, int64) bool) error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if s.gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil
		}
		n := binary.LittleEndian.Uint32(hdr[:4])
		if n > 64<<20 {
			return nil
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil
		}
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(hdr[4:]) {
			return nil
		}
		var it OutItem
		if json.Unmarshal(body, &it) != nil {
			continue
		}
		if it.Seq <= since {
			continue
		}
		var t int64
		if it.B != nil {
			t = it.B.Time
		} else if it.Event != nil {
			t = it.Event.Time
		}
		if !fn(&it, t) {
			return nil
		}
	}
}

// Pull returns gzipped JSON lines of items after since, up to maxBytes,
// waiting up to wait for new data. The cursor is epoch:seq; a cursor from
// another epoch (the agent's buffer was reset) starts from the beginning.
func (o *Outbox) Pull(ctx context.Context, cursor string, wait time.Duration, maxBytes int) ([]byte, string, error) {
	epoch, seqS, _ := strings.Cut(cursor, ":")
	var since uint64
	if epoch == o.epoch {
		since, _ = strconv.ParseUint(seqS, 10, 64)
	}
	// The hub asking for data after since acknowledges everything up to it.
	o.mu.Lock()
	if since > o.acked {
		o.acked = since
		fsx.WriteAtomic(filepath.Join(o.dir, "acked"), []byte(strconv.FormatUint(since, 10)), 0o600)
	}
	if o.seq <= since && wait > 0 {
		ch := o.notify
		o.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		o.mu.Lock()
	}
	o.w.Flush()
	// Copy the segment values: Add updates the current one concurrently.
	segs := make([]*obSeg, len(o.segs))
	for i, s := range o.segs {
		c := *s
		segs[i] = &c
	}
	o.mu.Unlock()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	last := since
	written := 0
	for _, s := range segs {
		if s.last <= since {
			continue
		}
		err := o.scanSeg(s, since, func(it *OutItem, _ int64) bool {
			b, err := json.Marshal(it)
			if err != nil {
				return true
			}
			zw.Write(b)
			zw.Write([]byte{'\n'})
			written += len(b) + 1
			last = it.Seq
			return written < maxBytes
		})
		if err != nil {
			return nil, "", err
		}
		if written >= maxBytes {
			break
		}
	}
	zw.Close()
	return buf.Bytes(), fmt.Sprintf("%s:%d", o.epoch, last), nil
}

// DecodePull parses a pull response body.
func DecodePull(b []byte) ([]OutItem, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out []OutItem
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var it OutItem
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			return out, err
		}
		if it.B != nil {
			if it.Batch, err = fromWire(it.B); err != nil {
				return out, err
			}
		}
		out = append(out, it)
	}
	return out, sc.Err()
}

// AgentBatches implements api.Hub.
func (d *Daemon) AgentBatches(ctx context.Context, since string, wait time.Duration) ([]byte, string, error) {
	if d.outbox == nil {
		return nil, "", errors.New("not an agent")
	}
	return d.outbox.Pull(ctx, since, wait, 4<<20)
}

// DropOldest deletes the oldest closed segment, pulled or not, when the
// disk is at its floor. It reports whether a segment was removed.
func (o *Outbox) DropOldest() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, s := range o.segs {
		if s == o.cur {
			continue
		}
		if os.Remove(s.path) == nil {
			if s.last > o.acked {
				o.deleted = append(o.deleted, fmt.Sprintf("seq %d-%d (disk floor)", s.first, s.last))
			}
			o.segs = append(o.segs[:i], o.segs[i+1:]...)
			return true
		}
	}
	return false
}

// pruneOutbox enforces the agent's disk floor on the outbox and records
// unpulled data that the size cap or the floor removed.
func (d *Daemon) pruneOutbox() {
	cfg := d.Config()
	floor := cfg.Disk.FloorPercent / 100
	if floor == 0 {
		floor = 0.05
	}
	for range 100 {
		u, err := disk.Stat(d.db.Dir())
		if err != nil || u.FreeFraction() >= floor+0.01 || !d.outbox.DropOldest() {
			break
		}
	}
	for _, r := range d.outbox.TakeDeleted() {
		d.emitEvent(model.Event{Kind: model.EvRetention, Severity: model.SevWarn, Msg: "agent buffer removed unpulled data " + r,
			Attrs: map[string]any{"reason": "agent-buffer", "range": r}})
	}
}

// NodeName is the agent node an item belongs to, "" for agent-wide items.
func (it *OutItem) NodeName() string {
	switch {
	case it.Batch != nil:
		return it.Batch.Node
	case it.Event != nil:
		return it.Event.Node
	case it.Session != nil:
		return it.Session.Node
	case it.Usage != nil:
		return it.UsageNode
	case it.Line != nil:
		return it.Line.Node
	case it.Annotation != nil:
		return it.Annotation.Node
	}
	return ""
}
