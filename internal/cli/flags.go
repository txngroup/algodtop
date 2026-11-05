package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// flagSet parses -x, --long, --long=value and repeatable flags. The stdlib
// flag package has no short and long forms for one flag.
type flagSet struct {
	name  string
	defs  []*flagDef
	byKey map[string]*flagDef
	args  []string
}

type flagDef struct {
	short, long string
	help        string
	boolean     bool
	repeat      bool
	vals        []string
	set         bool
	valueName   string
}

func newFlags(name string) *flagSet { return &flagSet{name: name, byKey: map[string]*flagDef{}} }

func (f *flagSet) add(short, long, valueName, help string, boolean, repeat bool) *flagDef {
	d := &flagDef{short: short, long: long, help: help, boolean: boolean, repeat: repeat, valueName: valueName}
	f.defs = append(f.defs, d)
	if short != "" {
		f.byKey["-"+short] = d
	}
	if long != "" {
		f.byKey["--"+long] = d
	}
	return d
}

func (f *flagSet) str(short, long, valueName, help string) *flagDef {
	return f.add(short, long, valueName, help, false, false)
}
func (f *flagSet) list(short, long, valueName, help string) *flagDef {
	return f.add(short, long, valueName, help, false, true)
}
func (f *flagSet) flag(short, long, help string) *flagDef {
	return f.add(short, long, "", help, true, false)
}

func (d *flagDef) String() string {
	if len(d.vals) == 0 {
		return ""
	}
	return d.vals[len(d.vals)-1]
}

func (d *flagDef) Bool() bool { return d.set }

func (d *flagDef) Int(def int) (int, error) {
	if !d.set {
		return def, nil
	}
	return strconv.Atoi(d.String())
}

func (d *flagDef) Duration(def time.Duration) (time.Duration, error) {
	if !d.set {
		return def, nil
	}
	s := d.String()
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	return time.ParseDuration(s)
}

func (f *flagSet) parse(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.args = append(f.args, args[i+1:]...)
			return nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			f.args = append(f.args, a)
			continue
		}
		key, val, hasVal := strings.Cut(a, "=")
		d, ok := f.byKey[key]
		if !ok {
			// -n5 style for short flags.
			if len(key) > 2 && key[1] != '-' {
				if d2, ok2 := f.byKey[key[:2]]; ok2 && !d2.boolean {
					d, ok, val, hasVal = d2, true, key[2:], true
				}
			}
		}
		if !ok && (key == "-h" || key == "--help") {
			// A command that defines no help flag still answers one.
			fmt.Printf("Usage: %s [flags]\n\n%s", f.name, f.usage())
			os.Exit(0)
		}
		if !ok {
			return fmt.Errorf("unknown flag %s (see %s --help)", key, f.name)
		}
		if d.boolean {
			d.set = !(hasVal && (val == "false" || val == "0"))
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s needs a value", key)
			}
			i++
			val = args[i]
		}
		d.set = true
		if d.repeat {
			d.vals = append(d.vals, val)
		} else {
			d.vals = []string{val}
		}
	}
	return nil
}

func (f *flagSet) usage() string {
	var b strings.Builder
	defs := append([]*flagDef(nil), f.defs...)
	sort.SliceStable(defs, func(i, j int) bool { return false })
	for _, d := range defs {
		var names []string
		if d.short != "" {
			names = append(names, "-"+d.short)
		}
		if d.long != "" {
			names = append(names, "--"+d.long)
		}
		n := strings.Join(names, ", ")
		if d.valueName != "" {
			n += " " + d.valueName
		}
		fmt.Fprintf(&b, "  %-34s %s\n", n, d.help)
	}
	return b.String()
}
