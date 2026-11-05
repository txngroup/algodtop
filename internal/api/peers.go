package api

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/store"
)

func (s *Server) handlePeerHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	node := q.Get("node")
	sessions := s.hub.Store().Sessions(node, "", from, to)
	clusters := FindClusters(sessions, 30*time.Second, 5)
	evs := s.hub.Store().Events(store.EventFilter{Node: node, From: from, To: to, Kinds: []string{model.EvDisconnect}})
	writeJSON(w, map[string]any{"peers": PeerHistories(sessions, clusters, evs)})
}

// PeerHistories groups sessions by remote IP.
func PeerHistories(sessions []model.Session, clusters []Cluster, disconnects []model.Event) []PeerHistory {
	by := map[string]*PeerHistory{}
	for _, s := range sessions {
		ip := s.IP
		if ip == "" {
			ip = s.Addr
		}
		h := by[ip]
		if h == nil {
			h = &PeerHistory{IP: ip, Reasons: map[string]int{}}
			by[ip] = h
		}
		h.Sessions++
		if s.End > s.Start {
			h.Connected += float64(s.End-s.Start) / 1e9
		}
		h.ConnectedMore = h.ConnectedMore || s.OpenBefore
		if s.Reason != "" {
			h.Reasons[s.Reason]++
		}
		h.BytesIn += s.BytesIn
		h.BytesOut += s.BytesOut
		h.Last = max(h.Last, s.End, s.Start)
		if !containsStr(h.Networks, s.Network) {
			h.Networks = append(h.Networks, s.Network)
		}
		if !containsStr(h.Directions, s.Direction) {
			h.Directions = append(h.Directions, s.Direction)
		}
	}
	for _, e := range disconnects {
		if e.Attrs["reset"] == true {
			if ip, _ := e.Attrs["ip"].(string); ip != "" {
				if h := by[ip]; h != nil {
					h.Resets++
				}
			}
		}
	}
	for _, c := range clusters {
		for _, ip := range c.IPs {
			if h := by[ip]; h != nil {
				h.Clusters = append(h.Clusters, c.ID)
			}
		}
	}
	out := make([]PeerHistory, 0, len(by))
	for _, h := range by {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].IP < out[j].IP
	})
	return out
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// FindClusters finds groups of at least min inbound sessions that ended
// within window of each other, and flags IPs that appear in more than one
// cluster, so peers that reset together show without grouping
// disconnects by hand.
func FindClusters(sessions []model.Session, window time.Duration, minSize int) []Cluster {
	var ended []model.Session
	for _, s := range sessions {
		if s.Direction == "in" && s.End > 0 && s.Reason != "node down" {
			ended = append(ended, s)
		}
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].End < ended[j].End })
	var clusters []Cluster
	w := int64(window)
	for i := 0; i < len(ended); {
		j := i
		for j+1 < len(ended) && ended[j+1].End-ended[i].End <= w {
			j++
		}
		group := ended[i : j+1]
		ips := map[string]bool{}
		for _, s := range group {
			ips[s.IP] = true
		}
		if len(ips) >= minSize {
			c := Cluster{ID: len(clusters) + 1, Start: group[0].End, End: group[len(group)-1].End, Reasons: map[string]int{}, Protocols: map[string]int{}}
			for ip := range ips {
				c.IPs = append(c.IPs, ip)
			}
			sort.Strings(c.IPs)
			for _, s := range group {
				c.Reasons[s.Reason]++
				c.TrafficIn += s.BytesIn
				c.TrafficOut += s.BytesOut
				if len(s.Protocols) > 0 {
					p := append([]string(nil), s.Protocols...)
					sort.Strings(p)
					c.Protocols[strings.Join(p, " ")]++
				}
			}
			c.Sessions = append([]model.Session(nil), group...)
			clusters = append(clusters, c)
			i = j + 1
			continue
		}
		i++
	}
	seen := map[string]int{}
	for _, c := range clusters {
		for _, ip := range c.IPs {
			seen[ip]++
		}
	}
	for i := range clusters {
		for _, ip := range clusters[i].IPs {
			if seen[ip] > 1 {
				clusters[i].Repeat = append(clusters[i].Repeat, ip)
			}
		}
	}
	return clusters
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 7*24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	window := 30 * time.Second
	if v := q.Get("window"); v != "" {
		window, _ = time.ParseDuration(v)
	}
	minSize := 5
	if v := q.Get("min"); v != "" {
		minSize, _ = strconv.Atoi(v)
	}
	sessions := s.hub.Store().Sessions(q.Get("node"), "", from, to)
	cl := FindClusters(sessions, window, max(2, minSize))
	if cl == nil {
		cl = []Cluster{}
	}
	writeJSON(w, map[string]any{"clusters": cl})
}

