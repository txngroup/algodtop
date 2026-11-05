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
