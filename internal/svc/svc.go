// Package svc renders systemd units and launchd plists for the daemon and
// the agent. `algodtop service` uses it on the local machine and
// `algodtop remote install` uses it for a remote host, so both write the
// same files.
package svc

import (
	"fmt"
	"sort"
	"strings"
)

// Spec describes one service.
type Spec struct {
	Name        string            // unit name without .service, for example algodtop
	Label       string            // launchd label, for example top.algod.daemon
	Description string            // systemd Description
	Args        []string          // command line; Args[0] is the binary
	System      bool              // a system unit, started at boot
	User, Group string            // system units only
	StateDir    string            // systemd StateDirectory, system units only
	After       []string          // extra units to order after, system units only
	Env         map[string]string // launchd only
	Log         string            // launchd stdout and stderr file
}

// Header is the first line of every file this package writes.
const Header = "Written by algodtop. `algodtop service install` rewrites it and `algodtop service uninstall` removes it."

// SystemdUnit renders a systemd unit with restart-on-failure and the low
// CPU and I/O priority from DESIGN.md section 16.
func SystemdUnit(s Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n[Unit]\nDescription=%s\n", Header, s.Description)
	after := append([]string{"network-online.target"}, s.After...)
	fmt.Fprintf(&b, "After=%s\n", strings.Join(after, " "))
	if s.System {
		b.WriteString("Wants=network-online.target\n")
	}
	b.WriteString("\n[Service]\n")
	if s.System {
		if s.User != "" {
			fmt.Fprintf(&b, "User=%s\n", s.User)
		}
		if s.Group != "" {
			fmt.Fprintf(&b, "Group=%s\n", s.Group)
		}
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", SystemdExec(s.Args))
	b.WriteString("ExecReload=/bin/kill -HUP $MAINPID\n")
	if s.System && s.StateDir != "" {
		fmt.Fprintf(&b, "StateDirectory=%s\nStateDirectoryMode=0750\n", s.StateDir)
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\nTimeoutStopSec=15\n")
	b.WriteString("Nice=10\nIOSchedulingClass=best-effort\nIOSchedulingPriority=7\n")
	if s.System {
		b.WriteString("NoNewPrivileges=true\n")
	}
	b.WriteString("\n[Install]\n")
	if s.System {
		b.WriteString("WantedBy=multi-user.target\n")
	} else {
		b.WriteString("WantedBy=default.target\n")
	}
	return b.String()
}

// SystemdExec quotes a command line for ExecStart.
func SystemdExec(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = systemdQuote(a)
	}
	return strings.Join(q, " ")
}

// systemdQuote quotes one word for a unit file. systemd expands % and $
// specifiers, splits on white space and reads C-style escapes inside
// double quotes. It never expands ~, so a leading ~/ becomes %h/, the
// home directory of the unit's user.
func systemdQuote(s string) string {
	home := strings.HasPrefix(s, "~/")
	if home {
		s = s[2:]
	}
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	if home {
		s = "%h/" + s
	}
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// LaunchdPlist renders a launchd user agent that starts at login and
// restarts after a crash.
func LaunchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
`)
	fmt.Fprintf(&b, "<!-- %s -->\n<plist version=\"1.0\">\n<dict>\n", xmlEscape(strings.ReplaceAll(Header, "--", "-")))
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", xmlEscape(s.Label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range s.Args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("  </array>\n")
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", xmlEscape(k), xmlEscape(s.Env[k]))
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString(`  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ExitTimeOut</key><integer>15</integer>
  <key>Nice</key><integer>10</integer>
  <key>LowPriorityIO</key><true/>
  <key>ProcessType</key><string>Background</string>
`)
	if s.Log != "" {
		fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", xmlEscape(s.Log))
		fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlEscape(s.Log))
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
