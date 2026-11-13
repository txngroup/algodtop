// Package model holds the types shared by collectors, the store, the API
// and both front ends.
package model

import (
	"sort"
	"strings"
	"time"
)

// Kind is how a series is stored and aggregated.
type Kind uint8

// Series kinds.
const (
	KindGauge    Kind = 1
	KindCounter  Kind = 2
	KindHistPart Kind = 3
)

func (k Kind) String() string {
	switch k {
	case KindGauge:
		return "gauge"
	case KindCounter:
		return "counter"
	case KindHistPart:
		return "histogram"
	}
	return "unknown"
}

// IsCumulative reports whether a series kind is a cumulative counter.
func (k Kind) IsCumulative() bool { return k == KindCounter || k == KindHistPart }

// Group is a collector group. Each group samples on its own clock and gets
// its own timestamp column in a block.
type Group uint8

// Collector groups.
const (
	GroupMetrics Group = 1  // algod /metrics
	GroupREST    Group = 2  // status, health, pool, participation
	GroupRound   Group = 3  // round watcher
	GroupProc    Group = 4  // process stats
	GroupNet     Group = 5  // per-process network bytes
	GroupHost    Group = 6  // host stats
	GroupLog     Group = 7  // log-derived counts
	GroupPeers   Group = 8  // peer counts
	GroupSelf    Group = 9  // algodtop's own stats
	GroupBlock   Group = 10 // block headers, fetched after each round
)

// GroupName returns a short name for a group.
func GroupName(g Group) string {
	switch g {
	case GroupMetrics:
		return "metrics"
	case GroupREST:
		return "rest"
	case GroupRound:
		return "round"
	case GroupProc:
		return "proc"
	case GroupNet:
		return "net"
	case GroupHost:
		return "host"
	case GroupLog:
		return "log"
	case GroupPeers:
		return "peers"
	case GroupSelf:
		return "self"
	case GroupBlock:
		return "block"
	}
	return "unknown"
}

// Label is one name=value pair.
type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Labels is a label set sorted by name.
type Labels []Label

// Sort sorts the labels by name.
func (ls Labels) Sort() { sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name }) }

// Get returns a label value, or "".
func (ls Labels) Get(name string) string {
	for _, l := range ls {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

// String renders the canonical form a=b,c=d. Callers sort first.
func (ls Labels) String() string {
	if len(ls) == 0 {
		return ""
	}
	var b strings.Builder
	for i, l := range ls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(escapeLabel(l.Value))
	}
	return b.String()
}

// Map returns the labels as a map.
func (ls Labels) Map() map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, ",=\\") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `,`, `\,`, `=`, `\=`)
	return r.Replace(s)
}

// ParseLabels parses the canonical form written by Labels.String.
func ParseLabels(s string) Labels {
	if s == "" {
		return nil
	}
	var out Labels
	var cur strings.Builder
	var name string
	inValue := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == '=' && !inValue:
			name = cur.String()
			cur.Reset()
			inValue = true
		case c == ',' && inValue:
			out = append(out, Label{name, cur.String()})
			cur.Reset()
			inValue = false
		default:
			cur.WriteByte(c)
		}
	}
	if inValue {
		out = append(out, Label{name, cur.String()})
	}
	return out
}

// Sample is one value for one series.
type Sample struct {
	Name   string
	Labels Labels
	Kind   Kind
	Value  float64
	// LabelKey is Labels.String() when the producer has it cached.
	LabelKey string
}

// Batch is one collector pass for one node and group.
type Batch struct {
	Node    string
	Group   Group
	Time    int64 // unix nanoseconds
	Samples []Sample
	// Reset marks a restart: every counter of the node resets at Time.
	Reset bool
}

// Severity of an event.
const (
	SevInfo     = "info"
	SevWarn     = "warn"
	SevError    = "error"
	SevCritical = "critical"
)

