package alert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/health"
	"algod.top/internal/model"
)

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeSMTP is enough of an SMTP server for net/smtp: EHLO, optional
// STARTTLS and AUTH PLAIN, MAIL, RCPT, DATA and QUIT.
type fakeSMTP struct {
	addr     string
	starttls *tls.Config // offer STARTTLS with this config
	rcpt     string      // reply to RCPT, "250 ok" when empty
	mu       sync.Mutex
	msgs     []string
	auth     []string // decoded AUTH PLAIN credentials
}

func startSMTP(t *testing.T, ln net.Listener, f *fakeSMTP) *fakeSMTP {
	t.Helper()
	f.addr = ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { c.Close() }()
	tc := textproto.NewConn(c)
	tc.PrintfLine("220 fake ESMTP")
	for {
		line, err := tc.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if _, isTLS := c.(*tls.Conn); f.starttls != nil && !isTLS {
				tc.PrintfLine("250-fake")
				tc.PrintfLine("250-STARTTLS")
				tc.PrintfLine("250 AUTH PLAIN")
			} else {
				tc.PrintfLine("250-fake")
				tc.PrintfLine("250 AUTH PLAIN")
			}
		case "STARTTLS":
			tc.PrintfLine("220 ready")
			c = tls.Server(c, f.starttls)
			tc = textproto.NewConn(c)
		case "AUTH":
			_, b64, _ := strings.Cut(arg, " ")
			b, _ := base64.StdEncoding.DecodeString(b64)
			f.mu.Lock()
			f.auth = append(f.auth, string(b))
			f.mu.Unlock()
			tc.PrintfLine("235 ok")
		case "MAIL":
			tc.PrintfLine("250 ok")
		case "RCPT":
			if f.rcpt != "" {
				tc.PrintfLine("%s", f.rcpt)
			} else {
				tc.PrintfLine("250 ok")
			}
		case "DATA":
			tc.PrintfLine("354 go ahead")
			b, err := tc.ReadDotBytes()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.msgs = append(f.msgs, string(b))
			f.mu.Unlock()
			tc.PrintfLine("250 queued")
		case "QUIT":
			tc.PrintfLine("221 bye")
			return
		default:
			tc.PrintfLine("502 unknown command")
		}
	}
}

func (f *fakeSMTP) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

func localListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// testTLS returns a server config and makes the client trust it.
func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	old := smtpTLS
	smtpTLS = func(host string) *tls.Config { return &tls.Config{ServerName: host, RootCAs: pool} }
	t.Cleanup(func() { smtpTLS = old })
	return srv.TLS.Clone()
}

func mailChan(addr string) config.Chan {
	return config.Chan{Type: "smtp", SMTP: &config.SMTP{Addr: addr, From: "algod.top@example.com", To: []string{"ops@example.com"}}}
}

// net/smtp takes no context. A server that accepts and never greets must
// not hold the channel past the deadline.
func TestSMTPHonorsDeadline(t *testing.T) {
	ln := localListener(t)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // and never write
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	err := Send(ctx, mailChan(ln.Addr().String()), Notification{Alert: "a", Node: "n", State: "firing"})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("Send took %s with a 300ms deadline", d)
	}
}

