package health

import (
	"strings"
	"testing"
	"time"

	"algod.top/internal/collector"
)

func base() collector.State {
	now := time.Now()
	return collector.State{Name: "n", Kind: "local", Up: true, Healthy: true, Ready: true, Round: 100, RoundAt: now.UnixNano(),
		DiskFree: 500, DiskTotal: 1000, HasAdmin: true, Peers: collector.PeerCounts{P2POut: 4, P2PIn: 10}, Version: "4.4.1", Network: "mainnet"}
}

func TestOK(t *testing.T) {
	s := base()
	st := Evaluate(&s, Context{})
	if st.Level != OK || st.Summary() != "OK" {
		t.Fatalf("%+v", st)
	}
}

func TestRules(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		mod   func(*collector.State)
		rule  string
		level Level
		text  string
	}{
		{"down", func(s *collector.State) { s.Up = false; s.LastError = "connection refused" }, "down", Critical, "algod is down"},
		{"stalled", func(s *collector.State) { s.RoundAt = now.Add(-45 * time.Second).UnixNano() }, "stalled", Critical, "last round is"},
		{"catchup", func(s *collector.State) { s.RoundAt = now.Add(-45 * time.Second).UnixNano(); s.Catchup = true }, "catchup", Warn, "catchup"},
		{"disk", func(s *collector.State) { s.DiskFree = 20 }, "disk", Critical, "2.0% free"},
		{"key6", func(s *collector.State) {
			s.Keys = []collector.PartKeyState{{Address: "ABCDEFGHIJ", RoundsLeft: 1000, DaysLeft: 6}}
		}, "key_expiry_warn", Warn, "participation key for ABCDEF... expires in 6 days"},
		{"key1", func(s *collector.State) {
			s.Keys = []collector.PartKeyState{{Address: "ABCDEFGHIJ", RoundsLeft: 10, DaysLeft: 1}}
		}, "key_expiry", Critical, "expires in 1 day"},
		{"suspended", func(s *collector.State) {
			s.Accounts = []collector.AccountState{{Address: "XYZXYZXYZ", Status: "Suspended"}}
		}, "account_offline", Critical, "suspended"},
		{"nooutgoing", func(s *collector.State) { s.Peers = collector.PeerCounts{P2PIn: 3} }, "no_outgoing", Warn, "0 outgoing"},
		{"clock", func(s *collector.State) { s.ClockKnown, s.ClockOff = true, 1.7 }, "clock", Critical, "+1.70s"},
	}
	for _, c := range cases {
		s := base()
		c.mod(&s)
		st := Evaluate(&s, Context{Now: now})
		found := false
		for _, r := range st.Reasons {
			if r.Rule == c.rule && r.Level == c.level && strings.Contains(r.Text, c.text) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: reasons %+v", c.name, st.Reasons)
		}
	}
}

func TestVersionBehind(t *testing.T) {
	rel := func(name, ver, branch, channel string) collector.State {
		s := base()
		s.Name, s.Version, s.Branch, s.Channel = name, ver, branch, channel
		return s
	}
	s := rel("n", "4.4.1", "rel/stable", "stable")
	other := rel("m2", "4.5.0", "rel/stable", "stable")
	st := Evaluate(&s, Context{Fleet: []collector.State{s, other}})
	if st.Level != Warn || !strings.Contains(st.Summary(), "behind 4.5.0 on m2") {
		t.Fatal(st.Summary())
	}
	// None of these is a newer release of the same channel.
	for _, o := range []collector.State{
		rel("dev", "4.5.310000", "feature/y", "dev"),
		rel("devrel", "4.4.310000", "rel/stable", "dev"),
		rel("bigbuild", "4.4.310000", "rel/stable", "stable"),
		rel("beta", "4.5.0", "rel/beta", "beta"),
		rel("nochannel", "4.5.0", "", ""),
	} {
		st := Evaluate(&s, Context{Fleet: []collector.State{s, o}})
		if st.Level != OK {
			t.Errorf("%s (%s %s): %s", o.Name, o.Version, o.Branch, st.Summary())
		}
	}
	// A developer build is never behind a release.
	d := rel("dev", "4.4.310000", "feature/x", "dev")
	if st := Evaluate(&d, Context{Fleet: []collector.State{d, other}}); st.Level != OK {
		t.Error(st.Summary())
	}
	if CompareVersions("5.1.310000", "4.8.300000") != 1 {
		t.Fatal("compare")
	}
}

// An account renewed with a new key keeps the old one installed. The old
// key's expiry is not a problem once the new key votes.
func TestRenewedKeyIsNotExpiring(t *testing.T) {
	s := base()
	s.Keys = []collector.PartKeyState{
		{Address: "ABCDEFGHIJ", First: 1, Last: 110, RoundsLeft: 10, DaysLeft: 0.7, Superseded: true},
		{Address: "ABCDEFGHIJ", First: 90, Last: 10000, RoundsLeft: 9900, DaysLeft: 300, Active: true},
	}
	if st := Evaluate(&s, Context{}); st.Level != OK {
		t.Fatal(st.Summary())
	}
	// Until the new key votes, the old one is in charge and the message
	// says that a later key exists.
	s.Keys[0].Superseded, s.Keys[0].Active, s.Keys[1].Active = false, true, false
	st := Evaluate(&s, Context{})
	if st.Level != Critical || !strings.Contains(st.Summary(), "a later key is installed but has not voted") {
		t.Fatal(st.Summary())
	}
}
