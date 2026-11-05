// Command algodtop-gen writes synthetic history into a store directory,
// for benchmarks of rollup, query and render times. It uses the Mainnet
// series mix from the /metrics fixture: gauges wander, counters grow, and
// one restart a day resets every counter.
//
//	go run ./internal/tools/algodtop-gen -dir /path/store -nodes 5 -days 30
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/promparse"
	"algod.top/internal/query"
	"algod.top/internal/store"
)

func main() {
	dir := flag.String("dir", "", "store directory (created)")
	nodes := flag.Int("nodes", 5, "nodes")
	days := flag.Float64("days", 7, "days of history")
	interval := flag.Duration("interval", 10*time.Second, "scrape interval")
	fixture := flag.String("fixture", "internal/fakealgod/mainnet.metrics", "metrics fixture")
	bench := flag.Bool("bench", true, "time queries after writing")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-dir is required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	res, err := promparse.Parse(bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	end := time.Now().Truncate(time.Hour)
	start := end.Add(-time.Duration(*days * float64(24*time.Hour)))
	now := start
	db, err := store.Open(store.Options{Dir: *dir, Now: func() time.Time { return now }, Retention: store.DefaultRetention,
		Caps: store.Caps{PerName: 1000, PerNode: 5000}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rnd := rand.New(rand.NewSource(1))
	type state struct {
		vals   []float64
		offset float64
	}
	st := make([]state, *nodes)
	for n := range st {
		st[n].vals = make([]float64, len(res.Series))
		for i, s := range res.Series {
			if s.Kind.IsCumulative() {
				st[n].vals[i] = 0
			} else {
				st[n].vals[i] = s.Value
			}
		}
	}
	steps := int(end.Sub(start) / *interval)
	t0 := time.Now()
	samples := 0
	for step := 0; step < steps; step++ {
		restart := step > 0 && step%int(24*time.Hour / *interval) == 0
		for n := 0; n < *nodes; n++ {
			b := &model.Batch{Node: fmt.Sprintf("node%d", n+1), Group: model.GroupMetrics, Time: now.UnixNano(), Reset: restart}
			b.Samples = make([]model.Sample, len(res.Series))
			for i, s := range res.Series {
				v := st[n].vals[i]
				switch {
				case restart && s.Kind.IsCumulative():
					v = 0
				case s.Kind.IsCumulative():
					// Most counters barely change; busy ones grow steadily.
					if s.Value > 1e6 {
						v += s.Value / 86400 * interval.Seconds() * (0.8 + 0.4*rnd.Float64())
					} else if rnd.Intn(10) == 0 {
						v++
					}
				default:
					if rnd.Intn(20) == 0 {
						v = math.Max(0, v+float64(rnd.Intn(3)-1))
					}
				}
				st[n].vals[i] = v
				b.Samples[i] = model.Sample{Name: s.Name, Labels: s.Labels, Kind: s.Kind, Value: v}
			}
			if err := db.Append(b); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			samples += len(b.Samples)
		}
		now = now.Add(*interval)
		if step%360 == 0 {
			db.Tick()
		}
		if step%(steps/20+1) == 0 {
			fmt.Fprintf(os.Stderr, "\r%3.0f%%  %s", float64(step)/float64(steps)*100, now.Format(time.DateTime))
		}
	}
	db.Tick()
	writeDur := time.Since(t0)
	t1 := time.Now()
	rep := db.Maintain()
	fmt.Fprintf(os.Stderr, "\rwrote %d samples (%d nodes, %.1f days) in %v (%.2f us/sample); maintenance %v, %d compactions\n",
		samples, *nodes, *days, writeDur.Round(time.Millisecond), float64(writeDur.Microseconds())/float64(samples), time.Since(t1).Round(time.Millisecond), rep.Compacted)
	sizes := db.Sizes()
	for _, t := range []string{"raw", "1m", "10m", "1h"} {
		perVal := float64(sizes[t]) / float64(samples)
		fmt.Printf("%-4s %10.1f MB   %.3f B per raw sample written\n", t, float64(sizes[t])/1e6, perVal)
	}
	if *bench {
		e := query.NewEngine(db, 2, time.Minute)
		for _, q := range []struct {
			name   string
			metric string
			rng    time.Duration
			tier   string
		}{
			{"1h rate", "algod_network_p2p_received_bytes_total", time.Hour, ""},
			{"24h rate", "algod_network_p2p_received_bytes_total", 24 * time.Hour, ""},
			{"7d rate", "algod_network_p2p_received_bytes_total", 7 * 24 * time.Hour, ""},
			{"all rate", "algod_network_p2p_received_bytes_total", end.Sub(start), ""},
			{"24h full-res", "algod_network_p2p_received_bytes_total", 24 * time.Hour, "raw"},
			{"7d gauge", "algod_network_peers", 7 * 24 * time.Hour, ""},
			{"24h p99 hist", "libp2p_swarm_dial_latency_seconds_bucket", 24 * time.Hour, ""},
		} {
			t := time.Now()
			r, err := e.Run(context.Background(), query.Request{Node: "node1", Metric: q.metric, From: end.Add(-q.rng).UnixNano(), To: end.UnixNano(), Points: 1000, Tier: q.tier})
			if err != nil {
				fmt.Printf("%-14s error %v\n", q.name, err)
				continue
			}
			tiers := ""
			for _, p := range r.Parts {
				tiers += p.Tier + " "
			}
			fmt.Printf("%-14s %8v  parts: %s\n", q.name, time.Since(t).Round(time.Microsecond), tiers)
		}
	}
	db.Close()
}
