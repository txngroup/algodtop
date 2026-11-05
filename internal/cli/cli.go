// Package cli is the entry point shared by both main packages: algodtop
// and algod.top run the same code.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"algod.top/internal/algod"
	"algod.top/internal/api"
	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/daemon"
	"algod.top/internal/fsx"
	"algod.top/internal/model"
	"algod.top/internal/release"
	"algod.top/internal/store"
	"algod.top/internal/sysstat"
	"algod.top/internal/tui"
	"algod.top/internal/version"
	"algod.top/internal/web"
)

var prog = "algod.top"

// Main runs algodtop and exits.
func Main() { os.Exit(Run(os.Args)) }

// commands maps subcommands to handlers.
var commands = map[string]func([]string) int{}

func init() {
	commands = map[string]func([]string) int{
		"daemon":  cmdDaemon,
		"agent":   cmdAgent,
		"export":  cmdExport,
		"silence": cmdSilence,
		"stdio":   cmdStdio,
		"remote":  cmdRemote,
		"service": cmdService,
		"reload":  cmdReload,
		"passwd":  cmdPasswd,
		"check":   cmdCheck,
		"upgrade": cmdUpgrade,
		"version": cmdVersion,
		"help":    cmdHelp,
	}
}

// Run runs a command line and returns the exit code. Help and error text
// print the name the binary was run under.
func Run(args []string) int {
	if len(args) > 0 {
		prog = strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	}
	if len(args) > 1 {
		if f, ok := commands[args[1]]; ok {
			return f(args[2:])
		}
	}
	return cmdTUI(args[1:])
}

func errf(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, prog+": "+format+"\n", a...)
	return 1
}

// cmdVersion prints when the build's commit was made and how old it is,
// then the Go version string and platform. algodtop check shows a
// remote's first line. With --json it prints the fields an upgrade reads
// from a new binary before it replaces the running one.
func cmdVersion(args []string) int {
	b := version.Info()
	if len(args) > 0 && args[0] == "--json" {
		json.NewEncoder(os.Stdout).Encode(release.BuildInfo{Version: b.Version, VersionTime: b.UnixNano(), Revision: b.Revision,
			Platform: runtime.GOOS + "/" + runtime.GOARCH})
		return 0
	}
	if !b.Time.IsZero() {
		fmt.Printf("%s %s\n", prog, b.Describe(time.Now()))
	}
	fmt.Printf("%s %s %s/%s %s\n", prog, b.String(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	return 0
}

func cmdHelp([]string) int {
	fmt.Printf(`%s monitors algod nodes: a terminal UI, a daemon that keeps history, and a web UI.

Usage:
  %s [flags]               terminal UI (top-style)
  %s daemon [flags]        collector, store, health, alerts, API and web UI
  %s agent [flags]         collector and local buffer for a remote host
  %s export ...            export series, events or sessions
  %s silence ...           add, list or remove alert silences
  %s stdio [--agent]       connect stdin and stdout to the local daemon or agent socket
  %s remote install|add|upgrade|sync|remove HOST
  %s upgrade               install the newest release here and on every remote agent
  %s service [install|uninstall|status]  start the daemon at login or boot
  %s reload                ask the daemon to reload its config
  %s passwd [--read-only|--token]
  %s check                 validate config, reach each node, project disk use
  %s version

Run '%s COMMAND --help' for a command's flags.
`, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog, prog)
	return 0
}

// loadConfig reads --config, $ALGODTOP_CONFIG or the platform default. ok
// is false when no file exists.
func loadConfig(path string) (*config.Config, bool, error) {
	explicit := path != ""
	if path == "" {
		path = config.DefaultPath()
	}
	path = algod.ExpandHome(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if _, err := os.Stat(path); err != nil {
		if explicit {
			return nil, false, err
		}
		return &config.Config{Path: path}, false, nil
	}
	c, err := config.Load(path)
	if err != nil {
		return nil, false, err
	}
	if w := config.CheckPermissions(path); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	return c, true, nil
}

// parseNamed splits [NAME=]VALUE.
func parseNamed(s string) (name, val string) {
	if i := strings.Index(s, "="); i > 0 && !strings.Contains(s[:i], "/") && !strings.Contains(s[:i], ":") {
		return s[:i], s[i+1:]
	}
	return "", s
}

// nodesFromFlags builds config nodes from -d and -u.
func nodesFromFlags(dirs, urls []string, tokenFile, adminFile string) ([]config.Node, error) {
	var out []config.Node
	seen := map[string]int{}
	uniq := func(n string) string {
		seen[n]++
		if seen[n] > 1 {
			return fmt.Sprintf("%s-%d", n, seen[n])
		}
		return n
	}
	for _, d := range dirs {
		name, dir := parseNamed(d)
		dir = algod.ExpandHome(dir)
		abs, err := filepath.Abs(dir)
		if err == nil {
			dir = abs
		}
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("-d %s: %w", d, err)
		}
		if name == "" {
			name = algod.NodeName(dir)
		}
		out = append(out, config.Node{Name: uniq(name), DataDir: dir})
	}
	for _, u := range urls {
		name, raw := parseNamed(u)
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		pu, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("-u %s: %w", u, err)
		}
		if name == "" {
			name = strings.ReplaceAll(pu.Hostname(), ".", "-")
			if pu.Scheme == "ssh" {
				name += "-" + algod.NodeName(strings.TrimPrefix(pu.Path, "/"))
			}
		}
		n := config.Node{Name: uniq(name), URL: raw}
		if pu.Scheme != "ssh" {
			n.TokenFile, n.AdminTokenFile = tokenFile, adminFile
		}
		out = append(out, n)
	}
	return out, nil
}

func liveFromNodes(nodes []config.Node, desc string, interval time.Duration) (*tui.LiveSource, error) {
	var names []string
	var srcs []collector.Source
	ctl := filepath.Join(os.TempDir(), fmt.Sprintf("algodtop-ssh-%d", os.Getuid()))
	for _, n := range nodes {
		s, err := daemon.SourceFor(n, ctl)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Name, err)
		}
		names = append(names, n.Name)
		srcs = append(srcs, s)
	}
	return tui.NewLiveSource(names, srcs, desc, interval), nil
}

