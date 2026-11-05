package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/config"
	"algod.top/internal/remote"
	"algod.top/internal/svc"
	"algod.top/internal/sysstat"
	"algod.top/internal/tui"
)

// Paths for a Linux system service, which runs as the account that owns
// the algod data directories rather than as the person who installed it.
const (
	systemConfig      = "/etc/algodtop/config.json"
	systemStore       = "/var/lib/algodtop"
	systemAgentConfig = "/etc/algodtop/agent.json"
	systemAgentStore  = "/var/lib/algodtop-agent"
	systemBin         = "/usr/local/bin/algod.top"
	// legacySystemBin is where installs before the rename put the binary.
	// An install points it at systemBin, so the old name and hubs that
	// run it as the agent relay get the new binary.
	legacySystemBin = "/usr/local/bin/algodtop"
	// systemGroup may use a system service's socket. Its members reach the
	// daemon's terminal UI, and the hub pulls from the agent over SSH as
	// one of them.
	systemGroup = "algodtop"
)

func systemSocket() string      { return filepath.Join(systemStore, "algodtop.sock") }
func systemAgentSocket() string { return filepath.Join(systemAgentStore, "agent.sock") }

type svcOpts struct {
	agent  bool
	system bool
	runAs  string
	config string
	// inPlace runs a system service from /usr/local/bin rather than its
	// own copy, when its account cannot run a binary in its store.
	inPlace bool
}

// svcEnv is the machine the service is for.
type svcEnv struct {
	goos      string
	systemd   bool   // systemd is the init system
	exe       string // this binary
	home      string
	user      string // the current account
	uid       int
	root      bool
	xdgConfig string // $XDG_CONFIG_HOME
	linger    string // loginctl's Linger for the current account: yes, no or ""
	sudoUser  string // $SUDO_USER, the person behind a root install
	exeOwner  string // the account that owns this binary
}

// svcFacts is what the service will monitor.
type svcFacts struct {
	config  string            // the config file the service reads
	cfg     *config.Config    // its contents; nil when missing or unreadable
	readErr error             // why an existing config could not be read
	found   []config.Node     // discovered nodes, written when the config is missing
	owners  map[string]string // local data directory to the account that owns it
}

type svcPlan struct {
	mode        string // launchd, systemd-user or systemd-system; "" when unsupported
	name        string // systemd unit name or launchd label
	file        string
	content     string
	config      string // config file, "" for an agent without one
	writeConfig *config.Config
	socket      string
	store       string
	runAs       string
	bin         string // the binary people run; the service's too, except a system service
	copyBin     bool   // install copies this binary to bin first
	// svcBin is a system service's own copy, in its store and owned by
	// the account it runs as, so the service can upgrade itself.
	svcBin   string
	listen   string
	log      string // launchd log file
	operator string // added to systemGroup by a system install
	problem  string // why install cannot go ahead
}

func cmdService(args []string) int {
	fs := newFlags(prog + " service")
	agentF := fs.flag("", "agent", "The agent instead of the daemon")
	systemF := fs.flag("", "system", "A system service that starts at boot and runs as the data directory owner (Linux, needs root)")
	runAsF := fs.str("", "run-as", "USER", "Account a system service runs as, default the owner of the node data directories")
	cfgF := fs.str("c", "config", "PATH", "Config file the service reads")
	helpF := fs.flag("h", "help", "Show help")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if helpF.Bool() {
		fmt.Printf(`Usage: %s service [show|install|uninstall|status] [flags]

  show       explain the service for this machine and print its file (the default)
  install    write the service file, enable it and start it
  uninstall  stop and disable the service and remove its file; config and data stay
  status     report whether the service is installed and running

%s`, prog, fs.usage())
		return 0
	}
	sub := "show"
	if len(fs.args) > 0 {
		sub = fs.args[0]
	}
	if len(fs.args) > 1 {
		return errf("unexpected argument %q", fs.args[1])
	}
	o := svcOpts{agent: agentF.Bool(), system: systemF.Bool(), runAs: runAsF.String(), config: cfgF.String()}
	if o.config != "" {
		if abs, err := filepath.Abs(algod.ExpandHome(o.config)); err == nil {
			o.config = abs
		}
	}
	env := currentEnv()
	switch sub {
	case "show":
		return serviceShow(env, o)
	case "install":
		return serviceInstall(env, o)
	case "uninstall", "remove":
		return serviceUninstall(env, o)
	case "status":
		return serviceStatus(env, o)
	}
	return errf("unknown service command %q; use show, install, uninstall or status", sub)
}

