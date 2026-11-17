//go:build darwin

package sysstat

import (
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
)

// The real nettop, against the test process: the counter must not fall
// when a connection that moved bytes closes.
func TestNettopOwnSockets(t *testing.T) {
	if _, err := exec.LookPath("nettop"); err != nil {
		t.Skip("no nettop")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	big, small := dial(), dial()
	defer small.Close()
	buf := make([]byte, 1<<20)
	for i := 0; i < 4; i++ {
		if _, err := big.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	small.Write(buf[:1000])

	c := New()
	defer c.Close()
	pid := os.Getpid()
	first, ok := c.NetBytes([]int{pid})[pid]
	if !ok {
		t.Skip("nettop does not list this process")
	}
	if first.Out < 4<<20 {
		t.Skipf("nettop saw %v bytes out, want at least 4 MiB", first.Out)
	}
	big.Close()
	small.Write(buf[:1000])
	second := c.NetBytes([]int{pid})[pid]
	if second.Out < first.Out || second.In < first.In {
		t.Errorf("counter fell after a close: %+v then %+v", first, second)
	}
}
