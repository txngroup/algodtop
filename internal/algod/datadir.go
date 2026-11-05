package algod

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DataDir is a local algod data directory.
type DataDir struct {
	Path string
}

// ExpandHome replaces a leading ~ with the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

func (d DataDir) file(name string) string { return filepath.Join(d.Path, name) }

func readTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Net returns the REST address from algod.net, for example
// 127.0.0.1:14001. algod rewrites it on every start.
func (d DataDir) Net() (string, error) { return readTrim(d.file("algod.net")) }

// Token returns the API token, or "" when the file is missing.
func (d DataDir) Token() string {
	t, _ := readTrim(d.file("algod.token"))
	return t
}

// AdminToken returns the admin API token, or "".
func (d DataDir) AdminToken() string {
	t, _ := readTrim(d.file("algod.admin.token"))
	return t
}

// PID returns the process ID from algod.pid. The file is absent when algod
// is stopped.
func (d DataDir) PID() (int, error) {
	s, err := readTrim(d.file("algod.pid"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// Config is a parsed config.json with its raw bytes and hash.
type Config struct {
	Values map[string]any
	Raw    []byte
	Hash   string
	Exists bool
}

// Config reads config.json. A missing file means all defaults.
func (d DataDir) Config() (*Config, error) {
	b, err := os.ReadFile(d.file("config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Values: map[string]any{}, Hash: "defaults"}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses config.json bytes.
func ParseConfig(b []byte) (*Config, error) {
	c := &Config{Values: map[string]any{}, Raw: b, Exists: true}
	sum := sha256.Sum256(b)
	c.Hash = hex.EncodeToString(sum[:8])
	if len(strings.TrimSpace(string(b))) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c.Values); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	return c, nil
}

// Bool returns a boolean config value.
func (c *Config) Bool(key string) bool {
	v, _ := c.Values[key].(bool)
	return v
}

// String returns a string config value.
func (c *Config) String(key string) string {
	v, _ := c.Values[key].(string)
	return v
}

// ConfigDiff is one key that changed between two configs.
type ConfigDiff struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

func (d ConfigDiff) String() string {
	f := func(v any) string {
		if v == nil {
			return "default"
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	return fmt.Sprintf("%s: %s -> %s", d.Key, f(d.From), f(d.To))
}

// Diff compares two configs key by key. A missing key is the default.
func Diff(old, cur *Config) []ConfigDiff {
	keys := map[string]bool{}
	var a, b map[string]any
	if old != nil {
		a = old.Values
	}
	if cur != nil {
		b = cur.Values
	}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var out []ConfigDiff
	for k := range keys {
		x, y := a[k], b[k]
		jx, _ := json.Marshal(x)
		jy, _ := json.Marshal(y)
		if string(jx) != string(jy) {
			out = append(out, ConfigDiff{Key: k, From: x, To: y})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Genesis is the identity part of genesis.json.
type Genesis struct {
	Network string `json:"network"`
	ID      string `json:"id"`
}

// GenesisID returns network-id, for example mainnet-v1.0.
func (g Genesis) GenesisID() string {
	if g.Network == "" {
		return ""
	}
	return g.Network + "-" + g.ID
}

// Genesis reads genesis.json.
func (d DataDir) Genesis() (Genesis, error) {
	var g Genesis
	b, err := os.ReadFile(d.file("genesis.json"))
	if err != nil {
		return g, err
	}
	err = json.Unmarshal(b, &g)
	return g, err
}

// LogPaths resolves node.log and the archive from config.json settings
// LogFileDir, LogArchiveDir and LogArchiveName. ArchiveGlob is set when
// LogArchiveName is a time template that produces many files.
type LogPaths struct {
	Live        string
	Archive     string
	ArchiveGlob string
}

// LogPaths returns where algod writes its logs.
func (d DataDir) LogPaths(c *Config) LogPaths {
	dir := d.Path
	if c != nil {
		if v := c.String("LogFileDir"); v != "" {
			dir = ExpandHome(v)
		}
	}
	lp := LogPaths{Live: filepath.Join(dir, "node.log")}
	adir := dir
	name := "node.archive.log"
	if c != nil {
		if v := c.String("LogArchiveDir"); v != "" {
			adir = ExpandHome(v)
		}
		if v := c.String("LogArchiveName"); v != "" {
			name = v
		}
	}
	if strings.Contains(name, "{{") {
		g := name
		for strings.Contains(g, "{{") {
			i := strings.Index(g, "{{")
			j := strings.Index(g[i:], "}}")
			if j < 0 {
				break
			}
			g = g[:i] + "*" + g[i+j+2:]
		}
		lp.ArchiveGlob = filepath.Join(adir, g)
	} else {
		lp.Archive = filepath.Join(adir, name)
	}
	return lp
}

// NewestArchive returns the archive file written most recently.
func (lp LogPaths) NewestArchive() string {
	if lp.ArchiveGlob == "" {
		return lp.Archive
	}
	m, _ := filepath.Glob(lp.ArchiveGlob)
	var best string
	var bestT int64
	for _, p := range m {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().UnixNano() > bestT {
			best, bestT = p, fi.ModTime().UnixNano()
		}
	}
	return best
}

// LooksLikeDataDir reports whether a directory holds algod.net and
// genesis.json, or genesis.json and a ledger directory.
func LooksLikeDataDir(p string) bool {
	if _, err := os.Stat(filepath.Join(p, "genesis.json")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(p, "algod.net")); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(p, "config.json"))
	return err == nil
}
