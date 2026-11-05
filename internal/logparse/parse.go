// Package logparse turns algod node.log lines into counts, templates and
// events. A byte-level prefilter handles the known noisy lines without
// decoding JSON: agreement trace lines, transaction pool floods and HTTP
// access lines. The rest become message templates, and specific extractors
// turn known lines into events.
package logparse

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Kind of a parsed line.
type Kind int

// Line kinds.
const (
	KindTrace Kind = iota + 1
	KindFlood
	KindHTTP
	KindTemplate
	KindBad
)

// Extract is a structured event pulled from a known line.
type Extract struct {
	Type     string // connect, disconnect, conn-reset, handshake-eof, gossip-stream, outgoing, incoming, catchup-start, catchup-end, panic, fatal, ledger-error
	Addr     string
	Incoming bool
	Reason   string
	Delay    int64 // MessageDelay, ns
	PeerID   string
	Protocol string
	Message  string
	Remote   string
	IP       string // the remote IP when Addr is a host name
}

// Line is one parsed log line.
type Line struct {
	Kind      Kind
	Time      time.Time
	Level     string // info, warning, error, ...
	Template  string
	Msg       string
	TraceType string // agreement message type for trace lines
	AppID     string // app for pool-flood lines, "" if none
	HTTPPath  string
	HTTPCode  string
	Self      bool // a request algodtop made, from algod's request log
	Extract   *Extract
	Round     uint64 // for BlockAccepted / RoundConcluded
}

var (
	traceMarker = []byte(`"file":"trace.go"`)
	floodMarker = []byte(`Pending transaction in pool no longer valid`)
	typeKey     = []byte(`"Type":"`)
	timeKey     = []byte(`"time":"`)
	appKey      = []byte(`app=`)
	roundKey    = []byte(`"Round":`)
)

type rawLine struct {
	Time    string          `json:"time"`
	Level   string          `json:"level"`
	Msg     string          `json:"msg"`
	File    string          `json:"file"`
	Details json.RawMessage `json:"details"`
	Remote  string          `json:"remote"`
	Event   string          `json:"event"`
	Type    string          `json:"Type"`
	Round   uint64          `json:"Round"`
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Status  json.Number     `json:"status"`
	URI     string          `json:"uri"`
}

// Parse classifies one line. It decodes JSON only for lines that survive
// the prefilter.
func Parse(b []byte) Line {
	b = bytes.TrimRight(b, "\r\n")
	if len(b) == 0 || b[0] != '{' {
		return Line{Kind: KindBad}
	}
	if bytes.Contains(b, traceMarker) {
		l := Line{Kind: KindTrace, Level: "info", TraceType: stringAfter(b, typeKey)}
		l.Time = timeOf(b)
		// Only a few useful types are decoded further.
		if l.TraceType == "RoundConcluded" || l.TraceType == "BlockAccepted" {
			l.Round = uintAfter(b, roundKey)
		}
		return l
	}
	if bytes.Contains(b, floodMarker) {
		l := Line{Kind: KindFlood, Level: "info", Time: timeOf(b)}
		if i := bytes.Index(b, appKey); i >= 0 {
			j := i + len(appKey)
			k := j
			for k < len(b) && b[k] >= '0' && b[k] <= '9' {
				k++
			}
			l.AppID = string(b[j:k])
		}
		return l
	}
	var r rawLine
	if err := json.Unmarshal(b, &r); err != nil {
		return Line{Kind: KindBad}
	}
	l := Line{Level: normLevel(r.Level), Msg: r.Msg}
	if t, ok := fastTime([]byte(r.Time)); ok {
		l.Time = t
	} else if t, err := time.Parse(time.RFC3339Nano, r.Time); err == nil {
		l.Time = t
	}
	if r.Method != "" && (r.Path != "" || r.URI != "") {
		l.Kind = KindHTTP
		l.HTTPPath = r.Path
		if l.HTTPPath == "" {
			l.HTTPPath = r.URI
		}
		if i := strings.IndexByte(l.HTTPPath, '?'); i >= 0 {
			l.HTTPPath = l.HTTPPath[:i]
		}
		l.HTTPPath = Template(l.HTTPPath)
		l.HTTPCode = r.Status.String()
		return l
	}
	if r.File == "logger.go" {
		if path, code, agent, ok := accessLine(r.Msg); ok {
			l.Kind = KindHTTP
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			l.HTTPPath = Template(path)
			l.HTTPCode = code
			l.Self = agent == "algodtop"
			return l
		}
	}
	l.Kind = KindTemplate
	l.Template = Template(r.Msg)
	l.Extract = extract(&r)
	if r.Msg == "/Agreement/BlockAccepted" || r.Type == "RoundConcluded" {
		l.Round = r.Round
	}
	return l
}

