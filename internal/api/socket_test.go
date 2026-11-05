//go:build !windows

package api

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestListenUnixGroup(t *testing.T) {
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	u, _ := user.Current()
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skip("no name for the primary group")
	}
	for _, c := range []struct {
		group string
		mode  os.FileMode
	}{{"", 0o600}, {g.Name, 0o660}} {
		p := filepath.Join(dir, "s.sock")
		ln, err := ListenUnix(p, c.group)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != c.mode {
			t.Errorf("group %q: mode %v", c.group, fi.Mode().Perm())
		}
		if gid := strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Gid)); c.group != "" && gid != g.Gid {
			t.Errorf("gid %s, want %s", gid, g.Gid)
		}
		ln.Close()
		os.Remove(p)
	}
	if _, err := ListenUnix(filepath.Join(dir, "x.sock"), "no-such-group-algodtop"); err == nil {
		t.Error("an unknown group was accepted")
	}
}
