package svc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/algodtop":           "/usr/local/bin/algodtop",
		"/Users/a b/algodtop":               `"/Users/a b/algodtop"`,
		"~/.local/bin/algodtop":             "%h/.local/bin/algodtop",
		"~/my bin/algodtop":                 `"%h/my bin/algodtop"`,
		"50%":                               "50%%",
		"$HOME":                             "$$HOME",
		`say "hi"`:                          `"say \"hi\""`,
		"/etc/algodtop/config.json":         "/etc/algodtop/config.json",
		"/Library/Application Support/x.db": `"/Library/Application Support/x.db"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
}

func TestSystemUnit(t *testing.T) {
	u := SystemdUnit(Spec{Name: "algodtop", Description: "algodtop daemon", System: true, User: "algorand", Group: "algorand",
		StateDir: "algodtop", After: []string{"algorand.service"},
		Args: []string{"/usr/local/bin/algodtop", "daemon", "--config", "/etc/algodtop/config.json"}})
	for _, want := range []string{
		"After=network-online.target algorand.service\n",
		"User=algorand\nGroup=algorand\n",
		"ExecStart=/usr/local/bin/algodtop daemon --config /etc/algodtop/config.json\n",
		"StateDirectory=algodtop\n",
		"Restart=on-failure\n",
		"Nice=10\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q in\n%s", want, u)
		}
	}
	user := SystemdUnit(Spec{Name: "algodtop", Description: "algodtop daemon", User: "ignored", Args: []string{"/a b/algodtop", "daemon"}})
	if strings.Contains(user, "User=") || !strings.Contains(user, "WantedBy=default.target") || !strings.Contains(user, `ExecStart="/a b/algodtop" daemon`) {
		t.Errorf("user unit:\n%s", user)
	}
}

func TestLaunchdPlist(t *testing.T) {
	p := LaunchdPlist(Spec{Label: "top.algod.daemon", Args: []string{"/Users/a&b/algodtop", "daemon"}, Env: map[string]string{"PATH": "/usr/bin"},
		Log: "/Users/a/Library/Logs/algodtop.log"})
	for _, want := range []string{
		"<key>Label</key><string>top.algod.daemon</string>",
		"<string>/Users/a&amp;b/algodtop</string>",
		"<key>PATH</key><string>/usr/bin</string>",
		"<key>StandardErrorPath</key><string>/Users/a/Library/Logs/algodtop.log</string>",
		"<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in\n%s", want, p)
		}
	}
	_, rest, _ := strings.Cut(p, "<!--")
	body, _, _ := strings.Cut(rest, "-->")
	if strings.Contains(body, "--") {
		t.Errorf("an XML comment may not contain --: %s", body)
	}
}

// plutil ships with macOS and rejects a malformed plist.
func TestLaunchdPlistLints(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is macOS only")
	}
	p := filepath.Join(t.TempDir(), "top.algod.daemon.plist")
	content := LaunchdPlist(Spec{Label: "top.algod.daemon", Args: []string{"/Users/a b/go/bin/algodtop", "daemon", "--config", "/Users/a b/.config/algodtop/config.json"},
		Env: map[string]string{"PATH": "/usr/bin:/bin"}, Log: "/Users/a b/Library/Logs/algodtop.log"})
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s\n%s", err, out, content)
	}
	out, err := exec.Command(plutil, "-extract", "ProgramArguments.0", "raw", p).Output()
	if err != nil || strings.TrimSpace(string(out)) != "/Users/a b/go/bin/algodtop" {
		t.Fatalf("ProgramArguments.0 = %q, %v", out, err)
	}
}
