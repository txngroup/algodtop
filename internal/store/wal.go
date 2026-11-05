package store

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"algod.top/internal/model"
)

// WAL record types.
const (
	walBatch     = 1
	walPeerUsage = 2
)

// WAL batch flags.
const (
	walFlagReset = 1
	walFlagLate  = 2
)

const walSegmentSize = 64 << 20

// wal is the append-only write-ahead log. Each record is u32 length, u32
// CRC32 and a body that starts with u8 type and u64 sequence.
type wal struct {
	dir  string
	f    *os.File
	w    *bufio.Writer
	size int64
	seq  uint64
	cur  *walSeg
	segs []*walSeg
	buf  []byte
}

// walSeg tracks which nodes have records in a segment, so the segment can
// be deleted once every one of them has flushed.
type walSeg struct {
	path  string
	first uint64
	last  uint64
	nodes map[uint32]*segNode
}

// WAL record categories, each with its own flush watermark.
const (
	catHead  = 0
	catLate  = 1
	catUsage = 2
	nCats    = 3
)

type segNode struct {
	max [nCats]uint64
}

func (s *walSeg) note(node uint32, seq uint64, cat int) {
	n := s.nodes[node]
	if n == nil {
		n = &segNode{}
		s.nodes[node] = n
	}
	n.max[cat] = max(n.max[cat], seq)
	s.last = max(s.last, seq)
}

// walRecord is a decoded WAL record.
type walRecord struct {
	typ   uint8
	seq   uint64
	node  uint32
	group model.Group
	flags uint8
	time  int64
	ids   []uint32
	vals  []float64
	usage []model.PeerUsage
}

func segPath(dir string, first uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%020d.wal", first))
}

// replayWAL reads every segment in order and calls fn for each record. It
// returns the segments found and the highest sequence seen.
func replayWAL(dir string, fn func(*walRecord)) ([]*walSeg, uint64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".wal") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var segs []*walSeg
	var maxSeq uint64
	for _, name := range names {
		first, _ := strconv.ParseUint(strings.TrimSuffix(name, ".wal"), 10, 64)
		seg := &walSeg{path: filepath.Join(dir, name), first: first, nodes: map[uint32]*segNode{}}
		f, err := os.Open(seg.path)
		if err != nil {
			return nil, 0, err
		}
		r := bufio.NewReaderSize(f, 256*1024)
		var hdr [8]byte
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				break
			}
			n := binary.LittleEndian.Uint32(hdr[:4])
			if n > 64<<20 {
				break
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				break // torn final record
			}
			if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(hdr[4:]) {
				break
			}
			rec, err := decodeWAL(body)
			if err != nil {
				break
			}
			maxSeq = max(maxSeq, rec.seq)
			seg.note(rec.node, rec.seq, rec.category())
			fn(rec)
		}
		f.Close()
		segs = append(segs, seg)
	}
	return segs, maxSeq, nil
}

