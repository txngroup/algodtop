package store

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	"algod.top/internal/enc"
	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// Block file layout (all integers little endian unless varint):
//
//	header  32 bytes: "ADTB", u16 version, u8 tier, u8 kind, u32 node,
//	        i64 start, i64 end, u32 gen
//	groups  per group: u8 group, uvarint n, uvarint len, ts bytes, u32 crc
//	series  per series: u32 id, u8 group, u8 kind, u8 flags, u8 ncols,
//	        [flags&1: f64 offset, f64 lastRaw, i64 lastT],
//	        uvarint len, presence bytes, ncols x (uvarint len, xor bytes),
//	        u32 crc
//	footer  uvarint ngroups, (u8 group, uvarint off, uvarint n)...,
//	        uvarint nseries, (u32 id, uvarint off)..., i64 minT, i64 maxT,
//	        uvarint nresets, varint resets...
//	trailer u32 footer length, u32 crc32 of everything before it, "ADTE"
//
// Section CRCs let a query verify only what it reads. The trailer CRC covers
// the whole block for the scrub.

const (
	blockMagic   = "ADTB"
	trailerMagic = "ADTE"
	blockVersion = 1
	headerSize   = 32
	trailerSize  = 12
)

// Block kinds.
const (
	bkMain      = 1 // raw block flushed from a head
	bkSupp      = 2 // supplementary raw block from late data
	bkPartial   = 3 // 2 h rollup block
	bkCompacted = 4 // day, week or month rollup block
)

var errCorrupt = errors.New("store: corrupt block")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// blockHeader identifies a block.
type blockHeader struct {
	Tier  Tier
	Kind  uint8
	Node  uint32
	Start int64
	End   int64
	Gen   uint32
}

// ctrStart is a counter series' reset-adjustment state at block start.
type ctrStart struct {
	Offset  float64
	LastRaw float64
	LastT   int64
}

// colSeries is one decoded series inside a block.
type colSeries struct {
	ID    uint32
	Group model.Group
	Kind  model.Kind
	T     []int64
	Cols  [][]float64
	Start *ctrStart
}

// blockWriter streams a block to disk. Groups must be written before series.
type blockWriter struct {
	path   string
	tmp    string
	f      *os.File
	w      *bufio.Writer
	off    int64
	crc    uint32
	hdr    blockHeader
	groups []groupEntry
	series []seriesEntry
	gcount map[model.Group]int
	minT   int64
	maxT   int64
	resets []int64
	lim    *limiter
}

type groupEntry struct {
	g   model.Group
	off int64
	n   int
}

type seriesEntry struct {
	id  uint32
	off int64
}

func newBlockWriter(path string, hdr blockHeader, lim *limiter) (*blockWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-blk-*")
	if err != nil {
		return nil, err
	}
	bw := &blockWriter{path: path, tmp: f.Name(), f: f, w: bufio.NewWriterSize(f, 64*1024), hdr: hdr,
		gcount: map[model.Group]int{}, minT: math.MaxInt64, maxT: math.MinInt64, lim: lim}
	var h [headerSize]byte
	copy(h[:4], blockMagic)
	binary.LittleEndian.PutUint16(h[4:], blockVersion)
	h[6] = byte(hdr.Tier)
	h[7] = hdr.Kind
	binary.LittleEndian.PutUint32(h[8:], hdr.Node)
	binary.LittleEndian.PutUint64(h[12:], uint64(hdr.Start))
	binary.LittleEndian.PutUint64(h[20:], uint64(hdr.End))
	binary.LittleEndian.PutUint32(h[28:], hdr.Gen)
	if err := bw.write(h[:]); err != nil {
		bw.abort()
		return nil, err
	}
	return bw, nil
}

func (bw *blockWriter) write(b []byte) error {
	if bw.lim != nil {
		bw.lim.wait(len(b))
	}
	n, err := bw.w.Write(b)
	bw.off += int64(n)
	bw.crc = crc32.Update(bw.crc, crcTable, b[:n])
	return err
}

func (bw *blockWriter) abort() {
	if bw == nil {
		return
	}
	bw.f.Close()
	os.Remove(bw.tmp)
}

