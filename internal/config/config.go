// Package config loads, validates and writes algodtop's JSON config file
// and resolves the per-platform default paths.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/fsx"
)

// Duration is a time.Duration that reads "10s", "30d", "1y" or "forever"
// from JSON. Forever is stored as -1.
type Duration time.Duration

// ForeverDuration marks a window that never ends.
const ForeverDuration = Duration(-1)

// ParseDuration parses Go durations plus d (days), w (weeks), y (years)
// and "forever".
func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "forever" || s == "inf" {
		return ForeverDuration, nil
	}
	if s == "" || s == "0" {
		return 0, nil
	}
	var total time.Duration
	rest := s
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		num, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		j := i
		for j < len(rest) && !(rest[j] >= '0' && rest[j] <= '9') {
			j++
		}
		unit := rest[i:j]
		rest = rest[j:]
		var mult time.Duration
		switch unit {
		case "y":
			mult = 365 * 24 * time.Hour
		case "w":
			mult = 7 * 24 * time.Hour
		case "d":
			mult = 24 * time.Hour
		default:
			d, err := time.ParseDuration("1" + unit)
			if err != nil {
				return 0, fmt.Errorf("bad duration %q", s)
			}
			mult = d
		}
		total += time.Duration(num * float64(mult))
	}
	return Duration(total), nil
}

func (d Duration) String() string {
	if d == ForeverDuration {
		return "forever"
	}
	td := time.Duration(d)
	day := 24 * time.Hour
	switch {
	case td == 0:
		return "0s"
	case td%(365*day) == 0:
		return fmt.Sprintf("%dy", td/(365*day))
	case td%day == 0:
		return fmt.Sprintf("%dd", td/day)
	}
	return td.String()
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON reads a string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		*d = Duration(n * float64(time.Second))
		return nil
	}
	v, err := ParseDuration(s)
	*d = v
	return err
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Or returns d, or def when d is zero.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// Config is the whole config file.
type Config struct {
	Path string `json:"-"`

	StoreDir string `json:"store_dir,omitempty"`
	Socket   string `json:"socket,omitempty"`
	// SocketGroup lets members of a group use the socket (mode 0660). A
	// system service sets it so its operators can reach it over SSH.
	SocketGroup string          `json:"socket_group,omitempty"`
	Web         Web             `json:"web,omitzero"`
	Nodes       []Node          `json:"nodes,omitempty"`
	Remotes     []Remote        `json:"remotes,omitempty"`
	Retention   Retention       `json:"retention,omitzero"`
	Disk        Disk            `json:"disk,omitzero"`
	Intervals   Intervals       `json:"intervals,omitzero"`
	Caps        Caps            `json:"caps,omitzero"`
	Alerts      []AlertRule     `json:"alerts,omitempty"`
	Channels    map[string]Chan `json:"channels,omitempty"`
	Heartbeat   Heartbeat       `json:"heartbeat,omitzero"`
	Runtime     Runtime         `json:"runtime,omitzero"`
	Logs        Logs            `json:"logs,omitzero"`
	NTP         NTP             `json:"ntp,omitzero"`
	Agent       Agent           `json:"agent,omitzero"`
}

// Web configures the TCP listener.
type Web struct {
	// Listen is the web UI address. Nil means the default
	// 127.0.0.1:8090, and an empty string disables the listener.
	Listen                     *string  `json:"listen,omitempty"`
	TLS                        string   `json:"tls,omitempty"` // "self-signed"
	TLSCert                    string   `json:"tls_cert,omitempty"`
	TLSKey                     string   `json:"tls_key,omitempty"`
	PasswordHash               string   `json:"password_hash,omitempty"`
	ReadOnlyPasswordHash       string   `json:"read_only_password_hash,omitempty"`
	ReadOnlyTokenHashes        []string `json:"read_only_token_hashes,omitempty"`
	SessionTTL                 Duration `json:"session_ttl,omitempty"`
	AllowUnauthenticatedRemote bool     `json:"allow_unauthenticated_remote,omitempty"`
	AllowConfigEdits           bool     `json:"allow_config_edits,omitempty"`
}

