// Package remote reaches nodes on other hosts through the system ssh
// binary: SSH without install (ssh cat plus ssh -W to algod), the SSH
// agent (ssh host algodtop stdio --agent), and agent installation.
package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/collector"
)

// Target is a parsed ssh://[user@]host[:port]/<path> URL.
type Target struct {
	User string
	Host string // host or ~/.ssh/config alias
	Port string
	Path string // remote path; a leading ~/ means the remote home
}

// ParseURL parses ssh://relay1/~/.mainnet or ssh://algorand@relay2:2222/var/lib/algorand.
func ParseURL(s string) (Target, error) {
	if !strings.HasPrefix(s, "ssh://") {
		return Target{}, fmt.Errorf("not an ssh:// URL: %q", s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, err
	}
	t := Target{Host: u.Hostname(), Port: u.Port()}
	if u.User != nil {
		t.User = u.User.Username()
	}
	p := u.Path
	if strings.HasPrefix(p, "/~") {
		p = strings.TrimPrefix(p, "/")
	}
	t.Path = p
	if t.Host == "" {
		return t, fmt.Errorf("ssh URL %q has no host", s)
	}
	// ssh reads a destination that starts with - as an option, and an
	// option such as -oProxyCommand runs a program.
	if strings.HasPrefix(t.Host, "-") || strings.HasPrefix(t.User, "-") {
		return t, fmt.Errorf("ssh URL %q: a host or user may not start with -", s)
	}
	return t, nil
}

// Dest renders user@host for the ssh command line.
func (t Target) Dest() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// SSH runs the system ssh. It honours ~/.ssh/config, aliases, ProxyJump,
// keys, ssh-agent and known_hosts. BatchMode=yes means a host that wants a
// password or has an unknown key fails with a clear error rather than
// prompting.
type SSH struct {
	Target     Target
	ControlDir string // for ControlMaster sockets, "" disables
	Binary     string // default "ssh"
}

// Args returns the common ssh arguments, ending with the destination.
func (s *SSH) Args(extra ...string) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	if p, ok := s.controlPath(); ok {
		os.MkdirAll(s.ControlDir, 0o700)
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath=\""+p+"\"", "-o", "ControlPersist=120")
	}
	if s.Target.Port != "" {
		args = append(args, "-p", s.Target.Port)
	}
	args = append(args, extra...)
	return append(args, s.Target.Dest())
}

func (s *SSH) bin() string {
	if s.Binary != "" {
		return s.Binary
	}
	return "ssh"
}

// Command builds an ssh command that runs a remote command string.
func (s *SSH) Command(ctx context.Context, remote string, extra ...string) *exec.Cmd {
	args := append(s.Args(extra...), remote)
	return exec.CommandContext(ctx, s.bin(), args...)
}

// Run runs a remote command and returns stdout. Errors carry stderr.
func (s *SSH) Run(ctx context.Context, remote string, stdin io.Reader) ([]byte, error) {
	cmd := s.Command(ctx, remote)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), sshError(s.Target, err, errb.String())
	}
	return out.Bytes(), nil
}

// sshError explains the common failures.
func sshError(t Target, err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "host key verification failed"), strings.Contains(low, "no matching host key"):
		return fmt.Errorf("ssh %s: the host key is not in known_hosts; run `ssh %s` once to accept it (%s)", t.Host, t.Dest(), msg)
	case strings.Contains(low, "permission denied"):
		return fmt.Errorf("ssh %s: permission denied; algod.top needs key or agent login without a password prompt (%s)", t.Host, msg)
	case msg != "":
		return fmt.Errorf("ssh %s: %v: %s", t.Host, err, msg)
	}
	return fmt.Errorf("ssh %s: %v", t.Host, err)
}

// ShellQuote quotes a string for a POSIX shell.
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// remotePath renders a path for a POSIX shell, expanding a leading ~/.
func remotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		return `"$HOME"/` + ShellQuote(p[2:])
	}
	return ShellQuote(p)
}

