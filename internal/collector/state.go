package collector

import (
	"math"
	"sort"
	"time"

	"algod.top/internal/model"
)

// State is a node's live status, kept in memory. Health checks and alerts
// run from it, never from stored data.
type State struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"` // local, rest, ssh, agent
	Host        string `json:"host,omitempty"`
	DataDir     string `json:"datadir,omitempty"`
	URL         string `json:"url,omitempty"`
	Network     string `json:"network,omitempty"`
	GenesisID   string `json:"genesis_id,omitempty"`
	GenesisHash string `json:"genesis_hash,omitempty"`

	Up        bool   `json:"up"`
	Healthy   bool   `json:"healthy"`
	Ready     bool   `json:"ready"`
	LastError string `json:"last_error,omitempty"`
	DownSince int64  `json:"down_since,omitempty"`
	Reachable bool   `json:"reachable"` // REST answered at all

	Version    string `json:"version,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Channel    string `json:"channel,omitempty"` // stable, beta, dev, ...
	PID        int    `json:"pid,omitempty"`
	StartTime  int64  `json:"start_time,omitempty"` // process start, unix ns
	UpSince    int64  `json:"up_since,omitempty"`   // first good scrape since restart
	AuthOff    bool   `json:"auth_disabled,omitempty"`
	HasAdmin   bool   `json:"has_admin,omitempty"`
	PeerUsage  bool   `json:"peer_usage,omitempty"`
	ConfigHash string `json:"config_hash,omitempty"`

	Round       uint64  `json:"round"`
	RoundAt     int64   `json:"round_at"` // local arrival, unix ns
	StatusLag   float64 `json:"status_lag"`
	StatusErr   string  `json:"status_error,omitempty"`       // why /v2/status fails, "" once it answers
	StatusErrAt int64   `json:"status_error_since,omitempty"` // first failure in a row, unix ns
	RoundTime   float64 `json:"round_time"`                   // mean of recent gaps, s
	RoundP99    float64 `json:"round_p99"`
	Catchup     bool    `json:"catchup"`
	TxnsPerBlk  float64 `json:"tpb"`
	PoolSize    float64 `json:"pool"`
	OnlineMoney float64 `json:"online_money,omitempty"`

	Peers     PeerCounts `json:"peers"`
	IdlePeers int        `json:"idle_peers"`
	PeerList  []PeerInfo `json:"-"`

	CPU        float64 `json:"cpu"` // percent of one core
	RSS        float64 `json:"rss"`
	Threads    float64 `json:"threads"`
	Handles    float64 `json:"handles"`
	NetIn      float64 `json:"net_in"`  // bytes/s
	NetOut     float64 `json:"net_out"` // bytes/s
	NetMethod  string  `json:"net_method,omitempty"`
	WSIn       float64 `json:"ws_in"` // app-level bytes/s from algod counters
	WSOut      float64 `json:"ws_out"`
	P2PIn      float64 `json:"p2p_in"`
	P2POut     float64 `json:"p2p_out"`
	DiskFree   float64 `json:"disk_free"`
	DiskTotal  float64 `json:"disk_total"`
	DataSize   float64 `json:"data_size"`
	DiskDays   float64 `json:"disk_days,omitempty"` // days until full, 0 = unknown
	ClockOff   float64 `json:"clock_offset"`
	ClockKnown bool    `json:"clock_known"`
	Load1      float64 `json:"load1"`

	Keys     []PartKeyState `json:"keys,omitempty"`
	Accounts []AccountState `json:"accounts,omitempty"`

	ErrorRate     float64  `json:"error_rate"` // error log lines per minute
	ErrorBaseline float64  `json:"error_baseline"`
	MissedScrapes int64    `json:"missed_scrapes"`
	Unavailable   []string `json:"unavailable,omitempty"` // stats not available on the platform

	LastEvent        *model.Event `json:"last_event,omitempty"`
	ConfigChanged    *model.Event `json:"config_changed,omitempty"`
	ClockOffsetAgent float64      `json:"agent_clock_offset,omitempty"`
}

// Lag returns seconds since the last round, from local arrival when known.
// A node whose status never answered has no round at all, so its lag runs
// from the first failure. Otherwise a rejected token would read as a lag
// of 0 and the node would never count as stalled.
func (s *State) Lag(now time.Time) float64 {
	if s.RoundAt == 0 {
		if s.StatusErrAt != 0 {
			return now.Sub(time.Unix(0, s.StatusErrAt)).Seconds()
		}
		return s.StatusLag
	}
	return now.Sub(time.Unix(0, s.RoundAt)).Seconds()
}

// PeerCounts counts peers by network and direction.
type PeerCounts struct {
	WSIn   int `json:"ws_in"`
	WSOut  int `json:"ws_out"`
	P2PIn  int `json:"p2p_in"`
	P2POut int `json:"p2p_out"`
}

// Total returns all peers.
func (p PeerCounts) Total() int { return p.WSIn + p.WSOut + p.P2PIn + p.P2POut }

// Out returns outgoing peers.
func (p PeerCounts) Out() int { return p.WSOut + p.P2POut }

// PeerInfo is one current peer.
type PeerInfo struct {
	Addr      string   `json:"addr"`
	IP        string   `json:"ip"`
	Network   string   `json:"network"`
	Direction string   `json:"direction"`
	PeerID    string   `json:"peer_id,omitempty"`
	Since     int64    `json:"since"`
	BytesIn   int64    `json:"bytes_in,omitempty"`
	BytesOut  int64    `json:"bytes_out,omitempty"`
	RateIn    float64  `json:"rate_in,omitempty"`
	RateOut   float64  `json:"rate_out,omitempty"`
	Streams   []string `json:"streams,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
	HasUsage  bool     `json:"has_usage,omitempty"`
	// RateUnknown means the rates need another poll: the peer is new, or
	// its totals went down. The flag is negative so that a front end
	// reading an older daemon, which never sends it, still shows rates.
	RateUnknown bool `json:"rate_unknown,omitempty"`
	// SinceBefore means the peer was connected before algodtop first
	// looked, so Since is a lower bound on the time connected.
	SinceBefore bool `json:"since_before,omitempty"`
}