// DefaultListen is the default web listener.
const DefaultListen = "127.0.0.1:8090"

// ListenAddr returns the effective listen address, or "" when disabled.
func (w Web) ListenAddr() string {
	if w.Listen == nil {
		return DefaultListen
	}
	return *w.Listen
}

// Node is one monitored algod.
type Node struct {
	Name           string   `json:"name"`
	DataDir        string   `json:"datadir,omitempty"`
	URL            string   `json:"url,omitempty"` // http(s):// or ssh://host/<datadir>
	TokenFile      string   `json:"token_file,omitempty"`
	AdminTokenFile string   `json:"admin_token_file,omitempty"`
	Token          string   `json:"token,omitempty"`
	AdminToken     string   `json:"admin_token,omitempty"`
	Accounts       []string `json:"accounts,omitempty"`
	Scrape         Duration `json:"scrape_interval,omitempty"`
	Peers          Duration `json:"peers_interval,omitempty"`
	Backfill       bool     `json:"backfill_logs,omitempty"`
	KeepRawDays    int      `json:"keep_raw_log_days,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
}

// IsSSH reports whether the node is reached over SSH without an agent.
func (n Node) IsSSH() bool { return strings.HasPrefix(n.URL, "ssh://") }

// Remote is a host running an algodtop agent.
type Remote struct {
	Name      string `json:"name"`
	Host      string `json:"host,omitempty"`       // ssh alias or ssh://user@host:port
	URL       string `json:"url,omitempty"`        // https://host:port for a TLS agent
	AgentPath string `json:"agent_path,omitempty"` // absolute path on the remote
	// AgentConfig is the agent's config file on the remote, as the agent
	// reported it to `remote add`. A path under /etc is a system service.
	AgentConfig string `json:"agent_config,omitempty"`
	// Names gives an agent's node a hub name other than <remote>/<node>,
	// such as the name of the SSH node it replaces, so that node's
	// history continues.
	Names     map[string]string `json:"names,omitempty"`
	TokenFile string            `json:"token_file,omitempty"`
	Token     string            `json:"token,omitempty"`
	CAFile    string            `json:"ca_file,omitempty"`
	CertFile  string            `json:"cert_file,omitempty"`
	KeyFile   string            `json:"key_file,omitempty"`
	Disabled  bool              `json:"disabled,omitempty"`
}

// Retention holds the guaranteed window per tier.
type Retention struct {
	Raw      Duration `json:"raw,omitempty"`
	OneMin   Duration `json:"1m,omitempty"`
	TenMin   Duration `json:"10m,omitempty"`
	OneHour  Duration `json:"1h,omitempty"`
	Sessions Duration `json:"peer_addresses,omitempty"`
}

// Windows returns the effective windows; 0 means forever.
func (r Retention) Windows() [4]time.Duration {
	day := 24 * time.Hour
	get := func(d Duration, def time.Duration) time.Duration {
		switch d {
		case 0:
			return def
		case ForeverDuration:
			return 0
		}
		return time.Duration(d)
	}
	return [4]time.Duration{get(r.Raw, 30*day), get(r.OneMin, 365*day), get(r.TenMin, 5*365*day), get(r.OneHour, 0)}
}

// Disk configures the disk floor.
type Disk struct {
	FloorPercent float64 `json:"floor_percent,omitempty"` // default 5
	AtFloor      string  `json:"at_floor,omitempty"`      // delete or pause
}

// Intervals override default collection intervals.
type Intervals struct {
	Scrape        Duration `json:"scrape,omitempty"`        // 10s
	Peers         Duration `json:"peers,omitempty"`         // 30s
	Participation Duration `json:"participation,omitempty"` // 5m
	Accounts      Duration `json:"accounts,omitempty"`      // 5m
	Supply        Duration `json:"supply,omitempty"`        // 10m
	Proc          Duration `json:"proc,omitempty"`          // 10s
	ProcNet       Duration `json:"proc_net,omitempty"`      // 60s
	GroupCommit   Duration `json:"group_commit,omitempty"`  // 1s
}

// Caps bound series counts.
type Caps struct {
	PerName     int `json:"series_per_metric,omitempty"`
	PerNode     int `json:"series_per_node,omitempty"`
	Templates   int `json:"templates_per_node,omitempty"`
	FloodAppIDs int `json:"flood_app_ids,omitempty"`
}

// AlertRule is one configured alert.
type AlertRule struct {
	Name     string   `json:"name"`
	Expr     string   `json:"expr"`
	For      Duration `json:"for,omitempty"`
	Cooldown Duration `json:"cooldown,omitempty"`
	Severity string   `json:"severity,omitempty"`
	Repeat   Duration `json:"repeat,omitempty"`
	Channels []string `json:"channels,omitempty"`
	Nodes    []string `json:"nodes,omitempty"`
}

// Chan is a notification channel. Channels live only in the config file.
type Chan struct {
	Type     string            `json:"type"` // webhook, smtp, command
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Template string            `json:"template,omitempty"`
	SMTP     *SMTP             `json:"smtp,omitempty"`
	Command  []string          `json:"command,omitempty"`
}

// SMTP configures an email channel.
type SMTP struct {
	Addr         string   `json:"addr"`
	From         string   `json:"from"`
	To           []string `json:"to"`
	Username     string   `json:"username,omitempty"`
	PasswordFile string   `json:"password_file,omitempty"`
	Password     string   `json:"password,omitempty"`
}

// Heartbeat is the dead-man switch ping.
type Heartbeat struct {
	URL      string   `json:"url,omitempty"`
	Interval Duration `json:"interval,omitempty"`
}

// Runtime tunes the Go runtime.
type Runtime struct {
	MemLimitMiB int  `json:"gomemlimit_mib,omitempty"` // default 96
	MaxProcs    int  `json:"gomaxprocs,omitempty"`     // default 2
	NoLowPrio   bool `json:"no_low_priority,omitempty"`
}

// Logs configures log ingestion defaults.
type Logs struct {
	Backfill      bool `json:"backfill,omitempty"`
	KeepRawInfo   int  `json:"keep_raw_info_days,omitempty"`
	KeepWarnDays  int  `json:"keep_warnings_days,omitempty"` // 90
	DisableTailer bool `json:"disable,omitempty"`
}

// NTP configures the clock offset check.
type NTP struct {
	Server   string   `json:"server,omitempty"` // pool.ntp.org
	Interval Duration `json:"interval,omitempty"`
	Disable  bool     `json:"disable,omitempty"`
}

// Agent configures agent mode.
type Agent struct {
	Buffer    Duration `json:"buffer,omitempty"`         // 7d
	BufferCap int64    `json:"buffer_cap_mib,omitempty"` // 1024
	Listen    string   `json:"listen,omitempty"`         // TLS agent listener
	TokenHash string   `json:"token_hash,omitempty"`
	TLSCert   string   `json:"tls_cert,omitempty"`
	TLSKey    string   `json:"tls_key,omitempty"`
	ClientCA  string   `json:"client_ca,omitempty"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks the config for mistakes.
func (c *Config) Validate() error {
	var errs []error
	names := map[string]bool{}
	for i, n := range c.Nodes {
		if !nameRE.MatchString(n.Name) {
			errs = append(errs, fmt.Errorf("node %d: name %q must be letters, digits, dot, dash or underscore", i, n.Name))
		}
		if names[n.Name] {
			errs = append(errs, fmt.Errorf("node %q listed twice", n.Name))
		}
		names[n.Name] = true
		if (n.DataDir == "") == (n.URL == "") {
			errs = append(errs, fmt.Errorf("node %q: set exactly one of datadir or url", n.Name))
		}
		if n.URL != "" && !strings.HasPrefix(n.URL, "http://") && !strings.HasPrefix(n.URL, "https://") && !strings.HasPrefix(n.URL, "ssh://") {
			errs = append(errs, fmt.Errorf("node %q: url must start with http://, https:// or ssh://", n.Name))
		}
	}
	for _, r := range c.Remotes {
		if !nameRE.MatchString(r.Name) {
			errs = append(errs, fmt.Errorf("remote %q: bad name", r.Name))
		}
		if names[r.Name] {
			errs = append(errs, fmt.Errorf("remote %q has the same name as a node", r.Name))
		}
		names[r.Name] = true
		if (r.Host == "") == (r.URL == "") {
			errs = append(errs, fmt.Errorf("remote %q: set exactly one of host (SSH agent) or url (TLS agent)", r.Name))
		}
	}
	// Hub names from names may reuse their own remote's name, which is the
	// usual case for an agent that replaces an SSH node of that name.
	mapped := map[string]string{}
	for _, r := range c.Remotes {
		for agentName, hubName := range r.Names {
			switch {
			case !nameRE.MatchString(agentName) || !nameRE.MatchString(hubName):
				errs = append(errs, fmt.Errorf("remote %q: bad name in names: %q -> %q", r.Name, agentName, hubName))
			case hubName != r.Name && names[hubName]:
				errs = append(errs, fmt.Errorf("remote %q: names gives %q, which is already a node or remote", r.Name, hubName))
			case mapped[hubName] != "":
				errs = append(errs, fmt.Errorf("remote %q: names gives %q, which remote %q also uses", r.Name, hubName, mapped[hubName]))
			}
			mapped[hubName] = r.Name
		}
	}
	for _, a := range c.Alerts {
		for _, ch := range a.Channels {
			if _, ok := c.Channels[ch]; !ok {
				errs = append(errs, fmt.Errorf("alert %q: unknown channel %q", a.Name, ch))
			}
		}
	}
	for name, ch := range c.Channels {
		switch ch.Type {
		case "webhook":
			if ch.URL == "" {
				errs = append(errs, fmt.Errorf("channel %q: webhook needs url", name))
			}
		case "smtp":
			if ch.SMTP == nil || ch.SMTP.Addr == "" || len(ch.SMTP.To) == 0 {
				errs = append(errs, fmt.Errorf("channel %q: smtp needs addr and to", name))
			}
		case "command":
			if len(ch.Command) == 0 {
				errs = append(errs, fmt.Errorf("channel %q: command needs a program", name))
			}
		default:
			errs = append(errs, fmt.Errorf("channel %q: type must be webhook, smtp or command", name))
		}
	}
	switch c.Disk.AtFloor {
	case "", "delete", "pause":
	default:
		errs = append(errs, fmt.Errorf("disk.at_floor must be delete or pause"))
	}
	return errors.Join(errs...)
}

// Save writes the config atomically with mode 0600.
func (c *Config) Save(path string) error {
	b, err := c.Marshal()
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(path, b, 0o600)
}

// Marshal renders the config as indented JSON.
func (c *Config) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// DefaultPath returns the platform's config file path.
func DefaultPath() string {
	if p := os.Getenv("ALGODTOP_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "algodtop", "config.json")
		}
	case "linux":
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			return filepath.Join(d, "algodtop", "config.json")
		}
	}
	return filepath.Join(home, ".config", "algodtop", "config.json")
}

// DefaultStoreDir returns the platform's data directory.
func DefaultStoreDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "algodtop")
		}
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "algodtop")
	case "linux":
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "algodtop")
		}
	}
	return filepath.Join(home, ".local", "share", "algodtop")
}

// StorePath returns the effective store directory.
func (c *Config) StorePath() string {
	if c.StoreDir != "" {
		return algod.ExpandHome(filepath.FromSlash(c.StoreDir))
	}
	return DefaultStoreDir()
}

// SocketPath returns the effective daemon socket path.
func (c *Config) SocketPath() string {
	if c.Socket != "" {
		return algod.ExpandHome(filepath.FromSlash(c.Socket))
	}
	return filepath.Join(c.StorePath(), "algodtop.sock")
}

// DefaultSocket returns the socket path with no config.
func DefaultSocket() string { return filepath.Join(DefaultStoreDir(), "algodtop.sock") }

// Find returns a node by name.
func (c *Config) Find(name string) (Node, bool) {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// CheckPermissions warns when the file is readable by other users.
func CheckPermissions(path string) string {
	return checkPerm(path)
}
