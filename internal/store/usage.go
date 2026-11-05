package store

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// usageStore keeps per-peer usage samples outside the series index, so peer
// data cannot inflate cardinality. Samples buffer in memory per node and
// 2 h window and flush to peers/<day>/n<id>-<start>.pu with the head.
//
// File layout: "ADTU", u32 node, i64 start, uvarint nkeys, then per key the
// strings key, network, direction, addr, uvarint nsamples, and per sample
// varint dt, varint bytes in, varint bytes out, u8 streams changed,
// [uvarint n, strings]. A CRC32 of everything before it ends the file.
type usageStore struct {
	db    *DB
	mu    sync.Mutex
	bufs  map[uint32]map[int64]*usageWin
	last  map[uint32]uint64 // newest usage seq per node
	files map[uint32][]usageRef
}

type usageWin struct {
	samples []model.PeerUsage
	minSeq  uint64
	maxSeq  uint64
}

type usageRef struct {
	start int64
	path  string
	size  int64
}

var usageName = regexp.MustCompile(`^n(\d+)-(-?\d+)\.pu$`)

func newUsageStore(db *DB) *usageStore {
	return &usageStore{db: db, bufs: map[uint32]map[int64]*usageWin{}, last: map[uint32]uint64{}, files: map[uint32][]usageRef{}}
}

func (u *usageStore) path(node uint32, start int64) string {
	day := time.Unix(0, start).UTC().Format("2006-01-02")
	return filepath.Join(u.db.dir, "peers", day, fmt.Sprintf("n%d-%d.pu", node, start/int64(time.Second)))
}

func (u *usageStore) scan() error {
	root := filepath.Join(u.db.dir, "peers")
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		m := usageName.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		node, _ := strconv.ParseUint(m[1], 10, 32)
		secs, _ := strconv.ParseInt(m[2], 10, 64)
		var size int64
		if fi, err := d.Info(); err == nil {
			size = fi.Size()
		}
		u.files[uint32(node)] = append(u.files[uint32(node)], usageRef{start: secs * int64(time.Second), path: p, size: size})
		return nil
	})
}

func (u *usageStore) add(node uint32, seq uint64, us []model.PeerUsage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	m := u.bufs[node]
	if m == nil {
		m = map[int64]*usageWin{}
		u.bufs[node] = m
	}
	for _, s := range us {
		ws := windowStart(s.Time)
		w := m[ws]
		if w == nil {
			w = &usageWin{minSeq: seq}
			m[ws] = w
		}
		w.samples = append(w.samples, s)
		w.minSeq = min(w.minSeq, seq)
		w.maxSeq = max(w.maxSeq, seq)
	}
	u.last[node] = max(u.last[node], seq)
}

// flushBefore writes every buffered window of a node that starts before t,
// then moves the usage watermark to just below the oldest record still in
// memory.
func (u *usageStore) flushBefore(node uint32, t int64) error {
	u.mu.Lock()
	var todo []int64
	for ws := range u.bufs[node] {
		if ws < t {
			todo = append(todo, ws)
		}
	}
	u.mu.Unlock()
	sort.Slice(todo, func(i, j int) bool { return todo[i] < todo[j] })
	for _, ws := range todo {
		if err := u.flushWindow(node, ws); err != nil {
			return err
		}
	}
	u.mu.Lock()
	seq := u.last[node]
	for _, w := range u.bufs[node] {
		if w.minSeq > 0 {
			seq = min(seq, w.minSeq-1)
		}
	}
	u.mu.Unlock()
	u.db.mu.Lock()
	wm := u.db.watermark(node)
	wm.UsageSeq = max(wm.UsageSeq, seq)
	u.db.mu.Unlock()
	return nil
}

func (u *usageStore) flushAll() error {
	u.mu.Lock()
	var nodes []uint32
	for n := range u.bufs {
		nodes = append(nodes, n)
	}
	u.mu.Unlock()
	for _, n := range nodes {
		if err := u.flushBefore(n, 1<<62); err != nil {
			return err
		}
	}
	return nil
}

// flushWindow merges a buffered window with any file already written for
// it and rewrites the file.
func (u *usageStore) flushWindow(node uint32, ws int64) error {
	u.mu.Lock()
	w := u.bufs[node][ws]
	u.mu.Unlock()
	if w == nil {
		return nil
	}
	path := u.path(node, ws)
	samples := append([]model.PeerUsage(nil), w.samples...)
	if old, err := readUsageFile(path); err == nil {
		samples = append(old, samples...)
	}
	b := encodeUsage(node, ws, samples)
	if err := fsx.WriteAtomic(path, b, 0o600); err != nil {
		return err
	}
	u.mu.Lock()
	delete(u.bufs[node], ws)
	refs := u.files[node]
	found := false
	for i := range refs {
		if refs[i].path == path {
			refs[i].size = int64(len(b))
			found = true
		}
	}
	if !found {
		u.files[node] = append(refs, usageRef{start: ws, path: path, size: int64(len(b))})
	}
	u.mu.Unlock()
	return nil
}

