package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/fsx"
	"algod.top/internal/health"
	"algod.top/internal/model"
	"algod.top/internal/remote"
	"algod.top/internal/store"
)

// remoteSet runs one puller per agent host. An SSH agent is reached with
// ssh host algodtop stdio --agent, speaking HTTP/2 without TLS over the
// pipe. A TLS agent is reached over HTTPS with a token or mutual TLS.
type remoteSet struct {
	d    *Daemon
	mu   sync.Mutex
	runs map[string]*remoteRun
	wg   sync.WaitGroup
}

type remoteRun struct {
	cfg     config.Remote
	cancel  context.CancelFunc
	mu      sync.Mutex
	views   []api.NodeView
	lastOK  time.Time
	lastErr string
	started time.Time // when this run began, for an agent never reached
	offset  float64   // agent clock minus hub clock, seconds
	client  *http.Client
	base    string
	sshConn *remote.Conn
}

func newRemoteSet(d *Daemon) *remoteSet { return &remoteSet{d: d, runs: map[string]*remoteRun{}} }

func (rs *remoteSet) start(ctx context.Context, cfg *config.Config) {
	for _, r := range cfg.Remotes {
		if !r.Disabled {
			rs.startOne(ctx, r)
		}
	}
}

func (rs *remoteSet) startOne(ctx context.Context, r config.Remote) {
	rctx, cancel := context.WithCancel(ctx)
	run := &remoteRun{cfg: r, cancel: cancel, started: time.Now()}
	rs.mu.Lock()
	rs.runs[r.Name] = run
	rs.mu.Unlock()
	rs.wg.Add(1)
	go func() { defer rs.wg.Done(); rs.d.runRemote(rctx, run) }()
}

func (rs *remoteSet) reload(ctx context.Context, cfg *config.Config) {
	want := map[string]config.Remote{}
	for _, r := range cfg.Remotes {
		if !r.Disabled {
			want[r.Name] = r
		}
	}
	rs.mu.Lock()
	var stop []*remoteRun
	for name, run := range rs.runs {
		if w, ok := want[name]; !ok || fmt.Sprint(w) != fmt.Sprint(run.cfg) {
			stop = append(stop, run)
			delete(rs.runs, name)
		}
	}
	rs.mu.Unlock()
	for _, r := range stop {
		r.cancel()
	}
	for name, r := range want {
		rs.mu.Lock()
		_, ok := rs.runs[name]
		rs.mu.Unlock()
		if !ok {
			rs.startOne(ctx, r)
		}
	}
}

func (rs *remoteSet) wait() { rs.wg.Wait() }

// noteRound moves an agent node's round forward as soon as its round
// batch arrives, between the 5 s polls of the agent's full status.
func (r *remoteRun) noteRound(node string, round uint64, at int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.views {
		if r.views[i].Name == node && round > r.views[i].Round {
			r.views[i].Round, r.views[i].RoundAt = round, at
		}
	}
}

func (rs *remoteSet) views() []api.NodeView {
	rs.mu.Lock()
	runs := make([]*remoteRun, 0, len(rs.runs))
	for _, r := range rs.runs {
		runs = append(runs, r)
	}
	rs.mu.Unlock()
	// Map order changes from call to call, and the node lists in the UI
	// follow this order.
	sort.Slice(runs, func(i, j int) bool { return runs[i].cfg.Name < runs[j].cfg.Name })
	var out []api.NodeView
	for _, r := range runs {
		r.mu.Lock()
		reachable := time.Since(r.lastOK) < 30*time.Second
		// The agent's clock reads offset seconds ahead of the hub's.
		agentNow := time.Now().Add(time.Duration(r.offset * float64(time.Second)))
		for _, v := range r.views {
			v.Remote = r.cfg.Name
			v.Kind = "agent"
			v.ClockOffsetAgent = r.offset
			if v.RoundAt != 0 || v.StatusErrAt != 0 {
				v.Lag = v.State.Lag(agentNow)
			}
			if !reachable {
				v.Health.Reasons = append([]healthReason{{Rule: "agent_unreachable", Level: 2, Text: r.downReason(time.Now())}}, v.Health.Reasons...)
				v.Health.Level = 2
				v.Summary = v.Health.Summary()
			}
			out = append(out, v)
		}
		if len(r.views) == 0 {
			// Nothing heard yet: show the host so the operator sees why,
			// under the names it will have.
			names := []string{r.cfg.Name + "/?"}
			if len(r.cfg.Names) > 0 {
				names = names[:0]
				for _, h := range r.cfg.Names {
					names = append(names, h)
				}
				sort.Strings(names)
			}
			for _, name := range names {
				v := api.NodeView{Remote: r.cfg.Name}
				v.Name = name
				v.Kind = "agent"
				v.LastError = r.lastErr
				v.Health.Level = 2
				v.Health.Reasons = []healthReason{{Rule: "agent_unreachable", Level: 2, Text: r.downReason(time.Now())}}
				v.Summary = v.Health.Summary()
				out = append(out, v)
			}
		}
		r.mu.Unlock()
	}
	return out
}

