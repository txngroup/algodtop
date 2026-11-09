package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"algod.top/internal/model"
	"algod.top/internal/query"
	"algod.top/internal/release"
	"algod.top/internal/store"
)

// Server serves the API on one listener.
type Server struct {
	hub    Hub
	auth   *Auth // nil on the unix socket
	static fs.FS
	mux    *http.ServeMux
	agent  bool
	logf   func(string, ...any)
	// AgentToken, when set, gates /agent/v1 on TCP (TLS agent mode).
	AgentTokenHash string
}

// Options configure a server.
type Options struct {
	Hub    Hub
	Auth   *Auth // nil for the unix socket: filesystem permissions are the access control
	Static fs.FS
	Agent  bool
	Logf   func(string, ...any)
}

// New builds a server.
func New(o Options) *Server {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	s := &Server{hub: o.Hub, auth: o.Auth, static: o.Static, mux: http.NewServeMux(), agent: o.Agent, logf: o.Logf}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/nodes", s.read(s.handleNodes))
	m.HandleFunc("GET /api/nodes/{name...}", s.read(s.handleNode))
	m.HandleFunc("GET /api/status", s.read(s.handleStatus))
	m.HandleFunc("GET /api/series", s.read(s.handleSeries))
	m.HandleFunc("GET /api/summary", s.read(s.handleSummary))
	m.HandleFunc("GET /api/export", s.read(s.handleExport))
	m.HandleFunc("GET /api/metrics", s.read(s.handleMetricNames))
	m.HandleFunc("GET /api/events", s.read(s.handleEvents))
	m.HandleFunc("GET /api/sessions", s.read(s.handleSessions))
	m.HandleFunc("GET /api/peers", s.read(s.handlePeers))
	m.HandleFunc("GET /api/peers/history", s.read(s.handlePeerHistory))
	m.HandleFunc("GET /api/peers/clusters", s.read(s.handleClusters))
	m.HandleFunc("GET /api/peers/usage", s.read(s.handlePeerUsage))
	m.HandleFunc("GET /api/logs/templates", s.read(s.handleTemplates))
	m.HandleFunc("GET /api/logs/lines", s.read(s.handleLogLines))
	m.HandleFunc("GET /api/annotations", s.read(s.handleAnnotations))
	m.HandleFunc("POST /api/annotations", s.write(s.handlePutAnnotation))
	m.HandleFunc("PUT /api/annotations", s.write(s.handlePutAnnotation))
	m.HandleFunc("DELETE /api/annotations", s.write(s.handleDeleteAnnotation))
	m.HandleFunc("GET /api/silences", s.read(s.handleSilences))
	m.HandleFunc("POST /api/silences", s.write(s.handleAddSilence))
	m.HandleFunc("DELETE /api/silences", s.write(s.handleDeleteSilence))
	m.HandleFunc("POST /api/admin/snapshot", s.write(s.handleSnapshot))
	m.HandleFunc("POST /api/admin/reload", s.write(s.handleReload))
	m.HandleFunc("POST /api/admin/upgrade", s.socketOnly(s.handleUpgrade))
	m.HandleFunc("GET /api/config", s.read(s.handleConfig))
	m.HandleFunc("PUT /api/config", s.write(s.handlePutConfig))
	m.HandleFunc("GET /api/discovered", s.read(s.handleDiscovered))
	m.HandleFunc("GET /api/stream", s.read(s.handleStream))
	m.HandleFunc("GET /api/session", s.handleSession)
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)
	m.HandleFunc("GET /metrics", s.read(s.handleSelfMetrics))
	m.HandleFunc("GET /agent/v1/batches", s.handleAgent)
	if s.static != nil {
		files := http.FileServerFS(s.static)
		m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if s.auth != nil && !s.auth.hostAllowed(r) {
				http.Error(w, "unexpected Host header", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("X-Frame-Options", "DENY")
			if !strings.Contains(r.URL.Path[1:], ".") {
				// Page routes all load the single-page app.
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				files.ServeHTTP(w, r2)
				return
			}
			files.ServeHTTP(w, r)
		})
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) read(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth != nil {
			if !s.auth.hostAllowed(r) {
				httpError(w, http.StatusForbidden, "unexpected Host header")
				return
			}
			if role, _ := s.auth.role(r); role < RoleRead {
				httpError(w, http.StatusUnauthorized, "login required")
				return
			}
		}
		h(w, r)
	}
}