func TestSMTPMessage(t *testing.T) {
	f := startSMTP(t, localListener(t), &fakeSMTP{})
	n := Notification{Alert: "caf\u00e9", Node: "mainnet", State: "firing", Severity: "critical", Message: "algod is down", Time: time.Now().UnixNano(), Host: "h"}
	if err := Send(context.Background(), mailChan(f.addr), n); err != nil {
		t.Fatal(err)
	}
	msgs := f.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	m := msgs[0]
	for _, want := range []string{"Date: ", "Message-ID: <", "@example.com>", "Subject: =?utf-8?q?[algod.top]_FIRING_caf=C3=A9_on_mainnet?=",
		"MIME-Version: 1.0", "algod is down", "node: mainnet"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
}

// A 5xx reply is permanent, so the engine does not try again.
func TestSMTPRejectIsPermanent(t *testing.T) {
	f := startSMTP(t, localListener(t), &fakeSMTP{rcpt: "550 no such user"})
	err := Send(context.Background(), mailChan(f.addr), Notification{})
	var p permanent
	if err == nil || !errors.As(err, &p) {
		t.Fatalf("err %v is not permanent", err)
	}
	f = startSMTP(t, localListener(t), &fakeSMTP{rcpt: "451 try later"})
	err = Send(context.Background(), mailChan(f.addr), Notification{})
	if err == nil || errors.As(err, &p) {
		t.Fatalf("err %v should allow a retry", err)
	}
}

// password_file takes ~ like every other path in the config, and AUTH
// runs after STARTTLS.
func TestSMTPStartTLSAndPasswordFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, "smtp.pw"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startSMTP(t, localListener(t), &fakeSMTP{starttls: testTLS(t)})
	ch := mailChan(f.addr)
	ch.SMTP.Username, ch.SMTP.PasswordFile = "u", "~/smtp.pw"
	if err := Send(context.Background(), ch, Notification{Alert: "a", Node: "n", State: "firing"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.msgs) != 1 || len(f.auth) != 1 || f.auth[0] != "\x00u\x00secret" {
		t.Fatalf("msgs %d auth %q", len(f.msgs), f.auth)
	}
}

// Port 465 speaks TLS from the first byte.
func TestSMTPImplicitTLS(t *testing.T) {
	cfg := testTLS(t)
	f := startSMTP(t, tls.NewListener(localListener(t), cfg), &fakeSMTP{})
	_, port, _ := net.SplitHostPort(f.addr)
	old := implicitTLSPort
	implicitTLSPort = port
	defer func() { implicitTLSPort = old }()
	if err := Send(context.Background(), mailChan(f.addr), Notification{Alert: "a", Node: "n", State: "firing"}); err != nil {
		t.Fatal(err)
	}
	if len(f.messages()) != 1 {
		t.Fatal("no message")
	}
}

// A command that leaves a background child holding its output returns
// once the command itself exits.
func TestCommandBackgroundChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ch := config.Chan{Type: "command", Command: []string{"sh", "-c", "sleep 6 & echo started"}}
	t0 := time.Now()
	if err := Send(context.Background(), ch, Notification{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("Send waited %s for the background child", d)
	}
}

type recorder struct {
	mu     sync.Mutex
	events []model.Event
}

func (r *recorder) add(e model.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) kinds(kind string) []model.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.Event
	for _, e := range r.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func downRule(t *testing.T, channels ...string) []Rule {
	t.Helper()
	rules, err := Compile([]config.AlertRule{{Name: "down", Expr: "down", Channels: channels}})
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

// step evaluates node n as down or up at t.
func step(e *Engine, up bool, at time.Time) {
	s := collector.State{Name: "n", Up: up, RoundAt: at.UnixNano()}
	e.Evaluate("n", Input{State: &s, Health: health.Evaluate(&s, health.Context{Now: at}), Now: at})
}

// A failure that may pass is tried again. One that will not is given up
// at once and recorded as an event.
func TestRetryAndGiveUp(t *testing.T) {
	var mu sync.Mutex
	codes := []int{503, 429, 200} // the firing notification gets through on the third try
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		code := http.StatusNotFound
		if hits < len(codes) {
			code = codes[hits]
		}
		hits++
		w.WriteHeader(code)
	}))
	defer srv.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return hits }
	var rec recorder
	e := NewEngine(downRule(t, "hook"), map[string]config.Chan{"hook": {Type: "webhook", URL: srv.URL}}, "", rec.add, nil)
	e.retry = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	now := time.Now()
	step(e, false, now)
	waitUntil(t, "three attempts", func() bool { return count() == 3 })
	time.Sleep(50 * time.Millisecond)
	if got := rec.kinds(model.EvAlertUndelivered); len(got) != 0 {
		t.Fatalf("undelivered after a retry succeeded: %+v", got)
	}
	// The cleared notification meets a 404, which no retry fixes.
	step(e, true, now.Add(time.Minute))
	step(e, true, now.Add(2*time.Minute))
	waitUntil(t, "an undelivered event", func() bool { return len(rec.kinds(model.EvAlertUndelivered)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if count() != 4 {
		t.Fatalf("%d attempts; a 404 is not retried", count())
	}
	ev := rec.kinds(model.EvAlertUndelivered)[0]
	if ev.Node != "n" || ev.Severity != model.SevError || !strings.Contains(ev.Msg, "cleared notification not delivered on hook: webhook returned HTTP 404") {
		t.Fatalf("event %+v", ev)
	}
}

// A channel that hangs holds back only its own notifications.
func TestSlowChannelDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer slow.Close()
	defer close(release) // before slow.Close, which waits for the handler
	var mu sync.Mutex
	fastGot := 0
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fastGot++
		mu.Unlock()
	}))
	defer fast.Close()
	chans := map[string]config.Chan{"slow": {Type: "webhook", URL: slow.URL}, "fast": {Type: "webhook", URL: fast.URL}}
	e := NewEngine(downRule(t, "slow", "fast"), chans, "", nil, nil)
	step(e, false, time.Now())
	waitUntil(t, "the fast channel", func() bool { mu.Lock(); defer mu.Unlock(); return fastGot == 1 })
}