// downReason says how long the agent has been out of reach and why. The
// caller holds r.mu.
func (r *remoteRun) downReason(now time.Time) string {
	since := r.lastOK
	if since.IsZero() {
		since = r.started
	}
	why := r.lastErr
	if why == "" {
		why = "no answer"
	}
	return fmt.Sprintf("agent %s unreachable for %s: %s", r.cfg.Name, now.Sub(since).Round(time.Second), why)
}

// remoteErr is one line saying why a connection to an agent ended. It
// drops the request URL, which for an SSH agent names a stand-in host,
// and joins what ssh printed, often two lines ending in CR LF.
func remoteErr(err error, stderr string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(lines, l) {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return err.Error()
	}
	return err.Error() + ": " + strings.Join(lines, "; ")
}

func (rs *remoteSet) reachable(name string) bool {
	rs.mu.Lock()
	r := rs.runs[name]
	rs.mu.Unlock()
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastOK) < 30*time.Second
}

// connect builds the HTTP client for a remote.
func (d *Daemon) connect(ctx context.Context, run *remoteRun) error {
	r := run.cfg
	if r.URL != "" {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if r.CAFile != "" {
			pem, err := os.ReadFile(algod.ExpandHome(r.CAFile))
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tc.RootCAs = pool
		}
		if r.CertFile != "" {
			c, err := tls.LoadX509KeyPair(algod.ExpandHome(r.CertFile), algod.ExpandHome(r.KeyFile))
			if err != nil {
				return err
			}
			tc.Certificates = []tls.Certificate{c}
		}
		tr := &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second}
		run.client = &http.Client{Transport: &tokenTransport{rt: tr, token: firstNonEmpty(r.Token, readFile(r.TokenFile))}}
		run.base = strings.TrimSuffix(r.URL, "/")
		return nil
	}
	t, err := sshTarget(r.Host)
	if err != nil {
		return err
	}
	// An agent pull is one long-lived ssh process, so it gains nothing
	// from a ControlMaster. A shared master can also predate the user
	// joining the algodtop group, and the hub's retries would keep it
	// alive, so every pull would meet the old groups.
	ssh := &remote.SSH{Target: t}
	cmd := exec.Command("ssh", ssh.Args()...)
	cmd.Args = append(cmd.Args, remote.AgentRelay(r.AgentPath, r.AgentConfig))
	conn, err := remote.DialCommand(cmd, r.Host)
	if err != nil {
		return err
	}
	run.sshConn = conn
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	used := false
	var mu sync.Mutex
	tr := &http.Transport{Protocols: p, DialContext: func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if used {
			return nil, fmt.Errorf("ssh connection to %s closed", r.Host)
		}
		used = true
		return conn, nil
	}, MaxConnsPerHost: 1}
	run.client = &http.Client{Transport: tr}
	run.base = "http://agent"
	return nil
}

func sshTarget(host string) (remote.Target, error) {
	if strings.HasPrefix(host, "ssh://") {
		return remote.ParseURL(host)
	}
	return remote.ParseURL("ssh://" + host)
}

type tokenTransport struct {
	rt    http.RoundTripper
	token string
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.rt.RoundTrip(r)
}

// hubName is the hub's name for an agent's node: the one in the remote's
// names, else <remote>/<node>.
func hubName(r config.Remote, node string) string {
	if h, ok := r.Names[node]; ok {
		return h
	}
	return r.Name + "/" + node
}

// parseNamesKey reverses namesKey.
func parseNamesKey(k string) map[string]string {
	if k == "" {
		return nil
	}
	m := map[string]string{}
	for _, p := range strings.Split(k, ",") {
		if a, h, ok := strings.Cut(p, "="); ok {
			m[a] = h
		}
	}
	return m
}

// cursorAfter reports whether agent cursor a is past b. Cursors are
// epoch:seq; a different epoch means the agent's buffer was reset, so
// nothing before it can be compared and a counts as past.
func cursorAfter(a, b string) bool {
	ea, sa, _ := strings.Cut(a, ":")
	eb, sb, _ := strings.Cut(b, ":")
	if ea != eb {
		return true
	}
	na, _ := strconv.ParseUint(sa, 10, 64)
	nb, _ := strconv.ParseUint(sb, 10, 64)
	return na > nb
}