func decodeWAL(b []byte) (*walRecord, error) {
	rd := &byteReader{b: b}
	rec := &walRecord{typ: rd.byte(), seq: rd.u64(), node: rd.u32()}
	switch rec.typ {
	case walBatch:
		rec.group = model.Group(rd.byte())
		rec.flags = rd.byte()
		rec.time = int64(rd.u64())
		n64 := rd.uvarint()
		if n64 > uint64(len(rd.b)) {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(n64)
		rec.ids = make([]uint32, n)
		rec.vals = make([]float64, n)
		for i := 0; i < n; i++ {
			rec.ids[i] = uint32(rd.uvarint())
			rec.vals[i] = math.Float64frombits(rd.u64())
		}
	case walPeerUsage:
		n64 := rd.uvarint()
		if n64 > uint64(len(rd.b)) {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(n64)
		for i := 0; i < n; i++ {
			u := model.PeerUsage{Time: rd.varint(), Key: rd.str(), Network: rd.str(), Direction: rd.str(), Addr: rd.str(),
				BytesIn: rd.varint(), BytesOut: rd.varint()}
			ns := int(min(rd.uvarint(), uint64(len(rd.b))))
			for j := 0; j < ns && rd.err == nil; j++ {
				u.Streams = append(u.Streams, rd.str())
			}
			rec.usage = append(rec.usage, u)
		}
	default:
		return nil, fmt.Errorf("wal: unknown record type %d", rec.typ)
	}
	if rd.err != nil {
		return nil, rd.err
	}
	return rec, nil
}

// openWAL starts a fresh segment after replay.
func openWAL(dir string, segs []*walSeg, seq uint64) (*wal, error) {
	w := &wal{dir: dir, seq: seq, segs: segs}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *wal) rotate() error {
	if w.f != nil {
		if err := w.w.Flush(); err != nil {
			return err
		}
		if err := w.f.Sync(); err != nil {
			return err
		}
		w.f.Close()
	}
	first := w.seq + 1
	path := segPath(w.dir, first)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.f = f
	w.w = bufio.NewWriterSize(f, 256*1024)
	w.size = 0
	w.cur = &walSeg{path: path, first: first, nodes: map[uint32]*segNode{}}
	// An empty segment left by a clean shutdown has this same name. Its
	// stale entry must go, or pruning it would delete the open file.
	keep := w.segs[:0]
	for _, s := range w.segs {
		if s.path != path {
			keep = append(keep, s)
		}
	}
	w.segs = append(keep, w.cur)
	return nil
}

func (w *wal) put(body []byte, node uint32, cat int) (uint64, error) {
	if w.size >= walSegmentSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(hdr[4:], crc32.Checksum(body, crcTable))
	if _, err := w.w.Write(hdr[:]); err != nil {
		return 0, err
	}
	if _, err := w.w.Write(body); err != nil {
		return 0, err
	}
	w.size += int64(8 + len(body))
	seq := binary.LittleEndian.Uint64(body[1:9])
	w.cur.note(node, seq, cat)
	return seq, nil
}

// appendBatch logs a batch with resolved IDs.
func (w *wal) appendBatch(node uint32, b *model.Batch, ids []uint32, late bool) (uint64, error) {
	w.seq++
	buf := w.buf[:0]
	buf = append(buf, walBatch)
	buf = binary.LittleEndian.AppendUint64(buf, w.seq)
	buf = binary.LittleEndian.AppendUint32(buf, node)
	var flags uint8
	if b.Reset {
		flags |= walFlagReset
	}
	if late {
		flags |= walFlagLate
	}
	buf = append(buf, byte(b.Group), flags)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(b.Time))
	n := 0
	for _, id := range ids {
		if id != 0 {
			n++
		}
	}
	buf = binary.AppendUvarint(buf, uint64(n))
	for i, id := range ids {
		if id == 0 {
			continue
		}
		buf = binary.AppendUvarint(buf, uint64(id))
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(b.Samples[i].Value))
	}
	w.buf = buf
	cat := catHead
	if late {
		cat = catLate
	}
	return w.put(buf, node, cat)
}

// appendUsage logs per-peer usage samples.
func (w *wal) appendUsage(node uint32, us []model.PeerUsage) (uint64, error) {
	w.seq++
	buf := w.buf[:0]
	buf = append(buf, walPeerUsage)
	buf = binary.LittleEndian.AppendUint64(buf, w.seq)
	buf = binary.LittleEndian.AppendUint32(buf, node)
	buf = binary.AppendUvarint(buf, uint64(len(us)))
	for _, u := range us {
		buf = binary.AppendVarint(buf, u.Time)
		buf = appendStr(buf, u.Key)
		buf = appendStr(buf, u.Network)
		buf = appendStr(buf, u.Direction)
		buf = appendStr(buf, u.Addr)
		buf = binary.AppendVarint(buf, u.BytesIn)
		buf = binary.AppendVarint(buf, u.BytesOut)
		buf = binary.AppendUvarint(buf, uint64(len(u.Streams)))
		for _, s := range u.Streams {
			buf = appendStr(buf, s)
		}
	}
	w.buf = buf
	return w.put(buf, node, catUsage)
}

func appendStr(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// sync flushes and fsyncs the current segment.
func (w *wal) sync() error {
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

func (w *wal) close() error {
	err := w.sync()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// prune deletes closed segments whose records have all been flushed.
func (w *wal) prune(flushed func(node uint32) [nCats]uint64) int {
	removed := 0
	keep := w.segs[:0]
	for _, s := range w.segs {
		if s == w.cur || s.path == w.cur.path {
			keep = append(keep, s)
			continue
		}
		done := true
		for node, n := range s.nodes {
			f := flushed(node)
			for c := range nCats {
				if n.max[c] > f[c] {
					done = false
				}
			}
			if !done {
				break
			}
		}
		if done && os.Remove(s.path) == nil {
			removed++
			continue
		}
		keep = append(keep, s)
	}
	w.segs = keep
	return removed
}

func (r *walRecord) category() int {
	switch {
	case r.typ == walPeerUsage:
		return catUsage
	case r.flags&walFlagLate != 0:
		return catLate
	}
	return catHead
}
