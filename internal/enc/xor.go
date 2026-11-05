package enc

import (
	"math"
	"math/bits"
)

// XOREncoder compresses float64 values with the Gorilla XOR scheme. An
// unchanged value costs one bit.
type XOREncoder struct {
	w        BitWriter
	n        int
	prev     uint64
	leading  uint8
	trailing uint8
}

// Count returns the number of values written.
func (e *XOREncoder) Count() int { return e.n }

// Bytes returns the encoded stream.
func (e *XOREncoder) Bytes() []byte { return e.w.Bytes() }

// Append adds one value.
func (e *XOREncoder) Append(v float64) {
	cur := math.Float64bits(v)
	if e.n == 0 {
		e.w.WriteBits(cur, 64)
		e.prev = cur
		e.leading = 0xff
		e.n++
		return
	}
	x := cur ^ e.prev
	e.prev = cur
	e.n++
	if x == 0 {
		e.w.WriteBit(false)
		return
	}
	e.w.WriteBit(true)
	lead := uint8(bits.LeadingZeros64(x))
	trail := uint8(bits.TrailingZeros64(x))
	if lead > 31 {
		lead = 31
	}
	if e.leading != 0xff && lead >= e.leading && trail >= e.trailing {
		e.w.WriteBit(false)
		sig := 64 - int(e.leading) - int(e.trailing)
		e.w.WriteBits(x>>e.trailing, sig)
		return
	}
	e.leading, e.trailing = lead, trail
	e.w.WriteBit(true)
	e.w.WriteBits(uint64(lead), 5)
	sig := 64 - int(lead) - int(trail)
	// sig is 1..64; 64 is stored as 0 in six bits.
	e.w.WriteBits(uint64(sig&63), 6)
	e.w.WriteBits(x>>trail, sig)
}

// DecodeXOR decodes n values from b.
func DecodeXOR(b []byte, n int) ([]float64, error) {
	out := make([]float64, 0, n)
	err := IterXOR(b, n, func(v float64) { out = append(out, v) })
	return out, err
}

// IterXOR calls fn for each of the n values in b.
func IterXOR(b []byte, n int, fn func(float64)) error {
	if n == 0 {
		return nil
	}
	r := NewBitReader(b)
	first, err := r.ReadBits(64)
	if err != nil {
		return err
	}
	prev := first
	fn(math.Float64frombits(prev))
	var leading, trailing uint8
	for i := 1; i < n; i++ {
		ctl, err := r.ReadBit()
		if err != nil {
			return err
		}
		if !ctl {
			fn(math.Float64frombits(prev))
			continue
		}
		fresh, err := r.ReadBit()
		if err != nil {
			return err
		}
		if fresh {
			l, err := r.ReadBits(5)
			if err != nil {
				return err
			}
			s, err := r.ReadBits(6)
			if err != nil {
				return err
			}
			sig := int(s)
			if sig == 0 {
				sig = 64
			}
			leading = uint8(l)
			if int(leading)+sig > 64 {
				return ErrShort
			}
			trailing = uint8(64 - int(leading) - sig)
		}
		sig := 64 - int(leading) - int(trailing)
		if sig <= 0 {
			return ErrShort
		}
		v, err := r.ReadBits(sig)
		if err != nil {
			return err
		}
		prev ^= v << trailing
		fn(math.Float64frombits(prev))
	}
	return nil
}
