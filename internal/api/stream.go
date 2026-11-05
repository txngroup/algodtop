package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/fsx"
	"algod.top/internal/model"
)

// handleStream is one multiplexed Server-Sent Events stream covering every
// node a client shows. Browsers allow six HTTP/1.1 connections per origin,
// so one stream per node would stall chart fetches.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	q := r.URL.Query()
	nodes := map[string]bool{}
	for _, n := range strings.Split(q.Get("nodes"), ",") {
		if n != "" {
			nodes[n] = true
		}
	}
	var subs []Sub
	for _, sv := range q["series"] {
		node, metric, ok := strings.Cut(sv, ":")
		if ok {
			subs = append(subs, Sub{Node: node, Metric: metric})
		}
	}
	ch, cancel := s.hub.Subscribe(nodes, subs)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		case m, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(m.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Event, b)
			fl.Flush()
		}
	}
}

// handleSelfMetrics serves algodtop's own metrics in Prometheus text.
func (s *Server) handleSelfMetrics(w http.ResponseWriter, r *http.Request) {
	st := s.hub.Status()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	p := func(name, typ, help string, v float64, labels ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		lbl := ""
		if len(labels) > 0 {
			lbl = "{" + strings.Join(labels, ",") + "}"
		}
		fmt.Fprintf(w, "%s%s %g\n", name, lbl, v)
	}
	p("algodtop_cpu_percent", "gauge", "algodtop CPU use, percent of one core", st.Self.CPU)
	p("algodtop_resident_bytes", "gauge", "algodtop resident memory", st.Self.RSS)
	p("algodtop_heap_bytes", "gauge", "Go heap in use", st.Self.Heap)
	p("algodtop_goroutines", "gauge", "goroutines", float64(st.Self.Goroutines))
	p("algodtop_write_queue_depth", "gauge", "batches waiting for the writer", float64(st.Self.QueueDepth))
	p("algodtop_write_queue_drops_total", "counter", "batches dropped because the writer was stuck", float64(st.Self.QueueDrops))
	p("algodtop_samples_total", "counter", "samples stored", float64(st.Self.Samples))
	p("algodtop_late_batches_total", "counter", "batches routed to the late-data path", float64(st.Self.Late))
	p("algodtop_dropped_samples_total", "counter", "samples dropped by series caps", float64(st.Self.Dropped))
	p("algodtop_store_bytes", "gauge", "store size", float64(st.StoreBytes))
	fmt.Fprintf(w, "# HELP algodtop_tier_bytes block bytes per tier\n# TYPE algodtop_tier_bytes gauge\n")
	tiers := make([]string, 0, len(st.TierBytes))
	for t := range st.TierBytes {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)
	for _, t := range tiers {
		fmt.Fprintf(w, "algodtop_tier_bytes{tier=%q} %d\n", t, st.TierBytes[t])
	}
	fmt.Fprintf(w, "# HELP algodtop_node_health node health: 0 ok, 1 warn, 2 critical\n# TYPE algodtop_node_health gauge\n")
	for _, n := range s.hub.Nodes() {
		fmt.Fprintf(w, "algodtop_node_health{node=%q} %d\n", n.Name, n.Health.Level)
	}
	fmt.Fprintf(w, "# HELP algodtop_node_lag_seconds seconds since the last round\n# TYPE algodtop_node_lag_seconds gauge\n")
	for _, n := range s.hub.Nodes() {
		if !math.IsNaN(n.Lag) {
			fmt.Fprintf(w, "algodtop_node_lag_seconds{node=%q} %g\n", n.Name, n.Lag)
		}
	}
}

func listenUnix(path, group string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A stale socket from a crash blocks the bind. Remove it only when no
	// daemon answers on it.
	if c, err := net.DialTimeout("unix", fsx.SocketAddr(path), 500*time.Millisecond); err == nil {
		c.Close()
		return nil, fmt.Errorf("another algodtop daemon is already listening on %s", path)
	}
	os.Remove(path)
	ln, err := net.Listen("unix", fsx.SocketAddr(path))
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		mode := os.FileMode(0o600)
		if group != "" {
			g, err := user.LookupGroup(group)
			if err != nil {
				ln.Close()
				return nil, fmt.Errorf("socket group: %w", err)
			}
			gid, _ := strconv.Atoi(g.Gid)
			if err := os.Chown(path, -1, gid); err != nil {
				ln.Close()
				return nil, fmt.Errorf("socket group %s: %w", group, err)
			}
			mode = 0o660
		}
		if err := os.Chmod(path, mode); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

var _ = model.Event{}
