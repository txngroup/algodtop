//go:build !windows

package config

import (
	"fmt"
	"os"
)

func checkPerm(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("warning: %s is readable by other users (mode %04o); it holds tokens and the password hash. Run chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	return ""
}