// writeGroup writes one group's timestamp column.
func (bw *blockWriter) writeGroup(g model.Group, ts []int64) error {
	if bw == nil {
		return nil
	}
	if len(ts) > 0 {
		bw.minT = min(bw.minT, ts[0])
		bw.maxT = max(bw.maxT, ts[len(ts)-1])
	}
	bw.groups = append(bw.groups, groupEntry{g, bw.off, len(ts)})
	bw.gcount[g] = len(ts)
	tb := enc.EncodeTS(ts)
	sec := make([]byte, 0, len(tb)+16)
	sec = append(sec, byte(g))
	sec = binary.AppendUvarint(sec, uint64(len(ts)))
	sec = binary.AppendUvarint(sec, uint64(len(tb)))
	sec = append(sec, tb...)
	sec = binary.LittleEndian.AppendUint32(sec, crc32.Checksum(sec, crcTable))
	return bw.write(sec)
}

// writeSeries writes one series given its presence against the group column
// and its encoded value columns.
func (bw *blockWriter) writeSeries(id uint32, g model.Group, kind model.Kind, pres *enc.Presence, cols [][]byte, start *ctrStart) error {
	if n, ok := bw.gcount[g]; !ok || pres.Len() > n {
		return fmt.Errorf("store: series %d presence %d exceeds group %d length %d", id, pres.Len(), g, n)
	} else if pres.Len() < n {
		p := pres.Clone()
		p.AppendN(false, n-pres.Len())
		pres = &p
	}
	bw.series = append(bw.series, seriesEntry{id, bw.off})
	pb := pres.Bytes()
	sec := make([]byte, 0, 64+len(pb))
	sec = binary.LittleEndian.AppendUint32(sec, id)
	var flags byte
	if start != nil {
		flags |= 1
	}
	sec = append(sec, byte(g), byte(kind), flags, byte(len(cols)))
	if start != nil {
		sec = binary.LittleEndian.AppendUint64(sec, math.Float64bits(start.Offset))
		sec = binary.LittleEndian.AppendUint64(sec, math.Float64bits(start.LastRaw))
		sec = binary.LittleEndian.AppendUint64(sec, uint64(start.LastT))
	}
	sec = binary.AppendUvarint(sec, uint64(len(pb)))
	sec = append(sec, pb...)
	for _, c := range cols {
		sec = binary.AppendUvarint(sec, uint64(len(c)))
		sec = append(sec, c...)
	}
	sec = binary.LittleEndian.AppendUint32(sec, crc32.Checksum(sec, crcTable))
	return bw.write(sec)
}

// writeColSeries encodes a decoded series against a group column.
func (bw *blockWriter) writeColSeries(s *colSeries, groupTS []int64) error {
	if bw == nil {
		return nil
	}
	pres := presenceFor(groupTS, s.T)
	cols := make([][]byte, len(s.Cols))
	for i, c := range s.Cols {
		var e enc.XOREncoder
		for _, v := range c {
			e.Append(v)
		}
		cols[i] = e.Bytes()
	}
	return bw.writeSeries(s.ID, s.Group, s.Kind, &pres, cols, s.Start)
}

// finish writes the footer and trailer, fsyncs and renames into place.
func (bw *blockWriter) finish() error {
	if bw == nil {
		return nil
	}
	if bw.minT == math.MaxInt64 {
		bw.minT, bw.maxT = bw.hdr.Start, bw.hdr.Start
	}
	ft := make([]byte, 0, 1024)
	ft = binary.AppendUvarint(ft, uint64(len(bw.groups)))
	for _, g := range bw.groups {
		ft = append(ft, byte(g.g))
		ft = binary.AppendUvarint(ft, uint64(g.off))
		ft = binary.AppendUvarint(ft, uint64(g.n))
	}
	ft = binary.AppendUvarint(ft, uint64(len(bw.series)))
	for _, s := range bw.series {
		ft = binary.LittleEndian.AppendUint32(ft, s.id)
		ft = binary.AppendUvarint(ft, uint64(s.off))
	}
	ft = binary.LittleEndian.AppendUint64(ft, uint64(bw.minT))
	ft = binary.LittleEndian.AppendUint64(ft, uint64(bw.maxT))
	ft = binary.AppendUvarint(ft, uint64(len(bw.resets)))
	for _, r := range bw.resets {
		ft = binary.AppendVarint(ft, r)
	}
	if err := bw.write(ft); err != nil {
		bw.abort()
		return err
	}
	var tr [trailerSize]byte
	binary.LittleEndian.PutUint32(tr[0:], uint32(len(ft)))
	binary.LittleEndian.PutUint32(tr[4:], bw.crc)
	copy(tr[8:], trailerMagic)
	if _, err := bw.w.Write(tr[:]); err != nil {
		bw.abort()
		return err
	}
	if err := bw.w.Flush(); err != nil {
		bw.abort()
		return err
	}
	if err := bw.f.Sync(); err != nil {
		bw.abort()
		return err
	}
	if err := bw.f.Close(); err != nil {
		os.Remove(bw.tmp)
		return err
	}
	if err := fsx.Rename(bw.tmp, bw.path); err != nil {
		os.Remove(bw.tmp)
		return err
	}
	return fsx.SyncDir(filepath.Dir(bw.path))
}