// write requires login and the CSRF token over TCP. The unix socket needs
// neither: anyone who can open it already runs as the daemon's user.
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth != nil {
			if !s.auth.hostAllowed(r) {
				httpError(w, http.StatusForbidden, "unexpected Host header")
				return
			}
			role, csrfTok := s.auth.role(r)
			if role < RoleWrite {
				httpError(w, http.StatusForbidden, "write access requires login")
				return
			}
			if csrfTok == "" || r.Header.Get("X-CSRF-Token") != csrfTok {
				httpError(w, http.StatusForbidden, "missing or bad CSRF token")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
				httpError(w, http.StatusForbidden, "cross-origin request")
				return
			}
		}
		h(w, r)
	}
}

func sameOrigin(origin, host string) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return strings.EqualFold(o, host)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	b, err := marshalNaN(v)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Write(b)
}

// marshalNaN encodes JSON with NaN and Inf as null, which encoding/json
// rejects.
func marshalNaN(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err == nil {
		return b, nil
	}
	var ue *json.UnsupportedValueError
	if !errors.As(err, &ue) {
		return nil, err
	}
	return json.Marshal(sanitize(v))
}

// sanitize replaces NaN in the float slices query results use.
func sanitize(v any) any {
	switch x := v.(type) {
	case *query.Result:
		c := *x
		c.Series = make([]query.Series, len(x.Series))
		for i, s := range x.Series {
			c.Series[i] = s
		}
		return seriesJSON(&c)
	}
	return v
}

type jsonFloats []float64

// MarshalJSON writes NaN and Inf as null.
func (f jsonFloats) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, v := range f {
		if i > 0 {
			b.WriteByte(',')
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			b.WriteString("null")
		} else {
			b.WriteString(strconv.FormatFloat(v, 'g', 8, 64))
		}
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

type seriesOut struct {
	Labels map[string]string `json:"labels"`
	Kind   string            `json:"kind"`
	T      []int64           `json:"t"`
	Avg    jsonFloats        `json:"avg"`
	Min    jsonFloats        `json:"min,omitempty"`
	Max    jsonFloats        `json:"max,omitempty"`
}

type heatOut struct {
	Labels map[string]string `json:"labels"`
	LE     jsonFloats        `json:"le"`
	T      []int64           `json:"t"`
	Counts []jsonFloats      `json:"counts"`
}

type resultOut struct {
	*query.Result
	Series   []seriesOut `json:"series"`
	Heatmaps []heatOut   `json:"heatmaps,omitempty"`
}

func seriesJSON(r *query.Result) resultOut {
	out := resultOut{Result: r, Series: make([]seriesOut, len(r.Series))}
	for i, s := range r.Series {
		out.Series[i] = seriesOut{Labels: s.Labels, Kind: s.Kind, T: s.T, Avg: s.Avg, Min: s.Min, Max: s.Max}
	}
	for _, h := range r.Heatmaps {
		ho := heatOut{Labels: h.Labels, LE: h.LE, T: h.T}
		for _, c := range h.Counts {
			ho.Counts = append(ho.Counts, c)
		}
		out.Heatmaps = append(out.Heatmaps, ho)
	}
	return out
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// timeParam parses unix ns, ms or s, with an optional fraction, RFC3339
// or a relative "-1h".
func timeParam(s string, def time.Time) (int64, error) {
	if s == "" {
		return def.UnixNano(), nil
	}
	if s == "now" {
		return time.Now().UnixNano(), nil
	}
	if strings.HasPrefix(s, "-") {
		d, err := parseDur(s[1:])
		if err != nil {
			return 0, err
		}
		return time.Now().Add(-d).UnixNano(), nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e17:
			return n, nil
		case n > 1e11:
			return n * int64(time.Millisecond), nil
		default:
			return n * int64(time.Second), nil
		}
	}
	// JavaScript times are fractional milliseconds, such as a nanosecond
	// time divided by 1e6.
	if f, ok := decimal(s); ok {
		switch {
		case f > 1e17:
			return int64(f), nil
		case f > 1e11:
			return int64(math.Round(f * 1e6)), nil
		default:
			return int64(math.Round(f * 1e9)), nil
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UnixNano(), nil
		}
	}
	return 0, fmt.Errorf("bad time %q", s)
}

// decimal parses digits with one decimal point, such as 1794094676942.322.
// It refuses exponents, signs and values past the int64 range.
func decimal(s string) (float64, bool) {
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || whole == "" || strings.Trim(whole, "0123456789") != "" || strings.Trim(frac, "0123456789") != "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f >= math.MaxInt64 {
		return 0, false
	}
	return f, true
}

