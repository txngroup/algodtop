package daemon_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/cli"
	"algod.top/internal/config"
	"algod.top/internal/daemon"
	"algod.top/internal/fakealgod"
	"algod.top/internal/tui"
	"algod.top/internal/web"
)

// TestMain lets the test binary act as algodtop and as a fake ssh: the SSH
// tests put a fake ssh first in PATH that runs the remote command locally,
// so the SSH modes and the stdio relay are tested without an SSH server.
func TestMain(m *testing.M) {
	switch os.Getenv("ALGODTOP_TEST_ROLE") {
	case "algodtop":
		os.Exit(cli.Run(append([]string{"algodtop"}, os.Args[1:]...)))
	case "ssh":
		os.Exit(fakeSSH(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeSSH implements the ssh options algodtop uses: -o, -p, -W and a
// remote command.
func fakeSSH(args []string) int {
	var forward string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "-p":
			i++
		case "-W":
			i++
			forward = args[i]
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "fake ssh: no destination")
		return 255
	}
	if forward != "" {
		c, err := net.Dial("tcp", forward)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake ssh -W:", err)
			return 255
		}
		done := make(chan struct{}, 2)
		go func() { io.Copy(c, os.Stdin); done <- struct{}{} }()
		go func() { io.Copy(os.Stdout, c); done <- struct{}{} }()
		<-done
		return 0
	}
	cmd := exec.Command("sh", "-c", strings.Join(rest[1:], " "))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 255
	}
	return 0
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "adt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type running struct {
	d      *daemon.Daemon
	addr   string
	sock   string
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, cfg *config.Config, agent bool, opts ...func(*daemon.Options)) *running {
	t.Helper()
	var mu sync.Mutex
	addrCh := make(chan string, 1)
	logger := log.New(io.Discard, "", 0)
	if testing.Verbose() {
		logger = log.New(os.Stderr, "daemon: ", log.Ltime)
	}
	o := daemon.Options{ConfigPath: cfg.Path, Config: cfg, Agent: agent, Logger: logger, OnListen: func(a string) {
		mu.Lock()
		defer mu.Unlock()
		addrCh <- a
	}}
	if !agent {
		o.Static = web.FS
	}
	for _, f := range opts {
		f(&o)
	}
	d, err := daemon.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{d: d, cancel: cancel, done: make(chan error, 1), sock: cfg.SocketPath()}
	if agent {
		r.sock = filepath.Join(filepath.Dir(cfg.SocketPath()), "agent.sock")
	}
	go func() { r.done <- d.Run(ctx) }()
	if l := cfg.Web.ListenAddr(); l != "" && !agent {
		select {
		case r.addr = <-addrCh:
		case err := <-r.done:
			t.Fatalf("daemon exited: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("no listener")
		}
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not shut down within 15 s")
		}
	})
	return r
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestDaemonEndToEnd(t *testing.T) {
	dir := shortDir(t)
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: filepath.Join(dir, "node"), RoundEvery: 300 * time.Millisecond, ChurnEvery: 400 * time.Millisecond,
		Token: "t", AdminToken: "a", PeerUsage: true, Peers: 6, Network: "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	// A real config.json predates the algod process.
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, "node", "config.json"), past, past)
	listen := "127.0.0.1:0"
	cfg := &config.Config{Path: filepath.Join(dir, "config.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &listen},
		Nodes:     []config.Node{{Name: "fake", DataDir: filepath.Join(dir, "node"), Scrape: config.Duration(300 * time.Millisecond), Peers: config.Duration(300 * time.Millisecond)}},
		NTP:       config.NTP{Disable: true},
		Intervals: config.Intervals{Proc: config.Duration(time.Second)}}
	if err := cfg.Save(cfg.Path); err != nil {
		t.Fatal(err)
	}
	r := start(t, cfg, false)
	base := "http://" + r.addr
	var nodes struct {
		Nodes []struct {
			Name        string `json:"name"`
			Up          bool   `json:"up"`
			Round       uint64 `json:"round"`
			HistoryFrom int64  `json:"history_from"`
			Health      struct {
				Level string `json:"level"`
			} `json:"health"`
		} `json:"nodes"`
	}
	t0 := time.Now()
	waitFor(t, "node up", 10*time.Second, func() bool {
		getJSON(t, base+"/api/nodes", &nodes)
		return len(nodes.Nodes) == 1 && nodes.Nodes[0].Up && nodes.Nodes[0].Round > 0
	})
	// History starts at the first sample, before any window is flushed, so
	// the fleet sparklines span what exists rather than a whole day.
	waitFor(t, "history_from from the head", 10*time.Second, func() bool {
		getJSON(t, base+"/api/nodes", &nodes)
		hf := nodes.Nodes[0].HistoryFrom
		return hf >= t0.Add(-time.Minute).UnixNano() && hf <= time.Now().UnixNano()
	})
	// Series come back from the head as soon as the writer accepts them.
	var series struct {
		Series []struct {
			Avg []*float64 `json:"avg"`
		} `json:"series"`
	}
	waitFor(t, "series", 10*time.Second, func() bool {
		getJSON(t, base+"/api/series?node=fake&metric=algodtop_round&fn=last&from=-1m&points=60&tier=raw", &series)
		return len(series.Series) == 1 && len(series.Series[0].Avg) > 2
	})
	// The unix socket answers the same API.
	c, err := tui.DialSocket(r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ns, _, err := c.Nodes(context.Background())
	if err != nil || len(ns) != 1 {
		t.Fatalf("socket nodes: %v %v", ns, err)
	}
	// Over TCP, writes need the CSRF token.
	resp, _ := http.Post(base+"/api/annotations", "application/json", strings.NewReader(`{"text":"run 3"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("annotation without CSRF: %d", resp.StatusCode)
	}
	resp.Body.Close()
	var sess struct {
		CSRF string `json:"csrf"`
	}
	getJSON(t, base+"/api/session", &sess)
	req, _ := http.NewRequest(http.MethodPost, base+"/api/annotations", strings.NewReader(`{"text":"run 3: limit 40","node":"fake","silence_for":"10m"}`))
	req.Header.Set("X-CSRF-Token", sess.CSRF)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("annotation with CSRF: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	var sil struct {
		Silences []struct {
			Nodes []string `json:"nodes"`
		} `json:"silences"`
	}
	getJSON(t, base+"/api/silences", &sil)
	if len(sil.Silences) != 1 || sil.Silences[0].Nodes[0] != "fake" {
		t.Fatalf("annotation did not carry a silence: %+v", sil)
	}
	// The silence's event belongs to the node it silences.
	var silEvs struct {
		Events []struct {
			Node string `json:"node"`
			Kind string `json:"kind"`
		} `json:"events"`
	}
	waitFor(t, "silence event on its node", 5*time.Second, func() bool {
		getJSON(t, base+"/api/events?from=-1h", &silEvs)
		for _, e := range silEvs.Events {
			if e.Kind == "silence-start" {
				return e.Node == "fake"
			}
		}
		return false
	})
	// SSE: one stream with a status message right away.
	sresp, err := http.Get(base + "/api/stream?nodes=fake")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(sresp.Body)
	gotStatus := false
	for range 20 {
		l, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(l, "event: status") {
			gotStatus = true
			break
		}
	}
	sresp.Body.Close()
	if !gotStatus {
		t.Fatal("no status event on the stream")
	}
	// Peer sessions from log churn and polls.
	var sessions struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	waitFor(t, "sessions", 10*time.Second, func() bool {
		getJSON(t, base+"/api/sessions?node=fake&from=-1h", &sessions)
		return len(sessions.Sessions) > 0
	})
	// Restart: a reset event and an effective config change.
	os.WriteFile(filepath.Join(dir, "node", "config.json"), []byte(`{"P2PHybridIncomingConnectionsLimit": 40}`), 0o600)
	time.Sleep(2500 * time.Millisecond)
	child := exec.Command("sleep", "120")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	fake.Restart(true, child.Process.Pid)
	var evs struct {
		Events []struct {
			Kind string `json:"kind"`
			Msg  string `json:"msg"`
		} `json:"events"`
	}
	defer func() {
		if t.Failed() {
			var all struct {
				Events []map[string]any `json:"events"`
			}
			getJSON(t, base+"/api/events?from=-1h", &all)
			for _, e := range all.Events {
				if e["kind"] != "connect" && e["kind"] != "disconnect" {
					t.Logf("event %v %v", e["kind"], e["msg"])
				}
			}
		}
	}()
	waitFor(t, "config effective event", 15*time.Second, func() bool {
		getJSON(t, base+"/api/events?node=fake&from=-1h&kind=config-effective", &evs)
		return len(evs.Events) > 0 && strings.Contains(evs.Events[0].Msg, "P2PHybridIncomingConnectionsLimit")
	})
	// Snapshot and reload.
	if err := c.Post(context.Background(), "/api/admin/snapshot", map[string]any{}, nil); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "store", "snapshots", "*", "meta.json"))
	if len(snaps) != 1 {
		t.Fatalf("snapshot files %v", snaps)
	}
	cfg.Nodes = append(cfg.Nodes, config.Node{Name: "second", URL: fake.URL(), Token: "t"})
	cfg.Save(cfg.Path)
	if err := c.Post(context.Background(), "/api/admin/reload", map[string]any{}, nil); err != nil {
		t.Fatalf("reload: %v", err)
	}
	waitFor(t, "second node", 10*time.Second, func() bool {
		getJSON(t, base+"/api/nodes", &nodes)
		return len(nodes.Nodes) == 2
	})
	// The web UI is served.
	if code := getJSON(t, base+"/", nil); code != 200 {
		t.Fatalf("index: %d", code)
	}
	if code := getJSON(t, base+"/node/fake", nil); code != 200 {
		t.Fatalf("page route: %d", code)
	}
	// A rebinding Host header is refused.
	req, _ = http.NewRequest(http.MethodGet, base+"/api/nodes", nil)
	req.Host = "evil.example.com"
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding host got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRemoteRefusesUnauthenticated(t *testing.T) {
	dir := shortDir(t)
	listen := "0.0.0.0:0"
	cfg := &config.Config{Path: filepath.Join(dir, "config.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &listen}, NTP: config.NTP{Disable: true}}
	d, err := daemon.New(daemon.Options{Config: cfg, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("got %v", err)
	}
}

// TestSSHAgent runs a hub that pulls from an agent through the fake ssh
// and `algodtop stdio --agent`, the SSH agent mode end to end.
// skipShellSSH skips tests whose fake ssh and algodtop are #!/bin/sh
// wrappers on PATH, which Windows cannot run.
func skipShellSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh is a shell script")
	}
}

func TestSSHAgent(t *testing.T) {
	skipShellSSH(t)
	dir := shortDir(t)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	self, _ := os.Executable()
	// algodtop and ssh are this test binary under a role.
	for _, name := range []string{"algodtop", "ssh"} {
		script := fmt.Sprintf("#!/bin/sh\nALGODTOP_TEST_ROLE=%s exec %q \"$@\"\n", name, self)
		os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: filepath.Join(dir, "node"), RoundEvery: 300 * time.Millisecond, DisableAuth: true, AdminToken: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	// Agent config on the "remote" (the same machine).
	agentCfgPath := filepath.Join(dir, "agent.json")
	empty := ""
	acfg := &config.Config{Path: agentCfgPath, StoreDir: filepath.Join(dir, "agentstore"), Web: config.Web{Listen: &empty}, NTP: config.NTP{Disable: true},
		Nodes: []config.Node{{Name: "mainnet", DataDir: filepath.Join(dir, "node"), Scrape: config.Duration(300 * time.Millisecond)}}}
	acfg.Save(agentCfgPath)
	t.Setenv("ALGODTOP_CONFIG", agentCfgPath)
	listen := "127.0.0.1:0"
	hub := &config.Config{Path: filepath.Join(dir, "hub.json"), StoreDir: filepath.Join(dir, "hubstore"), Web: config.Web{Listen: &listen}, NTP: config.NTP{Disable: true},
		Remotes: []config.Remote{{Name: "relay1", Host: "relay1", AgentPath: filepath.Join(bin, "algodtop") + " -c " + agentCfgPath}}}
	hub.Save(hub.Path)
	// The agent path carries -c so stdio finds the agent config.
	hub.Remotes[0].AgentPath = filepath.Join(bin, "algodtop")
	t.Setenv("ALGODTOP_AGENT_CONFIG", agentCfgPath)
	r := start(t, hub, false)
	base := "http://" + r.addr
	var nodes struct {
		Nodes []struct {
			Name   string `json:"name"`
			Up     bool   `json:"up"`
			Kind   string `json:"kind"`
			Remote string `json:"remote"`
		} `json:"nodes"`
	}
	waitFor(t, "agent node on the hub", 30*time.Second, func() bool {
		getJSON(t, base+"/api/nodes", &nodes)
		for _, n := range nodes.Nodes {
			if n.Name == "relay1/mainnet" && n.Up && n.Kind == "agent" {
				return true
			}
		}
		return false
	})
	var series struct {
		Series []struct {
			Avg []*float64 `json:"avg"`
		} `json:"series"`
	}
	waitFor(t, "pulled series on the hub", 30*time.Second, func() bool {
		getJSON(t, base+"/api/series?node=relay1/mainnet&metric=algodtop_round&fn=last&from=-2m&points=60&tier=raw", &series)
		if len(series.Series) == 0 {
			return false
		}
		n := 0
		for _, v := range series.Series[0].Avg {
			if v != nil {
				n++
			}
		}
		return n > 2
	})
}

// TestSSHNoInstall reaches a node with nothing installed: one ssh command
// reads the data directory, and ssh -W carries REST.
func TestSSHNoInstall(t *testing.T) {
	skipShellSSH(t)
	dir := shortDir(t)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	self, _ := os.Executable()
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(fmt.Sprintf("#!/bin/sh\nALGODTOP_TEST_ROLE=ssh exec %q \"$@\"\n", self)), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: filepath.Join(dir, "node"), RoundEvery: 300 * time.Millisecond, Token: "tok", AdminToken: "adm", Peers: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	listen := "127.0.0.1:0"
	cfg := &config.Config{Path: filepath.Join(dir, "config.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &listen}, NTP: config.NTP{Disable: true},
		Nodes: []config.Node{{Name: "remote", URL: "ssh://relay2:2222" + filepath.Join(dir, "node"), Scrape: config.Duration(300 * time.Millisecond), Peers: config.Duration(500 * time.Millisecond)}}}
	cfg.Save(cfg.Path)
	r := start(t, cfg, false)
	base := "http://" + r.addr
	var nodes struct {
		Nodes []struct {
			Name  string `json:"name"`
			Up    bool   `json:"up"`
			Kind  string `json:"kind"`
			Peers struct {
				P2PIn  int `json:"p2p_in"`
				P2POut int `json:"p2p_out"`
				WSIn   int `json:"ws_in"`
				WSOut  int `json:"ws_out"`
			} `json:"peers"`
			Version string `json:"version"`
		} `json:"nodes"`
	}
	waitFor(t, "ssh node up with peers", 20*time.Second, func() bool {
		getJSON(t, base+"/api/nodes", &nodes)
		if len(nodes.Nodes) != 1 {
			return false
		}
		n := nodes.Nodes[0]
		return n.Up && n.Kind == "ssh" && n.Version == "4.4.1" && n.Peers.P2PIn+n.Peers.P2POut+n.Peers.WSIn+n.Peers.WSOut == 5
	})
	// Tokens read over SSH stay in memory: nothing in the store holds them.
	found := false
	filepath.Walk(filepath.Join(dir, "store"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), "adm") && strings.Contains(string(b), "tok") && strings.Contains(string(b), "admin_token") {
				found = true
			}
		}
		return nil
	})
	if found {
		t.Fatal("a token reached the hub's disk")
	}
}

// TestTLSAgent pulls from an agent over HTTPS with a token and a pinned
// self-signed certificate. Requests without the token are refused on every
// path, not only the pull endpoint.
func TestTLSAgent(t *testing.T) {
	dir := shortDir(t)
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: filepath.Join(dir, "node"), RoundEvery: 300 * time.Millisecond, DisableAuth: true, AdminToken: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	tok := "agent-secret-token"
	empty := ""
	acfg := &config.Config{Path: filepath.Join(dir, "agent.json"), StoreDir: filepath.Join(dir, "agentstore"), Web: config.Web{Listen: &empty}, NTP: config.NTP{Disable: true},
		Nodes: []config.Node{{Name: "alpha", DataDir: filepath.Join(dir, "node"), Scrape: config.Duration(300 * time.Millisecond)}},
		Agent: config.Agent{Listen: "127.0.0.1:0", TokenHash: hashToken(tok)}}
	acfg.Save(acfg.Path)
	addrCh := make(chan string, 1)
	d, err := daemon.New(daemon.Options{Config: acfg, ConfigPath: acfg.Path, Agent: true, Logger: log.New(io.Discard, "", 0), OnListen: func(a string) { addrCh <- a }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var agentAddr string
	select {
	case agentAddr = <-addrCh:
	case err := <-done:
		t.Fatalf("agent exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not listen")
	}
	certPath := filepath.Join(dir, "agentstore", "agent", "state", "tls-cert.pem")
	// Unauthenticated requests fail on every path.
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: insecureTLS()}}
	for _, p := range []string{"/api/nodes", "/agent/v1/batches", "/api/config"} {
		resp, err := insecure.Get("https://" + agentAddr + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without token: %d", p, resp.StatusCode)
		}
	}
	tokFile := filepath.Join(dir, "token")
	os.WriteFile(tokFile, []byte(tok+"\n"), 0o600)
	listen := "127.0.0.1:0"
	hub := &config.Config{Path: filepath.Join(dir, "hub.json"), StoreDir: filepath.Join(dir, "hubstore"), Web: config.Web{Listen: &listen}, NTP: config.NTP{Disable: true},
		Remotes: []config.Remote{{Name: "edge", URL: "https://" + agentAddr, TokenFile: tokFile, CAFile: certPath}}}
	hub.Save(hub.Path)
	r := start(t, hub, false)
	var series struct {
		Series []struct {
			Avg []*float64 `json:"avg"`
		} `json:"series"`
	}
	waitFor(t, "series pulled over TLS", 30*time.Second, func() bool {
		getJSON(t, "http://"+r.addr+"/api/series?node=edge/alpha&metric=algodtop_round&fn=last&from=-2m&points=60&tier=raw", &series)
		return len(series.Series) == 1 && len(series.Series[0].Avg) > 2
	})

	// Names give the agent's node a hub name of its own, such as the SSH
	// node it replaced. The hub pulls the agent's buffer again, so the data
	// it already sent lands under the new name too.
	var raw struct {
		Series []struct {
			T []int64 `json:"t"`
		} `json:"series"`
	}
	firstOf := func(node string) int64 {
		raw.Series = nil
		getJSON(t, "http://"+r.addr+"/api/series?node="+node+"&metric=algodtop_round&from=-5m&tier=raw", &raw)
		if len(raw.Series) == 0 || len(raw.Series[0].T) == 0 {
			return 0
		}
		return raw.Series[0].T[0]
	}
	first := firstOf("edge/alpha")
	hub.Remotes[0].Names = map[string]string{"alpha": "edge"}
	if err := hub.Save(hub.Path); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "renamed node", 30*time.Second, func() bool {
		var nodes struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		}
		getJSON(t, "http://"+r.addr+"/api/nodes", &nodes)
		return len(nodes.Nodes) == 1 && nodes.Nodes[0].Name == "edge"
	})
	waitFor(t, "buffer pulled again under the new name", 30*time.Second, func() bool {
		f := firstOf("edge")
		return f != 0 && f <= first
	})
}

func TestRemoteNamesValidate(t *testing.T) {
	c := &config.Config{Nodes: []config.Node{{Name: "edge2", URL: "ssh://host2/var/lib/algorand"}},
		Remotes: []config.Remote{{Name: "edge1", Host: "host1", Names: map[string]string{"mainnet": "edge1"}}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("a remote's own name: %v", err)
	}
	c.Remotes[0].Names["testnet"] = "edge2"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "already a node") {
		t.Fatalf("collision with a node: %v", err)
	}
}

func hashToken(s string) string { return api.HashToken(s) }

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

// An alert rule that does not parse leaves the daemon running with the
// others, and an event says which rule is missing.
func TestBadAlertRuleIsAnEvent(t *testing.T) {
	dir := shortDir(t)
	listen := "127.0.0.1:0"
	cfg := &config.Config{Path: filepath.Join(dir, "config.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &listen},
		NTP:    config.NTP{Disable: true},
		Alerts: []config.AlertRule{{Name: "stall", Expr: "down or stalled"}, {Name: "lagging", Expr: "lag > 30 s"}}}
	if err := cfg.Save(cfg.Path); err != nil {
		t.Fatal(err)
	}
	r := start(t, cfg, false)
	var evs struct {
		Events []struct {
			Severity string `json:"severity"`
			Msg      string `json:"msg"`
		} `json:"events"`
	}
	waitFor(t, "config-error event", 10*time.Second, func() bool {
		getJSON(t, "http://"+r.addr+"/api/events?from=-1h&kind=config-error", &evs)
		return len(evs.Events) == 1
	})
	if e := evs.Events[0]; e.Severity != "error" || !strings.Contains(e.Msg, `alert rules not running: alert "lagging"`) || strings.Contains(e.Msg, `"stall"`) {
		t.Fatalf("event %+v", e)
	}
}
