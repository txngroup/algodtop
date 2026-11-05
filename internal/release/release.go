// Package release fetches and verifies algodtop release binaries. Every
// merge to main publishes a GitHub release with a binary per platform, a
// checksums file and an ed25519 signature over it. Only binaries whose
// checksum appears in a validly signed file are installed.
package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PublicKey is the release signing key, base64. It is public, so every
// build carries it and can verify a download. A test can set another with
// -ldflags "-X algod.top/internal/release.PublicKey=...".
var PublicKey = "OHC+ZemH92eInOsGQ79k24Fu7QFpIo8rks1XoMMLx6U="

// BaseURL is the GitHub releases page. A release's files are under
// download/TAG/, and the newest release's under latest/download/.
var BaseURL = "https://github.com/txngroup/algodtop/releases"

// ErrNoKey is returned when the binary has no release key built in.
var ErrNoKey = errors.New("this build has no release signing key; use --binary with --unsigned for development")

// Tag is the release tag for a version. A release is named by its
// commit, as Go names it: the tag is the pseudo-version without its
// "v0.0.0-", such as 20261104123549-37f0e9db466c. It is not a semver
// tag, so go install keeps following main. "latest" stays as it is.
func Tag(ver string) string {
	return strings.TrimPrefix(ver, "v0.0.0-")
}

// URL is where a release file can be downloaded.
func URL(ver, name string) string {
	if ver == "latest" {
		return BaseURL + "/latest/download/" + name
	}
	return BaseURL + "/download/" + Tag(ver) + "/" + name
}

// BinaryName returns the release file name for an OS and architecture.
func BinaryName(goos, goarch string) string {
	n := fmt.Sprintf("algodtop_%s_%s", goos, goarch)
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

func key() (ed25519.PublicKey, error) {
	if PublicKey == "" {
		return nil, ErrNoKey
	}
	b, err := base64.StdEncoding.DecodeString(PublicKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("release: bad built-in public key")
	}
	return ed25519.PublicKey(b), nil
}

// Verify checks the signature over the checksums file and the binary's
// sha256 against its entry.
func Verify(checksums, sig []byte, name string, binary []byte) error {
	pub, err := key()
	if err != nil {
		return err
	}
	return VerifyWith(pub, checksums, sig, name, binary)
}

// VerifyWith is Verify with an explicit key, for tests.
func VerifyWith(pub ed25519.PublicKey, checksums, sig []byte, name string, binary []byte) error {
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		s = sig
	}
	if !ed25519.Verify(pub, checksums, s) {
		return errors.New("release: checksums signature is not valid for the algodtop release key")
	}
	want, err := lookup(checksums, name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(binary)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("release: %s does not match its signed checksum", name)
	}
	return nil
}

func lookup(checksums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("release: %s is not in the checksums file", name)
}

// Sign signs a checksums file.
func Sign(priv ed25519.PrivateKey, checksums []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, checksums)) + "\n")
}

// Checksums renders "sha256  name" lines for files in dir.
func Checksums(dir string, names []string) ([]byte, error) {
	var b bytes.Buffer
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		s := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(s[:]), n)
	}
	return b.Bytes(), nil
}

// FromDir reads and verifies a binary from a local release directory
// holding checksums.txt, checksums.txt.sig and the binaries.
func FromDir(dir, goos, goarch string) ([]byte, error) {
	name := BinaryName(goos, goarch)
	sums, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, "checksums.txt.sig"))
	if err != nil {
		return nil, err
	}
	bin, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	if err := Verify(sums, sig, name, bin); err != nil {
		return nil, err
	}
	return bin, nil
}

// Fetch downloads a release on the hub, so the remote needs no internet
// access, and verifies it.
func Fetch(ctx context.Context, ver, goos, goarch string) ([]byte, error) {
	if _, err := key(); err != nil {
		return nil, err
	}
	get := func(name string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, URL(ver, name), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "algodtop")
		c := &http.Client{Timeout: 10 * time.Minute}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GET %s: %s", URL(ver, name), resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	}
	sums, err := get("checksums.txt")
	if err != nil {
		return nil, err
	}
	sig, err := get("checksums.txt.sig")
	if err != nil {
		return nil, err
	}
	name := BinaryName(goos, goarch)
	bin, err := get(name)
	if err != nil {
		return nil, err
	}
	if err := Verify(sums, sig, name, bin); err != nil {
		return nil, err
	}
	return bin, nil
}
