package logparse

import (
	"strings"
)

// Template replaces the variable parts of a message with placeholders:
// addresses, numbers, base32 IDs, hashes, durations, peer IDs, multiaddrs,
// IP addresses, host names with a port and hex. Messages that differ only
// in those parts share a template.
func Template(msg string) string {
	if len(msg) > 400 {
		msg = msg[:400]
	}
	var b strings.Builder
	b.Grow(len(msg))
	i := 0
	for i < len(msg) {
		c := msg[i]
		if !isWordByte(c) {
			b.WriteByte(c)
			i++
			continue
		}
		j := i
		for j < len(msg) && isWordByte(msg[j]) {
			j++
		}
		b.WriteString(classify(msg[i:j]))
		i = j
	}
	return collapse(b.String())
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == ':' || c == '/' || c == '-' || c == '_' || c == '+' || c == '=' || c == '@'
}

func classify(w string) string {
	// Keep trailing sentence punctuation outside the token.
	suffix := ""
	for len(w) > 1 && (w[len(w)-1] == '.' || w[len(w)-1] == ':' || w[len(w)-1] == ',') {
		suffix = string(w[len(w)-1]) + suffix
		w = w[:len(w)-1]
	}
	switch {
	case strings.HasPrefix(w, "/ip4/") || strings.HasPrefix(w, "/ip6/") || strings.HasPrefix(w, "/dns"):
		return "<maddr>" + suffix
	case strings.HasPrefix(w, "12D3KooW") || strings.HasPrefix(w, "Qm") && len(w) == 46:
		return "<peer>" + suffix
	case strings.HasPrefix(w, "http://") || strings.HasPrefix(w, "https://") || strings.HasPrefix(w, "ws://"):
		return "<url>" + suffix
	case isIPPort(w) || isHostPort(w):
		return "<addr>" + suffix
	case isAlgoAddr(w):
		return "<account>" + suffix
	case isBase32ID(w):
		return "<id>" + suffix
	case isNumber(w):
		return "<n>" + suffix
	case isDuration(w):
		return "<dur>" + suffix
	case isHex(w):
		return "<hex>" + suffix
	}
	// key=value pairs keep the key.
	if k, v, ok := strings.Cut(w, "="); ok && v != "" {
		return k + "=" + classify(v) + suffix
	}
	if hasDigitRun(w) && digitShare(w) >= 0.5 {
		return digitsToN(w) + suffix
	}
	return w + suffix
}

func isAlgoAddr(w string) bool { return len(w) == 58 && allBase32(w) }

// isBase32ID wants a digit in shorter words, which keeps upper-case words
// out. libp2p logs DHT keys such as CIQAAAA... that are mostly padding and
// can lack one.
func isBase32ID(w string) bool {
	return len(w) >= 26 && allBase32(w) && (hasDigit(w) || len(w) >= 40)
}

func allBase32(w string) bool {
	for i := 0; i < len(w); i++ {
		c := w[i]
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func hasDigit(w string) bool {
	for i := 0; i < len(w); i++ {
		if w[i] >= '0' && w[i] <= '9' {
			return true
		}
	}
	return false
}

func isHex(w string) bool {
	s := strings.TrimPrefix(w, "0x")
	if len(s) < 8 {
		return false
	}
	digit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			digit = true
			continue
		}
		if !(c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return digit || strings.HasPrefix(w, "0x")
}

func isNumber(w string) bool {
	if w == "" {
		return false
	}
	s := strings.TrimLeft(w, "+-")
	dot := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			dot++
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0 && dot <= 3 && s != "."
}

func isDuration(w string) bool {
	units := []string{"ns", "us", "\u00b5s", "ms", "s", "m", "h"}
	for _, u := range units {
		if strings.HasSuffix(w, u) {
			n := strings.TrimSuffix(w, u)
			if n != "" && isNumber(n) {
				return true
			}
			// compound like 1m30.5s
			if n != "" && strings.ContainsAny(n, "hms") && isNumber(strings.Map(func(r rune) rune {
				if r == 'h' || r == 'm' || r == 's' {
					return '0'
				}
				return r
			}, n)) {
				return true
			}
		}
	}
	return false
}

func isIPPort(w string) bool {
	host := w
	if i := strings.LastIndexByte(w, ':'); i > 0 {
		port := w[i+1:]
		if !isNumber(port) || strings.Contains(port, ".") {
			return false
		}
		host = w[:i]
	}
	host = strings.Trim(host, "[]")
	parts := strings.Split(host, ".")
	if len(parts) == 4 {
		for _, p := range parts {
			if p == "" || len(p) > 3 || !isNumber(p) {
				return false
			}
		}
		return true
	}
	return strings.Count(host, ":") >= 2 && isHexish(strings.ReplaceAll(host, ":", ""))
}

// isHostPort matches a DNS name with a port, such as a relay from SRV
// records. Each relay would otherwise get its own template. The name needs
// a dot and an alphabetic last label, and a Go source position such as
// node.go:42 does not count.
func isHostPort(w string) bool {
	i := strings.LastIndexByte(w, ':')
	if i <= 0 {
		return false
	}
	port := w[i+1:]
	if len(port) == 0 || len(port) > 5 || !isNumber(port) || strings.ContainsAny(port, ".+-") {
		return false
	}
	labels := strings.Split(w[:i], ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for j := 0; j < len(l); j++ {
			c := l[j]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 || tld == "go" {
		return false
	}
	for j := 0; j < len(tld); j++ {
		if c := tld[j]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func isHexish(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func hasDigitRun(w string) bool {
	run := 0
	for i := 0; i < len(w); i++ {
		if w[i] >= '0' && w[i] <= '9' {
			run++
			if run >= 2 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func digitsToN(w string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c >= '0' && c <= '9' {
			if !in {
				b.WriteString("<n>")
				in = true
			}
			continue
		}
		in = false
		b.WriteByte(c)
	}
	return b.String()
}

// collapse joins repeated placeholders such as "<n> <n> <n>".
func collapse(s string) string {
	for _, p := range []string{"<n> <n>", "<n>, <n>", "<id> <id>"} {
		for strings.Contains(s, p) {
			s = strings.ReplaceAll(s, p, p[:strings.IndexByte(p, '>')+1])
		}
	}
	return s
}

func digitShare(w string) float64 {
	d := 0
	for i := 0; i < len(w); i++ {
		if w[i] >= '0' && w[i] <= '9' {
			d++
		}
	}
	return float64(d) / float64(len(w))
}