// presenceFor builds the presence bitmap of sub against the sorted group
// column all. Every element of sub must appear in all.
func presenceFor(all, sub []int64) enc.Presence {
	var p enc.Presence
	j := 0
	for _, t := range all {
		if j < len(sub) && sub[j] == t {
			p.Append(true)
			j++
		} else {
			p.Append(false)
		}
	}
	return p
}

// blockReader reads sections of a block on demand.
type blockReader struct {
	path   string
	f      *os.File
	size   int64
	hdr    blockHeader
	groups map[model.Group]groupEntry
	series map[uint32]int64
	ends   map[int64]int64 // section start -> end
	minT   int64
	maxT   int64
	resets []int64
}

func openBlock(path string) (*blockReader, error) {
	f, err := fsx.OpenShared(path)
	if err != nil {
		return nil, err
	}
	br, err := readBlockMeta(f, path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return br, nil
}

func readBlockMeta(f *os.File, path string) (*blockReader, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size < headerSize+trailerSize {
		return nil, fmt.Errorf("%w: %s too small", errCorrupt, path)
	}
	var h [headerSize]byte
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return nil, err
	}
	if string(h[:4]) != blockMagic {
		return nil, fmt.Errorf("%w: %s bad magic", errCorrupt, path)
	}
	if v := binary.LittleEndian.Uint16(h[4:]); v != blockVersion {
		return nil, fmt.Errorf("store: %s has unsupported block version %d", path, v)
	}
	br := &blockReader{path: path, f: f, size: size, groups: map[model.Group]groupEntry{}, series: map[uint32]int64{}, ends: map[int64]int64{}}
	br.hdr = blockHeader{Tier: Tier(h[6]), Kind: h[7], Node: binary.LittleEndian.Uint32(h[8:]),
		Start: int64(binary.LittleEndian.Uint64(h[12:])), End: int64(binary.LittleEndian.Uint64(h[20:])),
		Gen: binary.LittleEndian.Uint32(h[28:])}
	var tr [trailerSize]byte
	if _, err := f.ReadAt(tr[:], size-trailerSize); err != nil {
		return nil, err
	}
	if string(tr[8:]) != trailerMagic {
		return nil, fmt.Errorf("%w: %s bad trailer", errCorrupt, path)
	}
	flen := int64(binary.LittleEndian.Uint32(tr[0:]))
	fstart := size - trailerSize - flen
	if fstart < headerSize {
		return nil, fmt.Errorf("%w: %s bad footer length", errCorrupt, path)
	}
	ft := make([]byte, flen)
	if _, err := f.ReadAt(ft, fstart); err != nil {
		return nil, err
	}
	rd := &byteReader{b: ft}
	ng := rd.uvarint()
	var offs []int64
	for i := uint64(0); i < ng && rd.err == nil; i++ {
		g := model.Group(rd.byte())
		off := int64(rd.uvarint())
		n := int(min(rd.uvarint(), 1<<31))
		br.groups[g] = groupEntry{g, off, n}
		offs = append(offs, off)
	}
	ns := rd.uvarint()
	for i := uint64(0); i < ns && rd.err == nil; i++ {
		id := rd.u32()
		off := int64(rd.uvarint())
		br.series[id] = off
		offs = append(offs, off)
	}
	br.minT = int64(rd.u64())
	br.maxT = int64(rd.u64())
	nr := rd.uvarint()
	for i := uint64(0); i < nr && rd.err == nil; i++ {
		br.resets = append(br.resets, rd.varint())
	}
	if rd.err != nil {
		return nil, fmt.Errorf("%w: %s footer: %v", errCorrupt, path, rd.err)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	for i, o := range offs {
		end := fstart
		if i+1 < len(offs) {
			end = offs[i+1]
		}
		if o < headerSize || end > fstart || end < o {
			return nil, fmt.Errorf("%w: %s bad section offset", errCorrupt, path)
		}
		br.ends[o] = end
	}
	return br, nil
}

func (br *blockReader) close() error { return br.f.Close() }

func (br *blockReader) section(off int64) ([]byte, error) {
	end, ok := br.ends[off]
	if !ok {
		return nil, errCorrupt
	}
	b := make([]byte, end-off)
	if _, err := br.f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil, err
	}
	if len(b) < 4 {
		return nil, errCorrupt
	}
	body := b[:len(b)-4]
	if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(b[len(b)-4:]) {
		return nil, fmt.Errorf("%w: %s section at %d fails CRC", errCorrupt, br.path, off)
	}
	return body, nil
}

