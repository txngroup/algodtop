package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"algod.top/internal/version"
)

// BuildInfo is what `algodtop version --json` prints. An upgrade runs the
// new binary to read it, because the signature covers the binary but not
// its version.
type BuildInfo struct {
	Version     string `json:"version"`
	VersionTime int64  `json:"version_time,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Platform    string `json:"platform"`
}

// Result says what Install did.
type Result struct {
	From string `json:"from"`
	To   string `json:"to"`
	Path string `json:"path"`
	// Same means the binary already runs this release, so nothing changed.
	Same bool `json:"same,omitempty"`
	// Restart means the daemon or agent is restarting onto the new binary.
	// Without it, the new binary runs from the next start.
	Restart bool `json:"restart,omitempty"`
}

// Error codes for an install that did not happen.
const (
	CodePlatform    = "platform"     // a binary for another OS or architecture
	CodeSignature   = "signature"    // not signed by the release key
	CodeNotWritable = "not-writable" // the running binary's directory is read-only to us
	CodeUnreadable  = "unreadable"   // the new binary did not report its version
	CodeOlder       = "older"        // not newer than the running build
)

// InstallError explains why Install refused. Code is one of the Code
// constants, so the hub can act on it.
type InstallError struct {
	Code string `json:"code"`
	Msg  string `json:"error"`
}

func (e *InstallError) Error() string { return e.Msg }

// IsCode reports whether err is an InstallError with this code.
func IsCode(err error, code string) bool {
	var ie *InstallError
	return errors.As(err, &ie) && ie.Code == code
}

// ReadBuildInfo runs a binary with `version --json`. Tests replace it.
var ReadBuildInfo = func(ctx context.Context, path string) (BuildInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version", "--json").Output()
	if err != nil {
		return BuildInfo{}, err
	}
	var b BuildInfo
	if err := json.Unmarshal(out, &b); err != nil {
		return BuildInfo{}, fmt.Errorf("it printed no version JSON, so it predates this check")
	}
	return b, nil
}

// Install replaces the binary at exe with a verified release, the one
// the process runs from. It refuses a binary for another platform, one
// the release key did not sign, and one that is not newer than cur, so
// whoever can ask for an upgrade can only move algodtop forward to a
// genuine release. The new file is written beside exe and renamed over
// it, so the running process keeps its old copy until it restarts.
func Install(ctx context.Context, f Files, exe string, cur version.Build) (Result, error) {
	res := Result{From: cur.Version, Path: exe}
	if want := BinaryName(runtime.GOOS, runtime.GOARCH); f.Name != want && f.Name != legacyName(runtime.GOOS, runtime.GOARCH) {
		return res, &InstallError{CodePlatform, fmt.Sprintf("this is %s/%s, which needs %s, not %s", runtime.GOOS, runtime.GOARCH, want, f.Name)}
	}
	if err := f.Verify(); err != nil {
		return res, &InstallError{CodeSignature, err.Error()}
	}
	pattern := ".algod.top-new-*"
	if runtime.GOOS == "windows" {
		pattern += ".exe" // so Windows will run it for its version
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), pattern)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
			return res, &InstallError{CodeNotWritable, fmt.Sprintf("cannot replace %s: this account may not write %s", exe, filepath.Dir(exe))}
		}
		return res, err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(f.Binary); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return res, err
	}
	info, err := ReadBuildInfo(ctx, name)
	if err != nil {
		return res, &InstallError{CodeUnreadable, fmt.Sprintf("the new binary did not report its version: %v", err)}
	}
	res.To = info.Version
	if info.Version == cur.Version {
		res.Same = true
		return res, nil
	}
	if curT, ok := version.TimeOf(cur.Version); ok {
		newT, ok := version.TimeOf(info.Version)
		if !ok || !newT.After(curT) {
			return res, &InstallError{CodeOlder, fmt.Sprintf("%s is not newer than the running %s", info.Version, cur.Version)}
		}
	}
	if runtime.GOOS == "windows" {
		// Windows cannot rename over a running executable, but it can
		// rename the running one aside.
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return res, err
		}
	}
	if err := os.Rename(name, exe); err != nil {
		return res, err
	}
	done = true
	return res, nil
}

// Executable is the running binary's real path, the one Install replaces.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
