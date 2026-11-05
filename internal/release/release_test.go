package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"algod.top/internal/version"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	name := BinaryName("linux", "amd64")
	os.WriteFile(filepath.Join(dir, name), []byte("binary"), 0o644)
	sums, err := Checksums(dir, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(priv, sums)
	if err := VerifyWith(pub, sums, sig, name, []byte("binary")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWith(pub, sums, sig, name, []byte("tampered")); err == nil {
		t.Fatal("tampered binary verified")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifyWith(other, sums, sig, name, []byte("binary")); err == nil {
		t.Fatal("wrong key verified")
	}
	if _, err := FromDir(dir, "linux", "amd64"); err == nil {
		t.Fatal("dev build with no key verified a release")
	}
}

func TestURL(t *testing.T) {
	for ver, want := range map[string]string{
		"v0.0.0-20261104123549-37f0e9db466c": BaseURL + "/download/20261104123549-37f0e9db466c/checksums.txt",
		"latest":                             BaseURL + "/latest/download/checksums.txt",
	} {
		if got := URL(ver, "checksums.txt"); got != want {
			t.Errorf("URL(%q) = %q, want %q", ver, got, want)
		}
	}
}

// testRelease signs a fake release for this platform with a fresh key,
// installed as the built-in key until the test ends.
func testRelease(t *testing.T, binary string) Files {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := PublicKey
	PublicKey = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { PublicKey = old })
	dir := t.TempDir()
	name := BinaryName(runtime.GOOS, runtime.GOARCH)
	os.WriteFile(filepath.Join(dir, name), []byte(binary), 0o644)
	sums, err := Checksums(dir, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	return Files{Name: name, Checksums: sums, Signature: Sign(priv, sums), Binary: []byte(binary)}
}

// fakeVersion makes Install read the new binary's version from its
// contents, so a test needs no real binary.
func fakeVersion(t *testing.T) {
	old := ReadBuildInfo
	ReadBuildInfo = func(_ context.Context, path string) (BuildInfo, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return BuildInfo{}, err
		}
		if !strings.HasPrefix(string(b), "v") {
			return BuildInfo{}, errors.New("no version")
		}
		return BuildInfo{Version: string(b)}, nil
	}
	t.Cleanup(func() { ReadBuildInfo = old })
}

func TestInstall(t *testing.T) {
	fakeVersion(t)
	const older, cur, newer = "v0.0.0-20261101000000-aaaaaaaaaaaa", "v0.0.0-20261104000000-bbbbbbbbbbbb", "v0.0.0-20261105090230-8526abb4ba52"
	running := version.Parse(cur)
	exe := filepath.Join(t.TempDir(), "algodtop")
	write := func() { os.WriteFile(exe, []byte(cur), 0o755) }
	content := func() string { b, _ := os.ReadFile(exe); return string(b) }
	ctx := context.Background()

	write()
	f := testRelease(t, newer)
	res, err := Install(ctx, f, exe, running)
	if err != nil || res.From != cur || res.To != newer || res.Same || content() != newer {
		t.Fatalf("newer: %+v %v, file %q", res, err, content())
	}
	// Windows cannot replace a running binary, so the old one moves aside.
	want := 1
	if runtime.GOOS == "windows" {
		want = 2
	}
	if ents, _ := os.ReadDir(filepath.Dir(exe)); len(ents) != want {
		t.Errorf("left files behind: %v", ents)
	}

	write()
	if res, err := Install(ctx, testRelease(t, cur), exe, running); err != nil || !res.Same {
		t.Errorf("same: %+v %v", res, err)
	}
	// Each case signs its release when it runs, since signing installs a
	// new built-in key.
	for name, c := range map[string]struct {
		f    func() Files
		code string
	}{
		"older":     {func() Files { return testRelease(t, older) }, CodeOlder},
		"no json":   {func() Files { return testRelease(t, "an old binary") }, CodeUnreadable},
		"tampered":  {func() Files { f := testRelease(t, newer); f.Binary = []byte(newer + "x"); return f }, CodeSignature},
		"platform":  {func() Files { f := testRelease(t, newer); f.Name = "algodtop_plan9_mips"; return f }, CodePlatform},
		"other key": {func() Files { f := testRelease(t, newer); testRelease(t, newer); return f }, CodeSignature},
	} {
		write()
		_, err := Install(ctx, c.f(), exe, running)
		if !IsCode(err, c.code) {
			t.Errorf("%s: %v, want code %s", name, err, c.code)
		}
		if content() != cur {
			t.Errorf("%s replaced the binary", name)
		}
	}

	// A dev build takes any signed release.
	write()
	if _, err := Install(ctx, testRelease(t, older), exe, version.Build{Version: "dev"}); err != nil || content() != older {
		t.Errorf("dev: %v", err)
	}

	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		write()
		dir := filepath.Dir(exe)
		os.Chmod(dir, 0o555)
		defer os.Chmod(dir, 0o755)
		if _, err := Install(ctx, testRelease(t, newer), exe, running); !IsCode(err, CodeNotWritable) {
			t.Errorf("read-only directory: %v", err)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r/latest":
			http.Redirect(w, r, "/r/tag/20261105090230-8526abb4ba52", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL + "/r"
	defer func() { BaseURL = old }()
	v, err := Latest(context.Background())
	if err != nil || v != "v0.0.0-20261105090230-8526abb4ba52" {
		t.Fatalf("%q %v", v, err)
	}
	BaseURL = srv.URL + "/none"
	if _, err := Latest(context.Background()); err == nil {
		t.Error("no redirect gave a version")
	}
}

// TestLegacyNames: a release from before the rename, whose files are
// named algodtop_<os>_<arch>, still verifies and installs.
func TestLegacyNames(t *testing.T) {
	fakeVersion(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := PublicKey
	PublicKey = base64.StdEncoding.EncodeToString(pub)
	defer func() { PublicKey = old }()
	dir := t.TempDir()
	name := legacyName(runtime.GOOS, runtime.GOARCH)
	const newer = "v0.0.0-20261105090230-8526abb4ba52"
	os.WriteFile(filepath.Join(dir, name), []byte(newer), 0o644)
	sums, _ := Checksums(dir, []string{name})
	os.WriteFile(filepath.Join(dir, "checksums.txt"), sums, 0o644)
	os.WriteFile(filepath.Join(dir, "checksums.txt.sig"), Sign(priv, sums), 0o644)
	f, err := FilesFromDir(dir, runtime.GOOS, runtime.GOARCH)
	if err != nil || f.Name != name {
		t.Fatalf("%q %v", f.Name, err)
	}
	exe := filepath.Join(t.TempDir(), "algod.top")
	os.WriteFile(exe, []byte("old"), 0o755)
	if res, err := Install(context.Background(), f, exe, version.Parse("v0.0.0-20261101000000-aaaaaaaaaaaa")); err != nil || res.To != newer {
		t.Fatalf("%+v %v", res, err)
	}
	if BinaryName("windows", "amd64") != "algod.top_windows_amd64.exe" {
		t.Error(BinaryName("windows", "amd64"))
	}
}
