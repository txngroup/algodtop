// Package algod reads algod data directories and talks to algod's REST API.
// algodtop is read-only toward algod: the client sends GET requests only,
// and only to paths on a code-level allowlist.
package algod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// allowed lists every algod path algodtop calls. Paths ending in "/" are
// prefixes.
var allowed = []string{
	"/metrics",
	"/health",
	"/ready",
	"/versions",
	"/genesis",
	"/v2/status",
	"/v2/status/wait-for-block-after/",
	"/v2/blocks/",
	"/v2/transactions/pending",
	"/v2/ledger/supply",
	"/v2/accounts/",
	"/v2/node/peers",
	"/v2/participation",
}

// adminPaths are the only paths that receive the admin token.
var adminPaths = []string{"/v2/node/peers", "/v2/participation"}

// Allowed reports whether algodtop may call path.
func Allowed(path string) bool {
	for _, a := range allowed {
		if path == a || (strings.HasSuffix(a, "/") && strings.HasPrefix(path, a)) {
			return true
		}
	}
	return false
}

// IsAdminPath reports whether a path is called with the admin token.
func IsAdminPath(path string) bool {
	for _, a := range adminPaths {
		if path == a {
			return true
		}
	}
	return false
}

// ErrNotAllowed is returned for a path outside the allowlist.
var ErrNotAllowed = errors.New("algod: path not on the read-only allowlist")

// HTTPError is a non-2xx response.
type HTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("algod %s: HTTP %d: %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

// Client calls one algod node.
type Client struct {
	base       *url.URL
	tokMu      sync.RWMutex
	token      string
	adminToken string
	hc         *http.Client // ordinary requests
	poll       *http.Client // the round watcher's long-poll
	// Requested records every path called, for the allowlist test.
	Requested func(method, path string)
}

// Options configure a client.
type Options struct {
	Token      string
	AdminToken string
	// Dial replaces the network dialer, for example with an SSH tunnel.
	// Each client keeps at most one connection per dial function.
	Dial     func(ctx context.Context) (net.Conn, error)
	PollDial func(ctx context.Context) (net.Conn, error)
	Timeout  time.Duration
}

// NewClient returns a client for base, for example http://127.0.0.1:8080.
func NewClient(base string, o Options) (*Client, error) {
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("algod: unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	mk := func(dial func(ctx context.Context) (net.Conn, error), timeout time.Duration) *http.Client {
		tr := &http.Transport{
			Proxy:               nil,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  false,
		}
		if dial != nil {
			tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }
			tr.MaxConnsPerHost = 1
		} else {
			tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		return &http.Client{Transport: tr, Timeout: timeout}
	}
	pd := o.PollDial
	if pd == nil {
		pd = o.Dial
	}
	return &Client{base: u, token: o.Token, adminToken: o.AdminToken, hc: mk(o.Dial, o.Timeout), poll: mk(pd, 90*time.Second)}, nil
}

// Base returns the base URL.
func (c *Client) Base() string { return c.base.String() }

// SetTokens replaces the tokens, after a token file changes.
func (c *Client) SetTokens(token, admin string) {
	c.tokMu.Lock()
	c.token, c.adminToken = token, admin
	c.tokMu.Unlock()
}

// HasAdmin reports whether an admin token is configured.
func (c *Client) HasAdmin() bool {
	c.tokMu.RLock()
	defer c.tokMu.RUnlock()
	return c.adminToken != ""
}

// CloseIdle closes idle connections.
func (c *Client) CloseIdle() {
	c.hc.CloseIdleConnections()
	c.poll.CloseIdleConnections()
}

// Get fetches a path and returns the body.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.get(ctx, c.hc, path, query)
}

