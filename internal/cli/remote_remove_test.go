//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"algod.top/internal/config"
)

// fakeSSH writes an ssh stand-in that logs each remote command. With
// down set it fails the way ssh does when the host does not answer.
// Otherwise it answers Detect for a Linux host.
func fakeSSH(t *testing.T, down bool) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := `#!/bin/sh
for a; do last=$a; done
printf '%s\n' "$last" >> "` + log + `"
`
	if down {
		script += "echo 'ssh: connect to host gone port 22: Operation timed out' >&2\nexit 255\n"
	} else {
		script += `case "$last" in *uname*) printf 'Linux\nx86_64\nalgo\n/home/algo\n';; esac
exit 0
`
	}
	bin = filepath.Join(dir, "ssh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func removeFixture(t *testing.T, sshBin string, remotes ...config.Remote) *remoteCmd {
	t.Helper()
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), "config.json"), Remotes: remotes}
	if err := cfg.Save(cfg.Path); err != nil {
		t.Fatal(err)
	}
	r, err := newRemoteCmd(cfg, "gone", "")
	if err != nil {
		t.Fatal(err)
	}
	r.ssh.Binary, r.yes = sshBin, true
	return r
}

// capture returns what f prints to stdout.
func capture(t *testing.T, f func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(pr); done <- b }()
	rc := f()
	pw.Close()
	os.Stdout = old
	return string(<-done), rc
}

func savedNames(t *testing.T, path string) []string {
	t.Helper()
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, x := range c.Remotes {
		n = append(n, x.Name)
	}
	return n
}

func TestRemoveUnreachableUserAgent(t *testing.T) {
	bin, log := fakeSSH(t, true)
	r := removeFixture(t, bin,
		config.Remote{Name: "gone", Host: "gone", AgentPath: "~/bin/algod.top"},
		config.Remote{Name: "edge", Host: "edge"})
	out, rc := capture(t, func() int { return r.remove(context.Background()) })
	if rc != 0 {
		t.Fatalf("rc %d, out %q", rc, out)
	}
	if got := savedNames(t, r.cfg.Path); len(got) != 1 || got[0] != "edge" {
		t.Errorf("remotes after remove %v, want [edge]", got)
	}
	for _, want := range []string{"cannot reach gone", "remote remove gone --path '~/bin/algod.top'", "removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "rm -f") {
		t.Errorf("ran a remote rm against an unreachable host:\n%s", b)
	}
}

func TestRemoveUnreachableSystemAgent(t *testing.T) {
	bin, _ := fakeSSH(t, true)
	r := removeFixture(t, bin, config.Remote{Name: "gone", Host: "gone", AgentPath: "/usr/local/bin/algod.top",
		AgentConfig: "/etc/algodtop/agent.json", Root: "su"})
	out, rc := capture(t, func() int { return r.remove(context.Background()) })
	if rc != 0 {
		t.Fatalf("rc %d, out %q", rc, out)
	}
	if got := savedNames(t, r.cfg.Path); len(got) != 0 {
		t.Errorf("remotes after remove %v, want none", got)
	}
	if !strings.Contains(out, `su -c "/usr/local/bin/algod.top service uninstall --system --agent"`) {
		t.Errorf("output lacks the su uninstall hint:\n%s", out)
	}
}

func TestRemoveUnreachableUnknownRemote(t *testing.T) {
	bin, _ := fakeSSH(t, true)
	r := removeFixture(t, bin, config.Remote{Name: "edge", Host: "edge"})
	_, rc := capture(t, func() int { return r.remove(context.Background()) })
	if rc == 0 {
		t.Fatal("remove of a remote the config lacks succeeded")
	}
	if got := savedNames(t, r.cfg.Path); len(got) != 1 {
		t.Errorf("remotes after remove %v, want [edge]", got)
	}
}

func TestRemoveUnreachableDeclined(t *testing.T) {
	bin, _ := fakeSSH(t, true)
	r := removeFixture(t, bin, config.Remote{Name: "gone", Host: "gone"})
	r.yes = false
	old := os.Stdin
	pr, pw, _ := os.Pipe()
	pw.WriteString("n\n")
	pw.Close()
	os.Stdin = pr
	defer func() { os.Stdin = old }()
	_, rc := capture(t, func() int { return r.remove(context.Background()) })
	if rc == 0 {
		t.Fatal("declined remove succeeded")
	}
	if got := savedNames(t, r.cfg.Path); len(got) != 1 {
		t.Errorf("remotes after declined remove %v, want [gone]", got)
	}
}

func TestRemoveReachableUserAgent(t *testing.T) {
	bin, log := fakeSSH(t, false)
	r := removeFixture(t, bin, config.Remote{Name: "gone", Host: "gone", AgentPath: "~/bin/algod.top"})
	out, rc := capture(t, func() int { return r.remove(context.Background()) })
	if rc != 0 {
		t.Fatalf("rc %d, out %q", rc, out)
	}
	if strings.Contains(out, "cannot reach") {
		t.Errorf("reachable host reported unreachable:\n%s", out)
	}
	b, _ := os.ReadFile(log)
	for _, want := range []string{"systemctl --user disable --now algodtop-agent", "rm -f $HOME/bin/algod.top"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("remote commands lack %q:\n%s", want, b)
		}
	}
	if got := savedNames(t, r.cfg.Path); len(got) != 0 {
		t.Errorf("remotes after remove %v, want none", got)
	}
}
