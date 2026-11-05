package enc

import "encoding/binary"

// Presence is a run-length bitmap of which group timestamps carry a value
// for a series. Runs alternate present, absent, present, ... and the first
// run may be empty. A series present at every timestamp costs a few bytes.
type Presence struct {
	runs []uint32
	n    int // total bits
	set  int // present bits
}

// Len returns the number of timestamps covered.
func (p *Presence) Len() int { return p.n }

// Count returns the number of present samples.
func (p *Presence) Count() int { return p.set }

// Append adds one bit.
func (p *Presence) Append(present bool) {
	// Even run indexes are present runs, odd are absent.
	want := 0
	if !present {
		want = 1
	}
	if len(p.runs) == 0 {
		if !present {
			p.runs = append(p.runs, 0)
		}
		p.runs = append(p.runs, 1)
	} else if (len(p.runs)-1)%2 == want {
		p.runs[len(p.runs)-1]++
	} else {
		p.runs = append(p.runs, 1)
	}
	p.n++
	if present {
		p.set++
	}
}

// AppendN adds n copies of the same bit.
func (p *Presence) AppendN(present bool, n int) {
	if n <= 0 {
		return
	}
	p.Append(present)
	if n > 1 {
		p.runs[len(p.runs)-1] += uint32(n - 1)
		p.n += n - 1
		if present {
			p.set += n - 1
		}
	}
}

// Bytes encodes the runs as uvarints.
func (p *Presence) Bytes() []byte {
	out := make([]byte, 0, 2+len(p.runs)*2)
	out = binary.AppendUvarint(out, uint64(len(p.runs)))
	for _, r := range p.runs {
		out = binary.AppendUvarint(out, uint64(r))
	}
	return out
}

// Bits expands the bitmap to a bool slice.
func (p *Presence) Bits() []bool {
	out := make([]bool, 0, p.n)
	for i, r := range p.runs {
		for j := uint32(0); j < r; j++ {
			out = append(out, i%2 == 0)
		}
	}
	return out
}

// Clone returns an independent copy.
func (p *Presence) Clone() Presence {
	return Presence{runs: append([]uint32(nil), p.runs...), n: p.n, set: p.set}
}

// DecodePresence decodes a bitmap written by Bytes.
func DecodePresence(b []byte) (Presence, error) {
	var p Presence
	cnt, k := binary.Uvarint(b)
	if k <= 0 {
		return p, ErrShort
	}
	b = b[k:]
	if cnt > uint64(len(b)) {
		return p, ErrShort
	}
	p.runs = make([]uint32, 0, cnt)
	for i := uint64(0); i < cnt; i++ {
		r, k := binary.Uvarint(b)
		if k <= 0 || r > 1<<31 {
			return p, ErrShort
		}
		b = b[k:]
		p.runs = append(p.runs, uint32(r))
		p.n += int(r)
		if i%2 == 0 {
			p.set += int(r)
		}
	}
	return p, nil
}

// PresenceFromBits builds a bitmap from a bool slice.
func PresenceFromBits(bits []bool) Presence {
	var p Presence
	for _, b := range bits {
		p.Append(b)
	}
	return p
}