func (c *Client) get(ctx context.Context, hc *http.Client, path string, query url.Values) ([]byte, error) {
	if !Allowed(path) {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, path)
	}
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	c.tokMu.RLock()
	tok := c.token
	if IsAdminPath(path) && c.adminToken != "" {
		tok = c.adminToken
	}
	c.tokMu.RUnlock()
	if tok != "" {
		req.Header.Set("X-Algo-API-Token", tok)
	}
	req.Header.Set("User-Agent", "algodtop")
	if c.Requested != nil {
		c.Requested(req.Method, path)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		if len(body) > 300 {
			body = body[:300]
		}
		return nil, &HTTPError{Path: path, Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

func (c *Client) getJSON(ctx context.Context, hc *http.Client, path string, q url.Values, out any) error {
	b, err := c.get(ctx, hc, path, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("algod %s: %w", path, err)
	}
	return nil
}

// Status is /v2/status.
type Status struct {
	LastRound                 uint64 `json:"last-round"`
	TimeSinceLastRound        int64  `json:"time-since-last-round"` // ns
	CatchupTime               int64  `json:"catchup-time"`          // ns, 0 when not catching up
	Catchpoint                string `json:"catchpoint"`
	LastVersion               string `json:"last-version"`
	NextVersion               string `json:"next-version"`
	NextVersionRound          uint64 `json:"next-version-round"`
	NextVersionSupported      bool   `json:"next-version-supported"`
	StoppedAtUnsupportedRound bool   `json:"stopped-at-unsupported-round"`
	UpgradeYesVotes           uint64 `json:"upgrade-yes-votes"`
	UpgradeVotesRequired      uint64 `json:"upgrade-votes-required"`
	UpgradeNodeVote           bool   `json:"upgrade-node-vote"`
}

// InCatchup reports whether the node is catching up.
func (s *Status) InCatchup() bool { return s.CatchupTime > 0 || s.Catchpoint != "" }

// Status calls /v2/status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.getJSON(ctx, c.hc, "/v2/status", nil, &s)
}

// WaitForBlockAfter long-polls until the round after r lands, or about a
// minute passes.
func (c *Client) WaitForBlockAfter(ctx context.Context, r uint64) (*Status, error) {
	var s Status
	return &s, c.getJSON(ctx, c.poll, "/v2/status/wait-for-block-after/"+strconv.FormatUint(r, 10), nil, &s)
}

// Versions is /versions.
type Versions struct {
	GenesisID   string `json:"genesis_id"`
	GenesisHash string `json:"genesis_hash_b64"`
	Build       struct {
		Major       int    `json:"major"`
		Minor       int    `json:"minor"`
		BuildNumber int    `json:"build_number"`
		CommitHash  string `json:"commit_hash"`
		Branch      string `json:"branch"`
		Channel     string `json:"channel"`
	} `json:"build"`
}

// Version renders the build as major.minor.build.
func (v *Versions) Version() string {
	return fmt.Sprintf("%d.%d.%d", v.Build.Major, v.Build.Minor, v.Build.BuildNumber)
}

// Versions calls /versions.
func (c *Client) Versions(ctx context.Context) (*Versions, error) {
	var v Versions
	return &v, c.getJSON(ctx, c.hc, "/versions", nil, &v)
}

// Health calls /health and reports whether it succeeded.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.get(ctx, c.hc, "/health", nil)
	return err
}

// Ready calls /ready.
func (c *Client) Ready(ctx context.Context) error {
	_, err := c.get(ctx, c.hc, "/ready", nil)
	return err
}

// Metrics fetches /metrics.
func (c *Client) Metrics(ctx context.Context) ([]byte, error) {
	return c.get(ctx, c.hc, "/metrics", nil)
}

// BlockHeader is the part of a block header algodtop uses.
type BlockHeader struct {
	Round      uint64 `json:"rnd"`
	Timestamp  int64  `json:"ts"`
	TxnCounter uint64 `json:"tc"`
	Proposer   string `json:"prp"`
}

// BlockHeader fetches a header with header-only=true.
func (c *Client) BlockHeader(ctx context.Context, r uint64) (*BlockHeader, error) {
	var resp struct {
		Block BlockHeader `json:"block"`
	}
	q := url.Values{"header-only": {"true"}}
	if err := c.getJSON(ctx, c.hc, "/v2/blocks/"+strconv.FormatUint(r, 10), q, &resp); err != nil {
		return nil, err
	}
	return &resp.Block, nil
}

// PoolSize returns the transaction pool size. max=1 returns one
// transaction rather than megabytes during a flood.
func (c *Client) PoolSize(ctx context.Context) (uint64, error) {
	var resp struct {
		Total uint64 `json:"total-transactions"`
	}
	err := c.getJSON(ctx, c.hc, "/v2/transactions/pending", url.Values{"max": {"1"}}, &resp)
	return resp.Total, err
}

// Supply is /v2/ledger/supply.
type Supply struct {
	Round       uint64 `json:"current_round"`
	OnlineMoney uint64 `json:"online-money"`
	TotalMoney  uint64 `json:"total-money"`
}