func parseDur(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		return time.Duration(n * float64(24*time.Hour)), err
	}
	if strings.HasSuffix(s, "y") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "y"), 64)
		return time.Duration(n * float64(365*24*time.Hour)), err
	}
	return time.ParseDuration(s)
}

func rangeParams(r *http.Request, def time.Duration) (int64, int64, error) {
	now := time.Now()
	to, err := timeParam(r.URL.Query().Get("to"), now)
	if err != nil {
		return 0, 0, err
	}
	from, err := timeParam(r.URL.Query().Get("from"), time.Unix(0, to).Add(-def))
	if err != nil {
		return 0, 0, err
	}
	return from, to, nil
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"nodes": s.hub.Nodes(), "status": s.hub.Status()})
}

func (s *Server) handleNode(w http.ResponseWriter, r *http.Request) {
	n, ok := s.hub.Node(r.PathValue("name"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such node")
		return
	}
	writeJSON(w, map[string]any{"node": n, "peers": n.PeerList})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.hub.Status()
	if s.auth != nil {
		role, _ := s.auth.role(r)
		st.Auth = map[int]string{RoleNone: "none", RoleRead: "read", RoleWrite: "write"}[role]
	} else {
		st.Auth = "write"
	}
	writeJSON(w, st)
}

func (s *Server) seriesRequest(r *http.Request) (query.Request, error) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, time.Hour)
	if err != nil {
		return query.Request{}, err
	}
	req := query.Request{Node: q.Get("node"), Metric: q.Get("metric"), From: from, To: to, Tier: q.Get("tier"), Fn: q.Get("fn"), By: q.Get("by")}
	if req.Node == "" || req.Metric == "" {
		return req, errors.New("node and metric are required")
	}
	if p := q.Get("points"); p != "" {
		req.Points, _ = strconv.Atoi(p)
	}
	if l := q.Get("limit"); l != "" {
		req.Limit, _ = strconv.Atoi(l)
	}
	for _, m := range q["match"] {
		mt, err := query.ParseMatcher(m)
		if err != nil {
			return req, err
		}
		req.Match = append(req.Match, mt)
	}
	return req, nil
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	req, err := s.seriesRequest(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.hub.Query().Run(r.Context(), req)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, query.ErrBusy) {
			code = http.StatusServiceUnavailable
		}
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, seriesJSON(res))
}

// handleSummary returns mean, p50, p99, min and max of a metric over a
// range, for the Compare page.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	req, err := s.seriesRequest(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Points == 0 {
		req.Points = 1000
	}
	res, err := s.hub.Query().Run(r.Context(), req)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := map[string]Summary{}
	for _, se := range res.Series {
		var vals []float64
		for _, v := range se.Avg {
			if !math.IsNaN(v) {
				vals = append(vals, v)
			}
		}
		key := labelKey(se.Labels)
		out[key] = summarize(vals)
	}
	writeJSON(w, map[string]any{"summary": out, "parts": res.Parts, "fn": res.Fn})
}

func labelKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func summarize(v []float64) Summary {
	if len(v) == 0 {
		return Summary{}
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	sum := 0.0
	for _, x := range c {
		sum += x
	}
	q := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(c)))) - 1
		return c[max(0, min(i, len(c)-1))]
	}
	return Summary{Mean: sum / float64(len(c)), P50: q(0.5), P99: q(0.99), Min: c[0], Max: c[len(c)-1], N: len(c)}
}

