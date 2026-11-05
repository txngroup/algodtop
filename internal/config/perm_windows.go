//go:build windows

package config

import (
	"os/exec"
	"strings"
)

// checkPerm warns when the file's ACL grants read access beyond the user,
// SYSTEM and Administrators. It reads the ACL with icacls, which ships with
// every Windows release.
func checkPerm(path string) string {
	out, err := exec.Command("icacls", path).Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		u := strings.ToUpper(l)
		for _, who := range []string{"EVERYONE", "BUILTIN\\USERS", "AUTHENTICATED USERS"} {
			if strings.Contains(u, who) {
				return "warning: " + path + " grants read access to " + strings.TrimSpace(l) + "; it holds tokens and the password hash"
			}
		}
	}
	return ""
}
