package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"algod.top/internal/algod"
	"algod.top/internal/svc"
)

// Host describes a remote machine.
type Host struct {
	OS   string // linux, darwin, windows
	Arch string // amd64, arm64
	User string
	Home string
}

// Detect finds the remote OS and architecture over SSH.
func (s *SSH) Detect(ctx context.Context) (Host, error) {
	out, err := s.Run(ctx, `sh -c 'uname -s; uname -m; id -un; printf "%s\n" "$HOME"'`, nil)
	if err == nil {
		f := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(f) >= 4 {
			h := Host{OS: strings.ToLower(strings.TrimSpace(f[0])), Arch: normArch(strings.TrimSpace(f[1])), User: strings.TrimSpace(f[2]), Home: strings.TrimSpace(f[3])}
			return h, nil
		}
	}
	// The default Windows OpenSSH shell is cmd.
	out, err2 := s.Run(ctx, `powershell -NoProfile -Command "$env:PROCESSOR_ARCHITECTURE; $env:USERNAME; $env:LOCALAPPDATA"`, nil)
	if err2 == nil {
		f := strings.Fields(strings.TrimSpace(string(out)))
		if len(f) >= 3 {
			return Host{OS: "windows", Arch: normArch(f[0]), User: f[1], Home: f[2]}, nil
		}
	}
	if err != nil {
		return Host{}, err
	}
	return Host{}, fmt.Errorf("cannot detect the remote OS: %q", out)
}

func normArch(a string) string {
	switch strings.ToLower(a) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	}
	return strings.ToLower(a)
}

// DefaultPath is where install puts the agent.
func DefaultPath(h Host) string {
	if h.OS == "windows" {
		return h.Home + `\algodtop\algodtop.exe`
	}
	return "~/.local/bin/algodtop"
}

// Copy writes the binary through the ssh pipe, so no scp is needed. Unix
// remotes get it through cat. Windows remotes get it through PowerShell,
// because the default Windows OpenSSH shell is cmd.
func (s *SSH) Copy(ctx context.Context, h Host, path string, bin []byte) error {
	if h.OS == "windows" {
		ps := fmt.Sprintf(`$d=Split-Path -Parent '%[1]s'; New-Item -ItemType Directory -Force -Path $d | Out-Null; $i=[Console]::OpenStandardInput(); $o=[IO.File]::Create('%[1]s.tmp'); $i.CopyTo($o); $o.Close(); Move-Item -Force '%[1]s.tmp' '%[1]s'`, path)
		_, err := s.Run(ctx, "powershell -NoProfile -Command \""+ps+"\"", bytes.NewReader(bin))
		return err
	}
	p := remotePath(path)
	script := fmt.Sprintf(`set -e; p=%s; mkdir -p "$(dirname "$p")"; cat > "$p.tmp"; chmod 755 "$p.tmp"; mv -f "$p.tmp" "$p"`, p)
	_, err := s.Run(ctx, "sh -c "+ShellQuote(script), bytes.NewReader(bin))
	return err
}

// WriteFile writes a small file on the remote with mode 0600.
func (s *SSH) WriteFile(ctx context.Context, h Host, path string, data []byte) error {
	if h.OS == "windows" {
		return s.Copy(ctx, h, path, data)
	}
	p := remotePath(path)
	script := fmt.Sprintf(`set -e; p=%s; mkdir -p "$(dirname "$p")"; umask 077; cat > "$p.tmp"; mv -f "$p.tmp" "$p"`, p)
	_, err := s.Run(ctx, "sh -c "+ShellQuote(script), bytes.NewReader(data))
	return err
}

// Discover runs discovery on the remote with the installed binary.
func (s *SSH) Discover(ctx context.Context, binPath string) ([]algod.Found, error) {
	out, err := s.Run(ctx, RemoteBinary(binPath)+" discover --json", nil)
	if err != nil {
		return nil, err
	}
	var f []algod.Found
	if err := json.Unmarshal(out, &f); err != nil {
		return nil, fmt.Errorf("remote discovery output: %w", err)
	}
	return f, nil
}

// Owners returns the owner of each path, to check that the agent runs as
// the account that owns the algod data directories.
func (s *SSH) Owners(ctx context.Context, h Host, paths []string) map[string]string {
	out := map[string]string{}
	if h.OS == "windows" || len(paths) == 0 {
		return out
	}
	flag := "-c %U"
	if h.OS == "darwin" {
		flag = "-f %Su"
	}
	var quoted []string
	for _, p := range paths {
		quoted = append(quoted, ShellQuote(p))
	}
	res, err := s.Run(ctx, "sh -c "+ShellQuote("for p in "+strings.Join(quoted, " ")+`; do printf '%s ' "$p"; stat `+flag+` "$p" 2>/dev/null || echo '?'; done`), nil)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(strings.TrimSpace(string(res)), "\n") {
		i := strings.LastIndexByte(l, ' ')
		if i > 0 {
			out[l[:i]] = l[i+1:]
		}
	}
	return out
}

// ServicePlan describes the user-level service install.
type ServicePlan struct {
	Kind    string // systemd, launchd, windows
	File    string // unit or plist path on the remote
	Content []byte
	Start   string // command to enable and start
	Stop    string // command to stop and disable
	Restart string
	Hints   []string
}

// PlanService builds the service files for the remote OS.
func PlanService(h Host, binPath string) ServicePlan {
	switch h.OS {
	case "linux":
		unit := svc.SystemdUnit(svc.Spec{Name: "algodtop-agent", Description: "algodtop agent", Args: []string{binPath, "agent"}})
		return ServicePlan{Kind: "systemd", File: "~/.config/systemd/user/algodtop-agent.service", Content: []byte(unit),
			Start:   "systemctl --user daemon-reload && systemctl --user enable --now algodtop-agent",
			Stop:    "systemctl --user disable --now algodtop-agent",
			Restart: "systemctl --user restart algodtop-agent",
			Hints:   []string{"A systemd user unit needs lingering to run without a login session: sudo loginctl enable-linger " + h.User}}
	case "darwin":
		bin := strings.Replace(binPath, "~", h.Home, 1)
		plist := svc.LaunchdPlist(svc.Spec{Label: "top.algod.agent", Args: []string{bin, "agent"}, Log: h.Home + "/Library/Logs/algodtop-agent.log"})
		return ServicePlan{Kind: "launchd", File: "~/Library/LaunchAgents/top.algod.agent.plist", Content: []byte(plist),
			Start:   `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/top.algod.agent.plist || launchctl load -w ~/Library/LaunchAgents/top.algod.agent.plist`,
			Stop:    `launchctl bootout gui/$(id -u)/top.algod.agent || launchctl unload ~/Library/LaunchAgents/top.algod.agent.plist`,
			Restart: `launchctl kickstart -k gui/$(id -u)/top.algod.agent`}
	}
	return ServicePlan{Kind: "windows", Hints: []string{fmt.Sprintf("Windows: a service that runs as a user needs that user's password. On the host run: %s agent --install-service", binPath)}}
}

// Shell runs a command through sh on a Unix remote.
func (s *SSH) Shell(ctx context.Context, script string) ([]byte, error) {
	return s.Run(ctx, "sh -c "+ShellQuote(script), nil)
}
