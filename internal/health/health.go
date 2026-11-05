// Package health turns a node's live state into OK, Warn or Critical with
// the reasons in words. Every rule is also available to alerts by name.
package health

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/collector"
)

// Level is a health level.
type Level int

// Levels.
const (
	OK       Level = 0
	Warn     Level = 1
	Critical Level = 2
)

func (l Level) String() string {
	switch l {
	case Warn:
		return "warn"
	case Critical:
		return "critical"
	}
	return "ok"
}

// MarshalText writes the level name.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText reads a level name.
func (l *Level) UnmarshalText(b []byte) error {
	v, err := ParseLevel(string(b))
	*l = v
	return err
}

// ParseLevel parses ok, warn or critical.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "ok":
		return OK, nil
	case "warn", "warning":
		return Warn, nil
	case "critical", "crit":
		return Critical, nil
	}
	return OK, fmt.Errorf("unknown level %q", s)
}

// Reason is one rule that fired.
type Reason struct {
	Rule  string  `json:"rule"`
	Level Level   `json:"level"`
	Text  string  `json:"text"`
	Value float64 `json:"value,omitempty"`
}

// Disabled is a rule that cannot run on this platform or node.
type Disabled struct {
	Rule string `json:"rule"`
	Why  string `json:"why"`
}

// Status is a node's health.
type Status struct {
	Level    Level      `json:"level"`
	Reasons  []Reason   `json:"reasons"`
	Disabled []Disabled `json:"disabled,omitempty"`
}

// Summary renders "Warn: participation key ... expires in 6 days".
func (s Status) Summary() string {
	if s.Level == OK || len(s.Reasons) == 0 {
		return "OK"
	}
	lvl := "Warn"
	if s.Level == Critical {
		lvl = "Critical"
	}
	var top []string
	for _, r := range s.Reasons {
		if r.Level == s.Level {
			top = append(top, r.Text)
		}
	}
	return lvl + ": " + strings.Join(top, "; ")
}

// Daemon carries algodtop's own problems, which raise Warn on every node.
type Daemon struct {
	Paused        bool
	DroppedSeries map[string]map[string]int64 // node -> metric -> drops
	FloorDeleted  map[string]string           // node -> "raw history is 12 days instead of 30"
	ProjectedOver bool                        // projected use exceeds free space minus floor
	SharedVolume  map[string]float64          // node -> free fraction when shared and under 15%
	QueueDrops    int64
}

// Context is what rules need beyond one node's state.
type Context struct {
	Now    time.Time
	Fleet  []collector.State
	Daemon Daemon
	// Thresholds; zero means the default.
	LagCritical    time.Duration // 30 s
	DiskCritical   float64       // 0.05
	KeyCriticalDay float64       // 2
	KeyWarnDay     float64       // 14
	DiskFillDays   float64       // 14
	ClockCritical  float64       // 1 s
	NoVoteFactor   float64       // 10
}

// Rules lists every rule name, for alert expressions and the docs.
var Rules = []string{"down", "health", "stalled", "disk", "key_expiry", "account_offline", "key_mismatch", "no_votes",
	"clock", "catchup", "key_expiry_warn", "incentive_lost", "disk_fill", "no_outgoing", "error_rate", "version_behind", "algodtop"}

