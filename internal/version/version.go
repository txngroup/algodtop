// Package version describes the build. algodtop has no release numbers:
// every commit on main is a release, and Go names it with a pseudo-version
// that holds the commit's UTC time, such as
// v0.0.0-20261104123549-37f0e9db466c. A build can still set a version at
// link time with -ldflags "-X algod.top/internal/version.Version=...".
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
	"time"
)

// Version is the release version set at link time, or "dev".
var Version = "dev"

// Build is what the binary knows about the commit it came from.
type Build struct {
	Version  string    // such as v0.0.0-20261104123549-37f0e9db466c, or "dev"
	Revision string    // the commit hash, when known
	Time     time.Time // the commit time in UTC, when known
	Modified bool      // built from a tree with uncommitted changes
}

// Info reads the build. A local build in a git checkout knows its commit
// from the VCS stamp. A build by go install knows it from the
// pseudo-version.
func Info() Build {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Build{Version: Version}
	}
	return fromBuildInfo(Version, bi)
}

func fromBuildInfo(linked string, bi *debug.BuildInfo) Build {
	b := Build{Version: linked}
	if b.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		b.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				b.Time = t.UTC()
			}
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	if t, rev, ok := pseudo(b.Version); ok {
		if b.Time.IsZero() {
			b.Time = t
		}
		if b.Revision == "" {
			b.Revision = rev
		}
	}
	if strings.HasSuffix(b.Version, "+dirty") {
		b.Modified = true
	}
	return b
}

// pseudo reads the commit time and hash from a Go pseudo-version, which
// ends in yyyymmddhhmmss-abcdefabcdef, perhaps with +dirty after it.
func pseudo(v string) (time.Time, string, bool) {
	v, _, _ = strings.Cut(v, "+")
	i := strings.LastIndexByte(v, '-')
	if i < 0 {
		return time.Time{}, "", false
	}
	rev := v[i+1:]
	if len(rev) != 12 || strings.Trim(rev, "0123456789abcdef") != "" {
		return time.Time{}, "", false
	}
	ts := v[strings.LastIndexAny(v[:i], "-.")+1 : i]
	t, err := time.Parse("20060102150405", ts)
	if err != nil || len(ts) != 14 {
		return time.Time{}, "", false
	}
	return t.UTC(), rev, true
}

// String is the version with the commit when the version does not hold
// it, such as "dev (37f0e9db)". Its first word is the version, which
// remote install looks up.
func (b Build) String() string {
	v := b.Version
	if len(b.Revision) >= 12 && !strings.Contains(v, b.Revision[:12]) {
		v += " (" + b.Revision[:8] + ")"
	}
	return v
}

// Describe names the build by its commit's time and age, such as
// "2026-11-04 12:35 UTC (37f0e9db), 3 days old". A version set at link
// time comes first. A build with no commit time, such as one from a
// source archive, gets String.
func (b Build) Describe(now time.Time) string {
	if b.Time.IsZero() {
		return b.String()
	}
	s := b.Time.UTC().Format("2006-01-02 15:04 UTC")
	if _, _, ok := pseudo(b.Version); !ok && b.Version != "dev" {
		s = b.Version + ", " + s
	}
	var tags []string
	if len(b.Revision) >= 8 {
		tags = append(tags, b.Revision[:8])
	}
	if b.Modified {
		tags = append(tags, "modified")
	}
	if len(tags) > 0 {
		s += " (" + strings.Join(tags, ", ") + ")"
	}
	return s + ", " + Age(now.Sub(b.Time))
}

// UnixNano is the commit time in Unix ns, or 0 when it is unknown.
func (b Build) UnixNano() int64 {
	if b.Time.IsZero() {
		return 0
	}
	return b.Time.UnixNano()
}

// String is Info().String().
func String() string { return Info().String() }

// Age says how old a build is in its largest whole unit, such as
// "40 minutes old", "5 hours old" or "3 days old".
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute old"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " old"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " old"
	}
	return plural(int(d/(24*time.Hour)), "day") + " old"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
