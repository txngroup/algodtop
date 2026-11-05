package collector

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// ntpEpochOffset is the seconds between 1900-01-01 and 1970-01-01.
const ntpEpochOffset = 2208988800

// ClockOffset queries an SNTP server and returns the local clock's offset
// from it (positive when the local clock is ahead) and the round trip.
func ClockOffset(ctx context.Context, server string) (offset, rtt time.Duration, err error) {
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "123")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)
	req := make([]byte, 48)
	req[0] = 0x23 // LI 0, version 4, mode 3 (client)
	t1 := time.Now()
	putNTP(req[40:], t1)
	if _, err := conn.Write(req); err != nil {
		return 0, 0, err
	}
	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	t4 := time.Now()
	if err != nil {
		return 0, 0, err
	}
	if n < 48 {
		return 0, 0, errors.New("sntp: short reply")
	}
	if resp[0]&0x07 != 4 && resp[0]&0x07 != 5 {
		return 0, 0, errors.New("sntp: not a server reply")
	}
	if resp[1] == 0 {
		return 0, 0, errors.New("sntp: kiss-of-death reply")
	}
	t2 := getNTP(resp[32:])
	t3 := getNTP(resp[40:])
	// offset = ((t2 - t1) + (t3 - t4)) / 2, positive when the server is
	// ahead. algodtop reports local minus server.
	serverAhead := (t2.Sub(t1) + t3.Sub(t4)) / 2
	rtt = t4.Sub(t1) - t3.Sub(t2)
	return -serverAhead, rtt, nil
}

func putNTP(b []byte, t time.Time) {
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	binary.BigEndian.PutUint32(b[0:], uint32(secs))
	binary.BigEndian.PutUint32(b[4:], uint32(frac))
}

func getNTP(b []byte) time.Time {
	secs := int64(binary.BigEndian.Uint32(b[0:])) - ntpEpochOffset
	frac := int64(binary.BigEndian.Uint32(b[4:]))
	return time.Unix(secs, frac*1e9>>32)
}
