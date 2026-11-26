package daemon

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"testing"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/config"
	"algod.top/internal/model"
)

func TestRemoteErr(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://agent/api/nodes", Err: io.ErrUnexpectedEOF}
	stderr := "kex_exchange_identification: read: Connection reset by peer\r\nConnection reset by 192.0.2.7 port 22"
	want := "unexpected EOF: kex_exchange_identification: read: Connection reset by peer; Connection reset by 192.0.2.7 port 22"
	if got := remoteErr(err, stderr); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := remoteErr(errors.New("agent /api/nodes: HTTP 401: unauthorized"), ""); got != "agent /api/nodes: HTTP 401: unauthorized" {
		t.Errorf("without stderr: %q", got)
	}
	if got := remoteErr(io.EOF, "same\nsame\n"); got != "EOF: same" {
		t.Errorf("repeated line: %q", got)
	}
}

func TestDownReason(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r := &remoteRun{cfg: config.Remote{Name: "edge"}, started: now.Add(-90 * time.Second), lastErr: "connection refused"}
	if got := r.downReason(now); got != "agent edge unreachable for 1m30s: connection refused" {
		t.Errorf("never reached: %q", got)
	}
	r.lastOK, r.lastErr = now.Add(-48*time.Minute-400*time.Millisecond), ""
	if got := r.downReason(now); got != "agent edge unreachable for 48m0s: no answer" {
		t.Errorf("after a success: %q", got)
	}
}

// The hub keeps its agents in a map. The node pickers in the web UI list
// nodes in the order views returns, so it must not change between calls.
// Each agent's own nodes keep the agent's order.
func TestViewsKeepAgentOrder(t *testing.T) {
	rs := &remoteSet{runs: map[string]*remoteRun{}}
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("agent%d", i)
		r := &remoteRun{cfg: config.Remote{Name: name}, lastOK: time.Now()}
		for _, n := range []string{"z", "a"} {
			v := api.NodeView{}
			v.Name = name + "/" + n
			r.views = append(r.views, v)
		}
		rs.runs[name] = r
	}
	var want []string
	for i := 1; i <= 8; i++ {
		want = append(want, fmt.Sprintf("agent%d/z", i), fmt.Sprintf("agent%d/a", i))
	}
	for range 20 {
		var got []string
		for _, v := range rs.views() {
			got = append(got, v.Name)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	}
}

// An agent's own events reach the hub with no node, which would match
// every node: an agent's restart shaded the hub's own nodes' charts as
// "algod.top was down". The hub gives each of the agent's nodes a copy.
func TestAgentEvents(t *testing.T) {
	ev := model.Event{Time: 5, Kind: model.EvMonitorGap, Severity: model.SevWarn, Msg: "algod.top was down for 1m36s",
		Attrs: map[string]any{"from": int64(1), "to": int64(5)}}
	got := agentEvents(ev, "edge", []string{"edge/a", "edge/b"})
	if len(got) != 2 || got[0].Node != "edge/a" || got[1].Node != "edge/b" {
		t.Fatalf("nodes: %+v", got)
	}
	for _, e := range got {
		if e.Msg != "agent edge: algod.top was down for 1m36s" || e.Attrs["agent"] != "edge" || e.Attrs["from"] != int64(1) || e.Time != 5 {
			t.Errorf("copy: %+v", e)
		}
	}
	got[0].Attrs["x"] = 1
	if _, ok := got[1].Attrs["x"]; ok {
		t.Error("copies share attrs")
	}
	if _, ok := ev.Attrs["agent"]; ok {
		t.Error("the input's attrs changed")
	}
	if got := agentEvents(ev, "edge", nil); len(got) != 1 || got[0].Node != "" || got[0].Attrs["agent"] != "edge" {
		t.Errorf("no node known: %+v", got)
	}
}

func TestNodeNamesPreferStatus(t *testing.T) {
	r := &remoteRun{}
	pulled := map[string]bool{"edge/b": true, "edge/a": true}
	if got := r.nodeNames(pulled); !slices.Equal(got, []string{"edge/a", "edge/b"}) {
		t.Errorf("before a status: %v", got)
	}
	v := api.NodeView{}
	v.Name = "edge"
	r.views = []api.NodeView{v}
	if got := r.nodeNames(pulled); !slices.Equal(got, []string{"edge"}) {
		t.Errorf("with a status: %v", got)
	}
}