// Evaluate runs every rule against one node.
func Evaluate(s *collector.State, c Context) Status {
	if c.LagCritical == 0 {
		c.LagCritical = 30 * time.Second
	}
	if c.DiskCritical == 0 {
		c.DiskCritical = 0.05
	}
	if c.KeyCriticalDay == 0 {
		c.KeyCriticalDay = 2
	}
	if c.KeyWarnDay == 0 {
		c.KeyWarnDay = 14
	}
	if c.DiskFillDays == 0 {
		c.DiskFillDays = 14
	}
	if c.ClockCritical == 0 {
		c.ClockCritical = 1
	}
	if c.NoVoteFactor == 0 {
		c.NoVoteFactor = 10
	}
	if c.Now.IsZero() {
		c.Now = time.Now()
	}
	var st Status
	add := func(rule string, l Level, v float64, format string, args ...any) {
		st.Reasons = append(st.Reasons, Reason{Rule: rule, Level: l, Text: fmt.Sprintf(format, args...), Value: v})
		if l > st.Level {
			st.Level = l
		}
	}
	disable := func(rule, why string) { st.Disabled = append(st.Disabled, Disabled{rule, why}) }

	if !s.Up {
		why := s.LastError
		if why == "" {
			why = "not reachable"
		}
		add("down", Critical, 1, "algod is down (%s)", why)
		return finish(st, s, c, add, disable)
	}
	if !s.Healthy {
		add("health", Critical, 1, "/health fails")
	}
	lag := s.Lag(c.Now)
	if lag > c.LagCritical.Seconds() && !s.Catchup {
		add("stalled", Critical, lag, "last round is %s old", fmtDur(lag))
	}
	if s.Catchup {
		add("catchup", Warn, 1, "catchup is running")
	}
	if s.DiskTotal > 0 {
		frac := s.DiskFree / s.DiskTotal
		if frac < c.DiskCritical {
			add("disk", Critical, frac*100, "disk is %.1f%% free", frac*100)
		}
		if s.DiskDays > 0 && s.DiskDays < c.DiskFillDays {
			add("disk_fill", Warn, s.DiskDays, "disk will fill in %.1f days", s.DiskDays)
		}
	} else if s.Kind != "local" {
		disable("disk", "disk stats need a local node or an agent")
	}
	// A renewed account has its old key installed until someone deletes
	// it. Only the key the account votes with can lapse.
	for _, k := range collector.ExpiryKeys(s.Keys) {
		later := ""
		if collector.LaterKey(s.Keys, k) {
			later = "; a later key is installed but has not voted"
		}
		if k.RoundsLeft <= 0 {
			add("key_expiry", Critical, 0, "participation key for %s has expired%s", shortAddr(k.Address), later)
			continue
		}
		if k.DaysLeft < c.KeyCriticalDay {
			add("key_expiry", Critical, k.DaysLeft, "participation key for %s expires in %s%s", shortAddr(k.Address), fmtDays(k.DaysLeft), later)
		} else if k.DaysLeft < c.KeyWarnDay {
			add("key_expiry_warn", Warn, k.DaysLeft, "participation key for %s expires in %s%s", shortAddr(k.Address), fmtDays(k.DaysLeft), later)
		}
	}
	for _, a := range s.Accounts {
		switch {
		case a.Status == "Suspended":
			add("account_offline", Critical, 0, "account %s is suspended (offline with its vote key set); send a heartbeat or a new keyreg", shortAddr(a.Address))
		case a.WasOnline && a.Status != "Online":
			add("account_offline", Critical, 0, "account %s was online and is now %s", shortAddr(a.Address), strings.ToLower(a.Status))
		}
		if a.Status == "Online" {
			hasKey := false
			for _, k := range s.Keys {
				if k.Address == a.Address {
					hasKey = true
				}
			}
			if hasKey && !a.KeyMatches {
				add("key_mismatch", Critical, 0, "account %s is online but the installed key is not the vote key registered on chain", shortAddr(a.Address))
			}
			if a.ExpectedInterval > 0 && a.RoundsSinceVote > 0 && float64(a.RoundsSinceVote) > c.NoVoteFactor*a.ExpectedInterval {
				add("no_votes", Critical, float64(a.RoundsSinceVote), "account %s has not voted for %d rounds (expected every %.0f)", shortAddr(a.Address), a.RoundsSinceVote, a.ExpectedInterval)
			}
		}
		if a.WasEligible && !a.IncentiveEligible {
			add("incentive_lost", Warn, 0, "account %s lost incentive eligibility", shortAddr(a.Address))
		}
	}
	if s.ClockKnown && math.Abs(s.ClockOff) > c.ClockCritical {
		add("clock", Critical, s.ClockOff, "clock offset is %+.2fs", s.ClockOff)
	}
	if s.HasAdmin && s.Peers.Total() > 0 && s.Peers.Out() == 0 {
		add("no_outgoing", Warn, 0, "the node has 0 outgoing peers")
	} else if !s.HasAdmin {
		disable("no_outgoing", "peer counts need the admin token")
	}
	if s.ErrorRate > 1 && s.ErrorRate > 3*s.ErrorBaseline+5 {
		add("error_rate", Warn, s.ErrorRate, "error log rate %.0f/min is above its baseline of %.1f/min", s.ErrorRate, s.ErrorBaseline)
	}
	if latest, from := latestVersion(s, c.Fleet); from != "" {
		add("version_behind", Warn, 0, "version %s is behind %s on %s, another %s %s node", s.Version, latest, from, s.Network, releaseChannel(s))
	}
	return finish(st, s, c, add, disable)
}

