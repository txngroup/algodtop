// Package api is the JSON-over-HTTP API that both front ends use. The same
// handlers serve the unix socket and the TCP listener.
package api

import (
	"context"
	"time"

	"algod.top/internal/collector"
	"algod.top/internal/health"
	"algod.top/internal/model"
	"algod.top/internal/query"
	"algod.top/internal/store"
)

// NodeView is a node's live status with its health.
type NodeView struct {
	collector.State
	Health   health.Status `json:"health"`
	Summary  string        `json:"summary"`
	Lag      float64       `json:"lag"`
	Firing   []string      `json:"firing,omitempty"`
	Silenced bool          `json:"silenced,omitempty"`
	Remote   string        `json:"remote,omitempty"` // agent host for agent nodes
	History  bool          `json:"history"`          // stored history exists
	// HistoryFrom is the oldest stored sample time, in unix ns.
	HistoryFrom int64 `json:"history_from,omitempty"`
	// MemoryCreep is set when the daily minimum RSS rose three days running.
	MemoryCreep bool `json:"memory_creep,omitempty"`
}

// DaemonStatus is the header line: host, uptime, store size.
type DaemonStatus struct {
	Host    string `json:"host"`
	Version string `json:"version"`
	// VersionTime is when the build's commit was made, Unix ns, or 0.
	VersionTime int64             `json:"version_time,omitempty"`
	Platform    string            `json:"platform"`
	Started     int64             `json:"started"`
	StoreBytes  int64             `json:"store_bytes"`
	TierBytes   map[string]int64  `json:"tier_bytes"`
	Retention   map[string]string `json:"retention"`
	Live        bool              `json:"live"` // no daemon: live only, no history
	Paused      bool              `json:"paused"`
	Self        SelfStats         `json:"self"`
	Agent       bool              `json:"agent,omitempty"`
	ReadOnly    bool              `json:"read_only"`
	Auth        string            `json:"auth"` // none, read, write
	ConfigPath  string            `json:"config_path,omitempty"`
	Warnings    []string          `json:"warnings,omitempty"`
}

// SelfStats are algodtop's own resource figures.
type SelfStats struct {
	CPU        float64 `json:"cpu"` // percent of one core
	RSS        float64 `json:"rss"`
	Heap       float64 `json:"heap"`
	Goroutines int     `json:"goroutines"`
	QueueDepth int     `json:"queue_depth"`
	QueueDrops int64   `json:"queue_drops"`
	WriteBPS   float64 `json:"write_bps"`
	WriteNanos int64   `json:"write_nanos"`
	HeadBytes  int64   `json:"head_bytes"`
	Samples    int64   `json:"samples"`
	Late       int64   `json:"late"`
	Dropped    int64   `json:"dropped"`
	SSEClients int     `json:"sse_clients"`
}

// PeerHistory is one remote address over a range.
type PeerHistory struct {
	IP        string  `json:"ip"`
	Sessions  int     `json:"sessions"`
	Connected float64 `json:"connected_s"`
	// ConnectedMore means a session was open before algodtop looked, so
	// the peer was connected for longer than Connected.
	ConnectedMore bool           `json:"connected_more,omitempty"`
	Reasons       map[string]int `json:"reasons"`
	Resets        int            `json:"resets"`
	Clusters      []int          `json:"clusters,omitempty"`
	BytesIn       int64          `json:"bytes_in,omitempty"`
	BytesOut      int64          `json:"bytes_out,omitempty"`
	Networks      []string       `json:"networks"`
	Directions    []string       `json:"directions"`
	Last          int64          `json:"last"`
}

// Cluster is a group of inbound sessions that ended within a window.
type Cluster struct {
	ID         int             `json:"id"`
	Start      int64           `json:"start"`
	End        int64           `json:"end"`
	IPs        []string        `json:"ips"`
	Reasons    map[string]int  `json:"reasons"`
	Repeat     []string        `json:"repeat_ips,omitempty"` // IPs in more than one cluster
	Sessions   []model.Session `json:"sessions"`
	Protocols  map[string]int  `json:"protocols,omitempty"` // supported-protocols fingerprints
	TrafficIn  int64           `json:"traffic_in,omitempty"`
	TrafficOut int64           `json:"traffic_out,omitempty"`
}

// TemplateCount is one log template over a range.
type TemplateCount struct {
	Level    string  `json:"level"`
	Template string  `json:"template"`
	Count    float64 `json:"count"`
}

// Summary is the compare table row for one metric over one range.
type Summary struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	N    int     `json:"n"`
}

// Hub is what the API needs from the daemon or agent.
type Hub interface {
	Nodes() []NodeView
	Node(name string) (NodeView, bool)
	Status() DaemonStatus
	Store() *store.DB
	Query() *query.Engine
	Silences() []model.Silence
	AddSilence(s model.Silence) (model.Silence, error)
	DeleteSilence(id string) error
	Reload() error
	Snapshot() (string, error)
	Subscribe(nodes map[string]bool, series []Sub) (<-chan Message, func())
	Discovered() []DiscoveredNode
	ConfigJSON() ([]byte, error)
	PutConfig(ctx context.Context, b []byte) error
	AgentBatches(ctx context.Context, since string, wait time.Duration) ([]byte, string, error)
}

// DiscoveredNode is a node found by discovery that is not in the config.
type DiscoveredNode struct {
	Name    string `json:"name"`
	DataDir string `json:"datadir"`
	Running bool   `json:"running"`
	Network string `json:"network"`
}

// Sub subscribes to new samples of one metric on one node.
type Sub struct {
	Node   string
	Metric string
}

// Message is one SSE message.
type Message struct {
	Event string
	Data  any
}

// LiveSample is a new sample pushed over SSE.
type LiveSample struct {
	Node   string            `json:"node"`
	Metric string            `json:"metric"`
	Labels map[string]string `json:"labels"`
	Kind   string            `json:"kind"`
	T      int64             `json:"t"`
	V      float64           `json:"v"`
}
