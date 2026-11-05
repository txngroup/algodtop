package enc

import "encoding/binary"

// TSEncoder stores int64 timestamps as delta-of-delta zigzag varints. A
// perfectly regular interval costs one byte per sample.
type TSEncoder struct {
	b         []byte
	n         int
	prev      int64
	prevDelta int64
}

// Count returns the number of timestamps written.
func (e *TSEncoder) Count() int { return e.n }

// Bytes returns the encoded stream.
func (e *TSEncoder) Bytes() []byte { return e.b }

// Last returns the newest timestamp, or 0 when empty.
func (e *TSEncoder) Last() int64 { return e.prev }

// Append adds a timestamp. Callers keep timestamps strictly increasing.
func (e *TSEncoder) Append(t int64) {
	switch e.n {
	case 0:
		e.b = binary.AppendVarint(e.b, t)
	default:
		d := t - e.prev
		e.b = binary.AppendVarint(e.b, d-e.prevDelta)
		e.prevDelta = d
	}
	e.prev = t
	e.n++
}

// DecodeTS decodes n timestamps from b.
func DecodeTS(b []byte, n int) ([]int64, error) {
	out := make([]int64, 0, n)
	var prev, delta int64
	for i := 0; i < n; i++ {
		v, k := binary.Varint(b)
		if k <= 0 {
			return out, ErrShort
		}
		b = b[k:]
		if i == 0 {
			prev = v
		} else {
			delta += v
			prev += delta
		}
		out = append(out, prev)
	}
	return out, nil
}

// EncodeTS encodes a whole timestamp slice.
func EncodeTS(ts []int64) []byte {
	var e TSEncoder
	for _, t := range ts {
		e.Append(t)
	}
	return e.Bytes()
}
