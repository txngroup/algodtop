package cli

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"algod.top/internal/config"
)

func TestRootCommand(t *testing.T) {
	for _, c := range []struct {
		method, path, want string
	}{
		{"sudo", "~/.local/bin/algodtop", `sudo $HOME/.local/bin/algodtop service install --system --agent`},
		{"su", "~/.local/bin/algodtop", `su -c "$HOME/.local/bin/algodtop service install --system --agent"`},
		{"su", "/usr/local/bin/algodtop", `su -c "/usr/local/bin/algodtop service install --system --agent"`},
		{"su", "~/my bin/algodtop", `su -c "\"$HOME\"/'my bin/algodtop' service install --system --agent"`},
	} {
		if got := rootCommand(c.method, c.path, "service", "install", "--system", "--agent"); got != c.want {
			t.Errorf("%s %s:\n got %s\nwant %s", c.method, c.path, got, c.want)
		}
	}
	// What a person pastes: the local shell must leave $HOME for the remote.
	want := `ssh -t node1 'su -c "$HOME/.local/bin/algodtop service install --system --agent"'`
	if got := sshT("node1", rootCommand("su", "~/.local/bin/algodtop", "service", "install", "--system", "--agent")); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRootCommandShell runs the commands through sh with fake su and
// sudo, to check the words root would run. su -c hands its argument to
// root's shell, and sudo runs its words as they are.
func TestRootCommandShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	fakes := `su() { sh -c "printf '%s|' $2"; }; sudo() { printf '%s|' "$@"; }; HOME=/home/steve; `
	for _, method := range []string{"su", "sudo"} {
		for _, p := range []string{"~/.local/bin/algodtop", "~/my bin/algodtop"} {
			cmd := rootCommand(method, p, "service", "install")
			out, err := exec.Command("sh", "-c", fakes+cmd).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v %s", cmd, err, out)
			}
			want := "/home/steve/" + strings.TrimPrefix(p, "~/") + "|service|install|"
			if string(out) != want {
				t.Errorf("%s ran %q, want %q", cmd, out, want)
			}
		}
	}
}

func TestRemoteForHost(t *testing.T) {
	cfg := &config.Config{Remotes: []config.Remote{
		{Name: "partmon1", Host: "node1", AgentPath: "/usr/local/bin/algodtop", AgentConfig: "/etc/algodtop/agent.json"},
		{Name: "partmon2", Host: "ssh://steve@node2:2222"},
		{Name: "tls", URL: "https://node3:8091"},
	}}
	for host, want := range map[string]string{
		"node1":                  "partmon1",
		"ssh://node1":            "partmon1",
		"steve@node1":            "partmon1",
		"ssh://steve@node2:2222": "partmon2",
		"node2":                  "node2", // another port
		"node3":                  "node3",
		"node1.example.com":      "node1-example-com",
	} {
		r, err := newRemoteCmd(cfg, host, "")
		if err != nil {
			t.Fatal(err)
		}
		if r.name != want {
			t.Errorf("%s: name %q, want %q", host, r.name, want)
		}
	}
	r, _ := newRemoteCmd(cfg, "node1", "")
	if !r.systemAgent() || r.agentPath() != "/usr/local/bin/algodtop" {
		t.Errorf("node1 is not the saved system agent: %v %q", r.systemAgent(), r.agentPath())
	}
	if r, _ := newRemoteCmd(cfg, "node1", "other"); r.name != "other" {
		t.Errorf("--name lost: %q", r.name)
	}
}
