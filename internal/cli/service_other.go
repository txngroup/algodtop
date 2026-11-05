//go:build !windows

package cli

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// installService keeps `daemon --install-service` working on Unix as
// another name for `service install`.
func installService(agent bool) int {
	args := []string{"install"}
	if agent {
		args = append(args, "--agent")
	}
	return cmdService(args)
}

func isRoot() bool { return os.Geteuid() == 0 }

// fileOwner returns the account that owns a file.
func fileOwner(path string) (name string, uid, gid uint32, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, 0, os.ErrInvalid
	}
	name = strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	return name, st.Uid, st.Gid, nil
}
