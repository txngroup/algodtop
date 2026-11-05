package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/config"
	"algod.top/internal/release"
	"algod.top/internal/remote"
	"algod.top/internal/sysstat"
	"algod.top/internal/version"
)

func init() {
	commands["discover"] = cmdDiscover
}

// cmdDiscover prints discovered data directories. The hub runs it on a
// remote during install. It never adds a node by itself.
func cmdDiscover(args []string) int {
	fs := newFlags(prog + " discover")
	jsonF := fs.flag("", "json", "JSON output")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	found := algod.Discover(sysstat.ListAlgod, sysstat.Alive)
	if found == nil {
		found = []algod.Found{}
	}
	if jsonF.Bool() {
		json.NewEncoder(os.Stdout).Encode(found)
		return 0
	}
	for _, f := range found {
		state := "stopped"
		if f.Running {
			state = fmt.Sprintf("running (pid %d)", f.PID)
		}
		fmt.Printf("%-12s %-10s %-45s %s\n", f.Name, f.Network, f.DataDir, state)
	}
	return 0
}

func cmdRemote(args []string) int {
	fs := newFlags(prog + " remote")
	nameF := fs.str("", "name", "NAME", "Remote name, default the host alias")
	nodeNameF := fs.str("", "node-name", "NAME", "add: hub name for the agent's only node, such as the SSH node it replaced")
	pathF := fs.str("", "path", "PATH", "Agent path on the remote, default ~/.local/bin/algod.top (add also tries /usr/local/bin/algod.top)")
	verF := fs.str("", "version", "VERSION", "Release to install: a version or latest, default this binary's version")
	dirF := fs.str("", "release-dir", "DIR", "A local release directory with checksums.txt and its signature")
	binF := fs.str("", "binary", "PATH", "A local binary (development only, needs --unsigned)")
	unsignedF := fs.flag("", "unsigned", "Allow --binary without a release signature (development only)")
	rootF := fs.str("", "root", "su|sudo", "How to become root on HOST when a system agent needs it, default ask")
	yesF := fs.flag("y", "yes", "Do not ask for confirmation")
	cfgF := fs.str("c", "config", "PATH", "Hub config file")
	helpF := fs.flag("h", "help", "Show help")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if v := rootF.String(); v != "" && v != "su" && v != "sudo" {
		return errf("--root must be su or sudo")
	}
	if helpF.Bool() || len(fs.args) < 2 {
		fmt.Printf("Usage: %s remote install|add|upgrade|sync|remove HOST [flags]\n\n  add  use an agent that already runs on HOST, such as a system service\n\n%s", prog, fs.usage())
		if helpF.Bool() {
			return 0
		}
		return 2
	}
	action, host := fs.args[0], fs.args[1]
	cfg, ok, err := loadConfig(cfgF.String())
	if err != nil {
		return errf("%v", err)
	}
	if !ok {
		cfg.Path = config.DefaultPath()
	}
	r, err := newRemoteCmd(cfg, host, nameF.String())
	if err != nil {
		return errf("%v", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	r.yes, r.path, r.ver, r.dir, r.bin, r.unsigned, r.nodeName, r.root = yesF.Bool(), pathF.String(), verF.String(), dirF.String(),
		binF.String(), unsignedF.Bool(), nodeNameF.String(), rootF.String()
	switch action {
	case "install":
		return r.install(ctx)
	case "add":
		return r.add(ctx)
	case "upgrade":
		return r.upgrade(ctx)
	case "sync":
		return r.sync(ctx)
	case "remove":
		return r.remove(ctx)
	}
	return errf("unknown remote action %q", action)
}

type remoteCmd struct {
	cfg                       *config.Config
	ssh                       *remote.SSH
	name, path, ver, dir, bin string
	nodeName                  string
	hostArg                   string
	yes, unsigned             bool
	host                      remote.Host
	root                      string        // su or sudo, from --root
	files                     *releaseCache // the release an upgrade installs
}

// newRemoteCmd prepares a remote command for a host. Without a name it
// uses the saved remote for that host, so `remote upgrade node1` finds a
// remote saved as partmon1, or else the host alias.
func newRemoteCmd(cfg *config.Config, host, name string) (*remoteCmd, error) {
	t, err := remote.ParseURL(sshURL(host))
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = strings.ReplaceAll(t.Host, ".", "-")
		if x, ok := remoteForHost(cfg, host); ok {
			name = x.Name
		}
	}
	return &remoteCmd{cfg: cfg, ssh: &remote.SSH{Target: t}, name: name, hostArg: host}, nil
}

func (r *remoteCmd) confirm(q string) bool {
	if r.yes {
		return true
	}
	fmt.Printf("%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
}

// binary returns a verified binary for the remote OS and architecture.
func (r *remoteCmd) binary(ctx context.Context) ([]byte, string, error) {
	h := r.host
	switch {
	case r.bin != "":
		if !r.unsigned {
			return nil, "", fmt.Errorf("--binary installs an unsigned binary; add --unsigned to confirm (development only)")
		}
		b, err := os.ReadFile(r.bin)
		return b, "unsigned local binary " + r.bin, err
	case r.dir != "":
		b, err := release.FromDir(r.dir, h.OS, h.Arch)
		return b, "signed release from " + r.dir, err
	}
	ver := r.ver
	if ver == "" {
		ver = strings.Fields(version.String())[0]
	}
	b, err := release.Fetch(ctx, ver, h.OS, h.Arch)
	return b, fmt.Sprintf("signed release %s from %s", ver, release.BaseURL), err
}

func (r *remoteCmd) agentPath() string {
	if r.path != "" {
		return r.path
	}
	for _, x := range r.cfg.Remotes {
		if x.Name == r.name && x.AgentPath != "" {
			return x.AgentPath
		}
	}
	return remote.DefaultPath(r.host)
}

func (r *remoteCmd) agentConfigPath() string {
	if r.host.OS == "windows" {
		return os.ExpandEnv(`%APPDATA%\algodtop\agent.json`)
	}
	if r.host.OS == "darwin" || r.host.OS == "linux" {
		return "~/.config/algodtop/agent.json"
	}
	return "~/.config/algodtop/agent.json"
}

// agentConfig renders the agent config: its local nodes plus the hub's
// alert rules and channels.
func (r *remoteCmd) agentConfig(nodes []algod.Found) ([]byte, error) {
	ac := config.Config{Alerts: r.cfg.Alerts, Channels: r.cfg.Channels, Heartbeat: r.cfg.Heartbeat}
	for _, f := range nodes {
		ac.Nodes = append(ac.Nodes, config.Node{Name: f.Name, DataDir: f.DataDir})
	}
	empty := ""
	ac.Web.Listen = &empty
	return ac.Marshal()
}

func (r *remoteCmd) install(ctx context.Context) int {
	h, err := r.ssh.Detect(ctx)
	if err != nil {
		return errf("%v\nrun `ssh %s` once to accept the host key and check that login needs no password", err, r.ssh.Target.Dest())
	}
	r.host = h
	path := r.agentPath()
	fmt.Printf("remote %s: %s/%s, user %s\n", r.hostArg, h.OS, h.Arch, h.User)
	bin, desc, err := r.binary(ctx)
	if err != nil {
		return errf("%v", err)
	}
	service := "a user-level service"
	if h.OS == "windows" {
		service = "a Windows service, as an administrator"
	}
	fmt.Printf("plan:\n  1. copy %s (%s) to %s\n  2. run discovery on the remote and show the agent config\n  3. install the agent as %s\n  4. add remote %q to %s\n", desc, sizeStr(len(bin)), path, service, r.name, r.cfg.Path)
	if !r.confirm("copy the binary and run discovery?") {
		return 1
	}
	if err := r.ssh.Copy(ctx, h, path, bin); err != nil {
		return errf("copy: %v", err)
	}
	found, err := r.ssh.Discover(ctx, path)
	if err != nil {
		return errf("discovery: %v", err)
	}
	if h.OS == "windows" {
		return r.installWindows(ctx, path, found)
	}
	var dirs []string
	for _, f := range found {
		dirs = append(dirs, f.DataDir)
	}
	owners := r.ssh.Owners(ctx, h, dirs)
	var foreign []string
	for d, o := range owners {
		if o != "?" && o != h.User {
			foreign = append(foreign, fmt.Sprintf("%s is owned by %s", d, o))
		}
	}
	acfg, err := r.agentConfig(found)
	if err != nil {
		return errf("%v", err)
	}
	sp := remote.PlanService(h, path)
	fmt.Printf("\nagent config %s:\n%s\n", r.agentConfigPath(), indent(string(acfg)))
	if sp.File != "" {
		fmt.Printf("service %s %s:\n%s\n  start: %s\n", sp.Kind, sp.File, indent(string(sp.Content)), sp.Start)
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		fmt.Println("\nThe agent must run as the account that owns the algod data directory:")
		for _, f := range foreign {
			fmt.Println("  " + f)
		}
		if h.OS == "linux" {
			// A user service would run as the wrong account. A system service
			// runs as the owner and lets the algodtop group reach its socket.
			// Installing it needs root once.
			fmt.Printf("\nA system service runs the agent as that account. Installing it needs root once:")
			method, rc := r.runAsRoot(ctx, path, "service", "install", "--system", "--agent")
			if rc != 0 {
				fmt.Printf("Then record it on the hub: %s remote add %s --name %s\n", prog, r.hostArg, r.name)
				return rc
			}
			r.root = method
			return r.add(ctx)
		}
		fmt.Println("Installing a service for that account needs root. Run these on the host:")
		for _, f := range foreign {
			owner := f[strings.LastIndexByte(f, ' ')+1:]
			fmt.Printf("  sudo -u %s %s agent --discover\n", owner, path)
		}
	}
	if !r.confirm("write the agent config and install the service?") {
		return 1
	}
	if err := r.ssh.WriteFile(ctx, h, r.agentConfigPath(), acfg); err != nil {
		return errf("write agent config: %v", err)
	}
	if sp.File != "" {
		if err := r.ssh.WriteFile(ctx, h, sp.File, sp.Content); err != nil {
			return errf("write service file: %v", err)
		}
		if out, err := r.ssh.Shell(ctx, sp.Start); err != nil {
			fmt.Printf("warn: starting the service failed: %v %s\n", err, strings.TrimSpace(string(out)))
			fmt.Println("     the hub still works: `stdio --agent` runs an agent for the life of each connection, with gaps while the hub is offline")
		}
	}
	for _, hint := range sp.Hints {
		fmt.Println("note: " + hint)
	}
	r.saveRemote(path)
	fmt.Printf("installed; run `%s reload` on the hub to start pulling from %s\n", prog, r.name)
	return 0
}

func (r *remoteCmd) saveRemote(path string) {
	found := false
	for i := range r.cfg.Remotes {
		if r.cfg.Remotes[i].Name == r.name {
			r.cfg.Remotes[i].Host, r.cfg.Remotes[i].AgentPath = r.hostArg, path
			found = true
		}
	}
	if !found {
		r.cfg.Remotes = append(r.cfg.Remotes, config.Remote{Name: r.name, Host: r.hostArg, AgentPath: path})
	}
	if err := r.cfg.Save(r.cfg.Path); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not update %s: %v\n", r.cfg.Path, err)
	}
}

// sync pushes the hub's alert rules and channels into the agent config.
// It runs over SSH as an operator command, never through the web UI.
func (r *remoteCmd) sync(ctx context.Context) int {
	if r.systemAgent() {
		return r.systemSync()
	}
	h, err := r.ssh.Detect(ctx)
	if err != nil {
		return errf("%v", err)
	}
	r.host = h
	out, err := r.ssh.Run(ctx, "cat "+remoteQuote(r.agentConfigPath()), nil)
	if err != nil {
		return errf("read agent config: %v", err)
	}
	var ac config.Config
	if err := json.Unmarshal(out, &ac); err != nil {
		return errf("agent config: %v", err)
	}
	ac.Alerts, ac.Channels, ac.Heartbeat = r.cfg.Alerts, r.cfg.Channels, r.cfg.Heartbeat
	b, err := ac.Marshal()
	if err != nil {
		return errf("%v", err)
	}
	if !r.confirm(fmt.Sprintf("write %d alert rules and %d channels to %s on %s?", len(ac.Alerts), len(ac.Channels), r.agentConfigPath(), r.hostArg)) {
		return 1
	}
	if err := r.ssh.WriteFile(ctx, h, r.agentConfigPath(), b); err != nil {
		return errf("%v", err)
	}
	if _, err := r.ssh.Run(ctx, remote.RemoteBinary(r.agentPath())+" reload --agent", nil); err != nil {
		fmt.Printf("note: the agent did not reload (%v); it picks up the config at its next start\n", err)
	}
	fmt.Println("synced")
	return 0
}

func (r *remoteCmd) remove(ctx context.Context) int {
	h, err := r.ssh.Detect(ctx)
	if err != nil {
		return errf("%v", err)
	}
	r.host = h
	path := r.agentPath()
	system := r.systemAgent()
	if system {
		if !r.confirm(fmt.Sprintf("drop %q from the hub config? The system service on %s keeps running until you remove it there.", r.name, r.hostArg)) {
			return 1
		}
		if h.OS == "windows" {
			fmt.Printf("To stop the agent on %s, as an administrator: ssh %s %s service uninstall --agent\n", r.hostArg, shellJoin(r.hostArg), remote.RemoteBinary(path))
		} else {
			fmt.Printf("To stop the agent on %s: %s\n", r.hostArg, sshT(r.hostArg, rootCommand(r.knownRoot(), path, "service", "uninstall", "--system", "--agent")))
		}
	} else {
		sp := remote.PlanService(h, path)
		if !r.confirm(fmt.Sprintf("stop the agent service on %s, remove %s and the service file, and drop %q from the hub config? (the agent's buffer is kept)", r.hostArg, path, r.name)) {
			return 1
		}
		if sp.Stop != "" {
			r.ssh.Shell(ctx, sp.Stop+"; rm -f "+sp.File)
		}
		r.ssh.Shell(ctx, "rm -f "+strings.Replace(path, "~", "$HOME", 1))
	}
	keep := r.cfg.Remotes[:0]
	for _, x := range r.cfg.Remotes {
		if x.Name != r.name {
			keep = append(keep, x)
		}
	}
	r.cfg.Remotes = keep
	if err := r.cfg.Save(r.cfg.Path); err != nil {
		return errf("%v", err)
	}
	fmt.Println("removed")
	return 0
}

func remoteQuote(p string) string { return remote.RemoteBinary(p) }

// installWindows installs the agent on a Windows host as a Windows
// service, which takes an administrator. Windows OpenSSH runs an
// administrator's session elevated, so the hub runs the install over SSH
// once confirmed, then records the agent.
func (r *remoteCmd) installWindows(ctx context.Context, path string, found []algod.Found) int {
	fmt.Println("\nnodes found:")
	for _, f := range found {
		fmt.Printf("  %-12s %-10s %s\n", f.Name, f.Network, f.DataDir)
	}
	cmd := remote.RemoteBinary(path) + " service install --agent"
	fmt.Printf("\nOn Windows the agent runs as a service, as LocalSystem, which an administrator installs:\n\n  ssh %s %s\n\n", shellJoin(r.hostArg), cmd)
	if !r.confirm("run it now?") {
		fmt.Printf("Run it as an administrator on the host, then record it on the hub: %s remote add %s --name %s --path %s\n", prog, r.hostArg, r.name, shellJoin(path))
		return 1
	}
	out, err := r.ssh.Run(ctx, cmd, nil)
	fmt.Print(indent(strings.TrimSpace(string(out))))
	if err != nil {
		return errf("%v\nThe SSH account must be an administrator: Windows OpenSSH runs an administrator's session elevated. Or run the command on the host from an elevated prompt, then %s remote add %s --path %s", err, prog, r.hostArg, shellJoin(path))
	}
	r.path = path
	return r.add(ctx)
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}

func sizeStr(n int) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) }

var _ = filepath.Join
var _ = time.Second