// namesKey identifies a remote's names, "" when it has none.
func namesKey(r config.Remote) string {
	if len(r.Names) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(r.Names))
	for a, h := range r.Names {
		pairs = append(pairs, a+"="+h)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// runRemote keeps one agent connection alive with backoff from 1 s up to
// 5 minutes, pulls batches and polls live status.
func (d *Daemon) runRemote(ctx context.Context, run *remoteRun) {
	backoff := time.Second
	cursorPath := filepath.Join(d.db.Dir(), "state", "remote-"+run.cfg.Name+".cursor")
	for ctx.Err() == nil {
		err := d.connect(ctx, run)
		if err == nil {
			err = d.serveRemote(ctx, run, cursorPath)
		}
		stderr := ""
		if run.sshConn != nil {
			stderr = run.sshConn.Stderr()
			run.sshConn.Close()
			run.sshConn = nil
		}
		if ctx.Err() != nil {
			return
		}
		why := "connection closed"
		if err != nil {
			why = remoteErr(err, stderr)
		}
		run.mu.Lock()
		run.lastErr = why
		healthy := time.Since(run.lastOK) < time.Minute
		run.mu.Unlock()
		if healthy {
			backoff = time.Second
		}
		d.logf("remote %s: %s (retry in %v)", run.cfg.Name, why, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (d *Daemon) serveRemote(ctx context.Context, run *remoteRun, cursorPath string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- d.pollRemoteStatus(ctx, run) }()
	go func() { errc <- d.pullRemote(ctx, run, cursorPath) }()
	err := <-errc
	cancel()
	<-errc
	return err
}

func (d *Daemon) get(ctx context.Context, run *remoteRun, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, run.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := run.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("agent %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (d *Daemon) pollRemoteStatus(ctx context.Context, run *remoteRun) error {
	for {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		t0 := time.Now()
		resp, err := d.get(rctx, run, "/api/nodes")
		if err != nil {
			cancel()
			return err
		}
		var body struct {
			Nodes []api.NodeView `json:"nodes"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		cancel()
		if err != nil {
			return err
		}
		mid := t0.Add(time.Since(t0) / 2)
		offset := 0.0
		if dt, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
			offset = dt.Sub(mid).Seconds()
		}
		for i := range body.Nodes {
			body.Nodes[i].Name = hubName(run.cfg, body.Nodes[i].Name)
			body.Nodes[i].History = true
		}
		run.mu.Lock()
		run.views = body.Nodes
		run.lastOK = time.Now()
		run.lastErr = ""
		run.offset = offset
		run.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// pullRemote pulls batches with a resumable cursor. The cursor advances
// only after the writer has applied the items, so a hub crash re-pulls
// rather than loses data.
func (d *Daemon) pullRemote(ctx context.Context, run *remoteRun, cursorPath string) error {
	// The cursor file starts with the names in force when it was written.
	// New names pull the agent's whole buffer again, so the data it already
	// sent lands under the new names too.
	key := namesKey(run.cfg)
	cursor := ""
	// replayTo is the old cursor while a re-pull after a name change is
	// behind it. Until then, items for nodes whose hub name did not change
	// are already stored, and are skipped.
	replayTo := ""
	old := config.Remote{Name: run.cfg.Name}
	if b, err := os.ReadFile(cursorPath); err == nil {
		k, c, ok := strings.Cut(strings.TrimSpace(string(b)), " ")
		if !ok {
			k, c = "", k // written before names existed
		}
		if k == key {
			cursor = c
		} else {
			d.logf("remote %s: node names changed; pulling the agent's buffer again", run.cfg.Name)
			replayTo, old.Names = c, parseNamesKey(k)
		}
	}
	saved := cursor
	renamed := func(n string) bool { return hubName(old, n) != hubName(run.cfg, n) }
	sink := &daemonSink{d: d, rename: func(n string) string { return hubName(run.cfg, n) }, onRound: run.noteRound}
	seen := newEventDedup()
	// pulled holds the hub names of the nodes this pull has seen data for,
	// in case an agent-wide event arrives before the first status poll.
	pulled := map[string]bool{}
	// pending holds agent-wide events until the agent's nodes are known.
	// The saved cursor waits for them, so a hub crash pulls them again.
	var pending []model.Event
	var pendingSince time.Time
	for {
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		resp, err := d.get(rctx, run, "/agent/v1/batches?wait=30s&since="+cursor)
		if err != nil {
			cancel()
			return err
		}
		body, err := io.ReadAll(resp.Body)
		next := resp.Header.Get("X-Algodtop-Cursor")
		resp.Body.Close()
		cancel()
		if err != nil {
			return err
		}
		items, err := DecodePull(body)
		if err != nil {
			return fmt.Errorf("decode agent batches: %w", err)
		}
		// A response that ends at or before the old cursor holds only data
		// the hub stored before; one that crosses it is kept whole, since
		// a repeated sample is harmless and a lost one is not.
		skipOld := replayTo != "" && !cursorAfter(next, replayTo)
		if replayTo != "" && !skipOld {
			replayTo = ""
		}
		for _, it := range items {
			if n := it.NodeName(); n != "" {
				pulled[hubName(run.cfg, n)] = true
			}
		}
		for _, it := range items {
			if skipOld && !renamed(it.NodeName()) {
				continue
			}
			switch {
			case it.Batch != nil:
				// Catch-up data advances the node's own head in order.
				sink.Batch(it.Batch)
			case it.Event != nil:
				if seen.add(*it.Event) {
					break
				}
				if it.Event.Node != "" {
					sink.Event(*it.Event)
					break
				}
				if len(pending) == 0 {
					pendingSince = time.Now()
				}
				pending = append(pending, *it.Event)
			case it.Session != nil:
				sink.Session(*it.Session)
			case it.Usage != nil:
				sink.Usage(it.UsageNode, it.Usage)
			case it.Line != nil:
				l := *it.Line
				sink.LogLine(collectorLine(l))
			case it.Annotation != nil:
				sink.Annotate(*it.Annotation)
			}
		}
		if len(pending) > 0 {
			// An agent with no nodes never names one: its events keep no node.
			if names := run.nodeNames(pulled); len(names) > 0 || time.Since(pendingSince) > time.Minute {
				for _, ev := range pending {
					for _, e := range agentEvents(ev, run.cfg.Name, names) {
						d.emitEvent(e)
					}
				}
				pending = nil
			}
		}
		run.mu.Lock()
		run.lastOK = time.Now()
		run.mu.Unlock()
		if next != "" && next != cursor {
			cursor = next
		}
		if cursor != saved && len(pending) == 0 {
			if err := d.Flush(); err != nil {
				return err
			}
			saved = cursor
			line := cursor
			if key != "" {
				line = key + " " + cursor
			}
			fsx.WriteAtomic(cursorPath, []byte(line), 0o600)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// nodeNames is the hub names of the agent's nodes: from its last status,
// or from the data pulled so far before the first status arrives.
func (r *remoteRun) nodeNames(pulled map[string]bool) []string {
	var out []string
	r.mu.Lock()
	for _, v := range r.views {
		out = append(out, v.Name)
	}
	r.mu.Unlock()
	if len(out) == 0 {
		for n := range pulled {
			out = append(out, n)
		}
		sort.Strings(out)
	}
	return out
}

// agentEvents attributes an agent-wide event, such as the agent's own
// start, monitor gap or buffer deletion, to the agent's nodes on the hub.
// Kept without a node, it would match every node, and an agent's restart
// would shade the hub's own nodes' charts as "algod.top was down". Each of
// the agent's nodes gets a copy, since its data has the same gap. With no
// node known yet, the one copy keeps no node and still names the agent.
func agentEvents(e model.Event, remote string, nodes []string) []model.Event {
	e.Msg = "agent " + remote + ": " + e.Msg
	attrs := map[string]any{}
	for k, v := range e.Attrs {
		attrs[k] = v
	}
	attrs["agent"] = remote
	if len(nodes) == 0 {
		e.Attrs = attrs
		return []model.Event{e}
	}
	out := make([]model.Event, 0, len(nodes))
	for _, n := range nodes {
		c := e
		c.Node = n
		c.Attrs = make(map[string]any, len(attrs))
		for k, v := range attrs {
			c.Attrs[k] = v
		}
		out = append(out, c)
	}
	return out
}

// eventDedup drops events a re-pull delivers twice.
type eventDedup struct {
	m     map[string]bool
	order []string
}

func newEventDedup() *eventDedup { return &eventDedup{m: map[string]bool{}} }

func (e *eventDedup) add(ev model.Event) bool {
	k := fmt.Sprintf("%d|%s|%s|%s", ev.Time, ev.Node, ev.Kind, ev.Msg)
	if e.m[k] {
		return true
	}
	e.m[k] = true
	e.order = append(e.order, k)
	if len(e.order) > 10000 {
		delete(e.m, e.order[0])
		e.order = e.order[1:]
	}
	return false
}

type healthReason = health.Reason

func collectorLine(l store.LogLine) collector.LogLine {
	return collector.LogLine{Time: l.Time, Node: l.Node, Level: l.Level, Template: l.Template, Line: l.Line}
}
