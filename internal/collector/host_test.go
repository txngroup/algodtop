package collector

import "testing"

func TestNetRateNeedsOneMethod(t *testing.T) {
	sock := netPoint{in: 1000, out: 2000, t: 100, method: "sock_diag"}
	for _, c := range []struct {
		name    string
		prev    netPoint
		cur     netPoint
		in, out float64
		ok      bool
	}{
		{"steady", sock, netPoint{in: 4000, out: 8000, t: 130, method: "sock_diag"}, 100, 200, true},
		{"first reading", netPoint{}, sock, 0, 0, false},
		{"counter went down", sock, netPoint{in: 10, out: 9000, t: 130, method: "sock_diag"}, 0, 0, false},
		// A switch to interface totals jumps from one process's bytes to
		// a whole namespace's, which is not traffic.
		{"method changed", sock, netPoint{in: 9e9, out: 9e9, t: 130, method: "netns interface totals"}, 0, 0, false},
	} {
		in, out, ok := netRate(c.prev, c.cur)
		if in != c.in || out != c.out || ok != c.ok {
			t.Errorf("%s: got %v %v %v, want %v %v %v", c.name, in, out, ok, c.in, c.out, c.ok)
		}
	}
}