func normLevel(s string) string {
	switch s {
	case "warn", "warning":
		return "warning"
	case "err", "error":
		return "error"
	case "":
		return "info"
	}
	return s
}

func timeOf(b []byte) time.Time {
	if t, ok := fastTime(bytesAfter(b, timeKey)); ok {
		return t
	}
	t, _ := time.Parse(time.RFC3339Nano, stringAfter(b, timeKey))
	return t
}

// bytesAfter is stringAfter without the allocation.
func bytesAfter(b, key []byte) []byte {
	i := bytes.Index(b, key)
	if i < 0 {
		return nil
	}
	j := i + len(key)
	k := bytes.IndexByte(b[j:], '"')
	if k < 0 {
		return nil
	}
	return b[j : j+k]
}

// fastTime parses algod's fixed layout 2006-01-02T15:04:05.999999Z by
// position. Trace lines are most of the log, and time.Parse is slow.
func fastTime(s []byte) (time.Time, bool) {
	if len(s) < 20 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' || s[len(s)-1] != 'Z' {
		return time.Time{}, false
	}
	num := func(p []byte) (int, bool) {
		v := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, false
			}
			v = v*10 + int(c-'0')
		}
		return v, true
	}
	y, ok1 := num(s[0:4])
	mo, ok2 := num(s[5:7])
	d, ok3 := num(s[8:10])
	h, ok4 := num(s[11:13])
	mi, ok5 := num(s[14:16])
	se, ok6 := num(s[17:19])
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) {
		return time.Time{}, false
	}
	ns := 0
	if rest := s[19 : len(s)-1]; len(rest) > 0 {
		if rest[0] != '.' || len(rest) > 10 {
			return time.Time{}, false
		}
		frac, ok := num(rest[1:])
		if !ok {
			return time.Time{}, false
		}
		for i := len(rest) - 1; i < 9; i++ {
			frac *= 10
		}
		ns = frac
	}
	return time.Date(y, time.Month(mo), d, h, mi, se, ns, time.UTC), true
}

func stringAfter(b, key []byte) string {
	i := bytes.Index(b, key)
	if i < 0 {
		return ""
	}
	j := i + len(key)
	k := bytes.IndexByte(b[j:], '"')
	if k < 0 {
		return ""
	}
	return string(b[j : j+k])
}

func uintAfter(b, key []byte) uint64 {
	i := bytes.Index(b, key)
	if i < 0 {
		return 0
	}
	j := i + len(key)
	k := j
	for k < len(b) && b[k] >= '0' && b[k] <= '9' {
		k++
	}
	v, _ := strconv.ParseUint(string(b[j:k]), 10, 64)
	return v
}

type netDetails struct {
	Address      string `json:"Address"`
	Incoming     bool   `json:"Incoming"`
	Reason       string `json:"Reason"`
	MessageDelay int64  `json:"MessageDelay"`
	Endpoint     string `json:"Endpoint"`
}

