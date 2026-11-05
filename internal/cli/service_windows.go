//go:build windows

package cli

import (
	"errors"
	"fmt"
)

func installService(agent bool) int {
	fmt.Print(windowsServiceHelp(agent))
	return 0
}

func isRoot() bool { return false }

func fileOwner(string) (name string, uid, gid uint32, err error) {
	return "", 0, 0, errors.New("file owners are not checked on Windows")
}
