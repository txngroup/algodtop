package remote

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The daemon's control directory on macOS is under "Application
// Support". ssh must accept the arguments: -G prints the resolved config
// without connecting.
func TestControlPathWithSpaceParses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no ControlMaster on Windows")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	dir, err := os.MkdirTemp("", "a b")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s := &SSH{Target: Target{User: "algo", Host: "relay.example.invalid", Port: "2222"}, ControlDir: dir}
	p, ok := s.controlPath()
	if !ok {
		t.Skipf("the temp directory %s is too long for a socket path", dir)
	}
	out, err := exec.Command("ssh", append([]string{"-G"}, s.Args()...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh rejected the arguments: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "controlpath "+p) {
		t.Fatalf("controlpath not applied:\n%s", out)
	}
}

func TestControlPathTooLongTurnsSharingOff(t *testing.T) {
	s := &SSH{Target: Target{Host: "h"}, ControlDir: "/" + strings.Repeat("d", 100)}
	if _, ok := s.controlPath(); ok {
		t.Fatal("a path past the socket limit was used")
	}
	for _, a := range s.Args() {
		if strings.HasPrefix(a, "ControlPath") {
			t.Fatal("ControlPath passed although it does not fit")
		}
	}
}

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"05:07":       5*time.Minute + 7*time.Second,
		" 01:02:03\n": time.Hour + 2*time.Minute + 3*time.Second,
		"12-01:02:03": 12*24*time.Hour + time.Hour + 2*time.Minute + 3*time.Second,
	} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "7", "a:b", "1-2", "1:2:3:4", "-1:00"} {
		if _, ok := parseEtime(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestParseURLRejectsOptions(t *testing.T) {
	for _, u := range []string{"ssh://-oProxyCommand=touch%20x/var/lib/algorand", "ssh://-oProxyCommand=x@host/d", "ssh://-p/d"} {
		if _, err := ParseURL(u); err == nil {
			t.Errorf("%s parsed", u)
		}
	}
	if _, err := ParseURL("ssh://algorand@relay-2:2222/var/lib/algorand"); err != nil {
		t.Error(err)
	}
}

func TestAgentRelay(t *testing.T) {
	// A system agent: straight to its socket, never a temporary agent.
	if got := AgentRelay("/usr/local/bin/algodtop", "/etc/algodtop/agent.json"); got != "'/usr/local/bin/algodtop' stdio --agent --no-start --socket /var/lib/algodtop-agent/agent.sock" {
		t.Error(got)
	}
	// A user agent may run for the life of the connection.
	if got := AgentRelay("~/.local/bin/algod.top", "~/.config/algodtop/agent.json"); got != `"$HOME"/'.local/bin/algod.top' stdio --agent` {
		t.Error(got)
	}
	if got := AgentRelay("", ""); !strings.HasPrefix(got, "sh -c ") || !strings.HasSuffix(got, " algod.top stdio --agent") {
		t.Error(got)
	}
}