// extract recognises the known lines. Each extractor has a fixture from a
// real log in the tests.
func extract(r *rawLine) *Extract {
	switch r.Msg {
	case "/Network/ConnectPeer", "/Network/DisconnectPeer":
		var d netDetails
		if json.Unmarshal(r.Details, &d) != nil {
			return nil
		}
		e := &Extract{Addr: d.Address, Incoming: d.Incoming, Reason: d.Reason, Delay: d.MessageDelay}
		// For an outgoing ws peer, Address is the bare IP and Endpoint the
		// host:port algod dialled, which is what /v2/node/peers reports.
		if d.Endpoint != "" && !strings.HasPrefix(d.Address, "/") {
			e.Addr, e.IP = d.Endpoint, d.Address
		}
		if r.Msg == "/Network/ConnectPeer" {
			e.Type = "connect"
		} else {
			e.Type = "disconnect"
		}
		return e
	}
	m := r.Msg
	switch {
	case strings.HasPrefix(m, "Made outgoing connection to peer "):
		return &Extract{Type: "outgoing", PeerID: strings.TrimPrefix(m, "Made outgoing connection to peer "), Addr: r.Remote, Remote: r.Remote}
	case strings.HasPrefix(m, "Accepted incoming connection from peer "):
		return &Extract{Type: "incoming", Incoming: true, PeerID: strings.TrimPrefix(m, "Accepted incoming connection from peer "), Addr: r.Remote, Remote: r.Remote}
	case strings.Contains(m, ": using protocol ") && strings.Contains(m, " with peer "):
		i := strings.Index(m, ": using protocol ")
		rest := m[i+len(": using protocol "):]
		proto, peer, _ := strings.Cut(rest, " with peer ")
		return &Extract{Type: "gossip-stream", Protocol: proto, PeerID: peer}
	case strings.HasPrefix(m, "wsStreamHandlerV22: error reading peer meta headers"):
		e := &Extract{Type: "handshake-eof", Message: m}
		if i := strings.Index(m, " ("); i >= 0 {
			if j := strings.Index(m[i:], ")"); j > 0 {
				e.Addr = m[i+2 : i+j]
			}
		}
		if i := strings.Index(m, "from peer "); i >= 0 {
			f := strings.Fields(m[i+len("from peer "):])
			if len(f) > 0 {
				e.PeerID = f[0]
			}
		}
		return e
	case strings.HasPrefix(m, "peer[") && (strings.Contains(m, " read err: ") || strings.Contains(m, " write err: ")):
		addr := m[len("peer["):]
		if i := strings.IndexByte(addr, ']'); i >= 0 {
			addr = addr[:i]
		}
		e := &Extract{Type: "peer-error", Addr: addr, Message: m}
		if strings.Contains(m, "connection reset by peer") {
			e.Type = "conn-reset"
		}
		return e
	case strings.Contains(m, "connection reset by peer"):
		return &Extract{Type: "conn-reset", Message: m}
	case strings.HasPrefix(m, "Catchup Service: ") && strings.Contains(strings.ToLower(m), "start"):
		return &Extract{Type: "catchup-start", Message: m}
	case strings.HasPrefix(m, "Catchup Service: ") && (strings.Contains(m, "done") || strings.Contains(m, "finished") || strings.Contains(m, "stopped")):
		return &Extract{Type: "catchup-end", Message: m}
	}
	switch r.Level {
	case "panic":
		return &Extract{Type: "panic", Message: m}
	case "fatal":
		return &Extract{Type: "fatal", Message: m}
	}
	if r.Level == "error" && (strings.Contains(r.File, "ledger") || strings.Contains(strings.ToLower(m), "ledger")) {
		return &Extract{Type: "ledger-error", Message: m}
	}
	return nil
}

// accessLine parses the message of algod's request log, written by its
// API logger middleware:
//
//	127.0.0.1:53709 - - [2026-11-03 17:56:51.718174 +0000 GMT m=+88600.37] "GET /v2/status HTTP/1.1" 200 879 "algodtop" 69us
func accessLine(msg string) (path, code, agent string, ok bool) {
	i := strings.Index(msg, `] "`)
	if i < 0 {
		return "", "", "", false
	}
	rest := msg[i+3:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return "", "", "", false
	}
	req := strings.Fields(rest[:j])
	if len(req) < 2 {
		return "", "", "", false
	}
	rest = rest[j+1:]
	if f := strings.Fields(rest); len(f) > 0 {
		code = f[0]
	}
	if a, b, found := strings.Cut(rest, `"`); found && a != "" {
		agent, _, _ = strings.Cut(b, `"`)
	}
	return req[1], code, agent, true
}
