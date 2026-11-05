//go:build !windows

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
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

// runsAs starts a binary as an account with `version --json`, to check
// that the account can run it. A filesystem mounted noexec fails here.
func runsAs(path, account string) error {
	u, err := user.Lookup(account)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cmd := exec.Command(path, "version", "--json")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Only Windows has a service control manager to run under.
func underServiceManager() bool { return false }

func serviceLog(string) io.Writer { return io.Discard }

func runService(string, func(context.Context) error, func() bool, func(string, ...any)) int { return 1 }

func removeOldBinary() {}
