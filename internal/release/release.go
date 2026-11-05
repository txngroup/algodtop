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

	"algod.top/internal/version"
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
func BinaryName(goos, goarch string) string { return binaryName("algod.top", goos, goarch) }

// legacyName is the file name in releases made before the program took
// the name algod.top.
func legacyName(goos, goarch string) string { return binaryName("algodtop", goos, goarch) }

func binaryName(prog, goos, goarch string) string {
	n := fmt.Sprintf("%s_%s_%s", prog, goos, goarch)
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// nameIn picks the platform's file name that a checksums file lists, so
// an older release with the old names still installs.
func nameIn(checksums []byte, goos, goarch string) string {
	if _, err := lookup(checksums, BinaryName(goos, goarch)); err != nil {
		if _, err := lookup(checksums, legacyName(goos, goarch)); err == nil {
			return legacyName(goos, goarch)
		}
	}
	return BinaryName(goos, goarch)
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
		return errors.New("release: checksums signature is not valid for the algod.top release key")
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
	f, err := FilesFromDir(dir, goos, goarch)
	return f.Binary, err
}

// FilesFromDir reads and verifies one platform's files from a local
// release directory.
func FilesFromDir(dir, goos, goarch string) (Files, error) {
	var f Files
	var err error
	if f.Checksums, err = os.ReadFile(filepath.Join(dir, "checksums.txt")); err != nil {
		return Files{}, err
	}
	f.Name = nameIn(f.Checksums, goos, goarch)
	if f.Signature, err = os.ReadFile(filepath.Join(dir, "checksums.txt.sig")); err != nil {
		return Files{}, err
	}
	if f.Binary, err = os.ReadFile(filepath.Join(dir, f.Name)); err != nil {
		return Files{}, err
	}
	if err := f.Verify(); err != nil {
		return Files{}, err
	}
	return f, nil
}

// Files is one platform's binary from a release, with the signed
// checksums file that vouches for it. The hub forwards all three to an
// agent, which checks the signature again before it installs anything.
type Files struct {
	Version   string // the release's pseudo-version, when known
	Name      string // the release file name, such as algodtop_linux_amd64
	Checksums []byte
	Signature []byte
	Binary    []byte
}

// Verify checks the files against the built-in release key.
func (f Files) Verify() error { return Verify(f.Checksums, f.Signature, f.Name, f.Binary) }

// Fetch downloads a release on the hub, so the remote needs no internet
// access, and verifies it.
func Fetch(ctx context.Context, ver, goos, goarch string) ([]byte, error) {
	f, err := FetchFiles(ctx, ver, goos, goarch)
	return f.Binary, err
}

// FetchFiles downloads and verifies one platform's files of a release.
// "latest" is resolved to a version first, so the three downloads come
// from the same release even if a new one is published meanwhile.
func FetchFiles(ctx context.Context, ver, goos, goarch string) (Files, error) {
	if _, err := key(); err != nil {
		return Files{}, err
	}
	if ver == "latest" {
		v, err := Latest(ctx)
		if err != nil {
			return Files{}, err
		}
		ver = v
	}
	f := Files{Version: ver}
	var err error
	if f.Checksums, err = get(ctx, URL(ver, "checksums.txt")); err != nil {
		return Files{}, err
	}
	f.Name = nameIn(f.Checksums, goos, goarch)
	if f.Signature, err = get(ctx, URL(ver, "checksums.txt.sig")); err != nil {
		return Files{}, err
	}
	if f.Binary, err = get(ctx, URL(ver, f.Name)); err != nil {
		return Files{}, err
	}
	if err := f.Verify(); err != nil {
		return Files{}, err
	}
	return f, nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// Latest returns the newest release's version. GitHub redirects
// releases/latest to releases/tag/TAG, and the tag is the pseudo-version
// without its "v0.0.0-".
func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "algodtop")
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if resp.StatusCode/100 != 3 || i < 0 {
		return "", fmt.Errorf("GET %s/latest: %s, no release tag in the redirect", BaseURL, resp.Status)
	}
	v := "v0.0.0-" + loc[i+len("/tag/"):]
	if _, ok := version.TimeOf(v); !ok {
		return "", fmt.Errorf("the latest release tag %q is not a version", loc[i+len("/tag/"):])
	}
	return v, nil
}
