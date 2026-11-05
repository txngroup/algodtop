// Package alert evaluates alert rules over the derived values that health
// uses, with fixed functions and no general query language. Channels are
// webhook, SMTP and command, and they are defined only in the config file.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/fsx"
	"algod.top/internal/health"
	"algod.top/internal/model"
)

// Expr is a parsed alert expression.
type Expr struct {
	Var   string  // lag, cpu, rss, ..., or rule:<name>, health, down
	Op    string  // > >= < <= == != or "" for a boolean
	Value float64 // threshold in base units (seconds, bytes, percent)
	Or    *Expr
}

var units = map[string]float64{
	"ms": 0.001, "s": 1, "m": 60, "h": 3600, "d": 86400,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "kib": 1024, "mib": 1 << 20, "gib": 1 << 30, "%": 1,
}

// Vars lists the variables an expression can use.
var Vars = []string{"lag", "round_time", "round_p99", "cpu", "rss", "disk_free_pct", "disk_days", "peers", "peers_out", "peers_in",
	"pool", "tpb", "error_rate", "clock_offset", "key_days", "net_in", "net_out", "health", "down", "stalled", "catchup", "agent_unreachable"}

// ParseExpr parses "lag > 30s", "down or stalled", "health >= warn" or
// "rule:key_expiry".
func ParseExpr(s string) (*Expr, error) {
	s = strings.TrimSpace(s)
	if a, b, ok := strings.Cut(s, " or "); ok {
		l, err := ParseExpr(a)
		if err != nil {
			return nil, err
		}
		r, err := ParseExpr(b)
		if err != nil {
			return nil, err
		}
		l.Or = r
		return l, nil
	}
	f := strings.Fields(s)
	switch len(f) {
	case 1:
		v := f[0]
		if strings.HasPrefix(v, "rule:") {
			name := strings.TrimPrefix(v, "rule:")
			for _, r := range health.Rules {
				if r == name {
					return &Expr{Var: v}, nil
				}
			}
			return nil, fmt.Errorf("unknown health rule %q", name)
		}
		switch v {
		case "down", "stalled", "catchup", "agent_unreachable":
			return &Expr{Var: v}, nil
		}
		return nil, fmt.Errorf("%q is not a boolean; write e.g. %s > 10", v, v)
	case 3:
		e := &Expr{Var: f[0], Op: f[1]}
		switch e.Op {
		case ">", ">=", "<", "<=", "==", "!=":
		default:
			return nil, fmt.Errorf("bad operator %q", e.Op)
		}
		known := false
		for _, v := range Vars {
			if v == e.Var {
				known = true
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown variable %q (known: %s)", e.Var, strings.Join(Vars, ", "))
		}
		if e.Var == "health" {
			l, err := health.ParseLevel(f[2])
			if err != nil {
				return nil, err
			}
			e.Value = float64(l)
			return e, nil
		}
		v, err := parseValue(f[2])
		if err != nil {
			return nil, err
		}
		e.Value = v
		return e, nil
	}
	return nil, fmt.Errorf("cannot parse %q; write e.g. \"lag > 30s\"", s)
}

func parseValue(s string) (float64, error) {
	ls := strings.ToLower(s)
	for _, u := range []string{"kib", "mib", "gib", "ms", "kb", "mb", "gb", "s", "m", "h", "d", "%"} {
		if strings.HasSuffix(ls, u) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(ls, u), 64)
			if err == nil {
				return n * units[u], nil
			}
		}
	}
	return strconv.ParseFloat(ls, 64)
}

// Input is what an expression sees for one node.
type Input struct {
	State  *collector.State
	Health health.Status
	Now    time.Time
	// AgentUnreachable is set by the hub for agent nodes.
	AgentUnreachable bool
}

