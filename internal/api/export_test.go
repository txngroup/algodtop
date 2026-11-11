package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/store"
)

func exportServer(t *testing.T) (*httptest.Server, *store.DB) {
	t.Helper()
	db, err := store.Open(store.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(New(Options{Hub: &stubHub{db: db}}))
	t.Cleanup(srv.Close)
	return srv, db
}

func getBody(t *testing.T, url string) (int, string, string) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, r.Header.Get("Content-Type"), string(b)
}

func readCSV(t *testing.T, s string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(s)).ReadAll()
	if err != nil {
		t.Fatalf("%v\n%s", err, s)
	}
	return rows
}

// `algod.top export --events` asks for CSV and every event in the range.
// The answer used to be the Events page's JSON, capped at its last 5000.
func TestEventsCSV(t *testing.T) {
	srv, db := exportServer(t)
	base := time.Now().Add(-time.Hour)
	const n = 5003
	for i := range n {
		e := model.Event{Time: base.Add(time.Duration(i) * time.Millisecond).UnixNano(), Node: "a", Kind: "connect", Severity: "info", Msg: fmt.Sprintf("connect %d, out", i)}
		if i == 0 {
			e.Attrs = map[string]any{"ip": "10.0.0.1", "network": "p2p"}
		}
		if err := db.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	code, ct, body := getBody(t, srv.URL+"/api/events?from=-2h&format=csv&limit=0")
	if code != 200 || ct != "text/csv" {
		t.Fatalf("%d %s: %s", code, ct, body)
	}
	rows := readCSV(t, body)
	if len(rows) != n+1 {
		t.Fatalf("%d rows, want %d", len(rows), n+1)
	}
	if got := fmt.Sprint(rows[0]); got != "[time node kind severity message attrs]" {
		t.Errorf("header %s", got)
	}
	first := rows[1]
	if first[1] != "a" || first[2] != "connect" || first[4] != "connect 0, out" || first[5] != `{"ip":"10.0.0.1","network":"p2p"}` {
		t.Errorf("first row %q", first)
	}
	if _, err := time.Parse(time.RFC3339Nano, first[0]); err != nil {
		t.Errorf("time %q: %v", first[0], err)
	}
	if rows[2][5] != "" {
		t.Errorf("no attrs: %q", rows[2][5])
	}

	// JSON without a limit keeps the Events page's cap.
	var js struct{ Events []model.Event }
	_, _, body = getBody(t, srv.URL+"/api/events?from=-2h")
	if err := json.Unmarshal([]byte(body), &js); err != nil || len(js.Events) != 5000 {
		t.Fatalf("json: %d events, %v", len(js.Events), err)
	}

	if code, _, _ := getBody(t, srv.URL+"/api/events?format=xml"); code != http.StatusBadRequest {
		t.Errorf("format=xml: %d", code)
	}
}

func TestSessionsCSV(t *testing.T) {
	srv, db := exportServer(t)
	start := time.Now().Add(-30 * time.Minute)
	for _, s := range []model.Session{
		{Node: "a", Network: "p2p", Direction: "out", Addr: "/ip4/10.0.0.1/tcp/4190", IP: "10.0.0.1", PeerID: "12D3", Start: start.UnixNano(), End: start.Add(time.Minute).UnixNano(), Reason: "ReadError", Source: "log", BytesIn: 10, BytesOut: 20, Protocols: []string{"/algorand-ws/1.0.0", "/algorand/vote"}},
		{Node: "a", Network: "ws", Direction: "in", Addr: "10.0.0.2:4160", IP: "10.0.0.2", Start: start.UnixNano(), Source: "poll", OpenBefore: true},
	} {
		if err := db.AppendSession(s); err != nil {
			t.Fatal(err)
		}
	}
	code, ct, body := getBody(t, srv.URL+"/api/sessions?node=a&from=-1h&format=csv")
	if code != 200 || ct != "text/csv" {
		t.Fatalf("%d %s: %s", code, ct, body)
	}
	rows := readCSV(t, body)
	if len(rows) != 3 {
		t.Fatalf("%d rows:\n%s", len(rows), body)
	}
	byNet := map[string][]string{}
	for _, r := range rows[1:] {
		byNet[r[1]] = r
	}
	p := byNet["p2p"]
	if p == nil || p[5] != "12D3" || p[7] == "" || p[8] != "ReadError" || p[11] != "10" || p[12] != "20" || p[13] != "/algorand-ws/1.0.0 /algorand/vote" {
		t.Errorf("p2p row %q", p)
	}
	ws := byNet["ws"]
	if ws == nil || ws[7] != "" || ws[10] != "true" {
		t.Errorf("open ws row %q", ws)
	}
}
