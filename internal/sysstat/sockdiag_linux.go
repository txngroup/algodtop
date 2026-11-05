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
type sockDiag struct {
	last  map[uint32][2]uint64 // inode -> acked, received at last poll
	total map[int][2]float64   // pid -> cumulative out, in
	ok    bool
}

const (
	netlinkInetDiag   = 4
	sockDiagByFamily  = 20
	inetDiagInfo      = 2
	tcpInfoBytesAcked = 120
	tcpInfoBytesRecv  = 128
)

func newSockDiag() *sockDiag {
	d := &sockDiag{last: map[uint32][2]uint64{}, total: map[int][2]float64{}}
	if _, err := dumpTCP(syscall.AF_INET); err == nil {
		d.ok = true
	}
	return d
}

func (d *sockDiag) bytes(pids []int) (map[int]NetIO, error) {
	if !d.ok {
		return nil, errors.New("sock_diag unavailable")
	}
	socks := map[uint32][2]uint64{}
	for _, fam := range []uint8{syscall.AF_INET, syscall.AF_INET6} {
		m, err := dumpTCP(fam)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			socks[k] = v
		}
	}
	out := map[int]NetIO{}
	next := map[uint32][2]uint64{}
	for _, pid := range pids {
		inodes := socketInodes(pid)
		tot := d.total[pid]
		for _, ino := range inodes {
			cur, ok := socks[ino]
			if !ok {
				continue
			}
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
		out[pid] = NetIO{In: tot[1], Out: tot[0], Method: "sock_diag"}
	}
	d.last = next
	return out, nil
}

func socketInodes(pid int) []uint32 {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []uint32
	for _, e := range ents {
		l, err := os.Readlink(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		if v, ok := strings.CutPrefix(l, "socket:["); ok {
			n, err := strconv.ParseUint(strings.TrimSuffix(v, "]"), 10, 32)
			if err == nil {
				out = append(out, uint32(n))
			}
		}
	}
	return out
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