func (in Input) value(v string) (float64, bool) {
	s := in.State
	switch v {
	case "lag":
		return s.Lag(in.Now), true
	case "round_time":
		return s.RoundTime, s.RoundTime > 0
	case "round_p99":
		return s.RoundP99, s.RoundP99 > 0
	case "cpu":
		return s.CPU, true
	case "rss":
		return s.RSS, s.RSS > 0
	case "disk_free_pct":
		if s.DiskTotal == 0 {
			return 0, false
		}
		return s.DiskFree / s.DiskTotal * 100, true
	case "disk_days":
		return s.DiskDays, s.DiskDays > 0
	case "peers":
		return float64(s.Peers.Total()), s.HasAdmin
	case "peers_out":
		return float64(s.Peers.Out()), s.HasAdmin
	case "peers_in":
		return float64(s.Peers.WSIn + s.Peers.P2PIn), s.HasAdmin
	case "pool":
		return s.PoolSize, true
	case "tpb":
		return s.TxnsPerBlk, true
	case "error_rate":
		return s.ErrorRate, true
	case "clock_offset":
		return math.Abs(s.ClockOff), s.ClockKnown
	case "key_days":
		if len(s.Keys) == 0 {
			return 0, false
		}
		m := math.Inf(1)
		for _, k := range collector.ExpiryKeys(s.Keys) {
			m = math.Min(m, k.DaysLeft)
		}
		return m, true
	case "net_in":
		return s.NetIn, s.NetMethod != ""
	case "net_out":
		return s.NetOut, s.NetMethod != ""
	case "health":
		return float64(in.Health.Level), true
	}
	return 0, false
}

// Eval returns whether the expression holds and a short description.
func (e *Expr) Eval(in Input) (bool, string) {
	ok, why := e.eval1(in)
	if !ok && e.Or != nil {
		return e.Or.Eval(in)
	}
	return ok, why
}

func (e *Expr) eval1(in Input) (bool, string) {
	s := in.State
	switch e.Var {
	case "down":
		return !s.Up, "algod is down"
	case "stalled":
		lag := s.Lag(in.Now)
		return s.Up && !s.Catchup && lag > 30, fmt.Sprintf("last round is %.0fs old", lag)
	case "catchup":
		return s.Catchup, "catchup is running"
	case "agent_unreachable":
		return in.AgentUnreachable, "agent unreachable"
	}
	if strings.HasPrefix(e.Var, "rule:") {
		name := strings.TrimPrefix(e.Var, "rule:")
		// Every match: two accounts can have expiring keys at once.
		var texts []string
		for _, r := range in.Health.Reasons {
			if r.Rule == name {
				texts = append(texts, r.Text)
			}
		}
		return len(texts) > 0, strings.Join(texts, "; ")
	}
	v, ok := in.value(e.Var)
	if !ok {
		return false, ""
	}
	var hit bool
	switch e.Op {
	case ">":
		hit = v > e.Value
	case ">=":
		hit = v >= e.Value
	case "<":
		hit = v < e.Value
	case "<=":
		hit = v <= e.Value
	case "==":
		hit = v == e.Value
	case "!=":
		hit = v != e.Value
	}
	return hit, fmt.Sprintf("%s %s %s (now %s)", e.Var, e.Op, fmtNum(e.Var, e.Value), fmtNum(e.Var, v))
}

func fmtNum(v string, x float64) string {
	switch v {
	case "lag", "round_time", "round_p99", "clock_offset":
		return strconv.FormatFloat(x, 'f', 2, 64) + "s"
	case "health":
		return health.Level(int(x)).String()
	}
	return strconv.FormatFloat(x, 'g', 4, 64)
}

// Rule is a compiled alert rule.
type Rule struct {
	config.AlertRule
	expr *Expr
}

// Compile parses the configured rules.
func Compile(rules []config.AlertRule) ([]Rule, error) {
	var out []Rule
	var errs []error
	for _, r := range rules {
		e, err := ParseExpr(r.Expr)
		if err != nil {
			errs = append(errs, fmt.Errorf("alert %q: %w", r.Name, err))
			continue
		}
		out = append(out, Rule{AlertRule: r, expr: e})
	}
	return out, errors.Join(errs...)
}

// Notification is the JSON payload sent to every channel.
type Notification struct {
	Alert    string `json:"alert"`
	Node     string `json:"node"`
	State    string `json:"state"` // firing, repeat, cleared
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Since    int64  `json:"since"`
	Time     int64  `json:"time"`
	Host     string `json:"host"`
	Expr     string `json:"expr"`
}