// timestamps decodes one group's timestamp column.
func (br *blockReader) timestamps(g model.Group) ([]int64, error) {
	ge, ok := br.groups[g]
	if !ok {
		return nil, nil
	}
	b, err := br.section(ge.off)
	if err != nil {
		return nil, err
	}
	rd := &byteReader{b: b}
	rd.byte()
	n := int(min(rd.uvarint(), 1<<31))
	l := int(min(rd.uvarint(), 1<<31))
	tb := rd.bytes(l)
	if rd.err != nil {
		return nil, errCorrupt
	}
	return enc.DecodeTS(tb, n)
}

// readSeries decodes one series. groupTS caches decoded group columns.
func (br *blockReader) readSeries(id uint32, groupTS map[model.Group][]int64) (*colSeries, error) {
	off, ok := br.series[id]
	if !ok {
		return nil, nil
	}
	b, err := br.section(off)
	if err != nil {
		return nil, err
	}
	rd := &byteReader{b: b}
	s := &colSeries{ID: rd.u32()}
	s.Group = model.Group(rd.byte())
	s.Kind = model.Kind(rd.byte())
	flags := rd.byte()
	ncols := int(rd.byte())
	if flags&1 != 0 {
		s.Start = &ctrStart{Offset: math.Float64frombits(rd.u64()), LastRaw: math.Float64frombits(rd.u64()), LastT: int64(rd.u64())}
	}
	pl := int(min(rd.uvarint(), 1<<31))
	pb := rd.bytes(pl)
	if rd.err != nil {
		return nil, errCorrupt
	}
	pres, err := enc.DecodePresence(pb)
	if err != nil {
		return nil, err
	}
	ts, ok := groupTS[s.Group]
	if !ok {
		ts, err = br.timestamps(s.Group)
		if err != nil {
			return nil, err
		}
		groupTS[s.Group] = ts
	}
	if pres.Len() != len(ts) {
		return nil, fmt.Errorf("%w: %s series %d presence %d vs %d timestamps", errCorrupt, br.path, id, pres.Len(), len(ts))
	}
	bits := pres.Bits()
	s.T = make([]int64, 0, pres.Count())
	for i, p := range bits {
		if p {
			s.T = append(s.T, ts[i])
		}
	}
	s.Cols = make([][]float64, ncols)
	for c := 0; c < ncols; c++ {
		l := int(min(rd.uvarint(), 1<<31))
		cb := rd.bytes(l)
		if rd.err != nil {
			return nil, errCorrupt
		}
		vals, err := enc.DecodeXOR(cb, pres.Count())
		if err != nil {
			return nil, fmt.Errorf("%w: %s series %d: %v", errCorrupt, br.path, id, err)
		}
		s.Cols[c] = vals
	}
	return s, nil
}

// verify checks the whole-block CRC and every section CRC.
func (br *blockReader) verify() error {
	fstart := br.size - trailerSize
	h := crc32.New(crcTable)
	if _, err := io.Copy(h, io.NewSectionReader(br.f, 0, fstart)); err != nil {
		return err
	}
	var tr [trailerSize]byte
	if _, err := br.f.ReadAt(tr[:], fstart); err != nil {
		return err
	}
	if h.Sum32() != binary.LittleEndian.Uint32(tr[4:]) {
		return fmt.Errorf("%w: %s fails block CRC", errCorrupt, br.path)
	}
	for off := range br.ends {
		if _, err := br.section(off); err != nil {
			return err
		}
	}
	return nil
}

// byteReader reads little-endian and varint fields with sticky errors.
type byteReader struct {
	b   []byte
	err error
}

func (r *byteReader) fail() {
	if r.err == nil {
		r.err = io.ErrUnexpectedEOF
	}
}

func (r *byteReader) byte() byte {
	if len(r.b) < 1 {
		r.fail()
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *byteReader) u32() uint32 {
	if len(r.b) < 4 {
		r.fail()
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *byteReader) u64() uint64 {
	if len(r.b) < 8 {
		r.fail()
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *byteReader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.fail()
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *byteReader) varint() int64 {
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.fail()
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *byteReader) bytes(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.fail()
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *byteReader) str() string {
	n := int(min(r.uvarint(), uint64(len(r.b))+1))
	return string(r.bytes(n))
}

func (bw *blockWriter) setResets(r []int64) {
	if bw != nil {
		bw.resets = r
	}
}