// handlePeerUsage returns per-peer traffic: top N peers stacked plus
// "other", as rates per bucket.
func (s *Server) handlePeerUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	points := 120
	if v := q.Get("points"); v != "" {
		points, _ = strconv.Atoi(v)
	}
	top := 8
	if v := q.Get("top"); v != "" {
		top, _ = strconv.Atoi(v)
	}
	dir := q.Get("dir")
	us := s.hub.Store().PeerUsage(q.Get("node"), q.Get("key"), from-int64(5*time.Minute), to)
	writeJSON(w, PeerTraffic(us, from, to, points, top, dir))
}

// PeerTrafficResult is per-peer traffic as stacked series.
type PeerTrafficResult struct {
	T      []int64             `json:"t"`
	Series []PeerTrafficSeries `json:"series"`
	Top    []PeerTotal         `json:"top"`
}

// PeerTrafficSeries is one peer's rate per bucket.
type PeerTrafficSeries struct {
	Key  string     `json:"key"`
	Addr string     `json:"addr"`
	Rate jsonFloats `json:"rate"`
}

// PeerTotal is one peer's bytes over the range.
type PeerTotal struct {
	Key     string  `json:"key"`
	Addr    string  `json:"addr"`
	Network string  `json:"network"`
	In      float64 `json:"in"`
	Out     float64 `json:"out"`
	RateIn  float64 `json:"rate_in"`
	RateOut float64 `json:"rate_out"`
}

// PeerTraffic computes rates from per-poll cumulative totals. A total that
// drops is a reset: the new session starts from zero.
func PeerTraffic(us []model.PeerUsage, from, to int64, points, top int, dir string) PeerTrafficResult {
	if points <= 0 {
		points = 120
	}
	step := float64(to-from) / float64(points)
	byKey := map[string][]model.PeerUsage{}
	addr := map[string]string{}
	netw := map[string]string{}
	for _, u := range us {
		byKey[u.Key] = append(byKey[u.Key], u)
		addr[u.Key] = u.Addr
		netw[u.Key] = u.Network
	}
	totals := map[string]*PeerTotal{}
	rates := map[string][]float64{}
	for k, ss := range byKey {
		sort.Slice(ss, func(i, j int) bool { return ss[i].Time < ss[j].Time })
		tot := &PeerTotal{Key: k, Addr: addr[k], Network: netw[k]}
		rs := make([]float64, points)
		cnt := make([]float64, points)
		for i := 1; i < len(ss); i++ {
			a, b := ss[i-1], ss[i]
			if b.Time < from || b.Time >= to {
				continue
			}
			din, dout := b.BytesIn-a.BytesIn, b.BytesOut-a.BytesOut
			if din < 0 {
				din = b.BytesIn
			}
			if dout < 0 {
				dout = b.BytesOut
			}
			tot.In += float64(din)
			tot.Out += float64(dout)
			dt := float64(b.Time-a.Time) / 1e9
			if dt <= 0 {
				continue
			}
			bi := min(int(float64(b.Time-from)/step), points-1)
			v := float64(din)
			switch dir {
			case "out":
				v = float64(dout)
			case "both":
				v = float64(din + dout)
			}
			rs[bi] += v / dt
			cnt[bi]++
		}
		for i := range rs {
			if cnt[i] > 0 {
				rs[i] /= cnt[i]
			} else {
				rs[i] = math.NaN()
			}
		}
		secs := float64(to-from) / 1e9
		tot.RateIn, tot.RateOut = tot.In/secs, tot.Out/secs
		totals[k] = tot
		rates[k] = rs
	}
	list := make([]PeerTotal, 0, len(totals))
	for _, t := range totals {
		list = append(list, *t)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].In+list[i].Out > list[j].In+list[j].Out })
	res := PeerTrafficResult{T: make([]int64, points), Top: list}
	for i := range res.T {
		res.T[i] = from + int64(float64(i)*step)
	}
	other := make([]float64, points)
	for i := range other {
		other[i] = math.NaN()
	}
	for i, t := range list {
		if i < top {
			res.Series = append(res.Series, PeerTrafficSeries{Key: t.Key, Addr: t.Addr, Rate: rates[t.Key]})
			continue
		}
		for j, v := range rates[t.Key] {
			if !math.IsNaN(v) {
				if math.IsNaN(other[j]) {
					other[j] = 0
				}
				other[j] += v
			}
		}
	}
	if len(list) > top {
		res.Series = append(res.Series, PeerTrafficSeries{Key: "other", Addr: "other", Rate: other})
	}
	return res
}
