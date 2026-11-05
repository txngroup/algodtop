package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/query"
	"algod.top/internal/store"
)

type stubHub struct{ db *store.DB }

func (h *stubHub) Nodes() []NodeView                                 { return []NodeView{} }
func (h *stubHub) Node(string) (NodeView, bool)                      { return NodeView{}, false }
func (h *stubHub) Status() DaemonStatus                              { return DaemonStatus{Host: "test"} }
func (h *stubHub) Store() *store.DB                                  { return h.db }
func (h *stubHub) Query() *query.Engine                              { return query.NewEngine(h.db, 2, time.Second) }
func (h *stubHub) Silences() []model.Silence                         { return nil }
func (h *stubHub) AddSilence(s model.Silence) (model.Silence, error) { return s, nil }
func (h *stubHub) DeleteSilence(string) error                        { return nil }
func (h *stubHub) Reload() error                                     { return nil }
func (h *stubHub) Snapshot() (string, error)                         { return "/x", nil }
func (h *stubHub) Subscribe(map[string]bool, []Sub) (<-chan Message, func()) {
	return make(chan Message), func() {}
}
func (h *stubHub) Discovered() []DiscoveredNode            { return nil }
func (h *stubHub) ConfigJSON() ([]byte, error)             { return []byte("{}"), nil }
func (h *stubHub) PutConfig(context.Context, []byte) error { return nil }
func (h *stubHub) AgentBatches(context.Context, string, time.Duration) ([]byte, string, error) {
	return nil, "", nil
}

func newTestServer(t *testing.T, ac AuthConfig) *httptest.Server {
	t.Helper()
	db, err := store.Open(store.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(New(Options{Hub: &stubHub{db: db}, Auth: NewAuth(ac)}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatal(h)
	}
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong") {
		t.Fatal("check")
	}
}

func TestLoginCSRFAndRoles(t *testing.T) {
	pw, _ := HashPassword("write-pass")
	ro, _ := HashPassword("read-pass")
	tok := NewToken()
	srv := newTestServer(t, AuthConfig{PasswordHash: pw, ReadOnlyPasswordHash: ro, ReadOnlyTokenHashes: []string{HashToken(tok)}})
	c := srv.Client()
	if r, _ := c.Get(srv.URL + "/api/nodes"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d", r.StatusCode)
	}
	// A read-only bearer token can read but not write.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if r, _ := c.Do(req); r.StatusCode != 200 {
		t.Fatalf("token read: %d", r.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if r, _ := c.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("token write: %d", r.StatusCode)
	}
	// Login with the write password; the cookie is HttpOnly, SameSite=Strict.
	jar, _ := cookiejar.New(nil)
	c = &http.Client{Jar: jar}
	r, _ := c.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"write-pass"}`))
	if r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	sc := r.Header.Get("Set-Cookie")
	if !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Strict") {
		t.Fatalf("cookie flags: %s", sc)
	}
	// Writes need the CSRF token from /api/session.
	r, _ = c.Post(srv.URL+"/api/admin/reload", "application/json", nil)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF: %d", r.StatusCode)
	}
	var sess struct {
		Role string `json:"role"`
		CSRF string `json:"csrf"`
	}
	r, _ = c.Get(srv.URL + "/api/session")
	decodeBody(r, &sess)
	if sess.Role != "write" || sess.CSRF == "" {
		t.Fatalf("session %+v", sess)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/admin/reload", nil)
	req.Header.Set("X-CSRF-Token", sess.CSRF)
	if r, _ := c.Do(req); r.StatusCode != 200 {
		t.Fatalf("write with CSRF: %d", r.StatusCode)
	}
	// A cross-origin write is refused even with the token.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/admin/reload", nil)
	req.Header.Set("X-CSRF-Token", sess.CSRF)
	req.Header.Set("Origin", "https://evil.example.com")
	if r, _ := c.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin write: %d", r.StatusCode)
	}
	// The read-only password gets a read session.
	jar2, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar2}
	c2.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"read-pass"}`))
	r, _ = c2.Get(srv.URL + "/api/session")
	decodeBody(r, &sess)
	if sess.Role != "read" {
		t.Fatalf("read-only login role %q", sess.Role)
	}
}

func TestLoginRateLimit(t *testing.T) {
	pw, _ := HashPassword("pw-123456")
	srv := newTestServer(t, AuthConfig{PasswordHash: pw})
	codes := []int{}
	for range 7 {
		r, _ := srv.Client().Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"nope"}`))
		codes = append(codes, r.StatusCode)
	}
	if codes[0] != http.StatusUnauthorized || codes[6] != http.StatusTooManyRequests {
		t.Fatalf("codes %v", codes)
	}
}

func TestNoPasswordStillNeedsCSRF(t *testing.T) {
	srv := newTestServer(t, AuthConfig{})
	r, _ := srv.Client().Get(srv.URL + "/api/nodes")
	if r.StatusCode != 200 {
		t.Fatalf("read without password: %d", r.StatusCode)
	}
	r, _ = srv.Client().Post(srv.URL+"/api/admin/reload", "application/json", nil)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("write without CSRF: %d", r.StatusCode)
	}
}

func TestClusters(t *testing.T) {
	base := time.Date(2026, 11, 2, 21, 1, 0, 0, time.UTC).UnixNano()
	morning := time.Date(2026, 11, 3, 8, 40, 0, 0, time.UTC).UnixNano()
	var ss []model.Session
	for i := range 9 {
		ip := "203.0.113." + string(rune('1'+i))
		ss = append(ss, model.Session{IP: ip, Direction: "in", Start: base - 3600e9, End: base + int64(i)*1e9, Reason: "ReadError"})
		ss = append(ss, model.Session{IP: ip, Direction: "in", Start: base + 60e9, End: morning + int64(i)*2e9, Reason: "ReadError"})
	}
	ss = append(ss, model.Session{IP: "198.51.100.1", Direction: "in", Start: base, End: base + 3600e9})
	cl := FindClusters(ss, 30*time.Second, 5)
	if len(cl) != 2 || len(cl[0].IPs) != 9 || len(cl[1].Repeat) != 9 {
		t.Fatalf("clusters %+v", cl)
	}
}

func decodeBody(r *http.Response, v any) {
	defer r.Body.Close()
	jsonDecode(r, v)
}

func jsonDecode(r *http.Response, v any) { _ = decodeJSON(r.Body, v) }
