package daemon

import (
	"context"
	"os"
	"runtime/pprof"
	"strings"
	"time"
)

// startProfile writes a CPU profile when ALGODTOP_CPUPROFILE is set to
// path[,seconds]. It starts 30 s after launch so startup does not dominate.
func (d *Daemon) startProfile(ctx context.Context) {
	spec := os.Getenv("ALGODTOP_CPUPROFILE")
	if spec == "" {
		return
	}
	path, secs, _ := strings.Cut(spec, ",")
	dur := 60 * time.Second
	if secs != "" {
		if v, err := time.ParseDuration(secs + "s"); err == nil {
			dur = v
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		f, err := os.Create(path)
		if err != nil {
			d.logf("cpu profile: %v", err)
			return
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		select {
		case <-ctx.Done():
		case <-time.After(dur):
		}
		pprof.StopCPUProfile()
		d.logf("cpu profile written to %s", path)
		if mp := os.Getenv("ALGODTOP_MEMPROFILE"); mp != "" {
			if mf, err := os.Create(mp); err == nil {
				pprof.Lookup("allocs").WriteTo(mf, 0)
				mf.Close()
			}
		}
	}()
}
