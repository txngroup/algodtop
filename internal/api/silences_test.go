package api

import (
	"strings"
	"testing"

	"algod.top/internal/model"
)

// The events answer carries the silences that overlap its range and cover
// its node. A silence for every node covers each one.
func TestSilencesIn(t *testing.T) {
	all := []model.Silence{
		{ID: "before", Nodes: []string{"a"}, Start: 0, End: 100},
		{ID: "a", Nodes: []string{"a"}, Start: 150, End: 250},
		{ID: "b", Nodes: []string{"b"}, Start: 150, End: 250},
		{ID: "every", Start: 290, End: 400},
		{ID: "after", Start: 300, End: 400},
	}
	ids := func(s []model.Silence) string {
		var out []string
		for _, x := range s {
			out = append(out, x.ID)
		}
		return strings.Join(out, " ")
	}
	if got := ids(silencesIn(all, "", 100, 300)); got != "a b every" {
		t.Errorf("every node: %q", got)
	}
	if got := ids(silencesIn(all, "a", 100, 300)); got != "a every" {
		t.Errorf("node a: %q", got)
	}
}
