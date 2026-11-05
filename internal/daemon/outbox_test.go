package daemon

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"algod.top/internal/model"
)

func TestOutboxPullResumeAndEpoch(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, time.Hour, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		o.Add(OutItem{Batch: &model.Batch{Node: "n", Group: model.GroupMetrics, Time: int64(i+1) * 1e9,
			Samples: []model.Sample{{Name: "c", Kind: model.KindCounter, Value: float64(i)}, {Name: "nan", Kind: model.KindGauge, Value: math.NaN()}}}})
	}
	o.Add(OutItem{Event: &model.Event{Time: 5, Node: "n", Kind: "restart", Msg: "x"}})
	body, cursor, err := o.Pull(context.Background(), "", 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	items, err := DecodePull(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 101 || items[0].Batch == nil || items[0].Batch.Samples[0].Name != "c" || len(items[0].Batch.Samples) != 1 {
		t.Fatalf("items %d first %+v", len(items), items[0].Batch)
	}
	if items[100].Event == nil || items[100].Event.Kind != "restart" {
		t.Fatal("event lost")
	}
	// Nothing new: a pull with wait returns when an item arrives.
	go func() { time.Sleep(200 * time.Millisecond); o.Add(OutItem{Event: &model.Event{Time: 6, Kind: "x"}}) }()
	t0 := time.Now()
	body, cursor2, _ := o.Pull(context.Background(), cursor, 5*time.Second, 1<<20)
	items, _ = DecodePull(body)
	if len(items) != 1 || time.Since(t0) > 3*time.Second {
		t.Fatalf("long-poll got %d items after %v", len(items), time.Since(t0))
	}
	o.Close()
	// Reopen: the cursor still works, and the acked sequence persisted.
	o, _ = OpenOutbox(dir, time.Hour, 1<<30)
	defer o.Close()
	body, _, _ = o.Pull(context.Background(), cursor2, 0, 1<<20)
	if items, _ := DecodePull(body); len(items) != 0 {
		t.Fatalf("resumed pull returned %d old items", len(items))
	}
	// A cursor from another epoch starts over.
	body, _, _ = o.Pull(context.Background(), "otherepoch:500", 0, 1<<20)
	if items, _ := DecodePull(body); len(items) != 102 {
		t.Fatalf("foreign epoch pull returned %d items", len(items))
	}
	if !strings.Contains(cursor, ":") {
		t.Fatal(cursor)
	}
}
