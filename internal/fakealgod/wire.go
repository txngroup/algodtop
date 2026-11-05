package fakealgod

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"time"
)

// algodtop measures a node's bytes on the wire from the node's sockets. A
// fake that only reports counters has no such traffic, so with
// Options.WireTo it moves the bytes its counters report through a sink in
// another process. Each request to the sink is 16 bytes, the count to
// send back and the payload length as big-endian uint64s, then the
// payload.

// wireOverhead is the share framing and encryption add to app bytes.
const wireOverhead = 1.03

// ServeWireSink answers fakes on ln until ln closes.
func ServeWireSink(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go sinkConn(c)
	}
}

func sinkConn(c net.Conn) {
	defer c.Close()
	zero := make([]byte, 64<<10)
	var hdr [16]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		back := binary.BigEndian.Uint64(hdr[:8])
		if _, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint64(hdr[8:]))); err != nil {
			return
		}
		for back > 0 {
			n := min(back, uint64(len(zero)))
			if _, err := c.Write(zero[:n]); err != nil {
				return
			}
			back -= n
		}
	}
}

// wireLoop sends each second's traffic to the sink at addr.
func (f *Fake) wireLoop(addr string) {
	defer f.wg.Done()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	defer c.Close()
	go io.Copy(io.Discard, c)
	zero := make([]byte, 64<<10)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastIn, lastOut := f.appBytes(time.Now())
	for {
		select {
		case <-f.stop:
			return
		case now := <-t.C:
			in, out := f.appBytes(now)
			dIn, dOut := (in-lastIn)*wireOverhead, (out-lastOut)*wireOverhead
			lastIn, lastOut = in, out
			f.mu.Lock()
			down := f.down
			f.mu.Unlock()
			// A restart starts the counters again from zero.
			if down || dIn < 0 || dOut < 0 {
				continue
			}
			var hdr [16]byte
			binary.BigEndian.PutUint64(hdr[:8], uint64(dIn))
			binary.BigEndian.PutUint64(hdr[8:], uint64(dOut))
			c.SetWriteDeadline(now.Add(2 * time.Second))
			if _, err := c.Write(hdr[:]); err != nil {
				return
			}
			for n := uint64(dOut); n > 0; {
				k := min(n, uint64(len(zero)))
				if _, err := c.Write(zero[:k]); err != nil {
					return
				}
				n -= k
			}
		}
	}
}

// appBytes is the app-level bytes received and sent by t.
func (f *Fake) appBytes(t time.Time) (in, out float64) {
	s := f.counterScale(t)
	return f.appIn * s, f.appOut * s
}

// waves are the periods in seconds and the amplitudes of the sines that
// make up Options.Wave. The amplitudes sum to 0.6, so the rate stays
// between 40 and 160 percent of its mean.
var waves = [...]struct{ period, amp float64 }{{5820, 0.25}, {1380, 0.15}, {378, 0.12}, {102, 0.08}}

// counterScale is how many days of the fixture's counters the process has
// gathered by t, at Load days per day on average.
func (f *Fake) counterScale(t time.Time) float64 {
	f.mu.Lock()
	started := f.started
	f.mu.Unlock()
	if !f.o.Wave {
		return t.Sub(started).Seconds() / 86400 * f.o.Load
	}
	return (f.waveTime(t) - f.waveTime(started)) / 86400 * f.o.Load
}

// waveTime integrates the wave: seconds of mean-rate traffic since the
// epoch. The rate stays positive, so counters never go back.
func (f *Fake) waveTime(t time.Time) float64 {
	s := float64(t.UnixNano()) / 1e9
	v := s
	for i, w := range waves {
		k := 2 * math.Pi / w.period
		v -= w.amp / k * math.Cos(k*s+f.phase[i])
	}
	return v
}
