package enc

import (
	"math"
	"math/rand"
	"testing"
)

func TestXORRoundTrip(t *testing.T) {
	cases := [][]float64{
		{0},
		{1, 1, 1, 1},
		{1, 2, 3, 4, 5, 1e9, -3.5, math.Inf(1), math.NaN(), 0, math.SmallestNonzeroFloat64},
		{math.MaxFloat64, -math.MaxFloat64, 12345.678, 12345.679},
	}
	r := rand.New(rand.NewSource(1))
	var big []float64
	v := 1000.0
	for i := 0; i < 5000; i++ {
		if r.Intn(4) == 0 {
			v += float64(r.Intn(100))
		}
		big = append(big, v)
	}
	cases = append(cases, big)
	for _, c := range cases {
		var e XOREncoder
		for _, v := range c {
			e.Append(v)
		}
		got, err := DecodeXOR(e.Bytes(), len(c))
		if err != nil {
			t.Fatal(err)
		}
		for i := range c {
			if math.Float64bits(got[i]) != math.Float64bits(c[i]) {
				t.Fatalf("value %d: got %v want %v", i, got[i], c[i])
			}
		}
	}
}

func TestXORConstantIsSmall(t *testing.T) {
	var e XOREncoder
	for i := 0; i < 720; i++ {
		e.Append(42)
	}
	// 8 bytes for the first value plus one bit per repeat.
	if n := len(e.Bytes()); n > 8+720/8+1 {
		t.Fatalf("constant series used %d bytes", n)
	}
}

func TestTSRoundTrip(t *testing.T) {
	var ts []int64
	cur := int64(1793709677_000_000_000)
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 1000; i++ {
		cur += 10_000_000_000 + int64(r.Intn(5_000_000))
		ts = append(ts, cur)
	}
	got, err := DecodeTS(EncodeTS(ts), len(ts))
	if err != nil {
		t.Fatal(err)
	}
	for i := range ts {
		if got[i] != ts[i] {
			t.Fatalf("ts %d: %d != %d", i, got[i], ts[i])
		}
	}
	regular := make([]int64, 720)
	for i := range regular {
		regular[i] = int64(i) * 10_000_000_000
	}
	if n := len(EncodeTS(regular)); n > 720+8 {
		t.Fatalf("regular ts used %d bytes", n)
	}
}

func TestPresence(t *testing.T) {
	bits := []bool{false, false, true, true, true, false, true, false, false}
	p := PresenceFromBits(bits)
	if p.Count() != 4 || p.Len() != 9 {
		t.Fatalf("count %d len %d", p.Count(), p.Len())
	}
	q, err := DecodePresence(p.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got := q.Bits()
	for i := range bits {
		if got[i] != bits[i] {
			t.Fatalf("bit %d", i)
		}
	}
	var a Presence
	a.AppendN(false, 3)
	a.AppendN(true, 5)
	a.Append(true)
	if a.Count() != 6 || a.Len() != 9 {
		t.Fatalf("AppendN count %d len %d", a.Count(), a.Len())
	}
	var all Presence
	all.AppendN(true, 720)
	if len(all.Bytes()) > 4 {
		t.Fatalf("all-present bitmap is %d bytes", len(all.Bytes()))
	}
}

func FuzzDecodeXOR(f *testing.F) {
	var e XOREncoder
	for _, v := range []float64{1, 2, 2, 3.5} {
		e.Append(v)
	}
	f.Add(e.Bytes(), 4)
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		if n < 0 || n > 10000 {
			return
		}
		_, _ = DecodeXOR(b, n)
	})
}

func FuzzDecodeTS(f *testing.F) {
	f.Add(EncodeTS([]int64{1, 2, 3}), 3)
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		if n < 0 || n > 10000 {
			return
		}
		_, _ = DecodeTS(b, n)
	})
}

func FuzzDecodePresence(f *testing.F) {
	p := PresenceFromBits([]bool{true, false, true})
	f.Add(p.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = DecodePresence(b)
	})
}