type alertState struct {
	TrueSince  int64  `json:"true_since,omitempty"`
	FalseSince int64  `json:"false_since,omitempty"`
	Firing     bool   `json:"firing"`
	FiredAt    int64  `json:"fired_at,omitempty"`
	LastSent   int64  `json:"last_sent,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Engine evaluates rules and sends notifications.
type Engine struct {
	mu       sync.Mutex
	rules    []Rule
	channels map[string]config.Chan
	state    map[string]*alertState // rule|node
	path     string
	events   func(model.Event)
	silenced func(t int64, node, rule string) bool
	host     string
	send     func(ch config.Chan, n Notification) error
	queue    chan delivery
	Logf     func(string, ...any)
}

// NewEngine returns an engine. statePath persists firing state across
// restarts so an alert does not re-fire.
func NewEngine(rules []Rule, channels map[string]config.Chan, statePath string, events func(model.Event), silenced func(int64, string, string) bool) *Engine {
	host, _ := os.Hostname()
	e := &Engine{rules: rules, channels: channels, state: map[string]*alertState{}, path: statePath, events: events, silenced: silenced, host: host, Logf: func(string, ...any) {}}
	e.queue = make(chan delivery, 1024)
	go e.deliverLoop()
	e.send = e.deliver
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &e.state)
	}
	return e
}

// SetRules replaces the rules and channels after a config reload.
func (e *Engine) SetRules(rules []Rule, channels map[string]config.Chan) {
	e.mu.Lock()
	e.rules, e.channels = rules, channels
	e.mu.Unlock()
}

// Firing returns the firing alerts per node.
func (e *Engine) Firing() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for k, s := range e.state {
		if s.Firing {
			rule, node, _ := strings.Cut(k, "|")
			out[node] = append(out[node], rule)
		}
	}
	return out
}

// Evaluate runs every rule for one node. Firing needs the condition true
// for the full duration, and clearing needs it false for the cooldown, so
// alerts do not flap.
func (e *Engine) Evaluate(node string, in Input) {
	e.mu.Lock()
	rules := e.rules
	e.mu.Unlock()
	now := in.Now
	changed := false
	for _, r := range rules {
		if len(r.Nodes) > 0 && !contains(r.Nodes, node) {
			continue
		}
		hit, why := r.expr.Eval(in)
		key := r.Name + "|" + node
		e.mu.Lock()
		st := e.state[key]
		if st == nil {
			st = &alertState{}
			e.state[key] = st
		}
		forD := r.For.D()
		cool := r.Cooldown.D()
		if cool == 0 {
			cool = max(forD, time.Minute)
		}
		repeat := r.Repeat.D()
		if repeat == 0 {
			repeat = time.Hour
		}
		var note *Notification
		if hit {
			st.FalseSince = 0
			if st.TrueSince == 0 {
				st.TrueSince = now.UnixNano()
			}
			st.Message = why
			if !st.Firing && now.Sub(time.Unix(0, st.TrueSince)) >= forD {
				st.Firing, st.FiredAt, st.LastSent = true, now.UnixNano(), now.UnixNano()
				note = &Notification{State: "firing"}
				changed = true
			} else if st.Firing && now.Sub(time.Unix(0, st.LastSent)) >= repeat {
				st.LastSent = now.UnixNano()
				note = &Notification{State: "repeat"}
			}
		} else {
			st.TrueSince = 0
			if st.Firing {
				if st.FalseSince == 0 {
					st.FalseSince = now.UnixNano()
				}
				if now.Sub(time.Unix(0, st.FalseSince)) >= cool {
					st.Firing = false
					note = &Notification{State: "cleared"}
					why = "cleared: " + st.Message
					changed = true
				}
			}
		}
		since := st.FiredAt
		e.mu.Unlock()
		if note == nil {
			continue
		}
		sev := r.Severity
		if sev == "" {
			sev = model.SevCritical
		}
		note.Alert, note.Node, note.Severity, note.Message, note.Since, note.Time, note.Host, note.Expr = r.Name, node, sev, why, since, now.UnixNano(), e.host, r.Expr
		kind := map[string]string{"firing": model.EvAlertFired, "repeat": model.EvAlertRepeated, "cleared": model.EvAlertCleared}[note.State]
		evSev := sev
		if note.State == "cleared" {
			evSev = model.SevInfo
		}
		silenced := e.silenced != nil && e.silenced(now.UnixNano(), node, r.Name)
		attrs := map[string]any{"alert": r.Name, "expr": r.Expr, "silenced": silenced}
		if e.events != nil {
			e.events(model.Event{Time: now.UnixNano(), Node: node, Kind: kind, Severity: evSev, Msg: fmt.Sprintf("%s: %s", r.Name, why), Attrs: attrs})
		}
		if silenced {
			continue
		}
		for _, chName := range r.Channels {
			e.mu.Lock()
			ch, ok := e.channels[chName]
			e.mu.Unlock()
			if !ok {
				continue
			}
			e.enqueue(delivery{ch: ch, n: *note, rule: r.Name, chName: chName})
		}
	}
	if changed {
		e.save()
	}
}

func (e *Engine) save() {
	if e.path == "" {
		return
	}
	e.mu.Lock()
	b, err := json.Marshal(e.state)
	e.mu.Unlock()
	if err == nil {
		fsx.WriteAtomic(e.path, b, 0o600)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// deliver sends one notification over a channel.
func (e *Engine) deliver(ch config.Chan, n Notification) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Send(ctx, ch, n)
}

// Send delivers a notification. It is exported for `algodtop check`.
func Send(ctx context.Context, ch config.Chan, n Notification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	switch ch.Type {
	case "webhook":
		if ch.Template != "" {
			t, err := template.New("t").Funcs(template.FuncMap{"json": func(v any) string { b, _ := json.Marshal(v); return string(b) }}).Parse(ch.Template)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := t.Execute(&buf, n); err != nil {
				return err
			}
			body = buf.Bytes()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "algodtop")
		for k, v := range ch.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
		}
		return nil
	case "command":
		cmd := exec.CommandContext(ctx, ch.Command[0], ch.Command[1:]...)
		cmd.Stdin = bytes.NewReader(append(body, '\n'))
		cmd.Env = append(os.Environ(), "ALGODTOP_ALERT="+n.Alert, "ALGODTOP_NODE="+n.Node, "ALGODTOP_STATE="+n.State, "ALGODTOP_MESSAGE="+n.Message)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "smtp":
		s := ch.SMTP
		pass := s.Password
		if s.PasswordFile != "" {
			b, err := os.ReadFile(s.PasswordFile)
			if err != nil {
				return err
			}
			pass = strings.TrimSpace(string(b))
		}
		var auth smtp.Auth
		if s.Username != "" {
			host := s.Addr
			if i := strings.LastIndexByte(host, ':'); i >= 0 {
				host = host[:i]
			}
			auth = smtp.PlainAuth("", s.Username, pass, host)
		}
		subj := fmt.Sprintf("[algod.top] %s %s on %s", strings.ToUpper(n.State), n.Alert, n.Node)
		msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n\r\nnode: %s\r\nhost: %s\r\nseverity: %s\r\ntime: %s\r\n",
			s.From, strings.Join(s.To, ", "), subj, n.Message, n.Node, n.Host, n.Severity, time.Unix(0, n.Time).UTC().Format(time.RFC3339))
		return smtp.SendMail(s.Addr, auth, s.From, s.To, []byte(msg))
	}
	return fmt.Errorf("unknown channel type %q", ch.Type)
}

// Heartbeat pings an external URL every interval, a dead-man switch. If the
// daemon or the machine dies, that service sends the alert.
func Heartbeat(ctx context.Context, url string, every time.Duration, logf func(string, ...any)) {
	if url == "" {
		return
	}
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
		if req != nil {
			req.Header.Set("User-Agent", "algodtop")
			if resp, err := http.DefaultClient.Do(req); err != nil {
				logf("heartbeat: %v", err)
			} else {
				resp.Body.Close()
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type delivery struct {
	ch     config.Chan
	n      Notification
	rule   string
	chName string
}

// enqueue hands a notification to the delivery worker. One worker keeps
// the order, so a receiver never sees "cleared" before "repeat". A slow
// channel delays later notifications rather than reordering them.
func (e *Engine) enqueue(d delivery) {
	select {
	case e.queue <- d:
	default:
		e.Logf("alert %s: channel %s: delivery queue full, notification dropped", d.rule, d.chName)
	}
}

func (e *Engine) deliverLoop() {
	for d := range e.queue {
		if err := e.send(d.ch, d.n); err != nil {
			e.Logf("alert %s: channel %s: %v", d.rule, d.chName, err)
		}
	}
}
