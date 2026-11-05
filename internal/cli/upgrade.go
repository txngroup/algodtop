package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"algod.top/internal/api"
	"algod.top/internal/config"
	"algod.top/internal/fsx"
	"algod.top/internal/release"
	"algod.top/internal/remote"
	"algod.top/internal/version"
)

// cmdUpgrade installs a release on this machine, restarts the local
// daemon or agent onto it, and then upgrades every remote agent in the
// hub config. Each daemon and agent installs the release itself: it gets
// the signed files over its socket, which for a remote agent is the SSH
// connection the hub already uses, and checks the signature and the
// version before it replaces its binary. Only an agent that still runs a
// binary root owns needs root, once.
func cmdUpgrade(args []string) int {
	fs := newFlags(prog + " upgrade")
	verF := fs.str("", "version", "VERSION", "Release to install, default the newest")
	rootF := fs.str("", "root", "su|sudo", "How to become root on a remote that needs it once, default ask")
	localF := fs.flag("", "local", "Upgrade this machine only, not the remote agents")
	yesF := fs.flag("y", "yes", "Do not ask for confirmation")
	cfgF := fs.str("c", "config", "PATH", "Hub config file")
	helpF := fs.flag("h", "help", "Show help")
	if err := fs.parse(args); err != nil {
		return errf("%v", err)
	}
	if helpF.Bool() {
		fmt.Printf("Usage: %s upgrade [flags]\n\nInstalls a signed release here and restarts the local daemon or agent onto it,\nthen upgrades each remote agent in the hub config.\n\n%s", prog, fs.usage())
		return 0
	}
	if r := rootF.String(); r != "" && r != "su" && r != "sudo" {
		return errf("--root must be su or sudo")
	}
	cfg, ok, err := loadConfig(cfgF.String())
	if err != nil {
		return errf("%v", err)
	}
	if !ok {
		cfg.Path = config.DefaultPath()
	}
	ctx, cancel := signalContext()
	defer cancel()
	ver := verF.String()
	if ver == "" || ver == "latest" {
		if ver, err = release.Latest(ctx); err != nil {
			return errf("find the newest release: %v", err)
		}
	}
	fmt.Printf("release %s\n", describeVersion(ver))
	files := &releaseCache{ver: ver, got: map[string]release.Files{}}
	failed := 0
	if !upgradeLocal(ctx, cfg, ok, files) {
		failed++
	}
	if !localF.Bool() {
		for _, x := range cfg.Remotes {
			switch {
			case x.Disabled:
				continue
			case x.URL != "":
				fmt.Printf("%s: a TLS agent; run `%s upgrade` on its host\n", x.Name, prog)
				continue
			}
			r, err := newRemoteCmd(cfg, x.Host, x.Name)
			if err != nil {
				fmt.Printf("%s: %v\n", x.Name, err)
				failed++
				continue
			}
			r.yes, r.root, r.files = yesF.Bool(), rootF.String(), files
			if r.upgrade(ctx) != 0 {
				failed++
			}
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// describeVersion names a release by its commit time, such as
// "2026-11-05 09:02 UTC (8526abb4), 3 hours old".
func describeVersion(v string) string { return version.Parse(v).Describe(time.Now()) }

// shortVersion is a version's first word, as a daemon's status gives it.
func shortVersion(v string) string {
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return v
}

// releaseCache downloads each platform's files of one release once.
type releaseCache struct {
	ver string
	got map[string]release.Files
}

// get returns the verified files for a platform such as linux/amd64.
func (c *releaseCache) get(ctx context.Context, platform string) (release.Files, error) {
	if f, ok := c.got[platform]; ok {
		return f, nil
	}
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok {
		return release.Files{}, fmt.Errorf("unknown platform %q", platform)
	}
	f, err := release.FetchFiles(ctx, c.ver, goos, goarch)
	if err != nil {
		return f, err
	}
	c.got[platform] = f
	return f, nil
}

// upgradeLocal upgrades the daemon and agent that run on this machine,
// then this command's own binary when it is another file.
func upgradeLocal(ctx context.Context, cfg *config.Config, ok bool, files *releaseCache) bool {
	f, err := files.get(ctx, runtime.GOOS+"/"+runtime.GOARCH)
	if err != nil {
		fmt.Printf("this machine: %v\n", err)
		return false
	}
	good := true
	var replaced []string
	for _, sock := range localSockets(cfg, ok) {
		conn, err := net.Dial("unix", fsx.SocketAddr(sock))
		if err != nil {
			continue
		}
		c := newAPIConn(conn, nil)
		var st api.DaemonStatus
		err = c.get(ctx, "/api/status", &st)
		var res release.Result
		if err == nil {
			res, err = c.upgrade(ctx, f)
		}
		c.Close()
		role := map[bool]string{true: "agent", false: "daemon"}[st.Agent]
		label := "this machine's " + role
		switch {
		case err == nil && res.Same:
			fmt.Printf("%s already runs %s\n", label, res.To)
			replaced = append(replaced, res.Path)
		case err == nil && res.Restart:
			replaced = append(replaced, res.Path)
			if waitVersion(ctx, res.To, func() (*apiConn, error) {
				conn, err := net.Dial("unix", fsx.SocketAddr(sock))
				if err != nil {
					return nil, err
				}
				return newAPIConn(conn, nil), nil
			}) {
				fmt.Printf("%s: upgraded from %s to %s\n", label, res.From, res.To)
			} else {
				fmt.Printf("%s: installed %s, but it did not answer on %s within a minute; check its log\n", label, res.To, sock)
				good = false
			}
		case err == nil:
			replaced = append(replaced, res.Path)
			fmt.Printf("%s: installed %s; restart it to run it\n", label, res.To)
		case release.IsCode(err, release.CodeOlder):
			fmt.Printf("%s runs %s, which is newer; left alone\n", label, shortVersion(st.Version))
		case release.IsCode(err, release.CodeNotWritable):
			cmd := []string{saveForRoot(f), "service", "install", "--system"}
			if st.Agent {
				cmd = append(cmd, "--agent")
			}
			fmt.Printf("%s is a system service that runs a binary root owns. Switch it once, as root, to its own copy, which it then upgrades itself:\n\n  %s\n  or: %s\n\n",
				label, asRoot("sudo", shellJoin(cmd...)), asRoot("su", shellJoin(cmd...)))
			good = false
		case errors.Is(err, errNoUpgradeAPI):
			fmt.Printf("%s runs %s, from before algodtop upgrade; restart it once to run the new binary\n", label, shortVersion(st.Version))
		default:
			fmt.Printf("%s: %v\n", label, err)
			good = false
		}
	}
	exe, err := release.Executable()
	if err != nil {
		fmt.Printf("this binary: %v\n", err)
		return false
	}
	for _, p := range replaced {
		if sameFile(p, exe) {
			return good
		}
	}
	res, err := release.Install(ctx, f, exe, version.Info())
	switch {
	case err == nil && res.Same:
		fmt.Printf("%s already runs %s\n", exe, res.To)
	case err == nil:
		fmt.Printf("%s: upgraded from %s to %s\n", exe, res.From, res.To)
	case release.IsCode(err, release.CodeOlder):
		fmt.Printf("%s is newer than %s; left alone\n", exe, f.Version)
	default:
		fmt.Printf("%s: %v\n", exe, err)
		good = false
	}
	return good
}

// localSockets lists where a daemon or agent on this machine may answer.
func localSockets(cfg *config.Config, ok bool) []string {
	paths := daemonSockets(cfg, ok)
	paths = append(paths, filepath.Join(filepath.Dir(cfg.SocketPath()), "agent.sock"))
	if runtime.GOOS == "linux" {
		paths = append(paths, systemAgentSocket())
	}
	var out []string
	for _, p := range paths {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// saveForRoot writes the new binary to a private directory, for a root
// command to install from.
func saveForRoot(f release.Files) string {
	dir, err := os.MkdirTemp("", "algodtop-upgrade-")
	if err == nil {
		p := filepath.Join(dir, "algodtop")
		if err = os.WriteFile(p, f.Binary, 0o755); err == nil {
			return p
		}
	}
	return "NEW-BINARY"
}

func sameFile(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(x, y)
}

// waitVersion polls a restarting daemon or agent until it answers with
// the wanted version, for up to 3 minutes: opening a large store can take
// the better part of one.
func waitVersion(ctx context.Context, want string, dial func() (*apiConn, error)) bool {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
		c, err := dial()
		if err != nil {
			continue
		}
		var st api.DaemonStatus
		err = c.get(ctx, "/api/status", &st)
		c.Close()
		if err == nil && (want == "" || shortVersion(st.Version) == want) {
			return true
		}
	}
	return false
}

// errNoUpgradeAPI means the daemon or agent predates algodtop upgrade.
var errNoUpgradeAPI = errors.New("it predates algodtop upgrade")

// apiConn is an HTTP client over one connection to a daemon or agent
// socket, either local or through `ssh host algodtop stdio --agent`.
type apiConn struct {
	hc     *http.Client
	conn   net.Conn
	stderr func() string
}

func newAPIConn(conn net.Conn, stderr func() string) *apiConn {
	var dialed atomic.Bool
	tr := &http.Transport{MaxConnsPerHost: 1, DialContext: func(context.Context, string, string) (net.Conn, error) {
		if dialed.Swap(true) {
			return nil, errors.New("the connection closed")
		}
		return conn, nil
	}}
	return &apiConn{hc: &http.Client{Transport: tr}, conn: conn, stderr: stderr}
}

func (c *apiConn) Close() error { return c.conn.Close() }

// fail prefers what ssh or the remote printed to stderr.
func (c *apiConn) fail(err error) error {
	if c.stderr != nil {
		if s := c.stderr(); s != "" {
			return errors.New(s)
		}
	}
	return err
}

func (c *apiConn) get(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://algodtop"+path, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// upgrade sends a release to the daemon or agent, which installs it.
func (c *apiConn) upgrade(ctx context.Context, f release.Files) (release.Result, error) {
	var res release.Result
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct {
		field, name string
		data        []byte
	}{{"checksums", "checksums.txt", f.Checksums}, {"signature", "checksums.txt.sig", f.Signature}, {"binary", f.Name, f.Binary}} {
		w, err := mw.CreateFormFile(part.field, part.name)
		if err != nil {
			return res, err
		}
		w.Write(part.data)
	}
	mw.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://algodtop/api/admin/upgrade", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.hc.Do(req)
	if err != nil {
		return res, c.fail(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		err = json.NewDecoder(resp.Body).Decode(&res)
		return res, err
	case http.StatusConflict:
		ie := &release.InstallError{}
		if err := json.NewDecoder(resp.Body).Decode(ie); err != nil {
			return res, err
		}
		return res, ie
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return res, errNoUpgradeAPI
	}
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	return res, fmt.Errorf("upgrade: %s %s", resp.Status, e.Error)
}

// dialAgent connects to the agent on the remote through its stdio relay.
// It never starts an agent, and for a system agent it goes straight to
// the service's socket.
func (r *remoteCmd) dialAgent(ctx context.Context, path string) (*apiConn, error) {
	cfgPath := ""
	if x, ok := r.saved(); ok {
		cfgPath = x.AgentConfig
	}
	relay := remote.AgentRelay(path, cfgPath)
	if !strings.Contains(relay, "--no-start") {
		relay += " --no-start"
	}
	cmd := r.ssh.Command(ctx, relay)
	conn, err := remote.DialCommand(cmd, r.hostArg)
	if err != nil {
		return nil, err
	}
	return newAPIConn(conn, conn.Stderr), nil
}

// upgrade moves the remote's agent to a release. A current agent installs
// it itself. A user agent from before that is replaced over SSH, as it
// always was. A system agent from before, or one that runs a binary root
// owns, needs root once to move onto its own copy.
func (r *remoteCmd) upgrade(ctx context.Context) int {
	if r.files == nil {
		ver := r.ver
		if ver == "" {
			ver = shortVersion(version.String())
		}
		if ver == "latest" {
			v, err := release.Latest(ctx)
			if err != nil {
				return errf("find the newest release: %v", err)
			}
			ver = v
		}
		r.files = &releaseCache{ver: ver, got: map[string]release.Files{}}
	}
	label := r.name
	if r.bin != "" {
		// An unsigned development binary: the agent would refuse it.
		return r.copyUpgrade(ctx)
	}
	path := r.agentPath()
	c, err := r.dialAgent(ctx, path)
	var st api.DaemonStatus
	if err == nil {
		err = c.get(ctx, "/api/status", &st)
		if err != nil {
			c.Close()
		}
	}
	if err != nil {
		if r.systemAgent() {
			return errf("%s: no agent answers through %s on %s: %v", label, path, r.hostArg, err)
		}
		return r.copyUpgrade(ctx)
	}
	f, err := r.releaseFiles(ctx, st.Platform)
	if err != nil {
		c.Close()
		return errf("%s: %v", label, err)
	}
	res, err := c.upgrade(ctx, f)
	c.Close()
	system := strings.HasPrefix(st.ConfigPath, "/etc/") || r.systemAgent()
	switch {
	case err == nil && res.Same:
		fmt.Printf("%s: already runs %s\n", label, res.To)
	case err == nil && res.Restart:
		fmt.Printf("%s: installed %s, restarting\n", label, res.To)
		if !r.waitAgent(ctx, res.To) {
			return errf("%s: the agent did not answer with %s within 3 minutes; check it on %s", label, res.To, r.hostArg)
		}
		fmt.Printf("%s: upgraded from %s to %s\n", label, res.From, res.To)
	case err == nil:
		fmt.Printf("%s: installed %s, which runs from the agent's next start\n", label, res.To)
	case release.IsCode(err, release.CodeOlder):
		fmt.Printf("%s: runs %s, which is newer; left alone\n", label, shortVersion(st.Version))
	case system && (errors.Is(err, errNoUpgradeAPI) || release.IsCode(err, release.CodeNotWritable)):
		return r.switchSystemAgent(ctx, f, shortVersion(st.Version))
	case errors.Is(err, errNoUpgradeAPI):
		return r.copyUpgrade(ctx)
	default:
		return errf("%s: %v", label, err)
	}
	return 0
}

// releaseFiles fetches the release for a platform, or reads it from
// --release-dir.
func (r *remoteCmd) releaseFiles(ctx context.Context, platform string) (release.Files, error) {
	if r.dir != "" {
		goos, goarch, _ := strings.Cut(platform, "/")
		return release.FilesFromDir(r.dir, goos, goarch)
	}
	return r.files.get(ctx, platform)
}

// waitAgent waits for the remote agent to answer with a version.
func (r *remoteCmd) waitAgent(ctx context.Context, want string) bool {
	return waitVersion(ctx, want, func() (*apiConn, error) { return r.dialAgent(ctx, r.agentPath()) })
}

// copyUpgrade replaces a user agent from before algodtop upgrade over
// SSH and restarts its service.
func (r *remoteCmd) copyUpgrade(ctx context.Context) int {
	h, err := r.ssh.Detect(ctx)
	if err != nil {
		return errf("%v", err)
	}
	r.host = h
	var bin []byte
	var desc string
	if r.bin != "" {
		bin, desc, err = r.binary(ctx)
	} else {
		var f release.Files
		f, err = r.releaseFiles(ctx, h.OS+"/"+h.Arch)
		bin, desc = f.Binary, "signed release "+cmp.Or(f.Version, "from "+r.dir)
	}
	if err != nil {
		return errf("%v", err)
	}
	path := r.agentPath()
	if !r.confirm(fmt.Sprintf("replace %s on %s with %s?", path, r.hostArg, desc)) {
		return 1
	}
	if err := r.ssh.Copy(ctx, h, path, bin); err != nil {
		return errf("copy: %v", err)
	}
	if sp := remote.PlanService(h, path); sp.Restart != "" {
		if out, err := r.ssh.Shell(ctx, sp.Restart); err != nil {
			return errf("%s: copied %s to %s, but restarting the agent service failed: %v %s", r.name, desc, path, err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Printf("%s: copied %s to %s and restarted the agent service\n", r.name, desc, path)
	return 0
}

// switchSystemAgent moves a system agent onto its own copy of algodtop,
// which needs root once: it copies the release to the SSH user's home
// and runs the system install from there over `ssh -t`, so su or sudo can
// ask for a password. From then on the agent upgrades itself.
func (r *remoteCmd) switchSystemAgent(ctx context.Context, f release.Files, running string) int {
	label := r.name
	h, err := r.ssh.Detect(ctx)
	if err != nil {
		return errf("%s: %v", label, err)
	}
	r.host = h
	tmp := remote.DefaultPath(h)
	fmt.Printf("%s: the system agent runs %s from a binary root owns. It needs root once to move onto its own copy, which it then upgrades itself.\n", label, running)
	if !r.confirm(fmt.Sprintf("copy %s to %s on %s?", f.Version, tmp, r.hostArg)) {
		return 1
	}
	if err := r.ssh.Copy(ctx, h, tmp, f.Binary); err != nil {
		return errf("%s: copy: %v", label, err)
	}
	method, rc := r.runAsRoot(ctx, tmp, "service", "install", "--system", "--agent")
	if rc != 0 {
		// The install may have worked and only given up waiting for a
		// slow agent. Ask the agent once before calling it a failure.
		if method == "" || !r.answersWith(ctx, f.Version) {
			return rc
		}
		fmt.Printf("%s: the install reported a problem, but the agent answers with %s\n", label, f.Version)
	} else if !r.waitAgent(ctx, f.Version) {
		return errf("%s: the agent did not answer with %s within 3 minutes; check it on %s", label, f.Version, r.hostArg)
	}
	r.saveRoot(method)
	if r.agentPath() == legacySystemBin {
		// The install linked the old name to the new one.
		r.savePath(systemBin)
	}
	fmt.Printf("%s: upgraded from %s to %s\n", label, running, cmp.Or(f.Version, "the release in "+r.dir))
	return 0
}

// runAsRoot runs algod.top at path on the remote as root, with su or sudo
// over `ssh -t`, after showing the command. It needs a terminal, since
// su and sudo ask for a password. It returns the method it ran with, or
// "" when it ran nothing. The caller saves the method once the result
// checks out.
func (r *remoteCmd) runAsRoot(ctx context.Context, path string, args ...string) (string, int) {
	method, err := r.rootMethod()
	if err != nil {
		return "", errf("%v", err)
	}
	cmd := rootCommand(method, path, args...)
	fmt.Printf("\n  %s\n\n", sshT(r.hostArg, cmd))
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Println("Run that in a terminal, since it asks for a password.")
		return "", 1
	}
	if !r.confirm("run it now?") {
		return "", 1
	}
	c := r.ssh.Command(ctx, cmd, "-t")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return method, errf("%s on %s: %v", method, r.hostArg, err)
	}
	return method, 0
}

// answersWith asks the agent once whether it runs a version.
func (r *remoteCmd) answersWith(ctx context.Context, want string) bool {
	c, err := r.dialAgent(ctx, r.agentPath())
	if err != nil {
		return false
	}
	defer c.Close()
	var st api.DaemonStatus
	return c.get(ctx, "/api/status", &st) == nil && (want == "" || shortVersion(st.Version) == want)
}

// lastRoot is the answer to the previous root question, the default for
// the next remote.
var lastRoot string

// rootMethod is how to become root on the remote: --root, then the
// remote's saved choice, then a question.
func (r *remoteCmd) rootMethod() (string, error) {
	if r.root != "" {
		return r.root, nil
	}
	if x, ok := r.saved(); ok && x.Root != "" {
		return x.Root, nil
	}
	def := cmp.Or(lastRoot, r.usualRoot(), "sudo")
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return def, nil
	}
	for {
		fmt.Printf("Become root on %s with su or sudo? [%s] ", r.hostArg, def)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch a := strings.TrimSpace(line); a {
		case "":
			lastRoot = def
			return def, nil
		case "su", "sudo":
			lastRoot = a
			return a, nil
		}
	}
}

// usualRoot is the way to become root that most remotes have saved, the
// default for one that has none.
func (r *remoteCmd) usualRoot() string {
	n := map[string]int{}
	for _, x := range r.cfg.Remotes {
		if x.Root != "" {
			n[x.Root]++
		}
	}
	if n["su"] > n["sudo"] {
		return "su"
	}
	if n["sudo"] > 0 {
		return "sudo"
	}
	return ""
}

// knownRoot is the saved or given way to become root, for printed hints.
func (r *remoteCmd) knownRoot() string {
	if r.root != "" {
		return r.root
	}
	if x, ok := r.saved(); ok && x.Root != "" {
		return x.Root
	}
	return "sudo"
}

// savePath records the agent's path for the remote.
func (r *remoteCmd) savePath(p string) {
	for i := range r.cfg.Remotes {
		if r.cfg.Remotes[i].Name == r.name {
			r.cfg.Remotes[i].AgentPath = p
			if err := r.cfg.Save(r.cfg.Path); err != nil {
				fmt.Fprintf(os.Stderr, "warn: could not update %s: %v\n", r.cfg.Path, err)
			}
		}
	}
}

// saveRoot remembers how to become root on this remote.
func (r *remoteCmd) saveRoot(method string) {
	r.root = method
	for i := range r.cfg.Remotes {
		if r.cfg.Remotes[i].Name == r.name && r.cfg.Remotes[i].Root != method {
			r.cfg.Remotes[i].Root = method
			if err := r.cfg.Save(r.cfg.Path); err != nil {
				fmt.Fprintf(os.Stderr, "warn: could not update %s: %v\n", r.cfg.Path, err)
			}
		}
	}
}

// rootCommand is the remote shell command that runs algodtop at path as
// root. A path in the home directory is written with $HOME, which the
// login shell expands to the SSH user's home before su or sudo starts.
func rootCommand(method, path string, args ...string) string {
	return asRoot(method, homeExpr(path)+" "+shellJoin(args...))
}

// asRoot wraps a shell command in su or sudo.
func asRoot(method, cmd string) string {
	if method == "su" {
		return "su -c " + dquote(cmd)
	}
	return "sudo " + cmd
}

// homeExpr renders a remote path for a shell, with ~/ as $HOME/.
func homeExpr(p string) string {
	switch {
	case strings.HasPrefix(p, "~/") && shellSafe(p[2:]):
		return "$HOME/" + p[2:]
	case shellSafe(p):
		return p
	}
	return remote.RemoteBinary(p)
}

// dquote puts a command in double quotes, leaving $ to expand.
func dquote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}

// sshT is the command a person types to run cmd on host with a terminal.
func sshT(host, cmd string) string { return "ssh -t " + shellJoin(host) + " " + remote.ShellQuote(cmd) }

// saved returns the hub's remote of this name.
func (r *remoteCmd) saved() (config.Remote, bool) {
	for _, x := range r.cfg.Remotes {
		if x.Name == r.name {
			return x, true
		}
	}
	return config.Remote{}, false
}

// remoteForHost finds the saved remote for an SSH host as typed, such as
// node1 or ssh://steve@node1:22, so commands find it without --name.
func remoteForHost(cfg *config.Config, host string) (config.Remote, bool) {
	t, err := remote.ParseURL(sshURL(host))
	if err != nil {
		return config.Remote{}, false
	}
	for _, x := range cfg.Remotes {
		if x.Host == "" {
			continue
		}
		u, err := remote.ParseURL(sshURL(x.Host))
		if err == nil && u.Host == t.Host && u.Port == t.Port && (u.User == t.User || t.User == "" || u.User == "") {
			return x, true
		}
	}
	return config.Remote{}, false
}
