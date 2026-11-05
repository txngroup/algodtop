package cli

import (
	"runtime"
	"strings"
	"testing"

	"algod.top/internal/config"
)

func linuxEnv() svcEnv {
	return svcEnv{goos: "linux", systemd: true, exe: "/home/alice/go/bin/algodtop", home: "/home/alice", user: "alice", uid: 1000, linger: "no"}
}

func factsWith(path string, owners map[string]string) svcFacts {
	c := &config.Config{Path: path}
	for d := range owners {
		c.Nodes = append(c.Nodes, config.Node{Name: "n", DataDir: d})
	}
	return svcFacts{config: path, cfg: c, owners: owners}
}

func TestServicePlanMacOS(t *testing.T) {
	env := svcEnv{goos: "darwin", exe: "/Users/alice/go/bin/algodtop", home: "/Users/alice", user: "alice", uid: 501}
	f := factsWith("/Users/alice/.config/algodtop/config.json", map[string]string{"/Users/alice/.mainnet": "alice"})
	p := buildPlan(env, svcOpts{}, f)
	if p.problem != "" {
		t.Fatal(p.problem)
	}
	if p.mode != "launchd" || p.file != "/Users/alice/Library/LaunchAgents/top.algod.daemon.plist" || p.log != "/Users/alice/Library/Logs/algodtop.log" {
		t.Fatalf("%+v", p)
	}
	for _, want := range []string{"<string>/Users/alice/go/bin/algodtop</string>", "<string>--config</string>",
		"<string>/Users/alice/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>"} {
		if !strings.Contains(p.content, want) {
			t.Errorf("missing %s in\n%s", want, p.content)
		}
	}
	cmds := p.controlCmds(env, "start")
	if shellJoin(cmds[len(cmds)-1]...) != "launchctl bootstrap gui/501 /Users/alice/Library/LaunchAgents/top.algod.daemon.plist" {
		t.Error(cmds)
	}
	if p := buildPlan(env, svcOpts{system: true}, f); !strings.Contains(p.problem, "not supported") {
		t.Error(p.problem)
	}
}

