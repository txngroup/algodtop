// Package tui is algodtop's terminal UI in the style of top, plus batch
// mode. It talks to the daemon over its socket with the same API as the
// web UI, or polls algod directly in live-only mode.
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/fsx"
	"algod.top/internal/health"
	"algod.top/internal/model"
	"algod.top/internal/remote"
	"algod.top/internal/sysstat"
	"algod.top/internal/version"
)

// ErrLive is returned for history queries in live-only mode.
var ErrLive = errors.New("no daemon: live only, no history")

// Series is a decoded /api/series result.
type Series struct {
	Parts []struct {
		From int64  `json:"from"`
		To   int64  `json:"to"`
		Tier string `json:"tier"`
	} `json:"parts"`
	Step   int64 `json:"step"`
	Series []struct {
		Labels map[string]string `json:"labels"`
		T      []int64           `json:"t"`
		Avg    []*float64        `json:"avg"`
		Min    []*float64        `json:"min"`
		Max    []*float64        `json:"max"`
	} `json:"series"`
}

// Values returns the first series' averages with NaN for gaps.
func (s *Series) Values() []float64 {
	if s == nil || len(s.Series) == 0 {
		return nil
	}
	return derefs(s.Series[0].Avg)
}

// Sum adds every series bucket by bucket.
func (s *Series) Sum() []float64 {
	if s == nil || len(s.Series) == 0 {
		return nil
	}
	out := make([]float64, len(s.Series[0].Avg))
	for i := range out {
		out[i] = math.NaN()
	}
	for _, se := range s.Series {
		for i, v := range se.Avg {
			if v != nil && i < len(out) {
				if math.IsNaN(out[i]) {
					out[i] = 0
				}
				out[i] += *v
			}
		}
	}
	return out
}

func derefs(p []*float64) []float64 {
	out := make([]float64, len(p))
	for i, v := range p {
		if v == nil {
			out[i] = math.NaN()
		} else {
			out[i] = *v
		}
	}
	return out
}

// Query is a series request.
type Query struct {
	Node, Metric, Fn, By string
	Match                []string
	From, To             int64
	Points               int
	Raw                  bool
}

// Source feeds the terminal UI.
type Source interface {
	Nodes(ctx context.Context) ([]api.NodeView, api.DaemonStatus, error)
	Events(ctx context.Context, node string, from, to int64, limit int) ([]model.Event, error)
	Series(ctx context.Context, q Query) (*Series, error)
	Templates(ctx context.Context, node string, from, to int64) ([]api.TemplateCount, error)
	PeerHistory(ctx context.Context, node string, from, to int64) ([]api.PeerHistory, error)
	Peers(ctx context.Context, node string) ([]collector.PeerInfo, error)
	AddAnnotation(ctx context.Context, a model.Annotation, silenceFor string) error
	AddSilence(ctx context.Context, nodes []string, d time.Duration, comment string) error
	Live() bool
	Describe() string
	Close()
}

// Client is a daemon API client.
type Client struct {
	hc    *http.Client
	base  string
	token string
	desc  string
	conn  *remote.Conn
}