func currentEnv() svcEnv {
	e := svcEnv{goos: runtime.GOOS, uid: os.Getuid(), xdgConfig: os.Getenv("XDG_CONFIG_HOME")}
	e.exe, _ = os.Executable()
	e.home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		e.user = u.Username
	}
	e.root = isRoot()
	if e.root {
		e.sudoUser = os.Getenv("SUDO_USER")
		if owner, _, _, err := fileOwner(e.exe); err == nil {
			e.exeOwner = owner
		}
	}
	if e.goos == "linux" {
		if fi, err := os.Stat("/run/systemd/system"); err == nil && fi.IsDir() {
			e.systemd = true
			// loginctl fails for an account with no session, so read the
			// flag file it writes.
			e.linger = "no"
			if _, err := os.Stat("/var/lib/systemd/linger/" + e.user); err == nil {
				e.linger = "yes"
			}
		}
	}
	return e
}

// recommendSystem reports whether a system service fits this machine
// better: on Linux when the node data belongs to another account, or when
// running as root.
func recommendSystem(env svcEnv, f svcFacts) bool {
	if env.goos != "linux" || !env.systemd {
		return false
	}
	if env.root {
		return true
	}
	for _, o := range f.owners {
		if o != env.user {
			return true
		}
	}
	return false
}

func configPathFor(env svcEnv, o svcOpts, system bool) string {
	switch {
	case o.config != "":
		return o.config
	case system && o.agent:
		return systemAgentConfig
	case system:
		return systemConfig
	case o.agent:
		return agentConfigPath()
	}
	return config.DefaultPath()
}

// gatherFacts reads the config the service would use and finds who owns
// each local data directory. With no config it discovers running nodes.
func gatherFacts(env svcEnv, o svcOpts, system bool) svcFacts {
	f := svcFacts{config: configPathFor(env, o, system), owners: map[string]string{}}
	var dirs []string
	if _, err := os.Stat(f.config); err == nil || !errors.Is(err, fs.ErrNotExist) {
		c, err := config.Load(f.config)
		if err != nil {
			f.readErr = err
		} else {
			f.cfg = c
			for _, n := range c.Nodes {
				if n.DataDir != "" {
					dirs = append(dirs, algod.ExpandHome(n.DataDir))
				}
			}
		}
	}
	if f.cfg == nil {
		for _, d := range algod.Discover(sysstat.ListAlgod, sysstat.Alive) {
			// Discovery also scans the caller's home, which differs between
			// `show` and `sudo ... install`. A system service takes the
			// running nodes, which both see.
			if system && !d.Running {
				continue
			}
			f.found = append(f.found, config.Node{Name: d.Name, DataDir: d.DataDir})
			dirs = append(dirs, d.DataDir)
		}
	}
	for _, d := range dirs {
		if owner, _, _, err := fileOwner(d); err == nil {
			f.owners[d] = owner
		}
	}
	return f
}