// A rule removed from the config stops showing as firing, also after a
// restart.
func TestRemovedRuleStopsFiring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	rules, err := Compile([]config.AlertRule{{Name: "z", Expr: "down"}, {Name: "a|b", Expr: "down"}})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(rules, nil, path, nil, nil)
	step(e, false, time.Now())
	if got := e.Firing()["n"]; len(got) != 2 || got[0] != "a|b" || got[1] != "z" {
		t.Fatalf("firing %q", got)
	}
	e.SetRules(rules[1:], nil)
	if got := e.Firing()["n"]; len(got) != 1 || got[0] != "a|b" {
		t.Fatalf("after reload %q", got)
	}
	if got := NewEngine(rules, nil, path, nil, nil).Firing()["n"]; len(got) != 1 || got[0] != "a|b" {
		t.Fatalf("after restart %q", got)
	}
	if got := NewEngine(nil, nil, path, nil, nil).Firing(); len(got) != 0 {
		t.Fatalf("restart with no rules %v", got)
	}
}

// A repeat is saved, so a restart does not send another one at once.
func TestRepeatSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	var mu sync.Mutex
	var states []string
	e := NewEngine(downRule(t, "hook"), nil, path, nil, nil)
	now := time.Now()
	step(e, false, now)
	step(e, false, now.Add(61*time.Minute)) // repeat
	e = NewEngine(downRule(t, "hook"), map[string]config.Chan{"hook": {Type: "webhook", URL: "http://127.0.0.1:1"}}, path, nil, nil)
	e.send = func(_ config.Chan, n Notification) error {
		mu.Lock()
		states = append(states, n.State)
		mu.Unlock()
		return nil
	}
	step(e, false, now.Add(62*time.Minute))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 0 {
		t.Fatalf("restart sent %v", states)
	}
}

func TestCompileLeavesOutBadRules(t *testing.T) {
	rules, err := Compile([]config.AlertRule{{Name: "a", Expr: "down"}, {Name: "a", Expr: "stalled"}, {Name: " ", Expr: "down"},
		{Name: "b", Expr: "lag >"}, {Name: "c", Expr: "catchup"}})
	if len(rules) != 2 || rules[0].Name != "a" || rules[0].Expr != "down" || rules[1].Name != "c" {
		t.Fatalf("rules %+v", rules)
	}
	for _, want := range []string{`alert "a": another alert has this name`, `alert " ": needs a name`, `alert "b": `} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
}
