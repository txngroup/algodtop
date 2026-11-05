package algod

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeName(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "algorand")
	os.Mkdir(pkg, 0o755)
	os.WriteFile(filepath.Join(pkg, "genesis.json"), []byte(`{"network":"mainnet","id":"v1.0"}`), 0o644)
	for dir, want := range map[string]string{
		"/home/a/.mainnet":             "mainnet",
		"/home/a/node/data":            "node-data",
		"/home/a/.testnet/data":        "testnet-data",
		pkg:                            "mainnet",
		filepath.Join(root, "nogen"):   "nogen",
		"/var/lib/algorand-unreadable": "algorand-unreadable",
	} {
		if got := nodeName(dir); got != want {
			t.Errorf("%s: got %s, want %s", dir, got, want)
		}
	}
	// No readable genesis.json: keep the directory name.
	if got := nodeName(filepath.Join(root, "x", "algorand")); got != "algorand" {
		t.Errorf("got %s", got)
	}
}
