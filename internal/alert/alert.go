// Package alert evaluates alert rules over the derived values that health
// uses, with fixed functions and no general query language. Channels are
// webhook, SMTP and command, and they are defined only in the config file.
package alert

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"algod.top/internal/algod"
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
		why := fmt.Sprintf("last round is %.0fs old", lag)
		if s.StatusErr != "" {
			why = fmt.Sprintf("cannot read the round for %.0fs: %s", lag, s.StatusErr)
		}
		return s.Up && !s.Catchup && lag > 30, why
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

// Compile parses the configured rules. A rule that fails is left out and
// named in the error, so the others still run. Two rules with one name
// would share firing state, so the second is left out.
func Compile(rules []config.AlertRule) ([]Rule, error) {
	var out []Rule
	var errs []error
	seen := map[string]bool{}
	for _, r := range rules {
		e, err := parseRule(r)
		if err == nil && seen[r.Name] {
			err = errors.New("another alert has this name")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("alert %q: %w", r.Name, err))
			continue
		}
		seen[r.Name] = true
		out = append(out, Rule{AlertRule: r, expr: e})
	}
	return out, errors.Join(errs...)
}

// CheckRule reports why Compile would leave out one rule on its own.
func CheckRule(r config.AlertRule) error {
	_, err := parseRule(r)
	return err
}

func parseRule(r config.AlertRule) (*Expr, error) {
	if strings.TrimSpace(r.Name) == "" {
		return nil, errors.New("needs a name")
	}
	return ParseExpr(r.Expr)
}

// Severities are the values an alert's severity can take.
var Severities = []string{model.SevInfo, model.SevWarn, model.SevError, model.SevCritical}

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
	queues   map[string]chan delivery // per channel name
	retry    []time.Duration          // waits between attempts
	Logf     func(string, ...any)
}

// retryAfter spaces the attempts at a notification over about six
// minutes, which outlasts a webhook service's restart or a DNS blip.
var retryAfter = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

// NewEngine returns an engine. statePath persists firing state across
// restarts so an alert does not re-fire.
func NewEngine(rules []Rule, channels map[string]config.Chan, statePath string, events func(model.Event), silenced func(int64, string, string) bool) *Engine {
	host, _ := os.Hostname()
	e := &Engine{rules: rules, channels: channels, state: map[string]*alertState{}, path: statePath, events: events, silenced: silenced, host: host,
		queues: map[string]chan delivery{}, retry: retryAfter, Logf: func(string, ...any) {}}
	e.send = e.deliver
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &e.state)
	}
	if e.state == nil {
		e.state = map[string]*alertState{} // the file held null
	}
	e.prune()
	return e
}

// SetRules replaces the rules and channels after a config reload.
func (e *Engine) SetRules(rules []Rule, channels map[string]config.Chan) {
	e.mu.Lock()
	e.rules, e.channels = rules, channels
	// A removed channel's worker stops once it has sent what it holds.
	for name, q := range e.queues {
		if _, ok := channels[name]; !ok {
			close(q)
			delete(e.queues, name)
		}
	}
	e.mu.Unlock()
	if e.prune() {
		e.save()
	}
}

// prune drops the state of rules that are no longer configured. Without
// it a removed rule would show as firing forever. It reports whether it
// dropped anything.
func (e *Engine) prune() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := map[string]bool{}
	for _, r := range e.rules {
		names[r.Name] = true
	}
	gone := false
	for k := range e.state {
		if rule, _ := splitKey(k); !names[rule] {
			delete(e.state, k)
			gone = true
		}
	}
	return gone
}

// splitKey splits a state key. Node names cannot hold "|", and rule names
// can, so the last one separates them.
func splitKey(k string) (rule, node string) {
	i := strings.LastIndexByte(k, '|')
	if i < 0 {
		return k, ""
	}
	return k[:i], k[i+1:]
}

