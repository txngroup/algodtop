package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"algod.top/internal/alert"
	"algod.top/internal/config"
)

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// check fails a rule the daemon would leave out, and --notify proves
// each channel works.
func TestCheckAlerts(t *testing.T) {
	var mu sync.Mutex
	var got []alert.Notification
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n alert.Notification
		json.NewDecoder(r.Body).Decode(&n)
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
	}))
	defer hook.Close()
	broken := httptest.NewServer(http.NotFoundHandler())
	defer broken.Close()
	cfg := &config.Config{
		Alerts: []config.AlertRule{
			{Name: "stall", Expr: "down or stalled", Channels: []string{"hook"}},
			{Name: "lagging", Expr: "lagg > 30s"},
			{Name: "stall", Expr: "catchup"},
			{Name: "keys", Expr: "rule:key_expiry", Severity: "warning"},
			{Name: "self", Expr: "rule:algodtop"},
		},
		Channels: map[string]config.Chan{"hook": {Type: "webhook", URL: hook.URL}, "broken": {Type: "webhook", URL: broken.URL}},
	}
	var fails int
	out := captureStdout(t, func() { fails = checkAlerts(cfg, false) })
	if fails != 2 {
		t.Errorf("%d failures without --notify, want 2:\n%s", fails, out)
	}
	for _, want := range []string{
		"ok   alert stall       down or stalled\n",
		`FAIL alert lagging     unknown variable "lagg"`,
		"FAIL alert stall       another alert has this name; the daemon runs only the first\n",
		`warn alert keys        severity "warning" is not one of info, warn, error, critical`,
		"ok   alert self        rule:algodtop\n",
		"info channel broken     webhook\ninfo channel hook       webhook\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	mu.Lock()
	if len(got) != 0 {
		t.Errorf("sent %d notifications without --notify", len(got))
	}
	mu.Unlock()

	out = captureStdout(t, func() { fails = checkAlerts(cfg, true) })
	if fails != 3 {
		t.Errorf("%d failures with --notify, want 3:\n%s", fails, out)
	}
	for _, want := range []string{"FAIL channel broken     webhook: webhook returned HTTP 404", "ok   channel hook       webhook: test notification sent"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].State != "test" || got[0].Alert != "test" {
		t.Fatalf("notifications %+v", got)
	}
}