// handleExport returns exactly what a chart shows as CSV or JSON.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	req, err := s.seriesRequest(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.hub.Query().Run(r.Context(), req)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := fmt.Sprintf("%s-%s", req.Node, req.Metric)
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, name))
		writeJSON(w, seriesJSON(res))
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, name))
	WriteCSV(w, res)
}

// WriteCSV writes a result as CSV: time, labels, avg, min, max.
func WriteCSV(w io.Writer, res *query.Result) {
	cw := csv.NewWriter(w)
	cw.Write([]string{"time", "series", "value", "min", "max", "tier"})
	tierAt := func(t int64) string {
		for _, p := range res.Parts {
			if t >= p.From && t < p.To {
				return p.Tier
			}
		}
		return ""
	}
	f := func(v []float64, i int) string {
		if i >= len(v) || math.IsNaN(v[i]) {
			return ""
		}
		return strconv.FormatFloat(v[i], 'g', -1, 64)
	}
	for _, h := range res.Heatmaps {
		key := labelKey(h.Labels)
		for i, t := range h.T {
			for j, le := range h.LE {
				if i < len(h.Counts) && j < len(h.Counts[i]) && h.Counts[i][j] > 0 {
					cw.Write([]string{time.Unix(0, t).UTC().Format(time.RFC3339Nano), key + " le=" + strconv.FormatFloat(le, 'g', -1, 64),
						strconv.FormatFloat(h.Counts[i][j], 'g', -1, 64), "", "", tierAt(t)})
				}
			}
		}
	}
	for _, s := range res.Series {
		key := labelKey(s.Labels)
		for i, t := range s.T {
			if i < len(s.Avg) && math.IsNaN(s.Avg[i]) {
				continue
			}
			cw.Write([]string{time.Unix(0, t).UTC().Format(time.RFC3339Nano), key, f(s.Avg, i), f(s.Min, i), f(s.Max, i), tierAt(t)})
		}
	}
	cw.Flush()
}

func (s *Server) handleMetricNames(w http.ResponseWriter, r *http.Request) {
	db := s.hub.Store()
	q := r.URL.Query()
	node, ok := db.NodeID(q.Get("node"))
	if !ok {
		httpError(w, http.StatusNotFound, "unknown node")
		return
	}
	filter := strings.ToLower(q.Get("q"))
	type entry struct {
		Name   string              `json:"name"`
		Kind   string              `json:"kind"`
		Series int                 `json:"series"`
		Labels map[string][]string `json:"labels,omitempty"`
	}
	var out []entry
	for _, n := range db.Names(node) {
		if filter != "" && !strings.Contains(strings.ToLower(n), filter) {
			continue
		}
		ms := db.Find(node, n)
		e := entry{Name: n, Series: len(ms)}
		if len(ms) > 0 {
			e.Kind = ms[0].Kind.String()
		}
		if q.Get("labels") != "" || filter != "" && len(out) < 50 {
			e.Labels = map[string][]string{}
			seen := map[string]bool{}
			for _, m := range ms {
				for _, l := range m.Labels {
					k := l.Name + "=" + l.Value
					if !seen[k] && len(e.Labels[l.Name]) < 200 {
						seen[k] = true
						e.Labels[l.Name] = append(e.Labels[l.Name], l.Value)
					}
				}
			}
		}
		out = append(out, e)
	}
	writeJSON(w, map[string]any{"metrics": out, "dropped": db.Dropped(node)})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	f := store.EventFilter{Node: q.Get("node"), From: from, To: to}
	if k := q.Get("kind"); k != "" {
		f.Kinds = strings.Split(k, ",")
	}
	if l := q.Get("limit"); l != "" {
		f.Limit, _ = strconv.Atoi(l)
	} else {
		f.Limit = 5000
	}
	evs, counts := s.hub.Store().EventsCounted(f)
	if evs == nil {
		evs = []model.Event{}
	}
	// counts has every kind in the range, so a client can tell what the
	// limit left out and ask for it by kind.
	writeJSON(w, map[string]any{"events": evs, "counts": counts, "annotations": s.hub.Store().Annotations(f.Node, from, to), "silences": silencesIn(s.hub.Silences(), f.Node, from, to)})
}