func finish(st Status, s *collector.State, c Context, add func(string, Level, float64, string, ...any), disable func(string, string)) Status {
	d := c.Daemon
	if d.Paused {
		add("algodtop", Critical, 0, "algod.top paused writes at the disk floor")
	}
	if drops := d.DroppedSeries[s.Name]; len(drops) > 0 {
		names := make([]string, 0, len(drops))
		for n := range drops {
			names = append(names, n)
		}
		if len(names) > 3 {
			names = append(names[:3], "...")
		}
		add("algodtop", Warn, 0, "algod.top dropped series over the cap: %s", strings.Join(names, ", "))
	}
	if msg := d.FloorDeleted[s.Name]; msg != "" {
		add("algodtop", Warn, 0, "disk floor, %s", msg)
	}
	if s.MissedScrapes > 0 && s.Up && !s.Reachable {
		add("algodtop", Warn, float64(s.MissedScrapes), "algod.top missed scrapes")
	}
	if f, ok := d.SharedVolume[s.Name]; ok {
		add("algodtop", Warn, f*100, "algod.top's data shares a volume with this node at %.0f%% free", f*100)
	}
	if d.ProjectedOver {
		add("algodtop", Warn, 0, "projected algod.top disk use exceeds free space minus the floor")
	}
	for _, u := range s.Unavailable {
		disable(u, "not available on this platform")
	}
	return st
}

// latestVersion returns a newer release seen on another node of the same
// network and channel, and that node's name. Developer builds take no
// part: their build numbers count hours from a fixed date, so 5.1.310000
// says nothing about whether 5.0.5 is behind.
func latestVersion(s *collector.State, fleet []collector.State) (string, string) {
	ch := releaseChannel(s)
	if ch == "" || s.Network == "" {
		return "", ""
	}
	best, from := s.Version, ""
	for _, o := range fleet {
		if o.Name == s.Name || o.Network != s.Network || !o.Up || releaseChannel(&o) != ch {
			continue
		}
		if CompareVersions(o.Version, best) > 0 {
			best, from = o.Version, o.Name
		}
	}
	return best, from
}

// releaseChannel returns stable, beta or another release channel, or ""
// for a developer build. A release build number is the patch number, so a
// large one marks a developer build made from a release branch.
func releaseChannel(s *collector.State) string {
	if s.Version == "" {
		return ""
	}
	ch := s.Channel
	if b, ok := strings.CutPrefix(s.Branch, "rel/"); ok && (ch == "" || ch == b) {
		ch = b
	} else if s.Branch != "" {
		return ""
	}
	if ch == "" || ch == "dev" {
		return ""
	}
	parts := strings.Split(s.Version, ".")
	if n, err := strconv.Atoi(parts[len(parts)-1]); err != nil || n >= 10000 {
		return ""
	}
	return ch
}

// CompareVersions compares dotted numeric versions.
func CompareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func shortAddr(a string) string {
	if len(a) > 6 {
		return a[:6] + "..."
	}
	return a
}

func fmtDays(d float64) string {
	if d < 1 {
		return fmt.Sprintf("%.0f hours", d*24)
	}
	if d < 1.5 {
		return "1 day"
	}
	return fmt.Sprintf("%.0f days", d)
}

func fmtDur(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	return d.String()
}
