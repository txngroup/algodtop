package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/release"
	"algod.top/internal/version"
)

// Upgrade implements api.Hub. It installs a signed release over the
// running binary, then stops so the process can start again from it:
// Run returns, and the command that called it execs the new binary in
// the same process, which a service manager keeps tracking. Windows
// cannot exec: a Windows service stops with an error code and its
// recovery action restarts it, and elsewhere on Windows the new binary
// waits for the next start.
func (d *Daemon) Upgrade(ctx context.Context, f release.Files) (release.Result, error) {
	d.upgradeMu.Lock()
	defer d.upgradeMu.Unlock()
	if d.restartExe.Load() != nil {
		return release.Result{}, errors.New("an upgrade is already restarting this " + d.role())
	}
	exe := d.o.Exe
	if exe == "" {
		var err error
		if exe, err = release.Executable(); err != nil {
			return release.Result{}, err
		}
	}
	res, err := release.Install(ctx, f, exe, version.Info())
	if err != nil {
		d.logf("upgrade refused: %v", err)
		return res, err
	}
	if res.Same {
		return res, nil
	}
	d.logf("installed %s over %s in %s", res.To, res.From, res.Path)
	if (runtime.GOOS == "windows" && !d.o.Supervised) || d.stop == nil {
		return res, nil
	}
	d.emitEvent(model.Event{Kind: "algodtop-upgrade", Severity: model.SevInfo, Msg: fmt.Sprintf("algodtop upgraded from %s to %s and restarts", res.From, res.To)})
	res.Restart = true
	d.restartExe.Store(&exe)
	// The response goes out before the listeners shut down.
	go func() {
		time.Sleep(300 * time.Millisecond)
		d.stop()
	}()
	return res, nil
}

// RestartExe is the binary to exec once Run returns, after an upgrade,
// or "" when the daemon stopped for any other reason.
func (d *Daemon) RestartExe() string {
	if p := d.restartExe.Load(); p != nil {
		return *p
	}
	return ""
}

func (d *Daemon) role() string {
	if d.o.Agent {
		return "agent"
	}
	return "daemon"
}

// updateLoop looks for a newer release a minute after start and then
// daily, on the hub only: agents follow the hub when it upgrades them.
// It only reports. algodtop upgrade installs.
func (d *Daemon) updateLoop(ctx context.Context) {
	if d.o.Agent || !d.Config().UpdateCheckOn() || release.PublicKey == "" {
		return
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		v, err := release.Latest(ctx)
		if err != nil {
			d.logf("update check: %v", err)
		} else if newer(v, version.Info()) {
			d.latest.Store(&v)
		} else {
			d.latest.Store(nil)
		}
		t.Reset(24 * time.Hour)
	}
}

// newer reports whether release v was made after this build's commit.
func newer(v string, b version.Build) bool {
	t, ok := version.TimeOf(v)
	return ok && !b.Time.IsZero() && t.After(b.Time)
}

// latestStatus fills in a newer release for Status.
func (d *Daemon) latestStatus() (string, int64) {
	p := d.latest.Load()
	if p == nil || !newer(*p, version.Info()) {
		return "", 0
	}
	t, _ := version.TimeOf(*p)
	return *p, t.UnixNano()
}
