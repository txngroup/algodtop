package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
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