// DialSocket connects to a daemon socket.
func DialSocket(path string) (*Client, error) {
	addr := fsx.SocketAddr(path)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", addr)
		},
		MaxIdleConnsPerHost: 4,
	}
	c := &Client{hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}, base: "http://algodtop", desc: path}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.status(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// DialHub connects to a remote daemon: ssh://host runs
// ssh host algodtop stdio and speaks HTTP/2 without TLS over the pipe;
// https://host:port uses a read-only token from $ALGODTOP_TOKEN.
func DialHub(hub, tokenFile string) (*Client, error) {
	if strings.HasPrefix(hub, "ssh://") {
		t, err := remote.ParseURL(hub)
		if err != nil {
			return nil, err
		}
		s := &remote.SSH{Target: t}
		// A hub of any version: try the new name, then the old one.
		bin := `sh -c 'if command -v algod.top >/dev/null 2>&1; then exec algod.top "$@"; fi; exec algodtop "$@"' algod.top`
		if t.Path != "" && t.Path != "/" {
			bin = remote.RemoteBinary(strings.TrimPrefix(t.Path, "/"))
			if strings.HasPrefix(t.Path, "/") && !strings.HasPrefix(t.Path, "/~") {
				bin = remote.RemoteBinary(t.Path)
			}
		}
		cmd := exec.Command("ssh", s.Args()...)
		cmd.Args = append(cmd.Args, bin+" stdio")
		conn, err := remote.DialCommand(cmd, t.Host)
		if err != nil {
			return nil, err
		}
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		var once sync.Once
		tr := &http.Transport{Protocols: p, DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn
			once.Do(func() { c = conn })
			if c == nil {
				return nil, errors.New("ssh connection closed")
			}
			return c, nil
		}}
		c := &Client{hc: &http.Client{Transport: tr, Timeout: 60 * time.Second}, base: "http://algodtop", desc: hub, conn: conn}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := c.status(ctx); err != nil {
			conn.Close()
			if s := conn.Stderr(); s != "" {
				return nil, fmt.Errorf("%v: %s", err, s)
			}
			return nil, err
		}
		return c, nil
	}
	if strings.HasPrefix(hub, "http://") || strings.HasPrefix(hub, "https://") {
		tok := os.Getenv("ALGODTOP_TOKEN")
		if tokenFile != "" {
			b, err := os.ReadFile(tokenFile)
			if err != nil {
				return nil, err
			}
			tok = strings.TrimSpace(string(b))
		}
		c := &Client{hc: &http.Client{Timeout: 30 * time.Second}, base: strings.TrimSuffix(hub, "/"), token: tok, desc: hub}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.status(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, fmt.Errorf("hub must be ssh://host or https://host:port, got %q", hub)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Get fetches a path into out, for the CLI.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, out)
}

// Post sends a body, for the CLI.
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// Delete sends a DELETE, for the CLI.
func (c *Client) Delete(ctx context.Context, path string, q url.Values) error {
	return c.do(ctx, http.MethodDelete, path, q, nil, nil)
}

// GetRaw returns a raw response body.
func (c *Client) GetRaw(ctx context.Context, path string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, err
}

func (c *Client) status(ctx context.Context) (api.DaemonStatus, error) {
	var st api.DaemonStatus
	err := c.do(ctx, http.MethodGet, "/api/status", nil, nil, &st)
	return st, err
}

// Nodes implements Source.
func (c *Client) Nodes(ctx context.Context) ([]api.NodeView, api.DaemonStatus, error) {
	var out struct {
		Nodes  []api.NodeView   `json:"nodes"`
		Status api.DaemonStatus `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, "/api/nodes", nil, nil, &out)
	return out.Nodes, out.Status, err
}

func tq(from, to int64) url.Values {
	return url.Values{"from": {fmt.Sprint(from)}, "to": {fmt.Sprint(to)}}
}

// Events implements Source.
func (c *Client) Events(ctx context.Context, node string, from, to int64, limit int) ([]model.Event, error) {
	q := tq(from, to)
	if node != "" {
		q.Set("node", node)
	}
	q.Set("limit", fmt.Sprint(limit))
	var out struct {
		Events []model.Event `json:"events"`
	}
	err := c.do(ctx, http.MethodGet, "/api/events", q, nil, &out)
	return out.Events, err
}

// Series implements Source.
func (c *Client) Series(ctx context.Context, sq Query) (*Series, error) {
	q := tq(sq.From, sq.To)
	q.Set("node", sq.Node)
	q.Set("metric", sq.Metric)
	if sq.Fn != "" {
		q.Set("fn", sq.Fn)
	}
	if sq.By != "" {
		q.Set("by", sq.By)
	}
	for _, m := range sq.Match {
		q.Add("match", m)
	}
	if sq.Points > 0 {
		q.Set("points", fmt.Sprint(sq.Points))
	}
	if sq.Raw {
		q.Set("tier", "raw")
	}
	var out Series
	err := c.do(ctx, http.MethodGet, "/api/series", q, nil, &out)
	return &out, err
}

// Templates implements Source.
func (c *Client) Templates(ctx context.Context, node string, from, to int64) ([]api.TemplateCount, error) {
	q := tq(from, to)
	q.Set("node", node)
	var out struct {
		Templates []api.TemplateCount `json:"templates"`
	}
	err := c.do(ctx, http.MethodGet, "/api/logs/templates", q, nil, &out)
	return out.Templates, err
}

// PeerHistory implements Source.
func (c *Client) PeerHistory(ctx context.Context, node string, from, to int64) ([]api.PeerHistory, error) {
	q := tq(from, to)
	q.Set("node", node)
	var out struct {
		Peers []api.PeerHistory `json:"peers"`
	}
	err := c.do(ctx, http.MethodGet, "/api/peers/history", q, nil, &out)
	return out.Peers, err
}

// Peers implements Source.
func (c *Client) Peers(ctx context.Context, node string) ([]collector.PeerInfo, error) {
	var out struct {
		Peers []collector.PeerInfo `json:"peers"`
	}
	err := c.do(ctx, http.MethodGet, "/api/peers", url.Values{"node": {node}}, nil, &out)
	return out.Peers, err
}

// AddAnnotation implements Source.
func (c *Client) AddAnnotation(ctx context.Context, a model.Annotation, silenceFor string) error {
	body := map[string]any{"node": a.Node, "start": a.Start, "end": a.End, "text": a.Text}
	if silenceFor != "" {
		body["silence_for"] = silenceFor
	}
	return c.do(ctx, http.MethodPost, "/api/annotations", nil, body, nil)
}

// AddSilence implements Source.
func (c *Client) AddSilence(ctx context.Context, nodes []string, d time.Duration, comment string) error {
	return c.do(ctx, http.MethodPost, "/api/silences", nil, map[string]any{"nodes": nodes, "for": d.String(), "comment": comment}, nil)
}

// Live implements Source.
func (c *Client) Live() bool { return false }

// Describe implements Source.
func (c *Client) Describe() string { return c.desc }

// Close implements Source.
func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// LiveSource polls algod directly with the same collector code as the
// daemon. It keeps nothing on disk, so it is safe on any host.
type LiveSource struct {
	nodes  []*collector.Node
	host   *collector.Host
	cancel context.CancelFunc
	mu     sync.Mutex
	events []model.Event
	start  time.Time
	desc   string
}

type liveSink struct{ s *LiveSource }

func (k liveSink) Batch(*model.Batch) {}
func (k liveSink) Event(e model.Event) {
	k.s.mu.Lock()
	k.s.events = append(k.s.events, e)
	if len(k.s.events) > 500 {
		k.s.events = k.s.events[len(k.s.events)-500:]
	}
	k.s.mu.Unlock()
}
func (k liveSink) Session(model.Session)           {}
func (k liveSink) Usage(string, []model.PeerUsage) {}
func (k liveSink) LogLine(collector.LogLine)       {}
func (k liveSink) Annotate(model.Annotation)       {}

// NewLiveSource starts collectors for the given node sources.
func NewLiveSource(names []string, sources []collector.Source, desc string, interval time.Duration) *LiveSource {
	ctx, cancel := context.WithCancel(context.Background())
	s := &LiveSource{cancel: cancel, start: time.Now(), desc: desc}
	sink := liveSink{s}
	for i, src := range sources {
		n := collector.NewNode(collector.Options{Name: names[i], Source: src, Sink: sink, Scrape: interval, PeersEvery: 30 * time.Second,
			Logs: collector.LogOptions{Disable: false}})
		s.nodes = append(s.nodes, n)
	}
	s.host = collector.NewHost(collector.HostOptions{ProcEvery: interval, NetEvery: 60 * time.Second}, sink, func() []*collector.Node { return s.nodes })
	for _, n := range s.nodes {
		go n.Run(ctx)
	}
	go s.host.Run(ctx)
	return s
}

// Nodes implements Source.
func (s *LiveSource) Nodes(ctx context.Context) ([]api.NodeView, api.DaemonStatus, error) {
	now := time.Now()
	var states []collector.State
	for _, n := range s.nodes {
		states = append(states, n.State())
	}
	var out []api.NodeView
	for i := range states {
		st := states[i]
		h := health.Evaluate(&st, health.Context{Now: now, Fleet: states})
		out = append(out, api.NodeView{State: st, Health: h, Summary: h.Summary(), Lag: st.Lag(now)})
	}
	host, _ := os.Hostname()
	return out, api.DaemonStatus{Host: host, Version: version.String(), VersionTime: version.Info().UnixNano(), Started: s.start.UnixNano(), Live: true, Platform: sysstat.Platform}, nil
}

// Events implements Source.
func (s *LiveSource) Events(ctx context.Context, node string, from, to int64, limit int) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Event
	for _, e := range s.events {
		if (node == "" || e.Node == node) && e.Time >= from && e.Time < to && e.Kind != model.EvConnect {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// Series implements Source.
func (s *LiveSource) Series(context.Context, Query) (*Series, error) { return nil, ErrLive }

// Templates implements Source.
func (s *LiveSource) Templates(context.Context, string, int64, int64) ([]api.TemplateCount, error) {
	return nil, ErrLive
}

// PeerHistory implements Source.
func (s *LiveSource) PeerHistory(context.Context, string, int64, int64) ([]api.PeerHistory, error) {
	return nil, ErrLive
}

// Peers implements Source.
func (s *LiveSource) Peers(_ context.Context, node string) ([]collector.PeerInfo, error) {
	for _, n := range s.nodes {
		if n.Name() == node {
			return n.State().PeerList, nil
		}
	}
	return nil, nil
}

// AddAnnotation implements Source.
func (s *LiveSource) AddAnnotation(context.Context, model.Annotation, string) error { return ErrLive }

// AddSilence implements Source.
func (s *LiveSource) AddSilence(context.Context, []string, time.Duration, string) error {
	return ErrLive
}

// Live implements Source.
func (s *LiveSource) Live() bool { return true }

// Describe implements Source.
func (s *LiveSource) Describe() string { return s.desc }

// Close implements Source.
func (s *LiveSource) Close() { s.cancel() }
