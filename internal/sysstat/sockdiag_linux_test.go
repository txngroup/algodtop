//go:build linux

package sysstat

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// sock_diag counts the bytes of the test process's own TCP connection.
func TestSockDiagOwnSockets(t *testing.T) {
	d := newSockDiag()
	if !d.ok {
		t.Skip("sock_diag unavailable")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			io.Copy(io.Discard, c)
		}
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid := os.Getpid()
	m, err := d.bytes([]int{pid})
	if err != nil {
		t.Fatal(err)
	}
	before := m[pid]
	if before.Method != methodSockDiag {
		t.Fatalf("method %q", before.Method)
	}
	if _, err := c.Write(make([]byte, 100000)); err != nil {
		t.Fatal(err)
	}
	m, _ = d.bytes([]int{pid})
	// Both ends belong to this process, so the bytes count once sent and
	// once received.
	if got := m[pid].Out - before.Out; got < 100000 {
		t.Errorf("out grew by %v, want at least 100000", got)
	}
}

// A process that sock_diag cannot see gets the interface totals of its
// own namespace, rather than a counter stuck at zero.
func TestSockDiagFallsBack(t *testing.T) {
	const (
		container = 10 // algod in another network namespace
		otherUser = 11 // fds algod.top may not read
		hidden    = 12 // sockets the dump never has, such as on an old kernel
		visible   = 13 // a normal local algod
	)
	d := &sockDiag{last: map[uint32][2]uint64{}, total: map[int][2]float64{}, miss: map[int]int{}, blind: map[int]string{}, ok: true}
	d.netns = func(pid int) string {
		if pid == container {
			return "net:[2]"
		}
		return "net:[1]"
	}
	d.inodes = func(pid int) ([]uint32, error) {
		switch pid {
		case otherUser:
			return nil, syscall.EACCES
		case hidden:
			return []uint32{120, 121}, nil
		case visible:
			return []uint32{130}, nil
		case container:
			t.Error("read the fds of a process in another namespace")
		}
		return nil, nil
	}
	sent := uint64(0)
	d.dump = func(fam uint8) (map[uint32][2]uint64, error) {
		if fam != syscall.AF_INET {
			return nil, nil
		}
		sent += 500
		return map[uint32][2]uint64{130: {sent, 2 * sent}, 999: {1, 1}}, nil
	}
	d.ifaces = func(pid int) (float64, float64, error) {
		if pid == visible {
			return 0, 0, errors.New("interface totals asked for a visible process")
		}
		return float64(pid) * 1000, float64(pid) * 100, nil
	}
	pids := []int{container, otherUser, hidden, visible}
	want := func(poll int, m map[int]NetIO, pid int, method string, in, out float64) {
		t.Helper()
		v, ok := m[pid]
		if method == "" {
			if ok {
				t.Errorf("poll %d pid %d: got %+v, want nothing", poll, pid, v)
			}
			return
		}
		if !ok || v.Method != method || v.In != in || v.Out != out {
			t.Errorf("poll %d pid %d: got %+v (present %v), want %s %v/%v", poll, pid, v, ok, method, in, out)
		}
	}
	for poll := 1; poll <= blindAfter; poll++ {
		m, err := d.bytes(pids)
		if err != nil {
			t.Fatal(err)
		}
		want(poll, m, container, methodNetnsIfaces, 10000, 1000)
		want(poll, m, otherUser, methodIfaces, 11000, 1100)
		if poll < blindAfter {
			want(poll, m, hidden, "", 0, 0)
		} else {
			want(poll, m, hidden, methodIfaces, 12000, 1200)
		}
		want(poll, m, visible, methodSockDiag, float64(2*sent), float64(sent))
	}
	// Once algod restarts under a new PID, the old one is forgotten.
	d.bytes([]int{visible})
	if len(d.blind) != 0 || len(d.miss) != 0 {
		t.Errorf("kept state for gone PIDs: blind %v miss %v", d.blind, d.miss)
	}
}