// silencesIn keeps the silences that overlap [from, to) and cover node.
// An empty node keeps every node's.
func silencesIn(all []model.Silence, node string, from, to int64) []model.Silence {
	out := []model.Silence{}
	for _, sl := range all {
		if sl.End <= from || sl.Start >= to {
			continue
		}
		if node != "" && len(sl.Nodes) > 0 && !slices.Contains(sl.Nodes, node) {
			continue
		}
		out = append(out, sl)
	}
	return out
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	ss := s.hub.Store().Sessions(q.Get("node"), q.Get("addr"), from, to)
	if ss == nil {
		ss = []model.Session{}
	}
	writeJSON(w, map[string]any{"sessions": ss})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	n, ok := s.hub.Node(r.URL.Query().Get("node"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such node")
		return
	}
	writeJSON(w, map[string]any{"peers": n.PeerList, "usage": n.PeerUsage, "counts": n.Peers, "has_admin": n.HasAdmin})
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.hub.Query().Run(r.Context(), query.Request{Node: q.Get("node"), Metric: "log_template_lines_total", From: from, To: to, Points: 1, Fn: "increase", Limit: 5000})
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	var out []TemplateCount
	for _, se := range res.Series {
		total := 0.0
		for _, v := range se.Avg {
			if !math.IsNaN(v) {
				total += v
			}
		}
		if total > 0 {
			out = append(out, TemplateCount{Level: se.Labels["level"], Template: se.Labels["template"], Count: total})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	writeJSON(w, map[string]any{"templates": out})
}

func (s *Server) handleLogLines(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := rangeParams(r, 24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := 1000
	if l := q.Get("limit"); l != "" {
		limit, _ = strconv.Atoi(l)
	}
	lines := s.hub.Store().LogLines(q.Get("node"), q.Get("template"), from, to, limit)
	if lines == nil {
		lines = []store.LogLine{}
	}
	writeJSON(w, map[string]any{"lines": lines})
}

func (s *Server) handleAnnotations(w http.ResponseWriter, r *http.Request) {
	from, to, err := rangeParams(r, 365*24*time.Hour)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a := s.hub.Store().Annotations(r.URL.Query().Get("node"), from, to)
	if a == nil {
		a = []model.Annotation{}
	}
	writeJSON(w, map[string]any{"annotations": a})
}

// annotationIn is an annotation that may carry a silence.
type annotationIn struct {
	model.Annotation
	SilenceFor   string   `json:"silence_for,omitempty"`
	SilenceNodes []string `json:"silence_nodes,omitempty"`
}

func (s *Server) handlePutAnnotation(w http.ResponseWriter, r *http.Request) {
	var in annotationIn
	if err := decode(r, &in); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	a := in.Annotation
	if strings.TrimSpace(a.Text) == "" {
		httpError(w, http.StatusBadRequest, "text is required")
		return
	}
	if a.Start == 0 {
		a.Start = time.Now().UnixNano()
	}
	// An annotation can carry a silence, so marking a run and silencing
	// its restart is one step.
	if in.SilenceFor != "" {
		d, err := parseDur(in.SilenceFor)
		if err != nil {
			httpError(w, http.StatusBadRequest, "silence_for: "+err.Error())
			return
		}
		nodes := in.SilenceNodes
		if len(nodes) == 0 && a.Node != "" {
			nodes = []string{a.Node}
		}
		sil, err := s.hub.AddSilence(model.Silence{Nodes: nodes, Start: time.Now().UnixNano(), End: time.Now().Add(d).UnixNano(), Comment: a.Text})
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.Silence = sil.ID
	}
	if r.Method == http.MethodPost {
		a.ID = ""
	}
	out, err := s.hub.Store().PutAnnotation(a)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Store().AppendEvent(model.Event{Time: out.Start, Node: out.Node, Kind: model.EvAnnotation, Severity: model.SevInfo, Msg: out.Text, Attrs: map[string]any{"id": out.ID}})
	writeJSON(w, out)
}

func (s *Server) handleDeleteAnnotation(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.Store().DeleteAnnotation(r.URL.Query().Get("id")); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleSilences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"silences": s.hub.Silences()})
}

type silenceIn struct {
	model.Silence
	For string `json:"for,omitempty"`
}

func (s *Server) handleAddSilence(w http.ResponseWriter, r *http.Request) {
	var in silenceIn
	if err := decode(r, &in); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	sil := in.Silence
	if sil.Start == 0 {
		sil.Start = time.Now().UnixNano()
	}
	if in.For != "" {
		d, err := parseDur(in.For)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		sil.End = time.Unix(0, sil.Start).Add(d).UnixNano()
	}
	if sil.End <= sil.Start {
		httpError(w, http.StatusBadRequest, "silence needs an end or for")
		return
	}
	out, err := s.hub.AddSilence(sil)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, out)
}

func (s *Server) handleDeleteSilence(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.DeleteSilence(r.URL.Query().Get("id")); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	p, err := s.hub.Snapshot()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"path": p})
}

