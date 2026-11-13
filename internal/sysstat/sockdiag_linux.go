//go:build linux

package sysstat

import (
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// sockDiag measures per-process TCP bytes through sock_diag over netlink.
// It dumps every TCP socket with tcp_info, maps socket inodes to processes
// through /proc/<pid>/fd, and keeps a cumulative counter per process. Bytes
// of sockets that closed between polls and all UDP or QUIC traffic are
// missed.
//
// sock_diag only sees the sockets of algod.top's own network namespace. A
// process it cannot see gets the interface totals of its own namespace
// instead of a counter that never moves. That covers algod in a container
// while algod.top runs on the host, fds that algod.top may not read, and a
// kernel whose tcp_info has no byte counts.
type sockDiag struct {
	last  map[uint32][2]uint64 // inode -> acked, received at last poll
	total map[int][2]float64   // pid -> cumulative out, in
	miss  map[int]int          // pid -> polls in a row with sockets, none of them in the dump
	blind map[int]string       // pid -> fallback method, once sock_diag cannot see it
	ok    bool

	// Seams for tests; newSockDiag sets the real ones.
	inodes func(pid int) ([]uint32, error)
	netns  func(pid int) string
	dump   func(family uint8) (map[uint32][2]uint64, error)
	ifaces func(pid int) (in, out float64, err error)
}

// blindAfter is how many polls in a row a process must have sockets that
// are all missing from the dump before sock_diag gives up on it. A single
// poll can catch a process that has just started.
const blindAfter = 3

const (
	methodSockDiag    = "sock_diag"
	methodIfaces      = "interface totals"
	methodNetnsIfaces = "netns interface totals"
)

const (
	netlinkInetDiag   = 4
	sockDiagByFamily  = 20
	inetDiagInfo      = 2
	tcpInfoBytesAcked = 120
	tcpInfoBytesRecv  = 128
)

func newSockDiag() *sockDiag {
	d := &sockDiag{last: map[uint32][2]uint64{}, total: map[int][2]float64{}, miss: map[int]int{}, blind: map[int]string{},
		inodes: socketInodes, netns: procNetns, dump: dumpTCP, ifaces: procInterfaces}
	if _, err := dumpTCP(syscall.AF_INET); err == nil {
		d.ok = true
	}
	return d
}

func (d *sockDiag) bytes(pids []int) (map[int]NetIO, error) {
	if !d.ok {
		return nil, errors.New("sock_diag unavailable")
	}
	self := d.netns(os.Getpid())
	inodes := map[int][]uint32{}
	var see []int
	for _, pid := range pids {
		if _, ok := d.blind[pid]; ok {
			continue
		}
		if ns := d.netns(pid); self != "" && ns != "" && ns != self {
			d.blind[pid] = methodNetnsIfaces
			continue
		}
		inos, err := d.inodes(pid)
		if err != nil {
			d.blind[pid] = methodIfaces
			continue
		}
		inodes[pid] = inos
		see = append(see, pid)
	}
	out := map[int]NetIO{}
	if len(see) > 0 {
		socks := map[uint32][2]uint64{}
		for _, fam := range []uint8{syscall.AF_INET, syscall.AF_INET6} {
			m, err := d.dump(fam)
			if err != nil {
				return nil, err
			}
			for k, v := range m {
				socks[k] = v
			}
		}
		next := map[uint32][2]uint64{}
		for _, pid := range see {
			tot := d.total[pid]
			found := 0
			for _, ino := range inodes[pid] {
				cur, ok := socks[ino]
				if !ok {
					continue
				}
				found++
				prev, seen := d.last[ino]
				if seen && cur[0] >= prev[0] && cur[1] >= prev[1] {
					tot[0] += float64(cur[0] - prev[0])
					tot[1] += float64(cur[1] - prev[1])
				} else if !seen {
					tot[0] += float64(cur[0])
					tot[1] += float64(cur[1])
				}
				next[ino] = cur
			}
			d.total[pid] = tot
			if found == 0 && len(inodes[pid]) > 0 {
				// Sockets that the dump does not have: report nothing
				// rather than a counter that did not move.
				if d.miss[pid]++; d.miss[pid] >= blindAfter {
					d.blind[pid] = methodIfaces
				}
				continue
			}
			delete(d.miss, pid)
			out[pid] = NetIO{In: tot[1], Out: tot[0], Method: methodSockDiag}
		}
		d.last = next
	}
	keep := make(map[int]bool, len(pids))
	for _, pid := range pids {
		keep[pid] = true
		if m, ok := d.blind[pid]; ok {
			if in, o, err := d.ifaces(pid); err == nil {
				out[pid] = NetIO{In: in, Out: o, Method: m}
			}
		}
	}
	// A restarted algod has a new PID and gets a fresh look.
	for pid := range d.blind {
		if !keep[pid] {
			delete(d.blind, pid)
		}
	}
	for pid := range d.miss {
		if !keep[pid] {
			delete(d.miss, pid)
		}
	}
	return out, nil
}

// procNetns names the network namespace of a process, or "" when it cannot
// be read.
func procNetns(pid int) string {
	s, _ := os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/net")
	return s
}

// socketInodes lists the socket inodes a process holds. It fails when the
// fds cannot be read, which is the case for another user's process.
func socketInodes(pid int) ([]uint32, error) {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []uint32
	failed := 0
	for _, e := range ents {
		l, err := os.Readlink(dir + "/" + e.Name())
		if err != nil {
			failed++
			continue
		}
		if v, ok := strings.CutPrefix(l, "socket:["); ok {
			n, err := strconv.ParseUint(strings.TrimSuffix(v, "]"), 10, 32)
			if err == nil {
				out = append(out, uint32(n))
			}
		}
	}
	if len(ents) > 0 && failed == len(ents) {
		return nil, errors.New("cannot read the fds of process " + strconv.Itoa(pid))
	}
	return out, nil
}

// dumpTCP returns inode -> (bytes_acked, bytes_received) for one family.
func dumpTCP(family uint8) (map[uint32][2]uint64, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, netlinkInetDiag)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}
	// nlmsghdr (16) + inet_diag_req_v2 (56).
	req := make([]byte, 72)
	ne := binary.NativeEndian
	ne.PutUint32(req[0:], 72)
	ne.PutUint16(req[4:], sockDiagByFamily)
	ne.PutUint16(req[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	ne.PutUint32(req[8:], 1)
	req[16] = family
	req[17] = syscall.IPPROTO_TCP
	req[18] = 1 << (inetDiagInfo - 1)
	ne.PutUint32(req[20:], 0xffffffff) // every state
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}
	out := map[uint32][2]uint64{}
	buf := make([]byte, 64*1024)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return out, nil
			case syscall.NLMSG_ERROR:
				return nil, errors.New("sock_diag: netlink error")
			}
			// inet_diag_msg is 72 bytes; inode at offset 68.
			if len(m.Data) < 72 {
				continue
			}
			ino := ne.Uint32(m.Data[68:])
			attrs := m.Data[72:]
			for len(attrs) >= 4 {
				l := int(ne.Uint16(attrs[0:]))
				t := ne.Uint16(attrs[2:])
				if l < 4 || l > len(attrs) {
					break
				}
				if t == inetDiagInfo && l-4 >= tcpInfoBytesRecv+8 {
					info := attrs[4:l]
					out[ino] = [2]uint64{ne.Uint64(info[tcpInfoBytesAcked:]), ne.Uint64(info[tcpInfoBytesRecv:])}
				}
				attrs = attrs[(l+3)&^3:]
			}
		}
	}
}

var _ = unsafe.Sizeof(0)
