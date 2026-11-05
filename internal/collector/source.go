package collector

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"algod.top/internal/algod"
)

// Files is what a node's data directory says right now.
type Files struct {
	Net        string // REST address
	PID        int    // 0 when algod.pid is absent
	Start      int64  // process start in unix ns, when the source knows it
	Config     *algod.Config
	Token      string
	AdminToken string
	Genesis    algod.Genesis
	LogPaths   algod.LogPaths
}

// Source abstracts how a node is reached: a local data directory, a REST
// URL, or SSH without install.
type Source interface {
	// Kind is local, rest or ssh.
	Kind() string
	// Files reads the data directory. REST sources return nil.
	Files(ctx context.Context) (*Files, error)
	// Dialers returns the dial functions for ordinary requests and the
	// long-poll, or nil for plain TCP.
	Dialers(addr string) (dial, poll func(ctx context.Context) (net.Conn, error))
	// Local reports whether process stats and logs are readable here.
	Local() bool
	// Close releases connections.
	Close()
}

// LocalSource reads a data directory on this machine.
type LocalSource struct {
	Dir            string
	TokenFile      string
	AdminTokenFile string
	Token          string // from the config file or environment
	AdminToken     string

	mu     sync.Mutex
	cfg    *algod.Config
	cfgKey string
	gen    algod.Genesis
	genKey string
	files  map[string]cachedFile
}

// Kind implements Source.
func (s *LocalSource) Kind() string { return "local" }

// Local implements Source.
func (s *LocalSource) Local() bool { return true }

// Close implements Source.
func (s *LocalSource) Close() {}

// Dialers implements Source.
func (s *LocalSource) Dialers(string) (func(context.Context) (net.Conn, error), func(context.Context) (net.Conn, error)) {
	return nil, nil
}

// Files implements Source.
func (s *LocalSource) Files(context.Context) (*Files, error) {
	d := algod.DataDir{Path: s.Dir}
	f := &Files{}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Small files are read again only when their size or time changes: a
	// stat is one system call, a read is three.
	f.Net = s.cached("algod.net")
	f.PID, _ = strconv.Atoi(s.cached("algod.pid"))
	// config.json and genesis.json are parsed again only when they change;
	// genesis.json is tens of kilobytes on Mainnet.
	cpath := filepath.Join(s.Dir, "config.json")
	ckey := statKey(cpath)
	if s.cfg == nil || ckey != s.cfgKey {
		c, err := d.Config()
		if err != nil {
			// A config.json that does not parse is reported, but the node
			// is still monitored with what we know.
			c = &algod.Config{Values: map[string]any{}, Hash: "unparseable"}
		}
		s.cfg, s.cfgKey = c, ckey
	}
	f.Config = s.cfg
	gkey := statKey(filepath.Join(s.Dir, "genesis.json"))
	if gkey != s.genKey {
		s.gen, _ = d.Genesis()
		s.genKey = gkey
	}
	f.Genesis = s.gen
	f.Token = firstNonEmpty(s.Token, readFile(s.TokenFile), s.cached("algod.token"))
	f.AdminToken = firstNonEmpty(s.AdminToken, readFile(s.AdminTokenFile), s.cached("algod.admin.token"))
	f.LogPaths = d.LogPaths(f.Config)
	return f, nil
}

type cachedFile struct {
	key string
	val string
}

// cached returns a trimmed small file from the data directory, or "".
func (s *LocalSource) cached(name string) string {
	p := filepath.Join(s.Dir, name)
	key := statKey(p)
	if c, ok := s.files[name]; ok && c.key == key {
		return c.val
	}
	val := ""
	if key != "missing" {
		if b, err := os.ReadFile(p); err == nil {
			val = strings.TrimSpace(string(b))
		}
	}
	if s.files == nil {
		s.files = map[string]cachedFile{}
	}
	s.files[name] = cachedFile{key, val}
	return val
}

// statKey identifies a file version by size and modification time.
func statKey(p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
}

// PIDOnly reads just algod.pid, for the check before each scrape.
func (s *LocalSource) PIDOnly() int {
	pid, _ := algod.DataDir{Path: s.Dir}.PID()
	return pid
}

func readFile(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(algod.ExpandHome(p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// RESTSource reaches algod by URL only. Process, log and host data need an
// agent on that host.
type RESTSource struct {
	URL        string
	Token      string
	AdminToken string
}

// Kind implements Source.
func (s *RESTSource) Kind() string { return "rest" }

// Local implements Source.
func (s *RESTSource) Local() bool { return false }

// Close implements Source.
func (s *RESTSource) Close() {}

// Dialers implements Source.
func (s *RESTSource) Dialers(string) (func(context.Context) (net.Conn, error), func(context.Context) (net.Conn, error)) {
	return nil, nil
}

// Files implements Source.
func (s *RESTSource) Files(context.Context) (*Files, error) {
	return &Files{Net: s.URL, Token: s.Token, AdminToken: s.AdminToken}, nil
}

// TokensFromEnv returns $ALGOD_TOKEN and $ALGOD_ADMIN_TOKEN.
func TokensFromEnv() (string, string) {
	return os.Getenv("ALGOD_TOKEN"), os.Getenv("ALGOD_ADMIN_TOKEN")
}