// socketOnly serves a handler on the unix socket and hides it on TCP.
// Anyone who can open the socket already runs as the daemon's account or
// belongs to its group.
func (s *Server) socketOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth != nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

// handleUpgrade takes a release from algodtop upgrade as a multipart form
// with the checksums file, its signature and the binary, whose file name
// is its release name. The hub's checks are not trusted: the daemon
// verifies the signature and the version itself.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 320<<20)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "upgrade: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	read := func(field string) ([]byte, string, error) {
		f, h, err := r.FormFile(field)
		if err != nil {
			return nil, "", fmt.Errorf("upgrade: no %s: %w", field, err)
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		return b, h.Filename, err
	}
	var rel release.Files
	var err error
	if rel.Checksums, _, err = read("checksums"); err == nil {
		if rel.Signature, _, err = read("signature"); err == nil {
			rel.Binary, rel.Name, err = read("binary")
		}
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.hub.Upgrade(r.Context(), rel)
	if err != nil {
		var ie *release.InstallError
		if errors.As(err, &ie) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(ie)
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, res)
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.Reload(); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	b, err := s.hub.ConfigJSON()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.hub.PutConfig(r.Context(), b); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleDiscovered(w http.ResponseWriter, r *http.Request) {
	d := s.hub.Discovered()
	if d == nil {
		d = []DiscoveredNode{}
	}
	writeJSON(w, map[string]any{"discovered": d})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, map[string]any{"role": "write", "csrf": "", "auth_required": false})
		return
	}
	if !s.auth.hostAllowed(r) {
		httpError(w, http.StatusForbidden, "unexpected Host header")
		return
	}
	role, csrfTok := s.auth.role(r)
	writeJSON(w, map[string]any{"role": map[int]string{RoleNone: "none", RoleRead: "read", RoleWrite: "write"}[role], "csrf": csrfTok, "auth_required": s.auth.Required()})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, map[string]string{"role": "write"})
		return
	}
	if !s.auth.hostAllowed(r) {
		httpError(w, http.StatusForbidden, "unexpected Host header")
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
		httpError(w, http.StatusForbidden, "cross-origin request")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	role, err := s.auth.login(w, r, in.Password)
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, errRateLimited) {
			code = http.StatusTooManyRequests
		}
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, map[string]string{"role": map[int]string{RoleRead: "read", RoleWrite: "write"}[role]})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		s.auth.logout(w, r)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleAgent serves the agent pull protocol. Over TCP it needs the agent
// token; over the unix socket (reached through ssh) SSH is the access
// control.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if !s.agent {
		httpError(w, http.StatusNotFound, "not an agent")
		return
	}
	// Over TCP the daemon wraps every agent request in a token or client
	// certificate check; over the unix socket, reached through ssh, SSH is
	// the access control.
	if s.auth != nil && r.TLS == nil {
		httpError(w, http.StatusForbidden, "agent pulls over TCP need TLS")
		return
	}
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		wait, _ = time.ParseDuration(v)
		wait = min(wait, 60*time.Second)
	}
	b, cursor, err := s.hub.AgentBatches(r.Context(), r.URL.Query().Get("since"), wait)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-algodtop-batches")
	w.Header().Set("X-Algodtop-Cursor", cursor)
	w.Write(b)
}

// ListenUnix creates the daemon socket with mode 0600, or 0660 for the
// group when one is given.
func ListenUnix(path, group string) (net.Listener, error) {
	return listenUnix(path, group)
}
