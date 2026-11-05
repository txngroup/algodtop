package algod

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Found is one discovered data directory.
type Found struct {
	Name    string `json:"name"`
	DataDir string `json:"datadir"`
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	Source  string `json:"source"` // env, process, scan
	Network string `json:"network,omitempty"`
}

// Process is a running process with its command line.
type Process struct {
	PID  int
	Args []string
}

// Discover finds algod data directories: $ALGORAND_DATA, running algod
// processes, and the platform's usual locations. listAlgod lists running
// algod processes; alive reports whether a PID is a live algod.
func Discover(listAlgod func() []Process, alive func(pid int) bool) []Found {
	seen := map[string]*Found{}
	var order []string
	add := func(dir, source string) *Found {
		dir = filepath.Clean(dir)
		if f, ok := seen[dir]; ok {
			return f
		}
		if !LooksLikeDataDir(dir) {
			return nil
		}
		f := &Found{DataDir: dir, Source: source}
		seen[dir] = f
		order = append(order, dir)
		return f
	}
	if env := os.Getenv("ALGORAND_DATA"); env != "" {
		add(ExpandHome(env), "env")
	}
	var procs []Process
	if listAlgod != nil {
		procs = listAlgod()
	}
	for _, p := range procs {
		for i, a := range p.Args {
			var dir string
			switch {
			case a == "-d" && i+1 < len(p.Args):
				dir = p.Args[i+1]
			case strings.HasPrefix(a, "-d="):
				dir = a[3:]
			}
			if dir != "" {
				if f := add(ExpandHome(dir), "process"); f != nil {
					f.Running, f.PID = true, p.PID
				}
			}
		}
	}
	for _, dir := range scanLocations() {
		add(dir, "scan")
	}
	// Match PIDs from algod.pid against live processes, which also finds
	// algod started through $ALGORAND_DATA rather than -d.
	live := map[int]bool{}
	for _, p := range procs {
		live[p.PID] = true
	}
	out := make([]Found, 0, len(order))
	names := map[string]int{}
	for _, dir := range order {
		f := seen[dir]
		if !f.Running {
			if pid, err := (DataDir{dir}).PID(); err == nil && (live[pid] || (alive != nil && alive(pid))) {
				f.Running, f.PID = true, pid
			}
		}
		if g, err := (DataDir{dir}).Genesis(); err == nil {
			f.Network = g.Network
		}
		f.Name = nodeName(dir)
		names[f.Name]++
		if names[f.Name] > 1 {
			f.Name = f.Name + "-" + itoa(names[f.Name])
		}
		out = append(out, *f)
	}
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// nodeName derives a short name from a data directory: ~/.mainnet becomes
// mainnet, and /var/lib/algorand becomes the network in its genesis.json.
func nodeName(dir string) string {
	base := filepath.Base(dir)
	base = strings.TrimPrefix(base, ".")
	// Package installs keep the data in /var/lib/algorand, which says
	// nothing about the node; genesis.json names the network.
	if base == "algorand" {
		if g, err := (DataDir{Path: dir}).Genesis(); err == nil && g.Network != "" {
			return g.Network
		}
	}
	if base == "data" || base == "node" {
		base = filepath.Base(filepath.Dir(dir)) + "-" + base
		base = strings.TrimPrefix(base, ".")
	}
	if base == "" {
		base = "node"
	}
	return base
}

// NodeName is exported for callers that name a -d directory.
func NodeName(dir string) string { return nodeName(dir) }

func scanLocations() []string {
	var out []string
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		// Added once the official installer defines its locations.
	default:
		if home != "" {
			ents, _ := os.ReadDir(home)
			for _, e := range ents {
				if e.IsDir() && strings.HasPrefix(e.Name(), ".") {
					out = append(out, filepath.Join(home, e.Name()))
				}
			}
			for _, sub := range []string{"node/data", "algorand/data", "node"} {
				out = append(out, filepath.Join(home, sub))
			}
		}
		out = append(out, "/var/lib/algorand")
	}
	sort.Strings(out)
	return out
}
