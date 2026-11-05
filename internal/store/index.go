package store

import (
	"bufio"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"algod.top/internal/model"
)

// SeriesMeta describes one stored series.
type SeriesMeta struct {
	ID     uint32
	Node   uint32
	Name   string
	Labels model.Labels
	Kind   model.Kind
	Group  model.Group
}

// Caps bound the number of series.
type Caps struct {
	PerName int // series per metric name, default 200
	PerNode int // series per node, default 5000
	// NameCaps overrides PerName for metrics with their own cap, such as
	// log templates.
	NameCaps map[string]int
}

// index maps series IDs to (node, name, labels). The file is append-only:
// each record is u32 len, u32 crc, then the body.
type index struct {
	mu     sync.RWMutex
	path   string
	f      *os.File
	w      *bufio.Writer
	dirty  bool
	caps   Caps
	byKey  map[seriesKeyT]uint32
	meta   []SeriesMeta // index = ID; ID 0 is unused
	byNode map[uint32]map[string][]uint32
	count  map[uint32]int
	drops  map[uint32]map[string]int64
}

// seriesKeyT identifies a series. A struct of strings is a map key that
// needs no allocation per lookup.
type seriesKeyT struct {
	node   uint32
	name   string
	labels string
}

func seriesKey(node uint32, name, labels string) seriesKeyT {
	return seriesKeyT{node, name, labels}
}

func openIndex(root string, caps Caps) (*index, error) {
	if caps.PerName <= 0 {
		caps.PerName = 200
	}
	if caps.PerNode <= 0 {
		caps.PerNode = 5000
	}
	dir := filepath.Join(root, "series")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ix := &index{path: filepath.Join(dir, "index.bin"), caps: caps, byKey: map[seriesKeyT]uint32{},
		meta: []SeriesMeta{{}}, byNode: map[uint32]map[string][]uint32{}, count: map[uint32]int{},
		drops: map[uint32]map[string]int64{}}
	f, err := os.OpenFile(ix.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	good, err := ix.load(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	// Drop a torn tail record left by a crash.
	if fi, err := f.Stat(); err == nil && fi.Size() > good {
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	ix.f = f
	ix.w = bufio.NewWriter(f)
	return ix, nil
}

func (ix *index) load(f *os.File) (int64, error) {
	r := bufio.NewReader(f)
	var off int64
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return off, nil
		}
		n := binary.LittleEndian.Uint32(hdr[:4])
		if n > 1<<20 {
			return off, nil
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return off, nil
		}
		if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(hdr[4:]) {
			return off, nil
		}
		rd := &byteReader{b: body}
		m := SeriesMeta{ID: rd.u32(), Node: rd.u32(), Kind: model.Kind(rd.byte()), Group: model.Group(rd.byte())}
		m.Name = rd.str()
		ls := rd.str()
		if rd.err != nil {
			return off, nil
		}
		m.Labels = model.ParseLabels(ls)
		if int(m.ID) != len(ix.meta) {
			return off, errors.New("store: series index out of order")
		}
		ix.insert(m, ls)
		off += int64(8 + n)
	}
}

func (ix *index) insert(m SeriesMeta, labels string) {
	ix.meta = append(ix.meta, m)
	ix.byKey[seriesKey(m.Node, m.Name, labels)] = m.ID
	nm := ix.byNode[m.Node]
	if nm == nil {
		nm = map[string][]uint32{}
		ix.byNode[m.Node] = nm
	}
	nm[m.Name] = append(nm[m.Name], m.ID)
	ix.count[m.Node]++
}

// lookup returns the ID for a series, creating it if the caps allow.
// ok is false when the series was dropped by a cap.
func (ix *index) lookup(node uint32, s *model.Sample, g model.Group) (uint32, bool) {
	ls := s.LabelKey
	if ls == "" && len(s.Labels) > 0 {
		ls = s.Labels.String()
	}
	key := seriesKey(node, s.Name, ls)
	ix.mu.RLock()
	id, ok := ix.byKey[key]
	ix.mu.RUnlock()
	if ok {
		return id, true
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if id, ok := ix.byKey[key]; ok {
		return id, true
	}
	perName := ix.caps.PerName
	if c, ok := ix.caps.NameCaps[s.Name]; ok {
		perName = c
	}
	if ix.count[node] >= ix.caps.PerNode || len(ix.byNode[node][s.Name]) >= perName {
		d := ix.drops[node]
		if d == nil {
			d = map[string]int64{}
			ix.drops[node] = d
		}
		d[s.Name]++
		return 0, false
	}
	m := SeriesMeta{ID: uint32(len(ix.meta)), Node: node, Name: s.Name, Labels: append(model.Labels(nil), s.Labels...), Kind: s.Kind, Group: g}
	ix.insert(m, ls)
	body := make([]byte, 0, 32+len(s.Name)+len(ls))
	body = binary.LittleEndian.AppendUint32(body, m.ID)
	body = binary.LittleEndian.AppendUint32(body, node)
	body = append(body, byte(m.Kind), byte(g))
	body = binary.AppendUvarint(body, uint64(len(s.Name)))
	body = append(body, s.Name...)
	body = binary.AppendUvarint(body, uint64(len(ls)))
	body = append(body, ls...)
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(hdr[4:], crc32.Checksum(body, crcTable))
	ix.w.Write(hdr[:])
	ix.w.Write(body)
	ix.dirty = true
	return m.ID, true
}

// sync flushes and fsyncs pending index records.
func (ix *index) sync() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if !ix.dirty {
		return nil
	}
	if err := ix.w.Flush(); err != nil {
		return err
	}
	ix.dirty = false
	return ix.f.Sync()
}

func (ix *index) close() error {
	if err := ix.sync(); err != nil {
		ix.f.Close()
		return err
	}
	return ix.f.Close()
}

// get returns the metadata for an ID.
func (ix *index) get(id uint32) (SeriesMeta, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if id == 0 || int(id) >= len(ix.meta) {
		return SeriesMeta{}, false
	}
	return ix.meta[id], true
}

// find returns the series of a node with a metric name.
func (ix *index) find(node uint32, name string) []SeriesMeta {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	ids := ix.byNode[node][name]
	out := make([]SeriesMeta, 0, len(ids))
	for _, id := range ids {
		out = append(out, ix.meta[id])
	}
	return out
}

// names returns the metric names of a node, sorted.
func (ix *index) names(node uint32) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.byNode[node]))
	for n := range ix.byNode[node] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// nodeSeries returns every series ID of a node.
func (ix *index) nodeSeries(node uint32) []uint32 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []uint32
	for _, ids := range ix.byNode[node] {
		out = append(out, ids...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// dropped returns cap drop counts by metric name for a node.
func (ix *index) dropped(node uint32) map[string]int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := map[string]int64{}
	for k, v := range ix.drops[node] {
		out[k] = v
	}
	return out
}

func (ix *index) seriesCount(node uint32) int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.count[node]
}