// buildPlan decides what to install. It does no I/O, so tests can build
// a plan for any platform.
func buildPlan(env svcEnv, o svcOpts, f svcFacts) svcPlan {
	p := svcPlan{config: f.config}
	role, name, label := "daemon", "algodtop", "top.algod.daemon"
	if o.agent {
		role, name, label = "agent", "algodtop-agent", "top.algod.agent"
	}
	switch {
	case env.goos == "windows":
		p.problem = windowsServiceHelp(o.agent)
		return p
	case env.goos == "darwin":
		p.mode, p.name = "launchd", label
		if o.system {
			p.problem = "On macOS algod.top installs a launchd user agent, which starts at login. A system daemon in /Library/LaunchDaemons is not supported yet. Run the install without --system."
			return p
		}
	case env.goos == "linux" && env.systemd:
		p.mode, p.name = "systemd-user", name
		if o.system {
			p.mode = "systemd-system"
		}
	default:
		p.problem = fmt.Sprintf("algod.top can install a service under systemd on Linux and launchd on macOS, and this machine has neither.\n"+
			"Start `%s %s` from your init system, or from cron with a line such as:\n\n  @reboot %s %s >>%s 2>&1\n",
			prog, role, shellJoin(env.exe), role, path.Join(env.home, "algodtop.log"))
		return p
	}
	if strings.Contains(env.exe, "/go-build") || strings.Contains(env.exe, `\go-build`) {
		p.problem = fmt.Sprintf("This binary is a `go run` build, which is deleted when it exits. Install it with `go install ./algodtop` and run `%s service install` from there.", prog)
		return p
	}
	if f.readErr != nil {
		if errors.Is(f.readErr, fs.ErrPermission) && p.mode == "systemd-system" && !env.root {
			// The existing system config belongs to the service account.
		} else {
			p.problem = fmt.Sprintf("Cannot read %s: %v", f.config, f.readErr)
			return p
		}
	}
	// A user agent discovers its own nodes when it has no config. A system
	// service always gets a config file.
	needConfig := !o.agent || p.mode == "systemd-system"
	if f.cfg == nil && f.readErr == nil && needConfig {
		if len(f.found) == 0 {
			p.problem = fmt.Sprintf("There is no config at %s and no running algod was found. Start your nodes or write the config, then run this again.", f.config)
			return p
		}
		p.writeConfig = &config.Config{Nodes: f.found}
		if o.agent {
			empty := ""
			p.writeConfig.Web.Listen = &empty
		}
	}

	// Who runs it.
	var owners []string
	for _, o := range f.owners {
		if !slices.Contains(owners, o) {
			owners = append(owners, o)
		}
	}
	sort.Strings(owners)
	foreign := slices.DeleteFunc(slices.Clone(owners), func(s string) bool { return s == env.user })
	if p.mode == "systemd-system" {
		p.runAs = o.runAs
		if p.runAs == "" {
			switch len(owners) {
			case 0:
				p.problem = "No local data directory says which account the service should run as. Pass --run-as USER."
				return p
			case 1:
				p.runAs = owners[0]
			default:
				p.problem = fmt.Sprintf("The node data directories belong to %s. Pass --run-as USER to choose the account the service runs as.", strings.Join(owners, " and "))
				return p
			}
		}
		if p.runAs == "root" {
			p.problem = "algod.top never needs root. Pass --run-as with the account that owns the algod data directories."
			return p
		}
	} else {
		if env.root && env.goos == "linux" {
			p.problem = fmt.Sprintf("As root, install a system service instead: %s service install --system", shellJoin(env.exe))
			return p
		}
		if len(foreign) > 0 {
			dirs := dirsOwnedBy(f.owners, foreign)
			if p.mode == "systemd-user" {
				p.problem = fmt.Sprintf("The node data in %s belongs to %s, and a user service would run as %s. A system service runs as %s from boot, like algorand.service. Install it with:\n\n  sudo %s",
					strings.Join(dirs, ", "), strings.Join(foreign, " and "), env.user, strings.Join(foreign, " and "), installCommand(env, o, true))
			} else {
				p.problem = fmt.Sprintf("The node data in %s belongs to %s, and a user agent would run as %s. Install the service while logged in as %s.",
					strings.Join(dirs, ", "), strings.Join(foreign, " and "), env.user, strings.Join(foreign, " and "))
			}
			return p
		}
	}

	// What it runs.
	p.bin = env.exe
	args := []string{p.bin, role}
	if p.writeConfig != nil || f.cfg != nil || f.readErr != nil || o.config != "" {
		args = append(args, "--config", f.config)
	} else {
		p.config = ""
	}
	p.listen = config.DefaultListen
	if f.cfg != nil {
		p.listen = f.cfg.Web.ListenAddr()
	}
	if o.agent {
		p.listen = ""
	}
	switch p.mode {
	case "launchd":
		p.log = path.Join(env.home, "Library", "Logs", name+".log")
		p.file = path.Join(env.home, "Library", "LaunchAgents", label+".plist")
		// launchd starts jobs with only /usr/bin:/bin:/usr/sbin:/sbin. Alert
		// commands may need Homebrew tools or a script next to algodtop.
		var dirs []string
		for _, d := range []string{path.Dir(p.bin), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
			if !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
		pathEnv := strings.Join(dirs, ":")
		p.content = svc.LaunchdPlist(svc.Spec{Label: label, Args: args, Env: map[string]string{"PATH": pathEnv}, Log: p.log})
		p.socket, p.store = userSocket(f, o.agent)
	case "systemd-user":
		dir := env.xdgConfig
		if dir == "" {
			dir = path.Join(env.home, ".config")
		}
		p.file = path.Join(dir, "systemd", "user", name+".service")
		p.content = svc.SystemdUnit(svc.Spec{Name: name, Description: "algod.top " + role, Args: args})
		p.socket, p.store = userSocket(f, o.agent)
	case "systemd-system":
		if !strings.HasPrefix(p.bin, "/usr/") && !strings.HasPrefix(p.bin, "/opt/") {
			p.bin, p.copyBin = systemBin, true
			args[0] = p.bin
		}
		// The service's group is systemGroup, so the state directory and
		// the socket belong to it and its members can reach the socket.
		spec := svc.Spec{Name: name, Description: "algod.top " + role, System: true, User: p.runAs, Group: systemGroup, After: []string{"algorand.service"}}
		store := systemStore
		if o.agent {
			store = systemAgentStore
		}
		p.store = store
		if f.cfg != nil && f.cfg.StoreDir != "" {
			p.store = f.cfg.StorePath()
		} else {
			args = append(args, "--store-dir", store)
			spec.StateDir = path.Base(store)
		}
		args = append(args, "--socket-group", systemGroup)
		// People and the hub's ssh relay run the root-owned copy in
		// /usr/local/bin. The service runs its own copy, which belongs to
		// its account, so `algodtop upgrade` can replace it without root.
		if !o.inPlace {
			p.svcBin = path.Join(p.store, "bin", "algod.top")
			args[0] = p.svcBin
		}
		spec.Args = args
		p.file = "/etc/systemd/system/" + name + ".service"
		p.content = svc.SystemdUnit(spec)
		p.socket = path.Join(p.store, "algodtop.sock")
		if o.agent {
			p.socket = path.Join(p.store, "agent.sock")
		}
		if f.cfg != nil && f.cfg.Socket != "" {
			p.socket = f.cfg.SocketPath()
		}
		p.operator = env.user
		if env.root {
			// A root shell from plain su has no $SUDO_USER. The binary was
			// most likely copied in by the person installing it.
			p.operator = cmp.Or(env.sudoUser, env.exeOwner)
		}
		if p.operator == "root" || p.operator == p.runAs {
			p.operator = ""
		}
	}
	return p
}

func userSocket(f svcFacts, agent bool) (socket, store string) {
	c := f.cfg
	if c == nil {
		c = &config.Config{}
	}
	socket, store = c.SocketPath(), c.StorePath()
	if agent {
		socket = filepath.Join(filepath.Dir(socket), "agent.sock")
	}
	return socket, store
}

func dirsOwnedBy(owners map[string]string, who []string) []string {
	var out []string
	for d, o := range owners {
		if slices.Contains(who, o) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func installCommand(env svcEnv, o svcOpts, system bool) string {
	args := []string{env.exe, "service", "install"}
	if o.agent {
		args = append(args, "--agent")
	}
	if system {
		args = append(args, "--system")
	}
	if o.runAs != "" {
		args = append(args, "--run-as", o.runAs)
	}
	if o.config != "" {
		args = append(args, "--config", o.config)
	}
	return shellJoin(args...)
}

func shellJoin(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if shellSafe(a) {
			q[i] = a
		} else {
			q[i] = remote.ShellQuote(a)
		}
	}
	return strings.Join(q, " ")
}

// shellSafe reports whether a word needs no quotes in a POSIX shell.
func shellSafe(a string) bool {
	return a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@%+,") == ""
}

// planFor gathers facts and builds the plan. Without --system it picks a
// system service when that fits the machine better, for show only.
func planFor(env svcEnv, o svcOpts, show bool) (svcPlan, svcOpts) {
	f := gatherFacts(env, o, o.system)
	if show && !o.system && !o.agent && recommendSystem(env, f) {
		o.system = true
		f = gatherFacts(env, o, true)
	}
	return buildPlan(env, o, f), o
}

func (p svcPlan) controlCmds(env svcEnv, action string) [][]string {
	switch p.mode {
	case "launchd":
		domain := "gui/" + strconv.Itoa(env.uid)
		switch action {
		case "start":
			return [][]string{{"launchctl", "enable", domain + "/" + p.name}, {"launchctl", "bootstrap", domain, p.file}}
		case "stop":
			return [][]string{{"launchctl", "bootout", domain + "/" + p.name}}
		}
	case "systemd-user", "systemd-system":
		sc := []string{"systemctl"}
		if p.mode == "systemd-user" {
			sc = append(sc, "--user")
		}
		with := func(a ...string) []string { return append(slices.Clone(sc), a...) }
		switch action {
		case "start":
			return [][]string{with("daemon-reload"), with("enable", p.name), with("restart", p.name)}
		case "stop":
			return [][]string{with("disable", "--now", p.name)}
		case "reload":
			return [][]string{with("daemon-reload")}
		}
	}
	return nil
}

func (p svcPlan) logs() string {
	switch p.mode {
	case "launchd":
		return p.log
	case "systemd-user":
		return "journalctl --user -u " + p.name + " -f"
	case "systemd-system":
		return "journalctl -u " + p.name + " -f"
	}
	return ""
}

// lingerNote explains how a user unit starts at boot rather than at login.
func (p svcPlan) lingerNote(env svcEnv) string {
	if p.mode != "systemd-user" || env.linger != "no" {
		return ""
	}
	return fmt.Sprintf("To start it at boot and keep it running after you log out, enable lingering once:\n\n  sudo loginctl enable-linger %s", env.user)
}

func (p svcPlan) describe(env svcEnv) string {
	role := "daemon"
	if strings.HasSuffix(p.name, "agent") {
		role = "agent"
	}
	switch p.mode {
	case "launchd":
		return fmt.Sprintf("A launchd user agent starts the %s when you log in and restarts it after a crash.", role)
	case "systemd-user":
		return fmt.Sprintf("A systemd user unit starts the %s when you log in and restarts it after a crash.", role)
	case "systemd-system":
		return fmt.Sprintf("A system service runs the %s as %s from boot, like algorand.service, and restarts it after a crash.", role, p.runAs)
	}
	return ""
}

func serviceShow(env svcEnv, o svcOpts) int {
	p, o := planFor(env, o, true)
	role := map[bool]string{false: "daemon", true: "agent"}[o.agent]
	if p.problem != "" {
		fmt.Println(p.problem)
		if p.mode == "" {
			return 0
		}
		return 1
	}
	fmt.Printf("%s Install it with:\n\n", p.describe(env))
	cmd := installCommand(env, o, o.system)
	if o.system && !env.root {
		cmd = "sudo " + cmd
	} else if !o.system {
		cmd = strings.Replace(cmd, shellJoin(env.exe), prog, 1)
	}
	fmt.Printf("  %s\n\nThat will:\n", cmd)
	if p.copyBin {
		fmt.Printf("- copy this binary to %s\n", p.bin)
		if fi, err := os.Lstat(legacySystemBin); err == nil && fi.Mode().IsRegular() && p.bin == systemBin {
			fmt.Printf("- replace the old %s with a link to it\n", legacySystemBin)
		}
	}
	if p.svcBin != "" {
		fmt.Printf("- copy it to %s for the service, owned by %s, so `%s upgrade` can replace it without root\n", p.svcBin, p.runAs, prog)
	}
	if p.writeConfig != nil {
		fmt.Printf("- write %s with the nodes found here:\n", p.config)
		for _, n := range p.writeConfig.Nodes {
			fmt.Printf("    %-12s %s\n", n.Name, n.DataDir)
		}
	} else if p.config != "" {
		fmt.Printf("- use the config in %s\n", p.config)
	}
	if p.mode == "systemd-system" {
		fmt.Printf("- create the %s group, which may use the service's socket", systemGroup)
		if p.operator != "" {
			fmt.Printf(", and add %s to it", p.operator)
		}
		fmt.Println()
	}
	fmt.Printf("- write %s:\n\n%s\n", p.file, indentBy(p.content, "    "))
	fmt.Println("- run:")
	for _, c := range p.controlCmds(env, "start") {
		fmt.Printf("    %s\n", shellJoin(c...))
	}
	if n := p.lingerNote(env); n != "" {
		fmt.Printf("\n%s\n", n)
	}
	if p.mode != "systemd-system" {
		fmt.Printf("\nRunning `%s %s` in a terminal or tmux needs no service. Stop that one before installing, since both would use %s.\n", prog, role, p.store)
	}
	return 0
}

func indentBy(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pre + l
		}
	}
	return strings.Join(lines, "\n")
}

