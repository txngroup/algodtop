package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/config"
	"algod.top/internal/remote"
	"algod.top/internal/version"
)

// add points the hub at an agent that already runs on the host, such as
// a system service from `algodtop service install --system --agent`. It
// checks that the agent answers over SSH before it saves anything.
func (r *remoteCmd) add(ctx context.Context) int {
	paths := []string{r.path}
	if r.path == "" {
		paths = []string{systemBin, legacySystemBin, "~/.local/bin/algod.top", "~/.local/bin/algodtop"}
	}
	var st api.DaemonStatus
	var nodes []api.NodeView
	var used string
	var errs []string
	for _, p := range paths {
		var err error
		st, nodes, err = r.probe(ctx, p)
		if err == nil {
			used = p
			break
		}
		errs = append(errs, fmt.Sprintf("  %s: %v", p, err))
	}
	if used == "" {
		return errf("no agent answers on %s:\n%s\nInstall one there with `%s remote install %s`, or pass --path", r.hostArg, strings.Join(errs, "\n"), prog, r.hostArg)
	}
	if !st.Agent {
		return errf("%s on %s answers as a daemon, not an agent. Install the agent there with `sudo %s service install --system --agent`", used, r.hostArg, used)
	}
	age := ""
	if st.VersionTime > 0 {
		age = ", " + version.Age(time.Since(time.Unix(0, st.VersionTime)))
	}
	fmt.Printf("agent %s%s on %s (%s), config %s\n", st.Version, age, r.hostArg, st.Platform, st.ConfigPath)

	// Hub names: the remote's existing ones, then --node-name, then the
	// SSH nodes this agent replaces, so their history continues.
	names := map[string]string{}
	for _, x := range r.cfg.Remotes {
		if x.Name == r.name {
			maps.Copy(names, x.Names)
		}
	}
	if r.nodeName != "" {
		if len(nodes) != 1 {
			return errf("--node-name needs an agent with one node, and %s has %d; set names in %s instead", r.hostArg, len(nodes), r.cfg.Path)
		}
		names[nodes[0].Name] = r.nodeName
	}

	// Nodes the hub already watches on this host over SSH with nothing
	// installed would show twice.
	var dups []string
	takeOver := map[string]string{} // agent node to the SSH node it replaces
	for _, n := range r.cfg.Nodes {
		t, err := remote.ParseURL(n.URL)
		if err != nil || !strings.HasPrefix(n.URL, "ssh://") || t.Host != r.ssh.Target.Host {
			continue
		}
		dups = append(dups, n.Name)
		for _, a := range nodes {
			if a.DataDir != "" && path.Clean(a.DataDir) == path.Clean(t.Path) {
				takeOver[a.Name] = n.Name
			}
		}
	}
	if !r.confirm(fmt.Sprintf("add remote %q to %s?", r.name, r.cfg.Path)) {
		return 1
	}
	if len(dups) > 0 && r.confirm(fmt.Sprintf("replace %s, which %s %s over SSH without the agent? The agent's node takes over the name and its history.",
		strings.Join(dups, " and "), map[bool]string{true: "watches", false: "watch"}[len(dups) == 1], r.hostArg)) {
		r.cfg.Nodes = slices.DeleteFunc(r.cfg.Nodes, func(n config.Node) bool { return slices.Contains(dups, n.Name) })
		for a, h := range takeOver {
			if _, set := names[a]; !set {
				names[a] = h
			}
		}
		fmt.Printf("replaced %s\n", strings.Join(dups, " and "))
	}
	r.saveRemote(used)
	for i := range r.cfg.Remotes {
		if r.cfg.Remotes[i].Name == r.name {
			r.cfg.Remotes[i].AgentConfig = st.ConfigPath
			if r.root != "" {
				r.cfg.Remotes[i].Root = r.root
			}
			r.cfg.Remotes[i].Names = nil
			if len(names) > 0 {
				r.cfg.Remotes[i].Names = names
			}
		}
	}
	for _, n := range nodes {
		h := r.name + "/" + n.Name
		if v, ok := names[n.Name]; ok {
			h = v
		}
		fmt.Printf("  %-18s %-10s %s\n", h, n.Network, n.Summary)
	}
	if err := r.cfg.Validate(); err != nil {
		return errf("%v", err)
	}
	if err := r.cfg.Save(r.cfg.Path); err != nil {
		return errf("%v", err)
	}
	fmt.Printf("added; run `%s reload` to start pulling from %s\n", prog, r.name)
	return 0
}

// probe asks the agent behind `path stdio --agent` for its status and
// nodes, through one ssh connection.
func (r *remoteCmd) probe(ctx context.Context, path string) (api.DaemonStatus, []api.NodeView, error) {
	var st api.DaemonStatus
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := r.ssh.Command(ctx, remote.RemoteBinary(path)+" stdio --agent --no-start")
	conn, err := remote.DialCommand(cmd, r.hostArg)
	if err != nil {
		return st, nil, err
	}
	defer conn.Close()
	dialed := false
	hc := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1, DialContext: func(context.Context, string, string) (net.Conn, error) {
		if dialed {
			return nil, errors.New("the ssh connection closed")
		}
		dialed = true
		return conn, nil
	}}}
	get := func(p string, v any) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://algodtop"+p, nil)
		resp, err := hc.Do(req)
		if err != nil {
			if s := conn.Stderr(); s != "" {
				return errors.New(s)
			}
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", p, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
	if err := get("/api/status", &st); err != nil {
		return st, nil, err
	}
	var body struct {
		Nodes []api.NodeView `json:"nodes"`
	}
	err = get("/api/nodes", &body)
	return st, body.Nodes, err
}

// systemAgent reports whether the hub's remote of this name is a system
// service, whose config and binary belong to the service account.
func (r *remoteCmd) systemAgent() bool {
	for _, x := range r.cfg.Remotes {
		if x.Name == r.name {
			return remote.SystemAgentSocketFor(x.AgentConfig) != ""
		}
	}
	return false
}

// systemSync prints the alert settings for a system agent's config, which
// the SSH user cannot write.
func (r *remoteCmd) systemSync() int {
	cfgPath := systemAgentConfig
	for _, x := range r.cfg.Remotes {
		if x.Name == r.name && x.AgentConfig != "" {
			cfgPath = x.AgentConfig
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"alerts": r.cfg.Alerts, "channels": r.cfg.Channels, "heartbeat": r.cfg.Heartbeat}, "", "  ")
	reload := asRoot(r.knownRoot(), "systemctl reload algodtop-agent")
	if strings.Contains(cfgPath, `\`) {
		reload = prog + " reload --agent, from an elevated prompt"
	}
	fmt.Printf(`The agent on %s is a system service, and %s belongs to its account, so
algod.top cannot write it over SSH. Merge these settings into it on the host,
then run `+"`%s`"+`:

%s
`, r.hostArg, cfgPath, reload, indent(string(b)))
	return 1
}
