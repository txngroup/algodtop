package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/config"
	"algod.top/internal/sysstat"
)

// A Windows service runs the daemon or the agent under the service control
// manager, as LocalSystem. FUNC runs algod under a per-service virtual
// account, and only LocalSystem or an administrator can read that
// process's figures. One directory under %ProgramData% holds everything
// the service owns, and only SYSTEM and Administrators may open it, so the
// Administrators group plays the part of the algodtop group on Linux:
//
//	C:\ProgramData\algodtop-agent\agent.json       the config
//	C:\ProgramData\algodtop-agent\agent.sock       the API socket
//	C:\ProgramData\algodtop-agent\store\           the store
//	C:\ProgramData\algodtop-agent\bin\algod.top.exe   the service's own copy
//	C:\ProgramData\algodtop-agent\log\algod.top.log   its log
//
// The daemon's is C:\ProgramData\algodtop, with config.json and
// algodtop.sock.
type winPlan struct {
	name, display, role string
	dir, config, socket string
	store, bin, log     string
	args                []string // after the binary
}

// winJoin joins a Windows path. Plans are built on any OS for tests.
func winJoin(parts ...string) string {
	for i := range parts {
		parts[i] = strings.TrimRight(parts[i], `\`)
	}
	return strings.Join(parts, `\`)
}

func windowsPlan(programData string, agent bool) winPlan {
	programData = cmp.Or(programData, `C:\ProgramData`)
	p := winPlan{name: "algodtop", role: "daemon"}
	cfgName, sock := "config.json", "algodtop.sock"
	if agent {
		p.name, p.role = "algodtop-agent", "agent"
		cfgName, sock = "agent.json", "agent.sock"
	}
	p.display = "algod.top " + p.role
	p.dir = winJoin(programData, p.name)
	p.config = winJoin(p.dir, cfgName)
	p.socket = winJoin(p.dir, sock)
	p.store = winJoin(p.dir, "store")
	p.bin = winJoin(p.dir, "bin", "algod.top.exe")
	p.log = winJoin(p.dir, "log", "algod.top.log")
	p.args = []string{p.role, "--config", p.config, "--store-dir", p.store, "--socket", p.socket}
	return p
}

// winOps manages Windows services. service_windows.go sets it; it is nil
// on other systems.
type winOps interface {
	elevated() bool
	exists(name string) bool
	state(name string) (string, error)
	start(name string) error
	stop(name string, wait time.Duration) error
	install(p winPlan) error // create or update, with restart on failure
	remove(name string) error
	secureDir(dir string) error // SYSTEM and Administrators only
}

var winSvc winOps

func windowsService(sub string, env svcEnv, o svcOpts) int {
	p := windowsPlan(env.programData, o.agent)
	if o.config != "" {
		p.config = o.config
		p.args[2] = o.config
	}
	switch sub {
	case "show":
		return windowsShow(env, p)
	case "install":
		return windowsInstall(env, p)
	case "uninstall", "remove":
		return windowsUninstall(p)
	case "status":
		return windowsStatus(p)
	}
	return errf("unknown service command %q; use show, install, uninstall or status", sub)
}

func windowsShow(env svcEnv, p winPlan) int {
	installed := winSvc != nil && winSvc.exists(p.name)
	if installed {
		fmt.Printf("The Windows service %s is installed. `%s service status%s` reports on it.\n\n", p.name, prog, agentFlag(p))
	}
	fmt.Printf("A Windows service runs the %s at boot as LocalSystem, so it can read the figures of an algod that\n", p.role)
	fmt.Printf("another account runs, as FUNC does. From an elevated prompt (Run as administrator):\n\n  %s service install%s\n\nThat will:\n", prog, agentFlag(p))
	if _, err := os.Stat(p.config); err == nil {
		fmt.Printf("- use the config in %s\n", p.config)
	} else {
		nodes := windowsNodes()
		if len(nodes) == 0 {
			fmt.Printf("- write %s with the nodes it finds; none are running now\n", p.config)
		} else {
			fmt.Printf("- write %s with the nodes found here:\n", p.config)
			for _, n := range nodes {
				fmt.Printf("    %-12s %s\n", n.Name, n.DataDir)
			}
		}
	}
	fmt.Printf("- keep its data in %s, open only to SYSTEM and Administrators\n", p.dir)
	fmt.Printf("- copy this binary to %s, which `%s upgrade` replaces\n", p.bin, prog)
	fmt.Printf("- register the service %q (%s), started at boot and restarted after a failure\n", p.name, p.display)
	fmt.Printf("- start it; it logs to %s\n", p.log)
	return 0
}

func agentFlag(p winPlan) string {
	if p.role == "agent" {
		return " --agent"
	}
	return ""
}

// windowsNodes discovers the nodes a new config lists: running ones, and
// FUNC's data directories.
func windowsNodes() []config.Node {
	var out []config.Node
	for _, f := range algod.Discover(sysstat.ListAlgod, sysstat.Alive) {
		out = append(out, config.Node{Name: f.Name, DataDir: f.DataDir})
	}
	return out
}

func windowsInstall(env svcEnv, p winPlan) int {
	if winSvc == nil {
		return errf("Windows services can only be installed on Windows")
	}
	if !winSvc.elevated() {
		return errf("installing a Windows service needs an elevated prompt: open PowerShell or Terminal with Run as administrator, then run `%s service install%s`", prog, agentFlag(p))
	}
	var cfg *config.Config
	if _, err := os.Stat(p.config); err == nil {
		c, err := config.Load(p.config)
		if err != nil {
			return errf("%v", err)
		}
		cfg = c
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errf("%v", err)
	}
	for _, d := range []string{p.dir, p.store, winDir(p.bin), winDir(p.log)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return errf("%v", err)
		}
	}
	if err := winSvc.secureDir(p.dir); err != nil {
		return errf("restrict %s to SYSTEM and Administrators: %v", p.dir, err)
	}
	if cfg == nil {
		cfg = &config.Config{Nodes: windowsNodes()}
		if len(cfg.Nodes) == 0 {
			return errf("there is no config at %s and no algod node was found. Start your nodes, or write the config, then run this again", p.config)
		}
		if p.role == "agent" {
			empty := ""
			cfg.Web.Listen = &empty
		}
		if err := cfg.Validate(); err != nil {
			return errf("%v", err)
		}
		if err := cfg.Save(p.config); err != nil {
			return errf("write %s: %v", p.config, err)
		}
		fmt.Printf("wrote %s with %s\n", p.config, plural(len(cfg.Nodes), "node"))
	}
	if winSvc.exists(p.name) {
		fmt.Printf("stopping the %s service\n", p.name)
		if err := winSvc.stop(p.name, time.Minute); err != nil {
			return errf("stop %s: %v", p.name, err)
		}
	}
	if err := copyBinary(env.exe, p.bin); err != nil {
		return errf("copy %s: %v", p.bin, err)
	}
	fmt.Printf("copied %s to %s\n", env.exe, p.bin)
	if err := winSvc.install(p); err != nil {
		return errf("register the %s service: %v", p.name, err)
	}
	if err := winSvc.start(p.name); err != nil {
		return errf("start %s: %v", p.name, err)
	}
	fmt.Printf("started the %s service\n", p.name)
	if !waitSocket(p.socket, 15*time.Second) {
		fmt.Printf("waiting for it to answer on %s; opening its store can take a minute\n", p.socket)
		if !waitSocket(p.socket, 105*time.Second) {
			return errf("the service started, but nothing answers on %s after 2 minutes. Its log is %s", p.socket, p.log)
		}
	}
	fmt.Printf("\nalgod.top's %s runs as a Windows service and answers on %s.\n", p.role, p.socket)
	fmt.Printf("Config:    %s, then `%s reload%s` from an elevated prompt\n", p.config, prog, agentFlag(p))
	fmt.Printf("Logs:      %s\n", p.log)
	fmt.Printf("Upgrade:   %s upgrade\n", prog)
	fmt.Printf("Remove:    %s service uninstall%s\n", prog, agentFlag(p))
	fmt.Println("Only SYSTEM and Administrators can reach its socket, so use the terminal UI and CLI from an elevated prompt.")
	return 0
}

func winDir(p string) string { return p[:max(strings.LastIndexByte(p, '\\'), 0)] }

func windowsUninstall(p winPlan) int {
	if winSvc == nil {
		return errf("Windows services can only be removed on Windows")
	}
	if !winSvc.elevated() {
		return errf("removing a Windows service needs an elevated prompt (Run as administrator)")
	}
	if !winSvc.exists(p.name) {
		return errf("no %s service is installed", p.name)
	}
	if err := winSvc.stop(p.name, time.Minute); err != nil {
		return errf("stop %s: %v", p.name, err)
	}
	if err := winSvc.remove(p.name); err != nil {
		return errf("remove %s: %v", p.name, err)
	}
	fmt.Printf("removed the %s service\n", p.name)
	if err := os.RemoveAll(winDir(p.bin)); err == nil {
		fmt.Printf("removed %s\n", winDir(p.bin))
	}
	fmt.Printf("kept %s and its store in %s; delete them if you no longer need that history\n", p.config, p.store)
	return 0
}

func windowsStatus(p winPlan) int {
	if winSvc == nil || !winSvc.exists(p.name) {
		fmt.Printf("no %s service is installed; `%s service%s` explains how to add one\n", p.name, prog, agentFlag(p))
		return 0
	}
	state, err := winSvc.state(p.name)
	if err != nil {
		state = err.Error()
	}
	fmt.Printf("Windows service %s: %s\n", p.name, state)
	answers := "does not answer"
	if waitSocket(p.socket, 2*time.Second) {
		answers = "answers"
	}
	fmt.Printf("  socket %s %s\n  logs: %s\n", p.socket, answers, p.log)
	return 0
}
