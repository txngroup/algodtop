package version

import (
	"runtime/debug"
	"testing"
	"time"
)

func TestPseudo(t *testing.T) {
	for _, c := range []struct {
		v, when, rev string
	}{
		{"v0.0.0-20261104123549-37f0e9db466c", "2026-11-04T12:35:49Z", "37f0e9db466c"},
		{"v0.0.0-20261104123549-37f0e9db466c+dirty", "2026-11-04T12:35:49Z", "37f0e9db466c"},
		{"v1.2.4-0.20261104123549-37f0e9db466c", "2026-11-04T12:35:49Z", "37f0e9db466c"},
		{"v1.3.0-rc.1.0.20261104123549-37f0e9db466c", "2026-11-04T12:35:49Z", "37f0e9db466c"},
	} {
		when, rev, ok := pseudo(c.v)
		if !ok || when.Format(time.RFC3339) != c.when || rev != c.rev {
			t.Errorf("pseudo(%q) = %v %q %v", c.v, when, rev, ok)
		}
	}
	for _, v := range []string{"dev", "(devel)", "v1.2.3", "v1.2.3-rc.1", "v0.0.0-2026110412354-37f0e9db466c", "v0.0.0-20261104123549-37f0e9db"} {
		if _, _, ok := pseudo(v); ok {
			t.Errorf("pseudo(%q) took it for a pseudo-version", v)
		}
	}
}

func TestDescribe(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 0, 0, 0, time.UTC)
	installed := fromBuildInfo("dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261104123549-37f0e9db466c"}})
	local := fromBuildInfo("dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261104123549-37f0e9db466c+dirty"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "37f0e9db466c114bdb23f06c51517779798bb5d1"},
		{Key: "vcs.time", Value: "2026-11-04T12:35:49Z"},
		{Key: "vcs.modified", Value: "true"},
	}})
	linked := fromBuildInfo("v1.2.3", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "37f0e9db466c114bdb23f06c51517779798bb5d1"},
		{Key: "vcs.time", Value: "2026-11-04T12:35:49Z"},
	}})
	archive := fromBuildInfo("dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
	for _, c := range []struct {
		b         Build
		str, desc string
	}{
		{installed, "v0.0.0-20261104123549-37f0e9db466c", "2026-11-04 12:35 UTC (37f0e9db), 3 days old"},
		{local, "v0.0.0-20261104123549-37f0e9db466c+dirty", "2026-11-04 12:35 UTC (37f0e9db, modified), 3 days old"},
		{linked, "v1.2.3 (37f0e9db)", "v1.2.3, 2026-11-04 12:35 UTC (37f0e9db), 3 days old"},
		{archive, "dev", "dev"},
	} {
		if s := c.b.String(); s != c.str {
			t.Errorf("String() = %q, want %q", s, c.str)
		}
		if s := c.b.Describe(now); s != c.desc {
			t.Errorf("Describe() = %q, want %q", s, c.desc)
		}
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Hour:           "under a minute old",
		30 * time.Second:     "under a minute old",
		time.Minute:          "1 minute old",
		40 * time.Minute:     "40 minutes old",
		time.Hour:            "1 hour old",
		47 * time.Hour:       "47 hours old",
		48 * time.Hour:       "2 days old",
		400 * 24 * time.Hour: "400 days old",
	} {
		if got := Age(d); got != want {
			t.Errorf("Age(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	b := Parse("v0.0.0-20261105090230-8526abb4ba52")
	if b.Time.Format(time.RFC3339) != "2026-11-05T09:02:30Z" || b.Revision != "8526abb4ba52" {
		t.Errorf("%+v", b)
	}
	if got := b.Describe(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)); got != "2026-11-05 09:02 UTC (8526abb4), 2 hours old" {
		t.Error(got)
	}
	if b := Parse("dev"); !b.Time.IsZero() || b.Version != "dev" {
		t.Errorf("%+v", b)
	}
}
