// Package promparse parses the Prometheus text exposition format that algod
// serves on /metrics, and classifies each series as a gauge, counter or
// histogram part.
package promparse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"algod.top/internal/model"
)

// Series is one parsed sample line.
type Series struct {
	Name   string
	Labels model.Labels
	Value  float64
	Kind   model.Kind
	// Key is the canonical label string and Full is name{labels}. Both
	// are cached by a Parser, so a repeated scrape allocates neither.
	Key  string
	Full string
}

// Result is a parsed scrape.
type Result struct {
	Series   []Series
	Families int // number of TYPE lines
}

// Parse reads a text exposition. Unknown or malformed lines are skipped and
// counted in the returned error only when nothing parsed at all.
func Parse(r io.Reader) (*Result, error) { return NewParser().Parse(r) }

// Parser parses scrapes of one target. It remembers each series it has
// seen, so a repeated scrape parses only values: names and labels are
// shared with the previous scrape. Callers must not modify them.
type Parser struct {
	cache      map[string]*Series
	types      map[string]string
	classified bool
	last       int
	sbuf       []byte
}

// NewParser returns a parser with an empty cache.
func NewParser() *Parser { return &Parser{cache: map[string]*Series{}} }

// Parse reads one text exposition.
func (p *Parser) Parse(r io.Reader) (*Result, error) {
	if p.types == nil {
		p.types = map[string]string{}
	}
	out := make([]Series, 0, p.last)
	ptrs := make([]*Series, 0, p.last)
	sc := bufio.NewScanner(r)
	if p.sbuf == nil {
		p.sbuf = make([]byte, 64*1024)
	}
	sc.Buffer(p.sbuf, 4*1024*1024)
	bad := 0
	families := 0
	changed := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if line[0] == '#' {
			name, typ, ok := typeLine(line)
			if !ok {
				continue
			}
			families++
			if old, seen := p.types[string(name)]; !seen || old != string(typ) {
				p.types[string(name)] = string(typ)
				changed = true
			}
			continue
		}
		s, c, err := p.line(line)
		if err == errMiss {
			changed, err = true, nil
		}
		if err != nil {
			bad++
			continue
		}
		if c == nil {
			changed = true
		}
		out = append(out, s)
		ptrs = append(ptrs, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 && bad > 0 {
		return nil, fmt.Errorf("promparse: %d malformed lines and no samples", bad)
	}
	// Kinds depend on which families exist, so they are worked out again
	// only when a new series or type appears, and cached otherwise.
	if changed || !p.classified {
		classify(out, p.types)
		for i, c := range ptrs {
			if c != nil {
				c.Kind = out[i].Kind
			}
		}
		p.classified = true
		if len(p.cache) > 50000 {
			p.cache = map[string]*Series{} // a target whose label values never repeat
			p.classified = false
		}
	}
	p.last = len(out)
	return &Result{Series: out, Families: families}, nil
}

// typeLine parses "# TYPE name kind" without allocating.
func typeLine(line []byte) (name, typ []byte, ok bool) {
	rest, found := bytes.CutPrefix(line, []byte("# TYPE "))
	if !found {
		return nil, nil, false
	}
	i := bytes.IndexByte(rest, ' ')
	if i <= 0 {
		return nil, nil, false
	}
	return rest[:i], bytes.TrimSpace(rest[i+1:]), true
}

func parseLine(line []byte) (Series, error) {
	var s Series
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	s.Name = string(line[:i])
	if s.Name == "" {
		return s, fmt.Errorf("empty name")
	}
	if i < len(line) && line[i] == '{' {
		i++
		for {
			for i < len(line) && (line[i] == ' ' || line[i] == ',') {
				i++
			}
			if i >= len(line) {
				return s, fmt.Errorf("unterminated labels")
			}
			if line[i] == '}' {
				i++
				break
			}
			j := i
			for j < len(line) && line[j] != '=' {
				j++
			}
			if j+1 >= len(line) || line[j+1] != '"' {
				return s, fmt.Errorf("bad label")
			}
			name := strings.TrimSpace(string(line[i:j]))
			j += 2
			var val bytes.Buffer
			for j < len(line) && line[j] != '"' {
				if line[j] == '\\' && j+1 < len(line) {
					j++
					switch line[j] {
					case 'n':
						val.WriteByte('\n')
					default:
						val.WriteByte(line[j])
					}
				} else {
					val.WriteByte(line[j])
				}
				j++
			}
			if j >= len(line) {
				return s, fmt.Errorf("unterminated label value")
			}
			s.Labels = append(s.Labels, model.Label{Name: name, Value: val.String()})
			i = j + 1
		}
	}
	rest := strings.Fields(string(line[i:]))
	if len(rest) == 0 {
		return s, fmt.Errorf("no value")
	}
	v, err := parseFloat(rest[0])
	if err != nil {
		return s, err
	}
	s.Value = v
	s.Labels.Sort()
	return s, nil
}

func parseFloat(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// classify sets Kind on each series. algod emits no histogram TYPE: each
// _bucket, _sum and _count is its own counter family. A family is grouped as
// a histogram when <base>_bucket series carry an le label and <base>_sum and
// <base>_count both exist.
func classify(ss []Series, types map[string]string) {
	names := map[string]bool{}
	bucketBases := map[string]bool{}
	for _, s := range ss {
		names[s.Name] = true
		if base, ok := strings.CutSuffix(s.Name, "_bucket"); ok && s.Labels.Get("le") != "" {
			bucketBases[base] = true
		}
	}
	hist := map[string]bool{}
	for base := range bucketBases {
		if names[base+"_sum"] && names[base+"_count"] {
			hist[base] = true
		}
	}
	for i := range ss {
		s := &ss[i]
		if base := histBase(s.Name); base != "" && (hist[base] || types[base] == "histogram") {
			s.Kind = model.KindHistPart
			continue
		}
		t, ok := types[s.Name]
		if !ok {
			// Summary and histogram children inherit the family type.
			if b := histBase(s.Name); b != "" {
				t = types[b]
			}
		}
		switch t {
		case "counter":
			s.Kind = model.KindCounter
		case "summary":
			if strings.HasSuffix(s.Name, "_sum") || strings.HasSuffix(s.Name, "_count") {
				s.Kind = model.KindCounter
			} else {
				s.Kind = model.KindGauge
			}
		default:
			s.Kind = model.KindGauge
		}
	}
}

func histBase(name string) string {
	for _, suf := range []string{"_bucket", "_sum", "_count"} {
		if b, ok := strings.CutSuffix(name, suf); ok {
			return b
		}
	}
	return ""
}

// HistBase returns the histogram family name for a histogram part, or "".
func HistBase(name string) string { return histBase(name) }

// line parses one sample line, reusing the cached name and labels when
// the part before the value was seen before.
func (p *Parser) line(line []byte) (Series, *Series, error) {
	end := keyEnd(line)
	if end <= 0 {
		s, err := parseLine(line)
		return s, nil, err
	}
	if c, ok := p.cache[string(line[:end])]; ok {
		tok := bytes.TrimSpace(line[end:])
		if i := bytes.IndexAny(tok, " \t"); i >= 0 {
			tok = tok[:i] // a timestamp follows
		}
		if len(tok) == 0 {
			return Series{}, nil, fmt.Errorf("no value")
		}
		v, err := parseFloat(string(tok))
		if err != nil {
			return Series{}, nil, err
		}
		s := *c
		s.Value = v
		return s, c, nil
	}
	s, err := parseLine(line)
	if err != nil {
		return s, nil, err
	}
	s.Key = s.Labels.String()
	s.Full = s.Name + "{" + s.Key + "}"
	c := s
	p.cache[string(line[:end])] = &c
	return s, &c, errMiss
}

// errMiss marks a series parsed for the first time.
var errMiss = errors.New("promparse: new series")

// keyEnd returns the end of name{labels}, or -1.
func keyEnd(line []byte) int {
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote && c == '\\':
			i++
		case c == '"':
			inQuote = !inQuote
		case !inQuote && (c == ' ' || c == '\t'):
			return i
		}
	}
	return -1
}