func TestServicePlanLinuxUser(t *testing.T) {
	env := linuxEnv()
	f := factsWith("/home/alice/.config/algodtop/config.json", map[string]string{"/home/alice/.mainnet": "alice"})
	if recommendSystem(env, f) {
		t.Fatal("own data should get a user unit")
	}
	p := buildPlan(env, svcOpts{}, f)
	if p.problem != "" || p.mode != "systemd-user" || p.file != "/home/alice/.config/systemd/user/algodtop.service" {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(p.content, "ExecStart=/home/alice/go/bin/algodtop daemon --config /home/alice/.config/algodtop/config.json\n") ||
		!strings.Contains(p.content, "WantedBy=default.target") {
		t.Error(p.content)
	}
	if !strings.Contains(p.lingerNote(env), "sudo loginctl enable-linger alice") {
		t.Error(p.lingerNote(env))
	}
	if got := shellJoin(p.controlCmds(env, "start")[2]...); got != "systemctl --user restart algodtop" {
		t.Error(got)
	}
	// As root, a user unit makes no sense.
	root := env
	root.root, root.user = true, "root"
	if p := buildPlan(root, svcOpts{}, f); !strings.Contains(p.problem, "--system") {
		t.Error(p.problem)
	}
}

// A server with a package install, where algod runs as algorand from
// /var/lib/algorand and alice logs in over SSH.
func TestServicePlanLinuxSystem(t *testing.T) {
	env := linuxEnv()
	user := svcFacts{config: "/home/alice/.config/algodtop/config.json", found: []config.Node{{Name: "mainnet", DataDir: "/var/lib/algorand"}},
		owners: map[string]string{"/var/lib/algorand": "algorand"}}
	if !recommendSystem(env, user) {
		t.Fatal("data owned by algorand should get a system service")
	}
	p := buildPlan(env, svcOpts{}, user)
	if !strings.Contains(p.problem, "sudo /home/alice/go/bin/algodtop service install --system") {
		t.Fatal(p.problem)
	}

	env.root, env.user, env.sudoUser = true, "root", "alice"
	sys := svcFacts{config: systemConfig, found: user.found, owners: user.owners}
	p = buildPlan(env, svcOpts{system: true}, sys)
	if p.problem != "" {
		t.Fatal(p.problem)
	}
	if p.mode != "systemd-system" || p.runAs != "algorand" || p.file != "/etc/systemd/system/algodtop.service" || !p.copyBin || p.bin != systemBin {
		t.Fatalf("%+v", p)
	}
	if p.writeConfig == nil || len(p.writeConfig.Nodes) != 1 || p.socket != "/var/lib/algodtop/algodtop.sock" {
		t.Fatalf("%+v", p)
	}
	if p.operator != "alice" {
		t.Errorf("operator %q", p.operator)
	}
	// A root shell from plain su has no $SUDO_USER; the binary's owner
	// stands in.
	su := env
	su.sudoUser, su.exeOwner = "", "alice"
	if p := buildPlan(su, svcOpts{system: true}, sys); p.operator != "alice" {
		t.Errorf("operator %q without SUDO_USER", p.operator)
	}
	su.exeOwner = "root"
	if p := buildPlan(su, svcOpts{system: true}, sys); p.operator != "" {
		t.Errorf("operator %q for a root-owned binary", p.operator)
	}
	for _, want := range []string{
		"User=algorand\nGroup=algodtop\n",
		// The service runs its own copy, which its account may replace.
		"ExecStart=/var/lib/algodtop/bin/algod.top daemon --config /etc/algodtop/config.json --store-dir /var/lib/algodtop --socket-group algodtop\n",
		"StateDirectory=algodtop\n",
		"After=network-online.target algorand.service\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(p.content, want) {
			t.Errorf("missing %q in\n%s", want, p.content)
		}
	}
	if got := shellJoin(p.controlCmds(env, "start")[1]...); got != "systemctl enable algodtop" {
		t.Error(got)
	}
	if p.bin != "/usr/local/bin/algod.top" || !p.copyBin || p.svcBin != "/var/lib/algodtop/bin/algod.top" {
		t.Errorf("bin %q copy %v svcBin %q", p.bin, p.copyBin, p.svcBin)
	}

	// A store its account cannot run binaries from: the service runs the
	// root-owned copy.
	if p := buildPlan(env, svcOpts{system: true, inPlace: true}, sys); p.svcBin != "" ||
		!strings.Contains(p.content, "ExecStart=/usr/local/bin/algod.top daemon ") {
		t.Errorf("in place: %q\n%s", p.svcBin, p.content)
	}

	// A binary already in a system directory runs in place.
	inPlace := env
	inPlace.exe = "/usr/bin/algodtop"
	if p := buildPlan(inPlace, svcOpts{system: true}, sys); p.copyBin || p.bin != "/usr/bin/algodtop" || p.svcBin != "/var/lib/algodtop/bin/algod.top" {
		t.Errorf("%+v", p)
	}

	mixed := sys
	mixed.owners = map[string]string{"/var/lib/algorand": "algorand", "/home/alice/.testnet": "alice"}
	if p := buildPlan(env, svcOpts{system: true}, mixed); !strings.Contains(p.problem, "--run-as") {
		t.Error(p.problem)
	}
	if p := buildPlan(env, svcOpts{system: true, runAs: "alice"}, mixed); p.problem != "" || p.runAs != "alice" {
		t.Errorf("%+v", p)
	}
	if p := buildPlan(env, svcOpts{system: true, runAs: "root"}, sys); !strings.Contains(p.problem, "never needs root") {
		t.Error(p.problem)
	}

	// The agent: its own config, store and socket, and no web UI.
	agentFacts := svcFacts{config: systemAgentConfig, found: user.found, owners: user.owners}
	p = buildPlan(env, svcOpts{system: true, agent: true}, agentFacts)
	if p.problem != "" {
		t.Fatal(p.problem)
	}
	if p.name != "algodtop-agent" || p.file != "/etc/systemd/system/algodtop-agent.service" || p.socket != "/var/lib/algodtop-agent/agent.sock" || p.listen != "" {
		t.Fatalf("%+v", p)
	}
	if p.writeConfig == nil || p.writeConfig.Web.ListenAddr() != "" {
		t.Fatalf("agent config %+v", p.writeConfig)
	}
	if !strings.Contains(p.content, "ExecStart=/var/lib/algodtop-agent/bin/algod.top agent --config /etc/algodtop/agent.json --store-dir /var/lib/algodtop-agent --socket-group algodtop\n") ||
		!strings.Contains(p.content, "StateDirectory=algodtop-agent\n") {
		t.Error(p.content)
	}
}

func TestServicePlanProblems(t *testing.T) {
	env := linuxEnv()
	empty := svcFacts{config: "/home/alice/.config/algodtop/config.json", owners: map[string]string{}}
	if p := buildPlan(env, svcOpts{}, empty); !strings.Contains(p.problem, "no running algod") {
		t.Error(p.problem)
	}
	// The agent discovers its own nodes when it starts.
	if p := buildPlan(env, svcOpts{agent: true}, empty); p.problem != "" || strings.Contains(p.content, "--config") || p.name != "algodtop-agent" {
		t.Errorf("%+v", p)
	}
	noSystemd := env
	noSystemd.systemd = false
	if p := buildPlan(noSystemd, svcOpts{}, empty); p.mode != "" || !strings.Contains(p.problem, "@reboot /home/alice/go/bin/algodtop daemon") {
		t.Error(p.problem)
	}
	goRun := env
	goRun.exe = "/tmp/go-build123/b001/exe/algodtop"
	f := factsWith("/home/alice/.config/algodtop/config.json", map[string]string{"/home/alice/.mainnet": "alice"})
	if p := buildPlan(goRun, svcOpts{}, f); !strings.Contains(p.problem, "go run") {
		t.Error(p.problem)
	}
	win := svcEnv{goos: "windows"}
	if p := buildPlan(win, svcOpts{}, f); p.mode != "" || !strings.Contains(p.problem, "service install") {
		t.Error(p.problem)
	}
}

// A root shell from plain su keeps a PATH without /usr/sbin.
func TestToolPathOutsidePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the system tool directories are Unix paths")
	}
	t.Setenv("PATH", "/nonexistent")
	if p := toolPath("sh"); p != "/bin/sh" && p != "/usr/bin/sh" {
		t.Errorf("sh resolved to %q", p)
	}
	if p := toolPath("no-such-tool-algodtop"); p != "no-such-tool-algodtop" {
		t.Errorf("got %q", p)
	}
}