// Firing returns the firing alerts per node, sorted by name.
func (e *Engine) Firing() map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for k, s := range e.state {
		if s.Firing {
			rule, node := splitKey(k)
			out[node] = append(out[node], rule)
		}
	}
	for _, l := range out {
		sort.Strings(l)
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
				changed = true // or a restart would repeat at once
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
			e.enqueue(chName, r.Name, *note)
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

// permanent marks a failure that a second attempt would repeat, such as a
// webhook that answers 404.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Send delivers a notification once. It is exported for `algod.top check
// --notify`.
func Send(ctx context.Context, ch config.Chan, n Notification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return permanent{err}
	}
	switch ch.Type {
	case "webhook":
		if ch.Template != "" {
			t, err := template.New("t").Funcs(template.FuncMap{"json": func(v any) string { b, _ := json.Marshal(v); return string(b) }}).Parse(ch.Template)
			if err != nil {
				return permanent{err}
			}
			var buf bytes.Buffer
			if err := t.Execute(&buf, n); err != nil {
				return permanent{err}
			}
			body = buf.Bytes()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL, bytes.NewReader(body))
		if err != nil {
			return permanent{err}
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
			err := fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
			// A 4xx is a wrong URL, token or body, except for a timeout
			// or a rate limit.
			if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
				return permanent{err}
			}
			return err
		}
		return nil
	case "command":
		cmd := exec.CommandContext(ctx, ch.Command[0], ch.Command[1:]...)
		cmd.Stdin = bytes.NewReader(append(body, '\n'))
		cmd.Env = append(os.Environ(), "ALGODTOP_ALERT="+n.Alert, "ALGODTOP_NODE="+n.Node, "ALGODTOP_STATE="+n.State, "ALGODTOP_MESSAGE="+n.Message)
		// A program that starts a background child, which keeps its
		// output open, would otherwise hold the channel until that
		// child exits. The program itself succeeded in that case.
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.CombinedOutput()
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		if err != nil {
			// A command may have done its work before it failed, so it
			// runs once.
			return permanent{fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))}
		}
		return nil
	case "smtp":
		s := ch.SMTP
		pass := s.Password
		if s.PasswordFile != "" {
			b, err := os.ReadFile(algod.ExpandHome(s.PasswordFile))
			if err != nil {
				return permanent{err}
			}
			pass = strings.TrimSpace(string(b))
		}
		host, _, err := net.SplitHostPort(s.Addr)
		if err != nil {
			return permanent{err}
		}
		var auth smtp.Auth
		if s.Username != "" {
			auth = smtp.PlainAuth("", s.Username, pass, host)
		}
		return sendMail(ctx, s.Addr, auth, s.From, s.To, mailMessage(s, n, time.Now()))
	}
	return permanent{fmt.Errorf("unknown channel type %q", ch.Type)}
}

// mailMessage renders a notification as a plain text mail. Date and
// Message-ID are headers that spam filters expect.
func mailMessage(s *config.SMTP, n Notification, now time.Time) []byte {
	subj := fmt.Sprintf("[algod.top] %s %s on %s", strings.ToUpper(n.State), n.Alert, n.Node)
	domain := "algod.top"
	if i := strings.LastIndexByte(s.From, '@'); i >= 0 && i < len(s.From)-1 {
		domain = strings.TrimRight(s.From[i+1:], "> ")
	}
	var id [8]byte
	rand.Read(id[:])
	var b strings.Builder
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%d.%x@%s>\r\n", now.UnixNano(), id, domain)
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", s.From, strings.Join(s.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subj))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "%s\r\n\r\nnode: %s\r\nhost: %s\r\nseverity: %s\r\ntime: %s\r\n",
		n.Message, n.Node, n.Host, n.Severity, time.Unix(0, n.Time).UTC().Format(time.RFC3339))
	return []byte(b.String())
}

// sendMail does what smtp.SendMail does within ctx. net/smtp takes no
// context and sets no deadline, so a server that accepts the connection
// and never answers would hold the channel forever. Port 465 speaks TLS
// from the first byte. Other ports upgrade with STARTTLS when the server
// offers it.
func sendMail(ctx context.Context, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	err := sendMail1(ctx, addr, auth, from, to, msg)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("smtp %s: %w", addr, ctx.Err())
	}
	return err
}

