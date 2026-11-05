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

// TestFuncDirs: FUNC keeps <app>/algorand and <app>/voi, unless
// <app>/<name>.data names another parent directory.
func TestFuncDirs(t *testing.T) {
	app := t.TempDir()
	moved := t.TempDir()
	os.WriteFile(filepath.Join(app, "voi.data"), []byte(moved+"\r\n"), 0o644)
	got := funcDirs(app)
	if len(got) != 2 || got[0] != filepath.Join(app, "algorand") || got[1] != filepath.Join(moved, "voi") {
		t.Errorf("%v", got)
	}
	// A FUNC Algorand node is named after its network.
	dir := got[0]
	os.MkdirAll(dir, 0o755)
	for name, data := range map[string]string{"genesis.json": `{"network":"mainnet","id":"v1.0"}`, "algod.net": "127.0.0.1:8080", "algod.token": "t", "config.json": "{}"} {
		os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644)
	}
	if !LooksLikeDataDir(dir) || nodeName(dir) != "mainnet" {
		t.Errorf("%s: looks like a data dir %v, name %q", dir, LooksLikeDataDir(dir), nodeName(dir))
	}
}
