//go:build windows

package cli

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"algod.top/internal/api"
	"algod.top/internal/release"
)

// TestWindowsServiceUpgrade installs the agent as a Windows service, sends
// it a newer signed release over its socket, and checks that the service
// manager restarts it on the new binary. It changes the machine, so it
// runs only where ALGODTOP_TEST_WINSVC=1, as on CI's Windows runner, which
// is elevated.
func TestWindowsServiceUpgrade(t *testing.T) {
	if os.Getenv("ALGODTOP_TEST_WINSVC") != "1" {
		t.Skip("set ALGODTOP_TEST_WINSVC=1 on a Windows machine you can change, from an elevated prompt")
	}
	if !(scm{}).elevated() {
		t.Fatal("needs an elevated prompt")
	}
	p := windowsPlan(os.Getenv("ProgramData"), true)
	if (scm{}).exists(p.name) {
		t.Fatalf("a %s service is already installed", p.name)
	}
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	const older, newer = "v0.0.0-20261101000000-aaaaaaaaaaaa", "v0.0.0-20261102000000-bbbbbbbbbbbb"
	build := func(ver, out string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", out, "-ldflags",
			"-X algod.top/internal/release.PublicKey="+key+" -X algod.top/internal/version.Version="+ver, "algod.top")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("build %s: %v", ver, err)
		}
	}
	a := filepath.Join(dir, "a", "algod.top.exe")
	rel := filepath.Join(dir, "rel")
	name := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	build(older, a)
	build(newer, filepath.Join(rel, name))
	sums, err := release.Checksums(rel, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := os.ReadFile(filepath.Join(rel, name))

	// An agent with no nodes still runs and answers.
	os.MkdirAll(p.dir, 0o700)
	if err := os.WriteFile(p.config, []byte(`{"web":{"listen":""},"ntp":{"disable":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command(a, "service", "uninstall", "--agent").Run()
		(scm{}).remove(p.name)
		os.RemoveAll(p.dir)
	})
	run := func(exe string, args ...string) string {
		t.Helper()
		out, err := exec.Command(exe, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", exe, args, err, out)
		}
		return string(out)
	}
	t.Log(run(a, "service", "install", "--agent"))

	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", p.socket)
	}}}
	version := func() string {
		resp, err := client.Get("http://algodtop/api/status")
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		var st api.DaemonStatus
		json.NewDecoder(resp.Body).Decode(&st)
		return shortVersion(st.Version)
	}
	if v := version(); v != older {
		t.Fatalf("the service runs %q, want %s", v, older)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct {
		field, file string
		data        []byte
	}{{"checksums", "checksums.txt", sums}, {"signature", "checksums.txt.sig", release.Sign(priv, sums)}, {"binary", name, bin}} {
		w, _ := mw.CreateFormFile(part.field, part.file)
		w.Write(part.data)
	}
	mw.Close()
	resp, err := client.Post("http://algodtop/api/admin/upgrade", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	var res release.Result
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || res.To != newer || !res.Restart {
		t.Fatalf("upgrade: %s %+v", resp.Status, res)
	}
	// The service stops with an error code, and its recovery action starts
	// it again from the new binary.
	deadline := time.Now().Add(2 * time.Minute)
	for version() != newer {
		if time.Now().After(deadline) {
			state, _ := (scm{}).state(p.name)
			logb, _ := os.ReadFile(p.log)
			t.Fatalf("still not %s after 2 minutes; service %s\n%s", newer, state, logb)
		}
		time.Sleep(time.Second)
	}
	if state, _ := (scm{}).state(p.name); state != "running" {
		t.Errorf("service %s", state)
	}
	if out := run(a, "service", "status", "--agent"); !strings.Contains(out, "running") || !strings.Contains(out, "answers") {
		t.Errorf("status:\n%s", out)
	}
	if logb, _ := os.ReadFile(p.log); !strings.Contains(string(logb), "upgraded") {
		t.Errorf("the log does not mention the upgrade:\n%s", logb)
	}
	run(a, "service", "uninstall", "--agent")
	if (scm{}).exists(p.name) {
		t.Error("the service is still installed")
	}
}