// Supply calls /v2/ledger/supply.
func (c *Client) Supply(ctx context.Context) (*Supply, error) {
	var s Supply
	return &s, c.getJSON(ctx, c.hc, "/v2/ledger/supply", nil, &s)
}

// Peer is one entry of /v2/node/peers. The usage fields come from algod
// builds that report per-peer usage and are absent on others.
type Peer struct {
	Address            string    `json:"network-address"`
	ConnectionType     string    `json:"connection-type"` // inbound or outbound
	NetworkType        string    `json:"network-type"`    // ws or p2p
	PeerID             string    `json:"peer-id,omitempty"`
	SupportedProtocols []string  `json:"supported-protocols,omitempty"`
	ActiveStreams      *[]string `json:"active-streams,omitempty"`
	TotalBytesReceived *int64    `json:"total-bytes-received,omitempty"`
	TotalBytesSent     *int64    `json:"total-bytes-sent,omitempty"`
}

// HasUsage reports whether a peer entry carries per-peer usage. Support is
// detected by the presence of active-streams, not by algod version.
func (p *Peer) HasUsage() bool { return p.ActiveStreams != nil }

// Peers calls /v2/node/peers with the admin token.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var resp struct {
		Peers []Peer `json:"Peers"`
		Lower []Peer `json:"peers"`
	}
	if err := c.getJSON(ctx, c.hc, "/v2/node/peers", nil, &resp); err != nil {
		return nil, err
	}
	if resp.Peers == nil {
		return resp.Lower, nil
	}
	return resp.Peers, nil
}

// PartKey is one entry of /v2/participation.
type PartKey struct {
	ID             string `json:"id"`
	Address        string `json:"address"`
	LastVote       uint64 `json:"last-vote,omitempty"`
	LastBlockProp  uint64 `json:"last-block-proposal,omitempty"`
	EffectiveFirst uint64 `json:"effective-first-valid,omitempty"`
	EffectiveLast  uint64 `json:"effective-last-valid,omitempty"`
	Key            struct {
		VoteFirst       uint64 `json:"vote-first-valid"`
		VoteLast        uint64 `json:"vote-last-valid"`
		VoteKeyDilution uint64 `json:"vote-key-dilution"`
		VoteKey         string `json:"vote-participation-key"`
		SelectionKey    string `json:"selection-participation-key"`
	} `json:"key"`
}

// Participation calls /v2/participation with the admin token.
func (c *Client) Participation(ctx context.Context) ([]PartKey, error) {
	var keys []PartKey
	b, err := c.get(ctx, c.hc, "/v2/participation", nil)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return nil, nil // no keys installed
		}
		return nil, err
	}
	if len(b) == 0 || string(b) == "null\n" || string(b) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("algod /v2/participation: %w", err)
	}
	return keys, nil
}

// Account is the part of /v2/accounts/{addr} algodtop uses.
type Account struct {
	Address           string `json:"address"`
	Amount            uint64 `json:"amount"`
	Status            string `json:"status"` // Online, Offline, NotParticipating
	IncentiveEligible bool   `json:"incentive-eligible"`
	LastProposed      uint64 `json:"last-proposed"`
	LastHeartbeat     uint64 `json:"last-heartbeat"`
	Round             uint64 `json:"round"`
	Participation     *struct {
		VoteFirst uint64 `json:"vote-first-valid"`
		VoteLast  uint64 `json:"vote-last-valid"`
		VoteKey   string `json:"vote-participation-key"`
	} `json:"participation,omitempty"`
}

// Account calls /v2/accounts/{addr}?exclude=all.
func (c *Client) Account(ctx context.Context, addr string) (*Account, error) {
	var a Account
	return &a, c.getJSON(ctx, c.hc, "/v2/accounts/"+url.PathEscape(addr), url.Values{"exclude": {"all"}}, &a)
}

// MetricsInto fetches /metrics into buf, reusing its memory. Mainnet's
// response is about 160 KB every scrape.
func (c *Client) MetricsInto(ctx context.Context, buf *bytes.Buffer) error {
	buf.Reset()
	u := *c.base
	u.Path = c.base.Path + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "algodtop")
	if c.Requested != nil {
		c.Requested(req.Method, "/metrics")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &HTTPError{Path: "/metrics", Status: resp.StatusCode, Body: string(b)}
	}
	_, err = buf.ReadFrom(io.LimitReader(resp.Body, 64<<20))
	return err
}