// cmdTUI follows the startup order: a running daemon, a config file,
// explicit nodes, discovery, and finally a help screen.
func cmdTUI(args []string) int {
	fs := newFlags(prog)
	cfgF := fs.str("c", "config", "PATH", "Config file")
	dF := fs.list("d", "datadir", "[NAME=]DIR", "A local algod data directory. Repeatable.")
	uF := fs.list("u", "url", "[NAME=]URL", "An algod REST URL or ssh://host/<datadir>. Repeatable.")
	tokF := fs.str("", "token-file", "PATH", "Token for -u REST nodes")
	admF := fs.str("", "admin-token-file", "PATH", "Admin token for -u REST nodes")
	sockF := fs.str("s", "socket", "PATH", "Daemon socket to connect to")
	hubF := fs.str("H", "hub", "URL", "A remote daemon: ssh://host or https://host:port")
	hubTok := fs.str("", "hub-token-file", "PATH", "Read-only token for an https hub (or $ALGODTOP_TOKEN)")
	liveF := fs.flag("", "live", "Ignore any daemon and poll algod directly")
	noDisc := fs.flag("", "no-discover", "Skip discovery of local nodes")
	batchF := fs.flag("b", "batch", "Print the node table and exit")
	nF := fs.str("n", "iterations", "N", "Batch mode iterations")
	iF := fs.str("i", "interval", "SECS", "Refresh interval")
	jsonF := fs.flag("", "json", "JSON output in batch mode")
	utcF := fs.flag("", "utc", "Show times in UTC")
	helpF := fs.flag("h", "help", "Show help")
	verF := fs.flag("V", "version", "Print the version")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if helpF.Bool() {
		cmdHelp(nil)
		fmt.Println("\nTerminal UI flags:")
		fmt.Print(fs.usage())
		fmt.Println("\nKeys:")
		for _, k := range tui.HelpKeys {
			fmt.Printf("  %-24s %s\n", k[0], k[1])
		}
		return 0
	}
	if verF.Bool() {
		return cmdVersion(nil)
	}
	if len(fs.args) > 0 {
		return errf("unknown command %q (see %s help)", fs.args[0], prog)
	}
	interval, err := iF.Duration(2 * time.Second)
	if err != nil {
		return errf("-i: %v", err)
	}
	iters, err := nF.Int(1)
	if err != nil {
		return errf("-n: %v", err)
	}
	src, empty, writeCfg, err := pickSource(cfgF.String(), dF.vals, uF.vals, tokF.String(), admF.String(), sockF.String(), hubF.String(), hubTok.String(),
		liveF.Bool(), noDisc.Bool(), interval)
	if err != nil {
		return errf("%v", err)
	}
	if src != nil {
		defer src.Close()
	}
	if batchF.Bool() || !term.IsTerminal(int(os.Stdout.Fd())) {
		if empty {
			for _, l := range tui.EmptyHelp[:len(tui.EmptyHelp)-1] {
				fmt.Fprintln(os.Stderr, l)
			}
			return 1
		}
		if err := tui.Batch(os.Stdout, tui.BatchOptions{Source: src, Iterations: iters, Interval: interval, JSON: jsonF.Bool(), UTC: utcF.Bool(),
			Warmup: 3 * time.Second}); err != nil {
			return errf("%v", err)
		}
		return 0
	}
	if src == nil {
		src = tui.NewLiveSource(nil, nil, "none", interval)
	}
	if err := tui.Run(tui.Options{Source: src, Style: tui.DetectStyle(), UTC: utcF.Bool(), Interval: interval, WriteConfig: writeCfg, Empty: empty}); err != nil {
		return errf("%v", err)
	}
	return 0
}

