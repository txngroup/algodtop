package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"algod.top/internal/alert"
	"algod.top/internal/algod"
	"algod.top/internal/collector"
	"algod.top/internal/config"
	"algod.top/internal/daemon"
	"algod.top/internal/model"
	"algod.top/internal/remote"
	"algod.top/internal/store"
	"algod.top/internal/tui"
)

// cmdCheck validates the config, reaches each node, prints projected disk
// use and previews any retention deletion.
func cmdCheck(args []string) int {
	fs := newFlags(prog + " check")
	cfgF := fs.str("c", "config", "PATH", "Config file")
	agentF := fs.flag("", "agent", "Check the agent config")
	notifyF := fs.flag("", "notify", "Send a test notification on every channel")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	path := cfgF.String()
	if path == "" && agentF.Bool() {
		path = agentConfigPath()
	}
	cfg, ok, err := loadConfig(path)
	if err != nil {
		fmt.Printf("FAIL config: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Printf("FAIL no config file at %s (run `%s daemon --discover`)\n", cfg.Path, prog)
		return 1
	}
	fails := 0
	fmt.Printf("ok   config %s (%d nodes, %d remotes, %d alerts, %d channels)\n", cfg.Path, len(cfg.Nodes), len(cfg.Remotes), len(cfg.Alerts), len(cfg.Channels))
	if w := config.CheckPermissions(cfg.Path); w != "" {
		fmt.Println("warn " + w)
	}
	if l := cfg.Web.ListenAddr(); l != "" {
		host := strings.Split(l, ":")[0]
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && cfg.Web.PasswordHash == "" && !cfg.Web.AllowUnauthenticatedRemote {
			fmt.Printf("FAIL web.listen %s is not loopback and no password is set; the daemon will refuse to start\n", l)
			fails++
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, n := range cfg.Nodes {
		if n.Disabled {
			fmt.Printf("skip node %s (disabled)\n", n.Name)
			continue
		}
		if msg, err := checkNode(ctx, cfg, n); err != nil {
			fmt.Printf("FAIL node %-12s %v\n", n.Name, err)
			fails++
		} else {
			fmt.Printf("ok   node %-12s %s\n", n.Name, msg)
		}
	}
	for _, r := range cfg.Remotes {
		if r.Host == "" {
			fmt.Printf("info remote %-10s TLS agent %s (checked when the daemon runs)\n", r.Name, r.URL)
			continue
		}
		t, err := remote.ParseURL(sshURL(r.Host))
		if err != nil {
			fmt.Printf("FAIL remote %s: %v\n", r.Name, err)
			fails++
			continue
		}
		s := &remote.SSH{Target: t}
		out, err := s.Run(ctx, remote.AgentBinary(r.AgentPath)+" version", nil)
		if err != nil {
			fmt.Printf("FAIL remote %-10s %v\n", r.Name, err)
			fmt.Printf("     run `ssh %s` once to accept the host key and check that login needs no password\n", t.Dest())
			fails++
			continue
		}
		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		fmt.Printf("ok   remote %-10s %s\n", r.Name, first)
	}
	fails += checkAlerts(cfg, notifyF.Bool())
	// Disk projection and retention preview.
	in, err := store.Inspect(cfg.StorePath())
	if err != nil {
		fmt.Printf("warn store %s: %v\n", cfg.StorePath(), err)
	} else {
		ret := cfg.Retention.Windows()
		fmt.Printf("\nprojected disk use for %d nodes (store %s):\n", len(cfg.Nodes), cfg.StorePath())
		var settled int64
		var growth int64
		for _, p := range in.Project(ret, len(cfg.Nodes), cfg.Intervals.Scrape.D()) {
			src := "estimate"
			if p.Measured {
				src = "measured"
			}
			if p.Settled < 0 {
				fmt.Printf("  %-4s forever      grows %s per year (%s)\n", p.Tier, tui.Bytes(float64(p.PerYear))+"B", src)
				growth += p.PerYear
			} else {
				fmt.Printf("  %-4s %-12s %sB once the window is full (%s)\n", p.Tier, config.Duration(p.Window).String(), tui.Bytes(float64(p.Settled)), src)
				settled += p.Settled
			}
		}
		fmt.Printf("  bounded tiers settle at about %sB; forever tiers add %sB per year\n", tui.Bytes(float64(settled)), tui.Bytes(float64(growth)))
		del := in.Preview(ret, time.Now())
		if len(del) > 0 {
			var bytes int64
			tiers := map[string]int{}
			for _, d := range del {
				bytes += d.Bytes
				tiers[d.Tier]++
			}
			fmt.Printf("\nwarn the configured windows would delete %d blocks (%sB) at the next maintenance pass:\n", len(del), tui.Bytes(float64(bytes)))
			for _, t := range sortedKeys(tiers) {
				fmt.Printf("  %s: %d blocks\n", t, tiers[t])
			}
		}
	}
	if fails > 0 {
		fmt.Printf("\n%d problems\n", fails)
		return 1
	}
	return 0
}

// checkAlerts parses each alert rule as the daemon will and, with notify,
// sends a test notification on each channel. A rule the daemon cannot
// parse would never fire, and nothing else would say so before the
// incident it was meant for. It returns the number of failures.
func checkAlerts(cfg *config.Config, notify bool) int {
	fails := 0
	seen := map[string]bool{}
	for _, a := range cfg.Alerts {
		err := alert.CheckRule(a)
		if err == nil && seen[a.Name] {
			err = errors.New("another alert has this name; the daemon runs only the first")
		}
		seen[a.Name] = true
		switch {
		case err != nil:
			fmt.Printf("FAIL alert %-11s %v\n", a.Name, err)
			fails++
		case a.Severity != "" && !slices.Contains(alert.Severities, a.Severity):
			fmt.Printf("warn alert %-11s severity %q is not one of %s\n", a.Name, a.Severity, strings.Join(alert.Severities, ", "))
		default:
			fmt.Printf("ok   alert %-11s %s\n", a.Name, a.Expr)
		}
	}
	host, _ := os.Hostname()
	for _, name := range sortedKeys(cfg.Channels) {
		ch := cfg.Channels[name]
		if !notify {
			fmt.Printf("info channel %-10s %s\n", name, ch.Type)
			continue
		}
		now := time.Now().UnixNano()
		n := alert.Notification{Alert: "test", Node: host, State: "test", Severity: model.SevInfo,
			Message: "test notification from " + prog + " check --notify", Since: now, Time: now, Host: host}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := alert.Send(ctx, ch, n)
		cancel()
		if err != nil {
			fmt.Printf("FAIL channel %-10s %s: %v\n", name, ch.Type, err)
			fails++
		} else {
			fmt.Printf("ok   channel %-10s %s: test notification sent\n", name, ch.Type)
		}
	}
	return fails
}

func sshURL(host string) string {
	if strings.HasPrefix(host, "ssh://") {
		return host
	}
	return "ssh://" + host
}

func checkNode(ctx context.Context, cfg *config.Config, n config.Node) (string, error) {
	src, err := daemon.SourceFor(n, "")
	if err != nil {
		return "", err
	}
	defer src.Close()
	f, err := src.Files(ctx)
	if err != nil {
		if strings.HasPrefix(n.URL, "ssh://") {
			return "", fmt.Errorf("%v (run `ssh %s` once if the host key is new)", err, strings.Split(strings.TrimPrefix(n.URL, "ssh://"), "/")[0])
		}
		return "", err
	}
	if n.DataDir != "" {
		if _, err := os.Stat(algod.ExpandHome(n.DataDir)); err != nil {
			return "", err
		}
		if f.PID == 0 {
			return "data directory found; algod is stopped (no algod.pid)", nil
		}
	}
	if f.Net == "" {
		return "", fmt.Errorf("no REST address (algod.net missing)")
	}
	dial, poll := src.Dialers(f.Net)
	base := f.Net
	if src.Kind() == "ssh" {
		base = "http://algod"
	}
	c, err := algod.NewClient(base, algod.Options{Token: f.Token, AdminToken: f.AdminToken, Dial: dial, PollDial: poll, Timeout: 20 * time.Second})
	if err != nil {
		return "", err
	}
	defer c.CloseIdle()
	v, err := c.Versions(ctx)
	if err != nil {
		return "", err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("status: %w (check the API token)", err)
	}
	admin := "admin token ok"
	if f.AdminToken == "" {
		admin = "no admin token: peers and participation are off"
	} else if _, err := c.Peers(ctx); err != nil {
		admin = "admin token rejected: " + err.Error()
	}
	return fmt.Sprintf("%s %s round %d, %s", v.GenesisID, v.Version(), st.LastRound, admin), nil
}

var _ = collector.State{}
