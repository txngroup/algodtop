package api

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Roles.
const (
	RoleNone  = 0
	RoleRead  = 1
	RoleWrite = 2
)

const pbkdf2Iter = 600_000

// HashPassword hashes a password with PBKDF2-SHA256 and a random salt:
// pbkdf2-sha256$600000$<salt>$<hash>.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword verifies a password against a hash in constant time.
func CheckPassword(hash, pw string) bool {
	p := strings.Split(hash, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// HashToken returns the sha256 hex of a bearer token, as stored in the
// config.
func HashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// NewToken returns a random 256-bit token.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthConfig is the TCP listener's auth settings.
type AuthConfig struct {
	PasswordHash         string
	ReadOnlyPasswordHash string
	ReadOnlyTokenHashes  []string
	SessionTTL           time.Duration
	TLS                  bool
	AllowedHosts         []string // Host header values accepted, beyond loopback
	Logf                 func(string, ...any)
}

type session struct {
	role    int
	csrf    string
	expires time.Time
}

// Auth holds sessions, login rate limits and the CSRF secret.
type Auth struct {
	mu       sync.Mutex
	cfg      AuthConfig
	sessions map[string]*session
	attempts map[string][]time.Time
	anonCSRF string
}

// NewAuth returns an Auth for a TCP listener.
func NewAuth(cfg AuthConfig) *Auth {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Auth{cfg: cfg, sessions: map[string]*session{}, attempts: map[string][]time.Time{}, anonCSRF: NewToken()}
}

// SetConfig updates passwords after a reload.
func (a *Auth) SetConfig(cfg AuthConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.Logf == nil {
		cfg.Logf = a.cfg.Logf
	}
	a.cfg = cfg
}

// Required reports whether any password is set.
func (a *Auth) Required() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.PasswordHash != "" || a.cfg.ReadOnlyPasswordHash != "" || len(a.cfg.ReadOnlyTokenHashes) > 0
}

const cookieName = "algodtop_session"

// role returns the caller's role and CSRF token.
func (a *Auth) role(r *http.Request) (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.PasswordHash == "" && a.cfg.ReadOnlyPasswordHash == "" && len(a.cfg.ReadOnlyTokenHashes) == 0 {
		// No password: the listener is loopback, or the operator allowed
		// unauthenticated remote access. Writes still need the CSRF token.
		return RoleWrite, a.anonCSRF
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		sum := HashToken(strings.TrimPrefix(h, "Bearer "))
		for _, t := range a.cfg.ReadOnlyTokenHashes {
			if subtle.ConstantTimeCompare([]byte(sum), []byte(t)) == 1 {
				return RoleRead, ""
			}
		}
		return RoleNone, ""
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return RoleNone, ""
	}
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(a.sessions, c.Value)
		return RoleNone, ""
	}
	return s.role, s.csrf
}

var errRateLimited = errors.New("too many login attempts; wait a minute")

// login checks a password and returns a session cookie.
func (a *Auth) login(w http.ResponseWriter, r *http.Request, pw string) (int, error) {
	ip := clientIP(r)
	a.mu.Lock()
	now := time.Now()
	var recent []time.Time
	for _, t := range a.attempts[ip] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 5 {
		a.attempts[ip] = recent
		a.mu.Unlock()
		a.cfg.Logf("auth: login rate limited for %s", ip)
		return 0, errRateLimited
	}
	a.attempts[ip] = append(recent, now)
	cfg := a.cfg
	a.mu.Unlock()
	role := RoleNone
	if cfg.PasswordHash != "" && CheckPassword(cfg.PasswordHash, pw) {
		role = RoleWrite
	} else if cfg.ReadOnlyPasswordHash != "" && CheckPassword(cfg.ReadOnlyPasswordHash, pw) {
		role = RoleRead
	}
	if role == RoleNone {
		a.cfg.Logf("auth: failed login from %s", ip)
		return 0, errors.New("wrong password")
	}
	tok := NewToken()
	a.mu.Lock()
	a.sessions[tok] = &session{role: role, csrf: NewToken(), expires: now.Add(cfg.SessionTTL)}
	delete(a.attempts, ip)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: cfg.TLS, Expires: now.Add(cfg.SessionTTL)})
	return role, nil
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// hostAllowed rejects DNS rebinding: a browser on another site that
// rebinds a name to 127.0.0.1 sends that name as Host.
func (a *Auth) hostAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return true // a literal IP cannot be rebound
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.cfg.AllowedHosts {
		if h == "*" || strings.EqualFold(h, host) {
			return true
		}
	}
	return a.cfg.PasswordHash != "" || a.cfg.ReadOnlyPasswordHash != ""
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}
