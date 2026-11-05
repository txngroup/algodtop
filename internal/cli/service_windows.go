//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func init() { winSvc = scm{} }

// installService keeps `daemon --install-service` working as another name
// for `service install`.
func installService(agent bool) int {
	return windowsService("install", currentEnv(), svcOpts{agent: agent})
}

func isRoot() bool { return false }

// runsAs is not needed on Windows: the service runs as LocalSystem.
func runsAs(string, string) error { return nil }

func fileOwner(string) (name string, uid, gid uint32, err error) {
	return "", 0, 0, errors.New("file owners are not checked on Windows")
}

// scm manages services through the Windows service control manager.
type scm struct{}

func (scm) elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// exists and state ask only for the rights a non-administrator has, so
// stdio and `service status` work from an ordinary prompt too.
func (scm) exists(name string) bool {
	_, err := queryStatus(name)
	return err == nil
}

func queryStatus(name string) (windows.SERVICE_STATUS, error) {
	var st windows.SERVICE_STATUS
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return st, err
	}
	defer windows.CloseServiceHandle(m)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return st, err
	}
	h, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return st, err
	}
	defer windows.CloseServiceHandle(h)
	err = windows.QueryServiceStatus(h, &st)
	return st, err
}

func open(name string) (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(name)
	if err != nil {
		m.Disconnect()
		return nil, nil, err
	}
	return m, s, nil
}

func (scm) state(name string) (string, error) {
	st, err := queryStatus(name)
	if err != nil {
		return "", err
	}
	return map[svc.State]string{svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running",
		svc.ContinuePending: "resuming", svc.PausePending: "pausing", svc.Paused: "paused"}[svc.State(st.CurrentState)], nil
}

func (scm) start(name string) error {
	m, s, err := open(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return s.Start()
}

func (scm) stop(name string, wait time.Duration) error {
	m, s, err := open(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	deadline := time.Now().Add(wait)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still not stopped after %s", wait)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// install registers the service, or updates it, to run as LocalSystem from
// boot. A failure, including the stop after an upgrade, restarts it.
func (scm) install(p winPlan) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	c := mgr.Config{StartType: mgr.StartAutomatic, DisplayName: p.display,
		Description: "algod.top " + p.role + ": monitors Algorand nodes. https://algod.top"}
	s, err := m.OpenService(p.name)
	if err == nil {
		old, err := s.Config()
		if err != nil {
			s.Close()
			return err
		}
		old.StartType, old.DisplayName, old.Description, old.ServiceStartName = c.StartType, c.DisplayName, c.Description, "LocalSystem"
		old.BinaryPathName = windows.EscapeArg(p.bin) + " " + joinArgs(p.args)
		if err := s.UpdateConfig(old); err != nil {
			s.Close()
			return err
		}
	} else {
		s, err = m.CreateService(p.name, p.bin, c, p.args...)
		if err != nil {
			return err
		}
	}
	defer s.Close()
	actions := []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 2 * time.Second}, {Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second}}
	if err := s.SetRecoveryActions(actions, 86400); err != nil {
		return err
	}
	// An upgrade stops the service with an error code, and the recovery
	// actions bring it back on the new binary.
	return s.SetRecoveryActionsOnNonCrashFailures(true)
}

func joinArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = windows.EscapeArg(a)
	}
	return strings.Join(q, " ")
}

func (scm) remove(name string) error {
	m, s, err := open(name)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return s.Delete()
}

// secureDir gives the directory and everything under it to SYSTEM and
// Administrators alone, without the rights Users inherit in ProgramData.
func (scm) secureDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// underServiceManager reports whether the service control manager started
// this process.
func underServiceManager() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// serviceLog is where a service logs, since no console is attached: log\
// algod.top.log beside its config, moved to .1 at 10 MB.
func serviceLog(cfgPath string) io.Writer {
	if cfgPath == "" {
		return io.Discard
	}
	return &rotatingFile{path: filepath.Join(filepath.Dir(cfgPath), "log", "algod.top.log"), max: 10 << 20}
}

type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func (r *rotatingFile) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil || r.size+int64(len(b)) > r.max {
		if r.f != nil {
			r.f.Close()
			r.f = nil
			os.Rename(r.path, r.path+".1")
		}
		os.MkdirAll(filepath.Dir(r.path), 0o700)
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return len(b), nil // a log it cannot write must not stop the service
		}
		r.f, r.size = f, 0
		if fi, err := f.Stat(); err == nil {
			r.size = fi.Size()
		}
	}
	n, err := r.f.Write(b)
	r.size += int64(n)
	return n, err
}

// runService runs the daemon or agent under the service control manager.
// When run returns after an upgrade, the service stops with an error code,
// so its recovery action starts it again from the new binary.
func runService(name string, run func(context.Context) error, restarting func() bool, logf func(string, ...any)) int {
	if err := svc.Run(name, &serviceHandler{run: run, restarting: restarting, logf: logf}); err != nil {
		logf("service: %v", err)
		return 1
	}
	return 0
}

type serviceHandler struct {
	run        func(context.Context) error
	restarting func() bool
	logf       func(string, ...any)
}

func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				if err := <-done; err != nil {
					h.logf("%v", err)
					return true, 2
				}
				return false, 0
			}
		case err := <-done:
			if h.restarting() {
				h.logf("upgraded; stopping so the service manager restarts it from the new binary")
				return true, 1
			}
			if err != nil {
				h.logf("%v", err)
				return true, 2
			}
			return false, 0
		}
	}
}

// removeOldBinary deletes the copy an upgrade moved aside, which Windows
// would not let it delete while it ran.
func removeOldBinary() {
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}
