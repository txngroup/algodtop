package daemon

import (
	"context"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/model"
)

// subscriber is one SSE client. Sends never block: a slow viewer loses
// live points, the store loses nothing.
type subscriber struct {
	nodes map[string]bool
	subs  map[string]map[string]bool // node -> metric
	ch    chan api.Message
}

// Subscribe implements api.Hub.
func (d *Daemon) Subscribe(nodes map[string]bool, series []api.Sub) (<-chan api.Message, func()) {
	s := &subscriber{nodes: nodes, subs: map[string]map[string]bool{}, ch: make(chan api.Message, 256)}
	for _, x := range series {
		if s.subs[x.Node] == nil {
			s.subs[x.Node] = map[string]bool{}
		}
		s.subs[x.Node][x.Metric] = true
	}
	d.subsMu.Lock()
	d.subs[s] = true
	d.subsMu.Unlock()
	// Send the current status right away.
	s.send(api.Message{Event: "status", Data: d.statusFor(s)})
	return s.ch, func() {
		d.subsMu.Lock()
		delete(d.subs, s)
		d.subsMu.Unlock()
	}
}

func (s *subscriber) send(m api.Message) {
	select {
	case s.ch <- m:
	default:
	}
}

func (s *subscriber) wants(node string) bool {
	return len(s.nodes) == 0 || s.nodes[node] || node == ""
}

func (d *Daemon) subCount() int {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	return len(d.subs)
}

func (d *Daemon) publish(m api.Message, node string) {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for s := range d.subs {
		if s.wants(node) {
			s.send(m)
		}
	}
}

// publishBatch sends new samples of subscribed series. With no viewers
// connected it costs one map lookup.
func (d *Daemon) publishBatch(b *model.Batch) {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	if len(d.subs) == 0 {
		return
	}
	for s := range d.subs {
		want := s.subs[b.Node]
		if len(want) == 0 {
			continue
		}
		for _, smp := range b.Samples {
			if want[smp.Name] {
				s.send(api.Message{Event: "sample", Data: api.LiveSample{Node: b.Node, Metric: smp.Name, Labels: smp.Labels.Map(),
					Kind: smp.Kind.String(), T: b.Time, V: smp.Value}})
			}
		}
	}
}

func (d *Daemon) statusFor(s *subscriber) any {
	var out []api.NodeView
	for _, n := range d.Nodes() {
		if s.wants(n.Name) {
			out = append(out, n)
		}
	}
	return map[string]any{"nodes": out, "status": d.Status(), "time": time.Now().UnixNano()}
}

// statusPublisher pushes live status to SSE clients when a node confirms
// a new round, and every 2 s besides. It does no work when nobody is
// connected.
func (d *Daemon) statusPublisher(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.closeSubs()
			return
		case <-t.C:
		case <-d.roundCh:
			// Nodes on one network confirm a round within a moment of each
			// other, so wait briefly and send them in one message.
			select {
			case <-ctx.Done():
				d.closeSubs()
				return
			case <-time.After(150 * time.Millisecond):
			}
			select {
			case <-d.roundCh:
			default:
			}
			t.Reset(2 * time.Second)
		}
		d.subsMu.Lock()
		subs := make([]*subscriber, 0, len(d.subs))
		for s := range d.subs {
			subs = append(subs, s)
		}
		d.subsMu.Unlock()
		if len(subs) == 0 {
			continue
		}
		for _, s := range subs {
			s.send(api.Message{Event: "status", Data: d.statusFor(s)})
		}
	}
}

func (d *Daemon) closeSubs() {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for s := range d.subs {
		close(s.ch)
		delete(d.subs, s)
	}
}

// roundArrived wakes the status publisher. It never blocks.
func (d *Daemon) roundArrived() {
	select {
	case d.roundCh <- struct{}{}:
	default:
	}
}
