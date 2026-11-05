package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{"10s": 10 * time.Second, "30d": 30 * 24 * time.Hour, "1y": 365 * 24 * time.Hour,
		"1h30m": 90 * time.Minute, "2w": 14 * 24 * time.Hour, "1d12h": 36 * time.Hour}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil || got.D() != want {
			t.Errorf("%s: got %v %v want %v", in, got.D(), err, want)
		}
	}
	if d, _ := ParseDuration("forever"); d != ForeverDuration {
		t.Error("forever")
	}
	if _, err := ParseDuration("10x"); err == nil {
		t.Error("10x parsed")
	}
}

func TestLoadValidate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte(`{"nodes":[{"name":"mainnet","datadir":"~/.mainnet"},{"name":"r","url":"ssh://relay1/~/.mainnet"}],
	"retention":{"raw":"60d","1h":"forever"}, "web":{"listen":""}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	w := c.Retention.Windows()
	if w[0] != 60*24*time.Hour || w[1] != 365*24*time.Hour || w[3] != 0 {
		t.Fatalf("windows %v", w)
	}
	if c.Web.ListenAddr() != "" {
		t.Fatal("empty listen should disable")
	}
	if !c.Nodes[1].IsSSH() {
		t.Fatal("ssh node")
	}
	os.WriteFile(p, []byte(`{"nodes":[{"name":"a"},{"name":"a","url":"ftp://x"}]}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("bad config loaded")
	}
	os.WriteFile(p, []byte(`{"nodez":[]}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDurationJSON(t *testing.T) {
	var r Retention
	json.Unmarshal([]byte(`{"raw":"30d","1m":"forever"}`), &r)
	b, _ := json.Marshal(r)
	if string(b) != `{"raw":"30d","1m":"forever"}` {
		t.Fatal(string(b))
	}
}