// PartKeyState is one installed participation key.
type PartKeyState struct {
	ID           string  `json:"id"`
	Address      string  `json:"address"`
	First        uint64  `json:"first"`
	Last         uint64  `json:"last"`
	VoteKey      string  `json:"vote_key"`
	LastVote     uint64  `json:"last_vote"`
	LastProposal uint64  `json:"last_proposal"`
	RoundsLeft   int64   `json:"rounds_left"`
	DaysLeft     float64 `json:"days_left"`
	// Active is the key the account votes with. Superseded is a key that
	// ends before the active key of the same account, so its expiry does
	// not matter.
	Active     bool `json:"active,omitempty"`
	Superseded bool `json:"superseded,omitempty"`
}

// AccountState is one listed account's on-chain view.
type AccountState struct {
	Address           string  `json:"address"`
	Status            string  `json:"status"` // Online, Offline, Suspended, NotParticipating
	WasOnline         bool    `json:"was_online"`
	Balance           uint64  `json:"balance"`
	VoteKey           string  `json:"vote_key,omitempty"`
	KeyMatches        bool    `json:"key_matches"`
	IncentiveEligible bool    `json:"incentive_eligible"`
	WasEligible       bool    `json:"was_eligible"`
	LastProposed      uint64  `json:"last_proposed"`
	LastHeartbeat     uint64  `json:"last_heartbeat"`
	LastVote          uint64  `json:"last_vote"`
	ExpectedInterval  float64 `json:"expected_interval"` // rounds between votes
	RoundsSinceVote   int64   `json:"rounds_since_vote"`
}

// ring keeps the last n float values.
type ring struct {
	v []float64
	n int
}

func (r *ring) add(x float64) {
	r.v = append(r.v, x)
	if len(r.v) > r.n {
		r.v = r.v[len(r.v)-r.n:]
	}
}

func (r *ring) mean() float64 {
	if len(r.v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range r.v {
		s += x
	}
	return s / float64(len(r.v))
}

func (r *ring) quantile(q float64) float64 {
	if len(r.v) == 0 {
		return 0
	}
	c := append([]float64(nil), r.v...)
	sort.Float64s(c)
	i := int(math.Ceil(q*float64(len(c)))) - 1
	return c[max(0, min(i, len(c)-1))]
}

// markKeys finds the key each account votes with. When the account is
// listed, its on-chain vote key decides. Otherwise the valid key that
// voted last decides, or, before any key has voted, the valid key that
// lasts longest.
func markKeys(keys []PartKeyState, accts []AccountState, round uint64) {
	onChain := map[string]string{}
	for _, a := range accts {
		if a.VoteKey != "" {
			onChain[a.Address] = a.VoteKey
		}
	}
	active := map[string]int{}
	for i := range keys {
		k := &keys[i]
		k.Active, k.Superseded = false, false
		if vk, ok := onChain[k.Address]; ok {
			if k.VoteKey == vk {
				active[k.Address] = i
			}
			continue
		}
		if round < k.First || round > k.Last {
			continue
		}
		j, ok := active[k.Address]
		if !ok || k.LastVote > keys[j].LastVote || (k.LastVote == keys[j].LastVote && k.Last > keys[j].Last) {
			active[k.Address] = i
		}
	}
	for _, i := range active {
		keys[i].Active = true
	}
	for i := range keys {
		if j, ok := active[keys[i].Address]; ok && i != j && keys[i].Last < keys[j].Last {
			keys[i].Superseded = true
		}
	}
}

// ExpiryKeys returns one key per account, the one whose expiry matters:
// the active key, or else the key that lasts longest.
func ExpiryKeys(keys []PartKeyState) []PartKeyState {
	at := map[string]int{}
	var out []PartKeyState
	for _, k := range keys {
		i, ok := at[k.Address]
		switch {
		case !ok:
			at[k.Address] = len(out)
			out = append(out, k)
		case k.Active || (!out[i].Active && k.Last > out[i].Last):
			out[i] = k
		}
	}
	return out
}

// LaterKey reports whether the account has a key that ends after k but
// is not active: installed, perhaps not yet registered on chain.
func LaterKey(keys []PartKeyState, k PartKeyState) bool {
	for _, o := range keys {
		if o.Address == k.Address && o.Last > k.Last && !o.Active {
			return true
		}
	}
	return false
}