func pickSource(cfgPath string, dirs, urls []string, tokFile, admFile, sock, hub, hubTok string, live, noDisc bool,
	interval time.Duration) (tui.Source, bool, func() ([]string, func() (string, error)), error) {
	// 1. Daemon.
	if hub != "" {
		c, err := tui.DialHub(hub, hubTok)
		if err != nil {
			return nil, false, nil, fmt.Errorf("hub %s: %w", hub, err)
		}
		return c, false, nil, nil
	}
	if sock != "" {
		c, err := tui.DialSocket(sock)
		if err != nil {
			return nil, false, nil, fmt.Errorf("socket %s: %w", sock, err)
		}
		return c, false, nil, nil
	}
	explicitNodes := len(dirs) > 0 || len(urls) > 0
	if !live && !explicitNodes {
		cfg, ok, _ := loadConfig(cfgPath)
		for _, p := range daemonSockets(cfg, ok) {
			c, err := tui.DialSocket(p)
			if err == nil {
				return c, false, nil, nil
			}
			if h := systemSocketHint(p, err); h != "" {
				return nil, false, nil, fmt.Errorf("%s, or pass --live to poll the nodes yourself", h)
			}
		}
	}
	// 2. Config file.
	if !explicitNodes {
		cfg, ok, err := loadConfig(cfgPath)
		if err != nil {
			return nil, false, nil, err
		}
		if ok && len(cfg.Nodes) > 0 {
			s, err := liveFromNodes(cfg.Nodes, cfg.Path, interval)
			return s, false, nil, err
		}
	}
	// 3. Explicit nodes.
	if explicitNodes {
		nodes, err := nodesFromFlags(dirs, urls, tokFile, admFile)
		if err != nil {
			return nil, false, nil, err
		}
		s, err := liveFromNodes(nodes, "flags", interval)
		return s, false, writeConfigFor(nodes, cfgPath), err
	}
	// 4. Discovery.
	if !noDisc {
		found := algod.Discover(sysstat.ListAlgod, sysstat.Alive)
		if len(found) > 0 {
			var nodes []config.Node
			for _, f := range found {
				nodes = append(nodes, config.Node{Name: f.Name, DataDir: f.DataDir})
			}
			s, err := liveFromNodes(nodes, "discovered", interval)
			return s, false, writeConfigFor(nodes, cfgPath), err
		}
	}
	// 5. Nothing found.
	return nil, true, nil, nil
}

