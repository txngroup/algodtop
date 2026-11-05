// Package enc holds the column encodings used by the chunk store: a bit
// stream, Gorilla XOR float compression, delta-of-delta timestamps and
// run-length presence bitmaps. Every encoder is append-only so the in-memory
// head can use the same bytes a block file stores.
package enc

import "errors"

// ErrShort is returned when a stream ends before a value is complete.
var ErrShort = errors.New("enc: stream too short")

// BitWriter appends bits to a byte slice, most significant bit first.
type BitWriter struct {
	b    []byte
	free uint8 // unused low bits in the last byte, 0..7
}

// Bytes returns the encoded bytes. The final byte may be partly used.
func (w *BitWriter) Bytes() []byte { return w.b }

// Len returns the number of whole or partial bytes written.
func (w *BitWriter) Len() int { return len(w.b) }

// WriteBit appends one bit.
func (w *BitWriter) WriteBit(bit bool) {
	if w.free == 0 {
		w.b = append(w.b, 0)
		w.free = 8
	}
	w.free--
	if bit {
		w.b[len(w.b)-1] |= 1 << w.free
	}
}

// WriteBits appends the low n bits of v, n <= 64.
func (w *BitWriter) WriteBits(v uint64, n int) {
	for n > 0 {
		if w.free == 0 {
			w.b = append(w.b, 0)
			w.free = 8
		}
		take := min(int(w.free), n)
		shift := n - take
		chunk := byte((v >> uint(shift)) & ((1 << uint(take)) - 1))
		w.free -= uint8(take)
		w.b[len(w.b)-1] |= chunk << w.free
		n -= take
	}
}

// BitReader reads bits written by BitWriter.
type BitReader struct {
	b   []byte
	pos int // bit position
}

// NewBitReader returns a reader over b.
func NewBitReader(b []byte) *BitReader { return &BitReader{b: b} }

// ReadBit reads one bit.
func (r *BitReader) ReadBit() (bool, error) {
	if r.pos >= len(r.b)*8 {
		return false, ErrShort
	}
	bit := r.b[r.pos>>3]&(0x80>>uint(r.pos&7)) != 0
	r.pos++
	return bit, nil
}

// ReadBits reads n bits, n <= 64.
func (r *BitReader) ReadBits(n int) (uint64, error) {
	if r.pos+n > len(r.b)*8 {
		return 0, ErrShort
	}
	var v uint64
	for n > 0 {
		off := r.pos & 7
		avail := 8 - off
		take := min(avail, n)
		cur := r.b[r.pos>>3]
		chunk := (cur >> uint(avail-take)) & ((1 << uint(take)) - 1)
		v = v<<uint(take) | uint64(chunk)
		r.pos += take
		n -= take
	}
	return v, nil
}