func runCmd(c []string) error {
	fmt.Printf("+ %s\n", shellJoin(c...))
	cmd := exec.Command(toolPath(c[0]), c[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// serviceActive reports whether the service manager has the service
// running, so a live socket belongs to it.
func serviceActive(env svcEnv, p svcPlan) bool {
	switch p.mode {
	case "launchd":
		return exec.Command(toolPath("launchctl"), "print", "gui/"+strconv.Itoa(env.uid)+"/"+p.name).Run() == nil
	case "systemd-user":
		return exec.Command(toolPath("systemctl"), "--user", "is-active", "--quiet", p.name).Run() == nil
	case "systemd-system":
		return exec.Command(toolPath("systemctl"), "is-active", "--quiet", p.name).Run() == nil
	}
	return false
}

func serviceInstall(env svcEnv, o svcOpts) int {
	p, o := planFor(env, o, false)
	if p.problem != "" {
		fmt.Fprintln(os.Stderr, p.problem)
		return 1
	}
	if p.mode == "systemd-system" && !env.root {
		return errf("a system service needs root: sudo %s", installCommand(env, o, true))
	}
	if p.mode == "systemd-user" {
		if out, err := exec.Command(toolPath("systemctl"), "--user", "show-environment").CombinedOutput(); err != nil {
			return errf("systemctl --user cannot reach your user manager (%s). A normal login or SSH session starts it; `su` and `sudo -u` do not.", strings.TrimSpace(string(out)))
		}
	}
	active := serviceActive(env, p)
	if !active {
		if c, err := tui.DialSocket(p.socket); err == nil {
			c.Close()
			return errf("a daemon is already running on %s, probably in a terminal or tmux. Stop it first, since the service would use the same store.", p.socket)
		}
	}
	if p.copyBin {
		if err := copyBinary(env.exe, p.bin); err != nil {
			return errf("copy %s: %v", p.bin, err)
		}
		fmt.Printf("copied %s to %s\n", env.exe, p.bin)
		if p.bin == systemBin {
			if linked, err := linkLegacy(); err != nil {
				fmt.Printf("warn: %s still holds an old binary: %v\n", legacySystemBin, err)
			} else if linked {
				fmt.Printf("pointed %s at %s\n", legacySystemBin, systemBin)
			}
		}
	}
	if p.writeConfig != nil {
		if err := p.writeConfig.Validate(); err != nil {
			return errf("%v", err)
		}
		if err := p.writeConfig.Save(p.config); err != nil {
			return errf("write %s: %v", p.config, err)
		}
		fmt.Printf("wrote %s with %s\n", p.config, plural(len(p.writeConfig.Nodes), "node"))
	}
	joined := false
	if p.mode == "systemd-system" {
		if err := chownConfig(p.config, p.runAs); err != nil {
			return errf("%v", err)
		}
		if err := ensureGroup(systemGroup); err != nil {
			return errf("%v", err)
		}
		if p.operator != "" {
			added, err := addToGroup(p.operator, systemGroup)
			if err != nil {
				return errf("%v", err)
			}
			joined = added
		}
		if err := installServiceBinary(env.exe, p.svcBin, p.store, p.runAs, systemGroup); err != nil {
			return errf("%v", err)
		}
		fmt.Printf("copied %s to %s\n", env.exe, p.svcBin)
		if err := runsAs(p.svcBin, p.runAs); err != nil {
			// Perhaps the store's filesystem is mounted noexec.
			fmt.Printf("warn: %s cannot run %s (%v)\n      The service runs %s instead, and upgrading it needs root.\n", p.runAs, p.svcBin, err, p.bin)
			os.RemoveAll(filepath.Dir(p.svcBin))
			o.inPlace = true
			again, _ := planFor(env, o, false)
			p.content, p.svcBin = again.content, ""
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.file), 0o755); err != nil {
		return errf("%v", err)
	}
	if err := os.WriteFile(p.file, []byte(p.content), 0o644); err != nil {
		return errf("write %s: %v", p.file, err)
	}
	fmt.Printf("wrote %s\n", p.file)
	if active && p.mode == "launchd" {
		for _, c := range p.controlCmds(env, "stop") {
			runCmd(c)
		}
		waitGone(env, p)
	}
	for _, c := range p.controlCmds(env, "start") {
		if err := runCmd(c); err != nil {
			return errf("%s: %v", c[0], err)
		}
	}
	if !waitSocket(p.socket, 15*time.Second) {
		return errf("the service started, but no daemon answers on %s yet. Check its log: %s", p.socket, p.logs())
	}
	// Name the installed binary, which may differ from the one running.
	name := strings.TrimSuffix(filepath.Base(p.bin), ".exe")
	fmt.Printf("\n%s is running as a service and answers on %s.\n%s\n\n", name, p.socket, p.describe(env))
	fmt.Printf("Logs:      %s\n", p.logs())
	switch p.mode {
	case "systemd-system":
		agentFlag := map[bool]string{true: " --agent"}[o.agent]
		fmt.Printf("Config:    %s, then `sudo systemctl reload %s`\n", p.config, p.name)
		if o.agent {
			fmt.Printf("Hub:       on the machine that runs your hub, `%s remote add HOST --path %s`\n", prog, p.bin)
		} else {
			fmt.Printf("Terminal:  %s, as a member of the %s group\n", name, systemGroup)
		}
		if p.listen != "" {
			fmt.Printf("Web UI:    http://%s on this host, or `ssh -L %s:%s HOST` from another\n", p.listen, portOf(p.listen), p.listen)
		}
		if o.agent {
			fmt.Printf("Upgrade:   `%s upgrade` on the hub, which upgrades this agent with the rest\n", prog)
		} else {
			fmt.Printf("Upgrade:   %s upgrade\n", prog)
		}
		fmt.Printf("Remove:    sudo %s service uninstall --system%s\n", name, agentFlag)
		if joined {
			fmt.Printf("\nAdded %s to the %s group. It applies to new logins, so log in again before using the socket.\n", p.operator, systemGroup)
		} else if p.operator == "" {
			fmt.Printf("\nNo account joined the %s group. Add the one you and your hub log in as: usermod -aG %s USER\n", systemGroup, systemGroup)
		}
	default:
		if p.listen != "" {
			fmt.Printf("Web UI:    http://%s\n", p.listen)
		}
		fmt.Printf("Upgrade:   %s upgrade\n", prog)
		fmt.Printf("Remove:    %s service uninstall%s\n", prog, map[bool]string{true: " --agent"}[o.agent])
		if n := p.lingerNote(env); n != "" {
			fmt.Printf("\n%s\n", n)
		}
	}
	return 0
}

func portOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

func waitSocket(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c, err := tui.DialSocket(path); err == nil {
			c.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// waitGone waits for launchd to finish tearing a job down, since
// bootstrap fails while the old one is still exiting.
func waitGone(env svcEnv, p svcPlan) {
	for range 50 {
		if !serviceActive(env, p) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func copyBinary(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// linkLegacy replaces an old /usr/local/bin/algodtop with a symlink to
// systemBin. It leaves the path alone when nothing is there.
func linkLegacy() (bool, error) {
	fi, err := os.Lstat(legacySystemBin)
	if err != nil {
		return false, nil
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if t, err := os.Readlink(legacySystemBin); err == nil && t == systemBin {
			return false, nil
		}
	}
	tmp := legacySystemBin + ".link"
	os.Remove(tmp)
	if err := os.Symlink(systemBin, tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, legacySystemBin); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// installServiceBinary copies algod.top into a system service's store as
// bin/algodtop, owned by the account the service runs as. A new store
// gets the owner and mode that systemd's StateDirectory would give it.
func installServiceBinary(src, dst, store, owner, group string) error {
	u, err := user.Lookup(owner)
	if err != nil {
		return fmt.Errorf("account %s: %w", owner, err)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return fmt.Errorf("group %s: %w", group, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(g.Gid)
	if _, err := os.Stat(store); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(store, 0o750); err != nil {
			return err
		}
		if err := os.Chown(store, uid, gid); err != nil {
			return err
		}
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return err
	}
	if err := copyBinary(src, dst); err != nil {
		return fmt.Errorf("copy %s: %w", dst, err)
	}
	return os.Chown(dst, uid, gid)
}

// chownConfig gives the service account its config, and the directory
// when algodtop created it, so the daemon can save edits from the web UI.
func chownConfig(path, owner string) error {
	u, err := user.Lookup(owner)
	if err != nil {
		return fmt.Errorf("account %s: %w", owner, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if path == systemConfig || path == systemAgentConfig {
		if err := os.Chown(filepath.Dir(path), uid, gid); err != nil {
			return err
		}
		os.Chmod(filepath.Dir(path), 0o750)
		return os.Chown(path, uid, gid)
	}
	if o, _, _, err := fileOwner(path); err == nil && o != owner {
		fmt.Fprintf(os.Stderr, "note: %s belongs to %s; the service reads it as %s\n", path, o, owner)
	}
	return nil
}

func serviceUninstall(env svcEnv, o svcOpts) int {
	p, o := planFor(env, o, false)
	if p.mode == "" {
		fmt.Println(p.problem)
		return 1
	}
	if p.file == "" {
		// The plan stopped early; the file path depends only on the mode.
		p = bareServicePlan(env, o)
	}
	if _, err := os.Stat(p.file); err != nil {
		if !o.system && env.goos == "linux" {
			if _, err := os.Stat(bareServicePlan(env, svcOpts{agent: o.agent, system: true}).file); err == nil {
				return errf("no user service is installed; the system service is: sudo %s service uninstall --system", prog)
			}
		}
		return errf("no service is installed at %s", p.file)
	}
	if p.mode == "systemd-system" && !env.root {
		return errf("removing a system service needs root: sudo %s service uninstall --system", prog)
	}
	for _, c := range p.controlCmds(env, "stop") {
		runCmd(c)
	}
	if err := os.Remove(p.file); err != nil {
		return errf("%v", err)
	}
	fmt.Printf("removed %s\n", p.file)
	for _, c := range p.controlCmds(env, "reload") {
		runCmd(c)
	}
	keep := []string{}
	if p.config != "" {
		keep = append(keep, p.config)
	}
	if p.store != "" {
		keep = append(keep, p.store)
	}
	if len(keep) > 0 {
		fmt.Printf("kept %s; delete them if you no longer need that history\n", strings.Join(keep, " and "))
	}
	if p.mode == "systemd-system" && p.store != "" {
		// The service's own copy is not history.
		if err := os.RemoveAll(filepath.Join(p.store, "bin")); err == nil {
			fmt.Printf("removed %s\n", filepath.Join(p.store, "bin"))
		}
	}
	if p.mode == "systemd-system" {
		// The daemon and the agent share the binary and the group.
		other := bareServicePlan(env, svcOpts{agent: !o.agent, system: true})
		if _, err := os.Stat(other.file); err == nil {
			fmt.Printf("%s still uses %s and the %s group\n", other.name, systemBin, systemGroup)
		} else {
			bins := []string{systemBin}
			if _, err := os.Lstat(legacySystemBin); err == nil {
				bins = append(bins, legacySystemBin)
			}
			fmt.Printf("kept %s and the %s group; `rm %s` and `groupdel %s` remove them\n", strings.Join(bins, " and "), systemGroup, strings.Join(bins, " "), systemGroup)
		}
	}
	return 0
}

// bareServicePlan is the service file location alone, for uninstall and
// status when the full plan has a problem such as stopped nodes.
func bareServicePlan(env svcEnv, o svcOpts) svcPlan {
	name, label := "algodtop", "top.algod.daemon"
	if o.agent {
		name, label = "algodtop-agent", "top.algod.agent"
	}
	p := svcPlan{config: configPathFor(env, o, o.system)}
	switch {
	case env.goos == "darwin":
		p.mode, p.name, p.file = "launchd", label, path.Join(env.home, "Library", "LaunchAgents", label+".plist")
		p.log = path.Join(env.home, "Library", "Logs", name+".log")
	case env.goos == "linux" && o.system:
		p.mode, p.name, p.file, p.store = "systemd-system", name, "/etc/systemd/system/"+name+".service", systemStore
		if o.agent {
			p.store = systemAgentStore
		}
	case env.goos == "linux":
		dir := env.xdgConfig
		if dir == "" {
			dir = path.Join(env.home, ".config")
		}
		p.mode, p.name, p.file = "systemd-user", name, path.Join(dir, "systemd", "user", name+".service")
	}
	return p
}

func serviceStatus(env svcEnv, o svcOpts) int {
	if env.goos == "windows" {
		fmt.Println(windowsServiceHelp(o.agent))
		return 0
	}
	plans := []svcPlan{bareServicePlan(env, svcOpts{agent: o.agent})}
	if env.goos == "linux" {
		plans = append(plans, bareServicePlan(env, svcOpts{agent: o.agent, system: true}))
	}
	found := false
	for _, p := range plans {
		if p.mode == "" {
			continue
		}
		kind := map[string]string{"launchd": "launchd user agent", "systemd-user": "systemd user unit", "systemd-system": "systemd system service"}[p.mode]
		if _, err := os.Stat(p.file); err != nil {
			continue
		}
		found = true
		state := "not running"
		if serviceActive(env, p) {
			state = "running"
		}
		fmt.Printf("%s: installed at %s, %s\n", kind, p.file, state)
		fmt.Printf("  logs: %s\n", p.logs())
	}
	if !found {
		fmt.Printf("no %s service is installed; `%s service` explains how to add one\n", prog, prog)
	}
	return 0
}

// daemonSockets lists the sockets a CLI command tries, in order: the
// config's, the default, then a Linux system service's.
func daemonSockets(cfg *config.Config, ok bool) []string {
	var paths []string
	if ok {
		paths = append(paths, cfg.SocketPath())
	}
	paths = append(paths, config.DefaultSocket())
	if runtime.GOOS == "linux" {
		paths = append(paths, systemSocket())
	}
	return slices.Compact(paths)
}

// systemSocketHint explains a system service's socket that this account
// cannot reach, or returns "".
func systemSocketHint(path string, err error) string {
	if runtime.GOOS != "linux" || (path != systemSocket() && path != systemAgentSocket()) || !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	owner, _, _, oerr := fileOwner(filepath.Dir(path))
	if oerr != nil {
		return ""
	}
	g, gerr := user.LookupGroup(systemGroup)
	u, uerr := user.Current()
	if gerr != nil || uerr != nil {
		// A service installed before the group existed.
		return fmt.Sprintf("the %s system service runs as %s, and its socket is private to that account. Use `sudo -u %s %s`, or install the service again with this binary to open it to the %s group", prog, owner, owner, prog, systemGroup)
	}
	listed := false
	if ids, err := u.GroupIds(); err == nil {
		listed = slices.Contains(ids, g.Gid)
	}
	if !listed {
		return fmt.Sprintf("the %s system service's socket %s is for members of the %s group. Join it with `sudo usermod -aG %s %s` and log in again", prog, path, systemGroup, systemGroup, u.Username)
	}
	gid, _ := strconv.Atoi(g.Gid)
	if groups, err := os.Getgroups(); err == nil && !slices.Contains(groups, gid) {
		return fmt.Sprintf("%s joined the %s group after this login began. Log in again to use the system service's socket %s", u.Username, systemGroup, path)
	}
	return fmt.Sprintf("the %s system service's socket %s refused this account. Install the service again with this binary to open it to the %s group", prog, path, systemGroup)
}

// windowsServiceHelp prints the sc.exe command that registers the
// service. A service that runs as a user account needs that account's
// password at install time, so algodtop prints the command rather than
// collecting the password itself. The service control handler needs
// golang.org/x/sys/windows/svc, which is pending approval (DESIGN.md
// section 22); until then run the daemon under a wrapper such as WinSW or
// from Task Scheduler at logon.
func windowsServiceHelp(agent bool) string {
	exe, _ := os.Executable()
	sub, name := "daemon", "algodtop"
	if agent {
		sub, name = "agent", "algodtop-agent"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "algod.top cannot install a Windows service yet. Run these from an elevated prompt, as the account that owns the algod data directory:\n\n")
	fmt.Fprintf(&b, "  sc.exe create %s binPath= \"\\\"%s\\\" %s\" start= auto obj= .\\%s password= <password>\n", name, exe, sub, os.Getenv("USERNAME"))
	fmt.Fprintf(&b, "  sc.exe failure %s reset= 86400 actions= restart/5000/restart/5000/restart/60000\n\n", name)
	b.WriteString("Until the Windows service handler ships, Task Scheduler (\"At log on\", restart on failure) runs the daemon the same way.\n")
	return b.String()
}

// toolPath finds a system tool. A root shell from plain su keeps the
// user's PATH, which on Debian lacks /usr/sbin, where groupadd and
// usermod live.
func toolPath(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// ensureGroup creates a system group when it does not exist.
func ensureGroup(name string) error {
	if _, err := user.LookupGroup(name); err == nil {
		return nil
	}
	if err := runCmd([]string{"groupadd", "--system", name}); err != nil {
		return fmt.Errorf("create group %s: %w", name, err)
	}
	return nil
}

// addToGroup adds an account to a group. It reports false when the
// account was already a member.
func addToGroup(account, group string) (bool, error) {
	u, err := user.Lookup(account)
	if err != nil {
		return false, fmt.Errorf("account %s: %w", account, err)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return false, err
	}
	if ids, err := u.GroupIds(); err == nil && slices.Contains(ids, g.Gid) {
		return false, nil
	}
	if err := runCmd([]string{"usermod", "-aG", group, account}); err != nil {
		return false, fmt.Errorf("add %s to %s: %w", account, group, err)
	}
	return true, nil
}