// Conn is a net.Conn over a child process's stdin and stdout, such as
// ssh -W or ssh host algodtop stdio.
type Conn struct {
	cmd    *exec.Cmd
	r      io.ReadCloser
	w      io.WriteCloser
	stderr *limitedBuffer
	once   sync.Once
	name   string
}

// DialCommand starts cmd and returns its stdio as a connection.
func DialCommand(cmd *exec.Cmd, name string) (*Conn, error) {
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	lb := &limitedBuffer{b: &bytes.Buffer{}, max: 4096}
	cmd.Stderr = lb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Conn{cmd: cmd, r: r, w: w, stderr: lb, name: name}, nil
}

type limitedBuffer struct {
	mu  sync.Mutex
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() < l.max {
		l.b.Write(p[:min(len(p), l.max-l.b.Len())])
	}
	return len(p), nil
}

// Read implements net.Conn.
func (c *Conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Write implements net.Conn.
func (c *Conn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close ends the process.
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		done := make(chan struct{})
		go func() { c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			c.cmd.Process.Kill()
			<-done
		}
	})
	return nil
}

// Stderr returns what the process printed on stderr.
func (c *Conn) Stderr() string {
	c.stderr.mu.Lock()
	defer c.stderr.mu.Unlock()
	return strings.TrimSpace(c.stderr.b.String())
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// LocalAddr implements net.Conn.
func (c *Conn) LocalAddr() net.Addr { return pipeAddr("local") }

// RemoteAddr implements net.Conn.
func (c *Conn) RemoteAddr() net.Addr { return pipeAddr(c.name) }

// SetDeadline implements net.Conn. Pipes have no deadlines; callers rely
// on context cancellation, which closes the connection.
func (c *Conn) SetDeadline(time.Time) error { return nil }

// SetReadDeadline implements net.Conn.
func (c *Conn) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline implements net.Conn.
func (c *Conn) SetWriteDeadline(time.Time) error { return nil }

// SSHSource reaches a node with nothing installed on the remote: one ssh
// cat for the data directory files, and ssh -W to algod's REST port. It
// needs cat and a POSIX shell, so it supports Linux and macOS remotes.
type SSHSource struct {
	SSH
	TokenFile, AdminTokenFile string
	Token, AdminToken         string
}

// NewSSHSource parses a node URL.
func NewSSHSource(rawURL, controlDir string) (*SSHSource, error) {
	t, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	if t.Path == "" {
		return nil, fmt.Errorf("ssh URL %q needs a data directory path", rawURL)
	}
	return &SSHSource{SSH: SSH{Target: t, ControlDir: controlDir}}, nil
}

// Kind implements collector.Source.
func (s *SSHSource) Kind() string { return "ssh" }

// Local implements collector.Source.
func (s *SSHSource) Local() bool { return false }

// Close implements collector.Source.
func (s *SSHSource) Close() {}

var fileList = []string{"algod.net", "algod.pid", "config.json", "algod.token", "algod.admin.token"}

// Files reads the data directory files in one ssh command. Tokens stay in
// memory only and are never written to the hub's disk.
func (s *SSHSource) Files(ctx context.Context) (*collector.Files, error) {
	var b strings.Builder
	// ps -o etime works on Linux and macOS, unlike etimes.
	fmt.Fprintf(&b, "cd %s || exit 3; for f in %s; do printf '\\n==algodtop:%%s==\\n' \"$f\"; if [ -f \"$f\" ]; then cat \"$f\"; fi; done; "+
		"p=$(cat algod.pid 2>/dev/null); if [ -n \"$p\" ]; then printf '\\n==algodtop:etime==\\n'; ps -o etime= -p \"$p\" 2>/dev/null; fi; printf '\\n==algodtop:end==\\n'",
		remotePath(s.Target.Path), strings.Join(fileList, " "))
	script := "sh -c " + ShellQuote(b.String())
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.Run(cctx, script, nil)
	if err != nil {
		return nil, err
	}
	files := parseFiles(out)
	f := &collector.Files{}
	f.Net = strings.TrimSpace(files["algod.net"])
	f.PID, _ = strconv.Atoi(strings.TrimSpace(files["algod.pid"]))
	if d, ok := parseEtime(files["etime"]); ok && f.PID > 0 {
		f.Start = time.Now().Add(-d).Truncate(time.Second).UnixNano()
	}
	if raw, ok := files["config.json"]; ok && strings.TrimSpace(raw) != "" {
		c, err := algod.ParseConfig([]byte(raw))
		if err != nil {
			c = &algod.Config{Values: map[string]any{}, Hash: "unparseable"}
		}
		f.Config = c
	} else {
		f.Config = &algod.Config{Values: map[string]any{}, Hash: "defaults"}
	}
	f.Token = firstNonEmpty(s.Token, strings.TrimSpace(files["algod.token"]))
	f.AdminToken = firstNonEmpty(s.AdminToken, strings.TrimSpace(files["algod.admin.token"]))
	return f, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func parseFiles(out []byte) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var cur string
	var b strings.Builder
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "==algodtop:") && strings.HasSuffix(l, "==") {
			if cur != "" {
				m[cur] = strings.TrimSuffix(b.String(), "\n")
			}
			cur = strings.TrimSuffix(strings.TrimPrefix(l, "==algodtop:"), "==")
			b.Reset()
			if cur == "end" {
				cur = ""
			}
			continue
		}
		if cur != "" {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	// Only files that exist get an entry.
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
	return m
}

// Dialers returns ssh -W dialers to algod's REST port. algod speaks
// HTTP/1.1, one request at a time per connection, so the round watcher's
// long-poll and every other request each get their own ssh -W process.
// ControlMaster, where available, shares one TCP connection.
func (s *SSHSource) Dialers(addr string) (func(context.Context) (net.Conn, error), func(context.Context) (net.Conn, error)) {
	target := forwardTarget(addr)
	dial := func(ctx context.Context) (net.Conn, error) {
		cmd := exec.Command(s.bin(), s.Args("-W", target)...)
		c, err := DialCommand(cmd, s.Target.Host+"->"+target)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return dial, dial
}

// forwardTarget turns algod.net's listen address into a connect address:
// 0.0.0.0 and [::] become the loopback address.
func forwardTarget(addr string) string {
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// ErrNoSSH is returned when the ssh binary is missing.
var ErrNoSSH = errors.New("the ssh binary was not found in PATH")

// CheckSSH reports whether ssh is installed.
func CheckSSH() error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return ErrNoSSH
	}
	return nil
}

// RemoteBinary renders the agent path for the remote shell. Non-interactive
// SSH sessions often skip the shell profile, so the hub runs the agent by
// the absolute path recorded at install rather than relying on PATH.
func RemoteBinary(p string) string { return remotePath(p) }

// AgentBinary is the remote command for an agent at path. With no path it
// runs ~/.local/bin/algod.top, or ~/.local/bin/algodtop from before the
// rename, whichever exists.
func AgentBinary(path string) string {
	if path != "" {
		return remotePath(path)
	}
	return `sh -c 'b="$HOME/.local/bin/algod.top"; [ -x "$b" ] || b="$HOME/.local/bin/algodtop"; exec "$b" "$@"' algod.top`
}

// controlPath returns the ControlMaster socket for this target, or false
// when connection sharing is off. The name is a short hash of the target,
// not ssh's 40-character %C, and the path is quoted for ssh, because the
// macOS store lives under "Application Support". A unix socket path holds
// 104 bytes on macOS and 108 on Linux, and ssh adds a temporary suffix of
// up to 17 characters while it creates the socket, so a path that would
// not fit turns sharing off; each connection then opens its own session.
func (s *SSH) controlPath() (string, bool) {
	if s.ControlDir == "" || runtime.GOOS == "windows" || strings.ContainsAny(s.ControlDir, "\"\\%") {
		return "", false
	}
	sum := sha256.Sum256([]byte(s.Target.Dest() + ":" + s.Target.Port))
	p := filepath.Join(s.ControlDir, hex.EncodeToString(sum[:6]))
	limit := 104
	if runtime.GOOS == "linux" {
		limit = 108
	}
	if len(p)+17 >= limit {
		return "", false
	}
	return p, true
}

// parseEtime reads ps etime output, [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	sec := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		sec = sec*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(sec)*time.Second, true
}
