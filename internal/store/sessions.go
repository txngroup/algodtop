package store

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// aggregateSessions keeps only aggregates of peer addresses older than
// cutoff: the IP becomes its /24 (IPv6 /48) and the exact address and
// peer ID are dropped. Rewriting a file twice changes nothing.
func (db *DB) aggregateSessions(cutoff int64) int {
	l := db.sessions
	cur := l.fileFor(db.now().UnixNano())
	l.mu.Lock()
	if l.name != "" && l.name != cur {
		// An old month still open for appends is closed before its rewrite.
		l.closeLocked()
	}
	l.mu.Unlock()
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") || name == cur {
			continue
		}
		start, err := time.Parse("2006-01", strings.TrimSuffix(name, ".log"))
		if err != nil || start.AddDate(0, 1, 0).UnixNano() > cutoff {
			continue
		}
		path := filepath.Join(l.dir, name)
		var out []byte
		changed := false
		sub := &recLog{dir: l.dir}
		sub.scanFile(path, func(b []byte) {
			var s model.Session
			if json.Unmarshal(b, &s) != nil {
				return
			}
			if agg := aggregateIP(s.IP); agg != s.IP || s.Addr != "" || s.PeerID != "" {
				s.IP, s.Addr, s.PeerID = agg, "", ""
				changed = true
			}
			body, _ := json.Marshal(s)
			out = append(out, encodeRec(body)...)
		})
		if changed && fsx.WriteAtomic(path, out, 0o600) == nil {
			n++
		}
	}
	return n
}

func aggregateIP(ip string) string {
	if strings.Contains(ip, "/") {
		return ip
	}
	p := net.ParseIP(ip)
	if p == nil {
		return "unknown"
	}
	if v4 := p.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String() + "/24"
	}
	m := net.CIDRMask(48, 128)
	return p.Mask(m).String() + "/48"
}
