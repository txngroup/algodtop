package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// BatchOptions configure batch mode, like top -b.
type BatchOptions struct {
	Source     Source
	Iterations int // 0 or 1 prints once
	Interval   time.Duration
	JSON       bool
	UTC        bool
	Width      int
	Warmup     time.Duration // live mode needs a scrape before the first print
}

// Batch prints the node table and exits. It works over SSH and in cron
// without a terminal.
func Batch(w io.Writer, o BatchOptions) error {
	if o.Iterations <= 0 {
		o.Iterations = 1
	}
	if o.Interval <= 0 {
		o.Interval = 2 * time.Second
	}
	if o.Width <= 0 {
		o.Width = 160
	}
	if o.Source.Live() && o.Warmup > 0 {
		time.Sleep(o.Warmup)
	}
	for i := 0; i < o.Iterations; i++ {
		if i > 0 {
			time.Sleep(o.Interval)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		nodes, st, err := o.Source.Nodes(ctx)
		if err != nil {
			cancel()
			return err
		}
		now := time.Now()
		evs, _ := o.Source.Events(ctx, "", now.Add(-time.Hour).UnixNano(), now.Add(time.Minute).UnixNano(), 10)
		cancel()
		if o.JSON {
			type row struct {
				Time   int64  `json:"time"`
				Host   string `json:"host"`
				Live   bool   `json:"live"`
				Nodes  any    `json:"nodes"`
				Events any    `json:"events,omitempty"`
			}
			enc := json.NewEncoder(w)
			if err := enc.Encode(row{Time: now.UnixNano(), Host: st.Host, Live: st.Live, Nodes: nodes, Events: evs}); err != nil {
				return err
			}
			continue
		}
		m := &Model{W: o.Width, H: 1000, Now: now, UTC: o.UTC, Status: st, Nodes: nodes, Events: evs, Sel: -1}
		lines := []string{m.header(), m.tableHeader()}
		for _, n := range m.VisibleNodes() {
			lines = append(lines, m.row(n, false))
		}
		if i > 0 || o.Iterations > 1 {
			fmt.Fprintln(w)
		}
		for _, l := range lines {
			fmt.Fprintln(w, trimRight(l))
		}
	}
	return nil
}

func trimRight(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
