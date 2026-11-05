package daemon_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/config"
	"algod.top/internal/daemon"
	"algod.top/internal/fakealgod"
	"algod.top/internal/fsx"
	"algod.top/internal/release"
)

// TestUpgradeRestarts sends a signed release to an agent over its socket.
// The agent replaces its binary, and Run returns with the binary to exec.
func TestUpgradeRestarts(t *testing.T) {
	dir := shortDir(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	oldKey, oldRead := release.PublicKey, release.ReadBuildInfo
	release.PublicKey = base64.StdEncoding.EncodeToString(pub)
	const newer = "v0.0.0-20991231000000-cccccccccccc"
	release.ReadBuildInfo = func(context.Context, string) (release.BuildInfo, error) {
		return release.BuildInfo{Version: newer}, nil
	}
	defer func() { release.PublicKey, release.ReadBuildInfo = oldKey, oldRead }()

	exe := filepath.Join(dir, "algodtop")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	rel := filepath.Join(dir, "rel")
	os.MkdirAll(rel, 0o755)
	name := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	os.WriteFile(filepath.Join(rel, name), []byte("new binary"), 0o644)
	sums, err := release.Checksums(rel, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	cfg := &config.Config{Path: filepath.Join(dir, "agent.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &empty}, NTP: config.NTP{Disable: true}}
	r := start(t, cfg, true, func(o *daemon.Options) { o.Exe = exe })

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range []struct{ field, file, data string }{
		{"checksums", "checksums.txt", string(sums)}, {"signature", "checksums.txt.sig", string(release.Sign(priv, sums))}, {"binary", name, "new binary"},
	} {
		w, _ := mw.CreateFormFile(p.field, p.file)
		w.Write([]byte(p.data))
	}
	mw.Close()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", fsx.SocketAddr(r.sock))
	}}}
	var resp *http.Response
	waitFor(t, "the agent socket", 10*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodPost, "http://algodtop/api/admin/upgrade", bytes.NewReader(body.Bytes()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err = hc.Do(req)
		return err == nil
	})
	defer resp.Body.Close()
	var res release.Result
	json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode != http.StatusOK || res.To != newer || !res.Restart || res.Path != exe {
		t.Fatalf("%s %+v", resp.Status, res)
	}
	select {
	case err := <-r.done:
		r.done <- err // the cleanup reads it too
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not stop to restart")
	}
	if r.d.RestartExe() != exe {
		t.Errorf("RestartExe %q", r.d.RestartExe())
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Errorf("binary %q", b)
	}
}

// TestStatusFollowsRounds: live viewers get a status message for each new
// round, rather than only every 2 s.
func TestStatusFollowsRounds(t *testing.T) {
	dir := shortDir(t)
	fake, err := fakealgod.Start(fakealgod.Options{DataDir: filepath.Join(dir, "node"), RoundEvery: 400 * time.Millisecond, Token: "t", AdminToken: "a", Network: "testnet"})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	empty := ""
	cfg := &config.Config{Path: filepath.Join(dir, "config.json"), StoreDir: filepath.Join(dir, "store"), Web: config.Web{Listen: &empty}, NTP: config.NTP{Disable: true},
		Nodes: []config.Node{{Name: "fake", DataDir: filepath.Join(dir, "node")}}}
	r := start(t, cfg, false)
	ch, stop := r.d.Subscribe(nil, nil)
	defer stop()
	rounds := map[uint64]bool{}
	pushes := 0
	// Count for 3 s from the first round, after the node connects.
	var deadline <-chan time.Time
	timeout := time.After(20 * time.Second)
	for done := false; !done; {
		select {
		case m := <-ch:
			if m.Event != "status" {
				continue
			}
			pushes++
			for _, n := range m.Data.(map[string]any)["nodes"].([]api.NodeView) {
				if n.Round > 0 {
					rounds[n.Round] = true
					if deadline == nil {
						deadline = time.After(3 * time.Second)
					}
				}
			}
		case <-deadline:
			done = true
		case <-timeout:
			t.Fatal("no round within 20 s")
		}
	}
	// Rounds every 0.4 s for 3 s: the 2 s timer alone would show 2 or 3.
	if len(rounds) < 5 {
		t.Errorf("saw %d distinct rounds in %d pushes", len(rounds), pushes)
	}
}