// writeConfigFor implements the W key: show the path and contents, then
// write on confirmation.
func writeConfigFor(nodes []config.Node, cfgPath string) func() ([]string, func() (string, error)) {
	return func() ([]string, func() (string, error)) {
		path := cfgPath
		if path == "" {
			path = config.DefaultPath()
		}
		cfg, ok, err := loadConfig(path)
		if err != nil || !ok {
			cfg = &config.Config{}
		}
		have := map[string]bool{}
		for _, n := range cfg.Nodes {
			have[n.Name] = true
		}
		for _, n := range nodes {
			if !have[n.Name] {
				cfg.Nodes = append(cfg.Nodes, n)
			}
		}
		b, _ := cfg.Marshal()
		text := []string{"path: " + path, ""}
		text = append(text, strings.Split(strings.TrimRight(string(b), "\n"), "\n")...)
		return text, func() (string, error) { return path, cfg.Save(path) }
	}
}

// signalContext is cancelled on SIGINT or SIGTERM (Ctrl+C on Windows).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdDaemon(args []string) int { return runDaemon(args, false) }
func cmdAgent(args []string) int  { return runDaemon(args, true) }

func agentConfigPath() string {
	return filepath.Join(filepath.Dir(config.DefaultPath()), "agent.json")
}

func runDaemon(args []string, agent bool) int {
	name := prog + " daemon"
	if agent {
		name = prog + " agent"
	}
	fs := newFlags(name)
	cfgF := fs.str("c", "config", "PATH", "Config file")
	sockF := fs.str("s", "socket", "PATH", "Socket to create")
	sockGroupF := fs.str("", "socket-group", "GROUP", "Let this group use the socket (mode 0660)")
	listenF := fs.str("l", "listen", "ADDR", "Web UI listener; an empty value disables it")
	storeF := fs.str("", "store-dir", "DIR", "algod.top's own data directory")
	dF := fs.list("d", "datadir", "[NAME=]DIR", "A local algod data directory. Repeatable.")
	uF := fs.list("u", "url", "[NAME=]URL", "An algod REST URL or ssh://host/<datadir>. Repeatable.")
	tokF := fs.str("", "token-file", "PATH", "Token for -u REST nodes")
	admF := fs.str("", "admin-token-file", "PATH", "Admin token for -u REST nodes")
	discF := fs.flag("", "discover", "Find local nodes, print them and write them to the config")
	svcF := fs.flag("", "install-service", "Install a service; the same as `service install`")
	helpF := fs.flag("h", "help", "Show help")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if helpF.Bool() {
		fmt.Printf("Usage: %s [flags]\n\n%s", name, fs.usage())
		return 0
	}
	if svcF.Bool() {
		return installService(agent)
	}
	path := cfgF.String()
	if path == "" && agent {
		path = agentConfigPath()
		if os.Getenv("ALGODTOP_AGENT_CONFIG") != "" {
			path = os.Getenv("ALGODTOP_AGENT_CONFIG")
		}
		if _, err := os.Stat(path); err != nil {
			path = ""
		}
	}
	cfg, ok, err := loadConfig(path)
	if err != nil {
		return errf("%v", err)
	}
	if agent && !ok && path == "" {
		cfg.Path = agentConfigPath()
	}
	flagNodes, err := nodesFromFlags(dF.vals, uF.vals, tokF.String(), admF.String())
	if err != nil {
		return errf("%v", err)
	}
	cfg.Nodes = mergeNodes(cfg.Nodes, flagNodes)
	if discF.Bool() || (agent && !ok && len(cfg.Nodes) == 0) {
		found := algod.Discover(sysstat.ListAlgod, sysstat.Alive)
		var add []config.Node
		for _, f := range found {
			state := "stopped"
			if f.Running {
				state = fmt.Sprintf("running, pid %d", f.PID)
			}
			fmt.Fprintf(os.Stderr, "found %-10s %-40s %s\n", f.Name, f.DataDir, state)
			add = append(add, config.Node{Name: f.Name, DataDir: f.DataDir})
		}
		cfg.Nodes = mergeNodes(cfg.Nodes, add)
		if discF.Bool() {
			if err := cfg.Validate(); err != nil {
				return errf("%v", err)
			}
			if err := cfg.Save(cfg.Path); err != nil {
				return errf("write %s: %v", cfg.Path, err)
			}
			fmt.Fprintf(os.Stderr, "wrote %d nodes to %s\n", len(cfg.Nodes), cfg.Path)
			ok = true
		}
	}
	if sockF.set {
		cfg.Socket = sockF.String()
	}
	if sockGroupF.set {
		cfg.SocketGroup = sockGroupF.String()
	}
	if listenF.set {
		v := listenF.String()
		if agent {
			cfg.Agent.Listen = v
		} else {
			cfg.Web.Listen = &v
		}
	}
	if storeF.set {
		cfg.StoreDir = storeF.String()
	}
	if err := cfg.Validate(); err != nil {
		return errf("%v", err)
	}
	if !ok {
		if len(flagNodes) == 0 && len(cfg.Nodes) == 0 && len(cfg.Remotes) == 0 {
			return errf("no config file at %s and no nodes; run `%s daemon --discover` or pass -d DIR", cfg.Path, prog)
		}
		if len(flagNodes) > 0 {
			cfg.Path = "" // nodes from flags only; reload needs a config file
		}
	}
	logger := log.New(os.Stderr, prog+": ", log.LstdFlags)
	o := daemon.Options{ConfigPath: cfg.Path, Config: cfg, Agent: agent, Logger: logger}
	if !agent {
		o.Static = web.FS
	}
	d, err := daemon.New(o)
	if err != nil {
		if errors.Is(err, store.ErrInUse) {
			return errf("another %s daemon is using %s", prog, cfg.StorePath())
		}
		return errf("%v", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := d.Run(ctx); err != nil {
		return errf("%v", err)
	}
	if exe := d.RestartExe(); exe != "" {
		logger.Printf("upgraded; restarting from %s", exe)
		if err := execSelf(exe); err != nil {
			// A service manager restarts a failed service, from the new binary.
			return errf("restart after the upgrade: %v", err)
		}
	}
	return 0
}

func mergeNodes(a, b []config.Node) []config.Node {
	have := map[string]bool{}
	dirs := map[string]bool{}
	for _, n := range a {
		have[n.Name] = true
		if n.DataDir != "" {
			dirs[filepath.Clean(algod.ExpandHome(n.DataDir))] = true
		}
	}
	out := append([]config.Node(nil), a...)
	for _, n := range b {
		if have[n.Name] || (n.DataDir != "" && dirs[filepath.Clean(n.DataDir)]) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// daemonClient connects to the running daemon for CLI commands.
func daemonClient(sock string) (*tui.Client, error) {
	if sock != "" {
		return tui.DialSocket(sock)
	}
	cfg, ok, _ := loadConfig("")
	var last error
	for _, p := range daemonSockets(cfg, ok) {
		c, err := tui.DialSocket(p)
		if err == nil {
			return c, nil
		}
		if h := systemSocketHint(p, err); h != "" {
			return nil, errors.New(h)
		}
		if last == nil || !errors.Is(err, fs.ErrNotExist) {
			last = err
		}
	}
	return nil, fmt.Errorf("no running daemon (%v); start one with `%s daemon`", last, prog)
}

func cmdReload(args []string) int {
	fs := newFlags(prog + " reload")
	sockF := fs.str("s", "socket", "PATH", "Daemon socket")
	agentF := fs.flag("", "agent", "Reload the agent instead of the daemon")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	sock := sockF.String()
	if sock == "" && agentF.Bool() {
		cfg, _, _ := loadConfig(agentConfigPath())
		if cfg == nil {
			cfg = &config.Config{}
		}
		sock = filepath.Join(filepath.Dir(cfg.SocketPath()), "agent.sock")
	}
	c, err := daemonClient(sock)
	if err != nil {
		return errf("%v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Post(ctx, "/api/admin/reload", map[string]any{}, nil); err != nil {
		return errf("reload: %v", err)
	}
	fmt.Println("config reloaded")
	return 0
}

func cmdSilence(args []string) int {
	fs := newFlags(prog + " silence")
	nodeF := fs.list("", "node", "NAME", "Node to silence. Repeatable; none means every node.")
	ruleF := fs.list("", "rule", "NAME", "Alert rule to silence. Repeatable; none means every rule.")
	forF := fs.str("", "for", "DURATION", "How long, for example 10m")
	sockF := fs.str("s", "socket", "PATH", "Daemon socket")
	helpF := fs.flag("h", "help", "Show help")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if helpF.Bool() {
		fmt.Printf("Usage:\n  %s silence --node NAME --for 10m \"comment\"\n  %s silence list\n  %s silence rm ID\n\n%s", prog, prog, prog, fs.usage())
		return 0
	}
	c, err := daemonClient(sockF.String())
	if err != nil {
		return errf("%v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if len(fs.args) > 0 && (fs.args[0] == "list" || fs.args[0] == "ls") {
		var out struct {
			Silences []model.Silence `json:"silences"`
		}
		if err := c.Get(ctx, "/api/silences", nil, &out); err != nil {
			return errf("%v", err)
		}
		now := time.Now().UnixNano()
		for _, s := range out.Silences {
			state := "active"
			if now >= s.End {
				state = "ended"
			} else if now < s.Start {
				state = "pending"
			}
			nodes := strings.Join(s.Nodes, ",")
			if nodes == "" {
				nodes = "all nodes"
			}
			fmt.Printf("%s  %-8s %s -> %s  %s  %s\n", s.ID, state, time.Unix(0, s.Start).Format("2006-01-02 15:04"), time.Unix(0, s.End).Format("15:04"), nodes, s.Comment)
		}
		return 0
	}
	if len(fs.args) > 1 && (fs.args[0] == "rm" || fs.args[0] == "remove") {
		if err := c.Delete(ctx, "/api/silences", url.Values{"id": {fs.args[1]}}); err != nil {
			return errf("%v", err)
		}
		fmt.Println("silence ended")
		return 0
	}
	if forF.String() == "" {
		return errf("silence needs --for, for example --for 10m")
	}
	var out model.Silence
	body := map[string]any{"nodes": nodeF.vals, "rules": ruleF.vals, "for": forF.String(), "comment": strings.Join(fs.args, " ")}
	if err := c.Post(ctx, "/api/silences", body, &out); err != nil {
		return errf("%v", err)
	}
	fmt.Printf("silence %s until %s\n", out.ID, time.Unix(0, out.End).Format("2006-01-02 15:04:05"))
	return 0
}

func cmdPasswd(args []string) int {
	fs := newFlags(prog + " passwd")
	cfgF := fs.str("c", "config", "PATH", "Config file")
	roF := fs.flag("", "read-only", "Set the read-only password instead")
	tokF := fs.flag("", "token", "Create a read-only bearer token and print it once")
	agentTokF := fs.flag("", "agent-token", "Create the token a hub uses to pull from a TLS agent")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	path := cfgF.String()
	if path == "" && agentTokF.Bool() {
		path = agentConfigPath()
	}
	cfg, _, err := loadConfig(path)
	if err != nil {
		return errf("%v", err)
	}
	if agentTokF.Bool() {
		tok := api.NewToken()
		cfg.Agent.TokenHash = api.HashToken(tok)
		if err := cfg.Save(cfg.Path); err != nil {
			return errf("%v", err)
		}
		fmt.Printf("agent token (shown once): %s\nPut it in a file on the hub and set token_file on the remote entry.\n", tok)
		return 0
	}
	if tokF.Bool() {
		tok := api.NewToken()
		cfg.Web.ReadOnlyTokenHashes = append(cfg.Web.ReadOnlyTokenHashes, api.HashToken(tok))
		if err := cfg.Save(cfg.Path); err != nil {
			return errf("%v", err)
		}
		fmt.Printf("read-only token (shown once): %s\nUse it with: ALGODTOP_TOKEN=... %s -H https://host:8090\n", tok, prog)
		return 0
	}
	pw, err := readPassword("New password: ")
	if err != nil {
		return errf("%v", err)
	}
	pw2, err := readPassword("Repeat: ")
	if err != nil {
		return errf("%v", err)
	}
	if pw != pw2 {
		return errf("passwords do not match")
	}
	if len(pw) < 8 {
		return errf("use at least 8 characters")
	}
	h, err := api.HashPassword(pw)
	if err != nil {
		return errf("%v", err)
	}
	if roF.Bool() {
		cfg.Web.ReadOnlyPasswordHash = h
	} else {
		cfg.Web.PasswordHash = h
	}
	if err := cfg.Save(cfg.Path); err != nil {
		return errf("%v", err)
	}
	fmt.Printf("password hash written to %s; run `%s reload`\n", cfg.Path, prog)
	return 0
}

func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	var s string
	_, err := fmt.Fscanln(os.Stdin, &s)
	return s, err
}

// cmdStdio connects stdin and stdout to the local daemon or agent socket,
// for use over SSH. With --agent it starts an agent for the life of the
// connection if none is running.
func cmdStdio(args []string) int {
	fs := newFlags(prog + " stdio")
	agentF := fs.flag("", "agent", "Use the agent socket, starting an agent if none is running")
	noStartF := fs.flag("", "no-start", "With --agent, fail rather than start an agent")
	sockF := fs.str("s", "socket", "PATH", "Socket")
	cfgF := fs.str("c", "config", "PATH", "Config file")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	path := sockF.String()
	var cfg *config.Config
	var also []string // a system service's socket
	if path == "" {
		cp := cfgF.String()
		if agentF.Bool() && cp == "" {
			if _, err := os.Stat(agentConfigPath()); err == nil {
				cp = agentConfigPath()
			}
		}
		c, _, err := loadConfig(cp)
		if err != nil {
			return errf("%v", err)
		}
		cfg = c
		path = cfg.SocketPath()
		if agentF.Bool() {
			path = filepath.Join(filepath.Dir(path), "agent.sock")
		}
		if runtime.GOOS == "linux" {
			also = []string{systemSocket()}
			if agentF.Bool() {
				also = []string{systemAgentSocket()}
			}
		}
	}
	// An agent or daemon service installed on this machine owns its
	// socket. stdio tries it first and never starts a second agent beside
	// it, which would hold the service's socket or store while the
	// service restarts, as it does after an upgrade.
	sysInstalled, userInstalled := false, false
	if sockF.String() == "" {
		sysInstalled, userInstalled = servicesInstalled(agentF.Bool())
	}
	tries := socketOrder(path, also, sysInstalled)
	conn, used, err := dialFirst(tries)
	if h := hintFor(err); h != "" {
		return errf("%s", h)
	}
	installed := sysInstalled || userInstalled
	if err != nil && installed {
		role := map[bool]string{true: "agent", false: "daemon"}[agentF.Bool()]
		fmt.Fprintf(os.Stderr, "%s: the %s service is installed but not answering yet; waiting up to 2 minutes\n", prog, role)
		deadline := time.Now().Add(2 * time.Minute)
		for err != nil && time.Now().Before(deadline) {
			time.Sleep(time.Second)
			conn, used, err = dialFirst(tries)
		}
	}
	if err == nil {
		path = used
	}
	var stop func()
	if err != nil && agentF.Bool() && !noStartF.Bool() && !installed && cfg != nil {
		// No agent service: run one in this process for the connection.
		logger := log.New(os.Stderr, prog+" agent: ", log.LstdFlags)
		if len(cfg.Nodes) == 0 {
			for _, f := range algod.Discover(sysstat.ListAlgod, sysstat.Alive) {
				cfg.Nodes = append(cfg.Nodes, config.Node{Name: f.Name, DataDir: f.DataDir})
			}
		}
		d, derr := daemon.New(daemon.Options{Config: cfg, ConfigPath: cfg.Path, Agent: true, Logger: logger})
		if derr != nil {
			return errf("start agent: %v", derr)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { d.Run(ctx); close(done) }()
		stop = func() { cancel(); <-done }
		for range 100 {
			time.Sleep(50 * time.Millisecond)
			if conn, err = net.Dial("unix", fsx.SocketAddr(path)); err == nil {
				break
			}
		}
	}
	if err != nil {
		if stop != nil {
			stop()
		}
		return errf("connect %s: %v", path, err)
	}
	relay(conn, os.Stdin, os.Stdout)
	conn.Close()
	if stop != nil {
		stop()
	}
	return 0
}

// relay copies bytes both ways until either side closes.
func relay(conn net.Conn, in io.Reader, out io.Writer) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, in); done <- struct{}{} }()
	go func() { io.Copy(out, conn); done <- struct{}{} }()
	<-done
}

func cmdExport(args []string) int {
	fs := newFlags(prog + " export")
	nodeF := fs.str("", "node", "NAME", "Node")
	metricF := fs.str("", "metric", "NAME", "Metric")
	matchF := fs.list("", "match", "LABEL:VALUE", "Label filter. Repeatable.")
	fromF := fs.str("", "from", "TIME", "Start: RFC3339, unix seconds, or -24h")
	toF := fs.str("", "to", "TIME", "End, default now")
	tierF := fs.str("", "tier", "TIER", "raw for full resolution; default picks by points")
	fnF := fs.str("", "fn", "FN", "rate, increase, avg, min, max, last, p50, p90, p99")
	pointsF := fs.str("", "points", "N", "Buckets, default 1000")
	formatF := fs.str("", "format", "csv|json", "Output format, default csv")
	evF := fs.flag("", "events", "Export events instead of a series")
	sessF := fs.flag("", "sessions", "Export peer sessions instead of a series")
	sockF := fs.str("s", "socket", "PATH", "Daemon socket")
	helpF := fs.flag("h", "help", "Show help")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if helpF.Bool() || (!evF.Bool() && !sessF.Bool() && (nodeF.String() == "" || metricF.String() == "")) {
		fmt.Printf("Usage: %s export --node X --metric Y [--from -24h] [--to now] [--tier raw]\n       %s export --events|--sessions [--node X]\n\n%s", prog, prog, fs.usage())
		if helpF.Bool() {
			return 0
		}
		return 2
	}
	c, err := daemonClient(sockF.String())
	if err != nil {
		return errf("%v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("node", nodeF.String())
	set("from", fromF.String())
	set("to", toF.String())
	switch {
	case evF.Bool():
		b, err := c.GetRaw(ctx, "/api/events", q)
		if err != nil {
			return errf("%v", err)
		}
		os.Stdout.Write(b)
		return 0
	case sessF.Bool():
		b, err := c.GetRaw(ctx, "/api/sessions", q)
		if err != nil {
			return errf("%v", err)
		}
		os.Stdout.Write(b)
		return 0
	}
	set("metric", metricF.String())
	set("tier", tierF.String())
	set("fn", fnF.String())
	pts := pointsF.String()
	if pts == "" {
		pts = "1000"
	}
	q.Set("points", pts)
	for _, m := range matchF.vals {
		q.Add("match", m)
	}
	q.Set("format", formatF.String())
	b, err := c.GetRaw(ctx, "/api/export", q)
	if err != nil {
		return errf("%v", err)
	}
	os.Stdout.Write(b)
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