// implicitTLSPort and smtpTLS are variables for the tests, which cannot
// listen on 465 or present a certificate the system trusts.
var (
	implicitTLSPort = "465"
	smtpTLS         = func(host string) *tls.Config { return &tls.Config{ServerName: host} }
)

func sendMail1(ctx context.Context, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return permanent{err}
	}
	implicit := port == implicitTLSPort
	var conn net.Conn
	if implicit {
		conn, err = (&tls.Dialer{Config: smtpTLS(host)}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	// An expired deadline fails the read or write in progress.
	defer context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })()
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return smtpErr(err)
	}
	defer c.Close()
	if !implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(smtpTLS(host)); err != nil {
				return smtpErr(err)
			}
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return permanent{errors.New("smtp: the server does not offer AUTH")}
		}
		if err := c.Auth(auth); err != nil {
			// PlainAuth refuses an unencrypted connection before the
			// server sees anything; that is permanent too.
			var te *textproto.Error
			if !errors.As(err, &te) {
				return permanent{err}
			}
			return smtpErr(err)
		}
	}
	if err := c.Mail(from); err != nil {
		return smtpErr(err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return smtpErr(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return smtpErr(err)
	}
	if _, err := w.Write(msg); err != nil {
		return smtpErr(err)
	}
	if err := w.Close(); err != nil {
		return smtpErr(err)
	}
	// The server has the message once it answers the data, so a failed
	// QUIT must not send it again.
	c.Quit()
	return nil
}

// smtpErr marks a 5xx reply permanent. A 4xx reply and a network error
// may pass.
func smtpErr(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return permanent{err}
	}
	return err
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
				if resp.StatusCode/100 != 2 {
					logf("heartbeat: HTTP %d", resp.StatusCode)
				}
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

// enqueue hands a notification to its channel's worker. One worker per
// channel keeps that channel's order, so a receiver never sees "cleared"
// before "repeat". A slow or failing channel delays its own later
// notifications and no other channel's.
func (e *Engine) enqueue(chName, rule string, n Notification) {
	e.mu.Lock()
	ch, ok := e.channels[chName]
	if !ok {
		e.mu.Unlock()
		return
	}
	q := e.queues[chName]
	if q == nil {
		q = make(chan delivery, 256)
		e.queues[chName] = q
		go e.deliverLoop(q)
	}
	queued := false
	select {
	case q <- delivery{ch: ch, n: n, rule: rule, chName: chName}:
		queued = true
	default:
	}
	e.mu.Unlock()
	if !queued {
		e.Logf("alert %s: channel %s: delivery queue full, notification dropped", rule, chName)
	}
}

func (e *Engine) deliverLoop(q chan delivery) {
	for d := range q {
		e.attempt(d)
	}
}

// attempt sends one notification and retries a failure that may pass. A
// notification that never gets through is recorded as an event, so the
// operator learns that a channel is broken.
func (e *Engine) attempt(d delivery) {
	var err error
	for i := 0; ; i++ {
		if err = e.send(d.ch, d.n); err == nil {
			return
		}
		var p permanent
		if errors.As(err, &p) || i >= len(e.retry) {
			break
		}
		e.Logf("alert %s: channel %s: %v; retrying in %s", d.rule, d.chName, err, e.retry[i])
		time.Sleep(e.retry[i])
	}
	e.Logf("alert %s: channel %s: %v", d.rule, d.chName, err)
	if e.events != nil {
		e.events(model.Event{Time: time.Now().UnixNano(), Node: d.n.Node, Kind: model.EvAlertUndelivered, Severity: model.SevError,
			Msg:   fmt.Sprintf("%s: %s notification not delivered on %s: %v", d.rule, d.n.State, d.chName, err),
			Attrs: map[string]any{"alert": d.rule, "channel": d.chName}})
	}
}
