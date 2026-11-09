package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/health"
	"algod.top/internal/model"
)

func TestParseExpr(t *testing.T) {
	good := []string{"lag > 30s", "down or stalled", "health >= warn", "rule:key_expiry", "rss > 4GiB", "disk_free_pct < 10%", "agent_unreachable"}
	for _, s := range good {
		if _, err := ParseExpr(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"lag >", "foo > 1", "rule:nope", "lag ~ 3"} {
		if _, err := ParseExpr(s); err == nil {
			t.Errorf("%s parsed", s)
		}
	}
	e, _ := ParseExpr("rss > 4GiB")
	if e.Value != 4*(1<<30) {
		t.Fatal(e.Value)
	}
}

// A rule alert names every account the rule fired for.
func TestRuleAlertListsEveryReason(t *testing.T) {
	e, _ := ParseExpr("rule:key_expiry")
	h := health.Status{Level: health.Critical, Reasons: []health.Reason{
		{Rule: "key_expiry", Level: health.Critical, Text: "participation key for AAAAAA... expires in 20 hours"},
		{Rule: "version_behind", Level: health.Warn, Text: "version 5.0.5 is behind 5.0.6 on b"},
		{Rule: "key_expiry", Level: health.Critical, Text: "participation key for BBBBBB... expires in 22 hours"},
	}}
	ok, why := e.Eval(Input{State: &collector.State{Up: true}, Health: h, Now: time.Now()})
	if !ok || why != "participation key for AAAAAA... expires in 20 hours; participation key for BBBBBB... expires in 22 hours" {
		t.Fatalf("%v %q", ok, why)
	}
}

// A node whose token algod rejects has never had a round. "down or
// stalled" fires for it after 30 s and says why.
func TestStalledNamesTheStatusError(t *testing.T) {
	e, _ := ParseExpr("down or stalled")
	now := time.Now()
	s := &collector.State{Up: true, Healthy: true, StatusErr: "algod rejects the API token (HTTP 401)", StatusErrAt: now.Add(-time.Minute).UnixNano()}
	ok, why := e.Eval(Input{State: s, Now: now})
	if !ok || why != "cannot read the round for 60s: algod rejects the API token (HTTP 401)" {
		t.Fatalf("%v %q", ok, why)
	}
	s.StatusErrAt = now.Add(-10 * time.Second).UnixNano()
	if ok, why := e.Eval(Input{State: s, Now: now}); ok {
		t.Fatalf("fired 10 s after the first failure: %q", why)
	}
}

// The down alert names the reason, as the health rule does, and the
// cleared notification names what fired. A node back from down is briefly
// stalled until its next round, and that must not become the message.
func TestDownNamesTheReasonAndClearNamesWhatFired(t *testing.T) {
	var mu sync.Mutex
	var got []Notification
	rules, err := Compile([]config.AlertRule{{Name: "down", Expr: "down or stalled", For: config.Duration(10 * time.Second), Channels: []string{"c"}}})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(rules, map[string]config.Chan{"c": {Type: "webhook", URL: "http://127.0.0.1:1"}}, filepath.Join(t.TempDir(), "a.json"), nil, nil)
	e.send = func(_ config.Chan, n Notification) error {
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
		return nil
	}
	now := time.Now()
	lastRound := now.Add(-5 * time.Second).UnixNano()
	down := collector.State{Name: "n", Up: false, LastError: "REST unreachable: context deadline exceeded", RoundAt: lastRound}
	step := func(s collector.State, at time.Time) {
		e.Evaluate("n", Input{State: &s, Now: at})
	}
	step(down, now)
	step(down, now.Add(11*time.Second))
	// Back up 80 s after the last round: stalled until the next one.
	up := collector.State{Name: "n", Up: true, Healthy: true, RoundAt: lastRound}
	step(up, now.Add(75*time.Second))
	up.RoundAt = now.Add(78 * time.Second).UnixNano()
	step(up, now.Add(78*time.Second))
	up.RoundAt = now.Add(138 * time.Second).UnixNano()
	step(up, now.Add(140*time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("notifications %+v", got)
	}
	if got[0].State != "firing" || got[0].Message != "algod is down (REST unreachable: context deadline exceeded)" {
		t.Errorf("firing %q %q", got[0].State, got[0].Message)
	}
	if got[1].State != "cleared" || got[1].Message != "cleared: algod is down (REST unreachable: context deadline exceeded)" {
		t.Errorf("cleared %q %q", got[1].State, got[1].Message)
	}
}

func TestFireRepeatClearAndSilence(t *testing.T) {
	var mu sync.Mutex
	var got []Notification
	var events []model.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &n)
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
	}))
	defer srv.Close()
	rules, err := Compile([]config.AlertRule{{Name: "stall", Expr: "down or stalled", For: config.Duration(time.Minute), Repeat: config.Duration(time.Hour), Channels: []string{"hook"}}})
	if err != nil {
		t.Fatal(err)
	}
	silenced := false
	e := NewEngine(rules, map[string]config.Chan{"hook": {Type: "webhook", URL: srv.URL}}, filepath.Join(t.TempDir(), "a.json"),
		func(ev model.Event) { mu.Lock(); events = append(events, ev); mu.Unlock() },
		func(int64, string, string) bool { return silenced })
	now := time.Now()
	down := collector.State{Name: "n", Up: false}
	up := collector.State{Name: "n", Up: true, RoundAt: now.UnixNano()}
	step := func(s collector.State, at time.Time) {
		e.Evaluate("n", Input{State: &s, Health: health.Evaluate(&s, health.Context{Now: at}), Now: at})
	}
	step(down, now)
	step(down, now.Add(30*time.Second))
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(got) != 0 {
		t.Fatal("fired before the duration")
	}
	mu.Unlock()
	step(down, now.Add(61*time.Second))
	step(down, now.Add(30*time.Minute))
	step(down, now.Add(62*time.Minute))
	up.RoundAt = now.Add(63 * time.Minute).UnixNano()
	step(up, now.Add(63*time.Minute))
	up.RoundAt = now.Add(65 * time.Minute).UnixNano()
	step(up, now.Add(65*time.Minute))
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	states := []string{}
	for _, n := range got {
		states = append(states, n.State)
	}
	mu.Unlock()
	if len(states) != 3 || states[0] != "firing" || states[1] != "repeat" || states[2] != "cleared" {
		t.Fatalf("notifications %v", states)
	}
	// A silenced alert records events but sends nothing.
	silenced = true
	step(down, now.Add(70*time.Minute))
	step(down, now.Add(72*time.Minute))
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("silenced alert sent %d", len(got)-3)
	}
	last := events[len(events)-1]
	if last.Kind != model.EvAlertFired || last.Attrs["silenced"] != true {
		t.Fatalf("event %+v", last)
	}
}

func TestCommandChannel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	out := filepath.Join(t.TempDir(), "out.json")
	ch := config.Chan{Type: "command", Command: []string{"sh", "-c", "cat > " + out}}
	if err := Send(context.Background(), ch, Notification{Alert: "a", Node: "n", State: "firing"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	var n Notification
	if json.Unmarshal(b, &n) != nil || n.Alert != "a" {
		t.Fatalf("command got %q", b)
	}
}