func encodeUsage(node uint32, ws int64, samples []model.PeerUsage) []byte {
	type keyed struct {
		key, net, dir, addr string
	}
	groups := map[keyed][]model.PeerUsage{}
	var order []keyed
	seen := map[string]map[int64]bool{}
	for _, s := range samples {
		if seen[s.Key] == nil {
			seen[s.Key] = map[int64]bool{}
		}
		if seen[s.Key][s.Time] {
			continue
		}
		seen[s.Key][s.Time] = true
		k := keyed{s.Key, s.Network, s.Direction, s.Addr}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}
	b := []byte("ADTU")
	b = binary.LittleEndian.AppendUint32(b, node)
	b = binary.LittleEndian.AppendUint64(b, uint64(ws))
	b = binary.AppendUvarint(b, uint64(len(order)))
	for _, k := range order {
		ss := groups[k]
		sort.Slice(ss, func(i, j int) bool { return ss[i].Time < ss[j].Time })
		b = appendStr(b, k.key)
		b = appendStr(b, k.net)
		b = appendStr(b, k.dir)
		b = appendStr(b, k.addr)
		b = binary.AppendUvarint(b, uint64(len(ss)))
		prevT, prevIn, prevOut := ws, int64(0), int64(0)
		var prevStreams []string
		for i, s := range ss {
			b = binary.AppendVarint(b, s.Time-prevT)
			b = binary.AppendVarint(b, s.BytesIn-prevIn)
			b = binary.AppendVarint(b, s.BytesOut-prevOut)
			prevT, prevIn, prevOut = s.Time, s.BytesIn, s.BytesOut
			if i == 0 || !slices.Equal(prevStreams, s.Streams) {
				b = append(b, 1)
				b = binary.AppendUvarint(b, uint64(len(s.Streams)))
				for _, st := range s.Streams {
					b = appendStr(b, st)
				}
				prevStreams = s.Streams
			} else {
				b = append(b, 0)
			}
		}
	}
	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, crcTable))
}

func readUsageFile(path string) ([]model.PeerUsage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 20 || string(b[:4]) != "ADTU" {
		return nil, errCorrupt
	}
	body := b[:len(b)-4]
	if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, errCorrupt
	}
	rd := &byteReader{b: body[4:]}
	rd.u32()
	ws := int64(rd.u64())
	nk := rd.uvarint()
	var out []model.PeerUsage
	for i := uint64(0); i < nk && rd.err == nil; i++ {
		key, net, dir, addr := rd.str(), rd.str(), rd.str(), rd.str()
		ns := rd.uvarint()
		t, in, outb := ws, int64(0), int64(0)
		var streams []string
		for j := uint64(0); j < ns && rd.err == nil; j++ {
			t += rd.varint()
			in += rd.varint()
			outb += rd.varint()
			if rd.byte() == 1 {
				n := rd.uvarint()
				streams = nil
				for k := uint64(0); k < n && rd.err == nil; k++ {
					streams = append(streams, rd.str())
				}
			}
			out = append(out, model.PeerUsage{Time: t, Key: key, Network: net, Direction: dir, Addr: addr, BytesIn: in, BytesOut: outb, Streams: streams})
		}
	}
	if rd.err != nil {
		return nil, rd.err
	}
	return out, nil
}

// PeerUsage returns usage samples of a node in [from, to). An empty key
// returns every peer.
func (db *DB) PeerUsage(nodeName, key string, from, to int64) []model.PeerUsage {
	node, ok := db.NodeID(nodeName)
	if !ok {
		return nil
	}
	u := db.usage
	u.mu.Lock()
	refs := append([]usageRef(nil), u.files[node]...)
	var mem []model.PeerUsage
	for ws, w := range u.bufs[node] {
		if ws+WindowNS > from && ws < to {
			mem = append(mem, w.samples...)
		}
	}
	u.mu.Unlock()
	var out []model.PeerUsage
	keep := func(s model.PeerUsage) {
		if s.Time >= from && s.Time < to && (key == "" || s.Key == key) {
			out = append(out, s)
		}
	}
	for _, r := range refs {
		if r.start+WindowNS <= from || r.start >= to {
			continue
		}
		ss, err := readUsageFile(r.path)
		if err != nil {
			continue
		}
		for _, s := range ss {
			keep(s)
		}
	}
	for _, s := range mem {
		keep(s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// pruneUsage deletes usage files that ended before t.
func (u *usageStore) pruneBefore(t int64) (removed int, bytes int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for node, refs := range u.files {
		keep := refs[:0]
		for _, r := range refs {
			if r.start+WindowNS <= t && os.Remove(r.path) == nil {
				removed++
				bytes += r.size
				continue
			}
			keep = append(keep, r)
		}
		u.files[node] = keep
	}
	return removed, bytes
}
