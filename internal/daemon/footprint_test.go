package daemon_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"algod.top/internal/config"
	"algod.top/internal/fakealgod"
	"algod.top/internal/fsx"
	"algod.top/internal/sysstat"
)

// TestFootprint runs the daemon against five Mainnet-sized fake nodes and
// fails if CPU, RSS or disk writes exceed the budget in DESIGN.md section
// 16: under 0.5% of one core, under 64 MB RSS and under 50 KB/s of disk
// writes. It runs for ALGODTOP_FOOTPRINT (for example 1h) and is skipped
// otherwise.
func TestFootprint(t *testing.T) {
	spec := os.Getenv("ALGODTOP_FOOTPRINT")
	if spec == "" {
		t.Skip("set ALGODTOP_FOOTPRINT=1h to run")
	}
	dur, err := time.ParseDuration(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := shortDir(t)
	var nodes []config.Node
	for i := range 5 {
		dd := filepath.Join(dir, fmt.Sprintf("node%d", i))
		// About 170 agreement trace lines per second, as Mainnet logs.
		f, err := fakealgod.Start(fakealgod.Options{DataDir: dd, RoundEvery: 2800 * time.Millisecond, ChurnEvery: 30 * time.Second,
			AdminToken: "a", DisableAuth: true, PeerUsage: true, Peers: 100, LogTraceRate: 480, Seed: int64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		nodes = append(nodes, config.Node{Name: fmt.Sprintf("node%d", i), DataDir: dd})
	}
	empty := ""
	cfg := &config.Config{Path: filepath.Join(dir, "config.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &empty},
		Nodes: nodes, NTP: config.NTP{Disable: true}}
	if err := cfg.Save(cfg.Path); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	cmd := exec.Command(self, "daemon", "-c", cfg.Path)
	cmd.Env = append(os.Environ(), "ALGODTOP_TEST_ROLE=algodtop")
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()
	sys := sysstat.New()
	defer sys.Close()
	pid := cmd.Process.Pid
	// Let it settle: the first scrape of every node and the first flush.
	time.Sleep(2 * time.Minute)
	p0 := sys.Procs([]int{pid})[pid]
	size0 := fsx.DirSize(cfg.StorePath())
	t0 := time.Now()
	var maxRSS float64
	for time.Since(t0) < dur {
		time.Sleep(10 * time.Second)
		if p, ok := sys.Procs([]int{pid})[pid]; ok {
			maxRSS = max(maxRSS, p.RSSBytes)
		}
	}
	p1 := sys.Procs([]int{pid})[pid]
	secs := time.Since(t0).Seconds()
	cpu := (p1.CPUSeconds - p0.CPUSeconds) / secs * 100
	var writeBPS float64
	if runtime.GOOS == "linux" && p1.WriteBytes >= 0 {
		writeBPS = (p1.WriteBytes - p0.WriteBytes) / secs
	} else {
		// No per-process disk I/O on macOS: the store's growth is a lower
		// bound, and the WAL is the bulk of what is rewritten.
		writeBPS = float64(fsx.DirSize(cfg.StorePath())-size0) / secs
	}
	t.Logf("five nodes over %v: CPU %.2f%% of one core, max RSS %.1f MB, disk writes %.1f KB/s", dur, cpu, maxRSS/1e6, writeBPS/1024)
	if cpu > 0.5 {
		t.Errorf("CPU %.2f%% is over the 0.5%% budget", cpu)
	}
	if maxRSS > 64<<20 {
		t.Errorf("RSS %.1f MB is over the 64 MB budget", maxRSS/1e6)
	}
	if writeBPS > 50*1024 {
		t.Errorf("disk writes %.1f KB/s are over the 50 KB/s budget", writeBPS/1024)
	}
}
