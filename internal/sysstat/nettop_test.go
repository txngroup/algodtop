package sysstat

import (
	"strings"
	"testing"
)

// nettopRun builds nettop -L 1 -J bytes_in,bytes_out -x output.
func nettopRun(lines ...string) string {
	return ",bytes_in,bytes_out,\n" + strings.Join(lines, "\n") + "\n"
}

func TestParseNettop(t *testing.T) {
	out := nettopRun(
		"algod.100,3000,300,",
		"tcp4 192.0.2.1:4160<->198.51.100.7:50001,1000,100,",
		"tcp4 192.0.2.1:4160<->*:*,,,",
		"tcp6 2001:db8::1.4160<->2001:db8::2.51234,2000,200,",
		"udp4 *:4160<->*:*,5,6,",
		"udp4 *:4160<->*:*,7,8,",
		"other.200,99,99,",
		"tcp4 192.0.2.1:9<->198.51.100.7:9,99,99,",
		"algod.300,0,0,",
	)
	m := parseNettop(out, []int{100, 300})
	if len(m) != 2 {
		t.Fatalf("got %d processes, want 2: %v", len(m), m)
	}
	a := m[100]
	if a.proc != [2]float64{3000, 300} {
		t.Errorf("process line %v", a.proc)
	}
	if len(a.socks) != 4 {
		t.Errorf("sockets %v, want the two TCP ones and both UDP ones", a.socks)
	}
	if a.socks["tcp6 2001:db8::1.4160<->2001:db8::2.51234"] != [2]float64{2000, 200} {
		t.Errorf("IPv6 socket %v", a.socks)
	}
	if a.socks["udp4 *:4160<->*:*"] != [2]float64{5, 6} || a.socks["udp4 *:4160<->*:*#2"] != [2]float64{7, 8} {
		t.Errorf("UDP sockets %v", a.socks)
	}
	if len(m[300].socks) != 0 {
		t.Errorf("a socket of another process went to 300: %v", m[300].socks)
	}
}

// A peer that disconnects takes its bytes out of nettop's process line.
// The counter must keep growing, by what the open sockets moved.
func TestNettopCounterSurvivesAClose(t *testing.T) {
	var tr nettopTracker
	pids := []int{100}
	poll := func(lines ...string) NetIO {
		t.Helper()
		return tr.update(parseNettop(nettopRun(lines...), pids), pids)[100]
	}
	a := "tcp4 192.0.2.1:50001<->198.51.100.7:4160"
	b := "tcp4 192.0.2.1:50002<->198.51.100.8:4160"
	c := "tcp4 192.0.2.1:50003<->198.51.100.9:4160"

	got := poll("algod.100,150000000,1100,", a+",100000000,1000,", b+",50000000,100,")
	if got.In != 150000000 || got.Out != 1100 || got.Method != "nettop" {
		t.Fatalf("first poll %+v", got)
	}
	// a closes after its last poll; b grows by 2 MB; c opens with 1 MB.
	// The process line falls from 150 MB to 53 MB.
	got = poll("algod.100,53000000,300,", b+",52000000,200,", c+",1000000,100,")
	if got.In != 153000000 || got.Out != 1300 {
		t.Errorf("after a close: %+v, want in 153000000 out 1300", got)
	}
	// c's name comes back on a new connection with fewer bytes.
	got = poll("algod.100,52500000,250,", b+",52000000,200,", c+",500000,50,")
	if got.In != 153500000 || got.Out != 1350 {
		t.Errorf("after a reused name: %+v, want in 153500000 out 1350", got)
	}
}

func TestNettopForgetsAGonePID(t *testing.T) {
	var tr nettopTracker
	s := "tcp4 192.0.2.1:50001<->198.51.100.7:4160"
	tr.update(parseNettop(nettopRun("algod.100,10,10,", s+",10,10,"), []int{100}), []int{100})
	tr.update(nil, []int{200})
	if len(tr.procs) != 0 {
		t.Fatalf("kept %v", tr.procs)
	}
	got := tr.update(parseNettop(nettopRun("algod.100,4,4,", s+",4,4,"), []int{100}), []int{100})[100]
	if got.In != 4 {
		t.Errorf("a PID seen again starts at its open sockets: %+v", got)
	}
}

// With no socket lines, the process line still counts, as one socket.
func TestNettopWithoutSocketLines(t *testing.T) {
	var tr nettopTracker
	pids := []int{100}
	tr.update(parseNettop(nettopRun("algod.100,1000,100,"), pids), pids)
	got := tr.update(parseNettop(nettopRun("algod.100,1500,150,"), pids), pids)[100]
	if got.In != 1500 || got.Out != 150 {
		t.Errorf("got %+v", got)
	}
}