// Event kinds.
const (
	EvRestart         = "restart"
	EvVersion         = "version"
	EvConfigPending   = "config-pending"
	EvConfigEffective = "config-effective"
	EvCatchupStart    = "catchup-start"
	EvCatchupEnd      = "catchup-end"
	EvLogRotation     = "log-rotation"
	EvCounterReset    = "counter-reset"
	EvDisconnect      = "disconnect"
	EvConnect         = "connect"
	EvLogError        = "log-error"
	EvLogWarn         = "log-warn"
	EvAlertFired      = "alert-fired"
	EvAlertRepeated   = "alert-repeated"
	EvAlertCleared    = "alert-cleared"
	EvSilenceStart    = "silence-start"
	EvSilenceEnd      = "silence-end"
	EvMonitorGap      = "monitor-gap"
	EvRetention       = "retention-deletion"
	EvClockJump       = "clock-jump"
	EvNodeDown        = "node-down"
	EvNodeUp          = "node-up"
	EvAnnotation      = "annotation"
)

const (
	// EvAlertUndelivered is a notification that failed on its channel
	// after every retry.
	EvAlertUndelivered = "alert-undelivered"
	// EvConfigError is a part of the config that could not take effect,
	// such as an alert rule that does not parse.
	EvConfigError = "config-error"
)

// Event is something that happened at a point in time.
type Event struct {
	Time     int64          `json:"time"`
	Node     string         `json:"node"`
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Msg      string         `json:"msg"`
	Attrs    map[string]any `json:"attrs,omitempty"`
}

// Session is one peer connection's lifetime.
type Session struct {
	Node      string `json:"node"`
	Network   string `json:"network"`   // ws or p2p
	Direction string `json:"direction"` // in or out
	Addr      string `json:"addr"`      // remote address as reported
	IP        string `json:"ip"`        // parsed remote IP
	PeerID    string `json:"peer_id,omitempty"`
	Start     int64  `json:"start"`
	End       int64  `json:"end,omitempty"` // 0 while open
	Reason    string `json:"reason,omitempty"`
	Source    string `json:"source"` // log or poll
	// OpenBefore marks a session that was already open when algodtop
	// first looked, so Start is later than the real connect.
	OpenBefore bool     `json:"open_before,omitempty"`
	BytesIn    int64    `json:"bytes_in,omitempty"`
	BytesOut   int64    `json:"bytes_out,omitempty"`
	Protocols  []string `json:"protocols,omitempty"`
}

// PeerUsage is one per-peer usage sample from /v2/node/peers.
type PeerUsage struct {
	Time      int64    `json:"time"`
	Key       string   `json:"key"` // session key: peer ID for p2p, address for ws
	Network   string   `json:"network"`
	Direction string   `json:"direction"`
	Addr      string   `json:"addr"`
	BytesIn   int64    `json:"bytes_in"`
	BytesOut  int64    `json:"bytes_out"`
	Streams   []string `json:"streams,omitempty"`
}

// Annotation is a user note over a time range.
type Annotation struct {
	ID      string `json:"id"`
	Node    string `json:"node,omitempty"` // empty means every node
	Start   int64  `json:"start"`
	End     int64  `json:"end,omitempty"`
	Text    string `json:"text"`
	Auto    bool   `json:"auto,omitempty"`
	Silence string `json:"silence,omitempty"` // silence ID carried by this annotation
	Created int64  `json:"created"`
	Author  string `json:"author,omitempty"`
}

// Silence suppresses alert notifications.
type Silence struct {
	ID      string   `json:"id"`
	Nodes   []string `json:"nodes,omitempty"` // empty means every node
	Rules   []string `json:"rules,omitempty"` // empty means every rule
	Start   int64    `json:"start"`
	End     int64    `json:"end"`
	Comment string   `json:"comment"`
	Created int64    `json:"created"`
}

// Active reports whether the silence covers node and rule at t.
func (s Silence) Active(t int64, node, rule string) bool {
	if t < s.Start || t >= s.End {
		return false
	}
	if len(s.Nodes) > 0 && !contains(s.Nodes, node) {
		return false
	}
	if len(s.Rules) > 0 && !contains(s.Rules, rule) {
		return false
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Now returns the current time in unix nanoseconds.
func Now() int64 { return time.Now().UnixNano() }
