// Command algodtop-sign creates a release signing key, signs a release
// directory's checksums and verifies them. It is a release tool, not part
// of algodtop.
//
//	algodtop-sign keygen KEYFILE            writes the private key, prints the public key
//	algodtop-sign sign KEYFILE DIR FILE...  writes DIR/checksums.txt and .sig
//	algodtop-sign verify DIR FILE...        checks them with the built-in public key
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"algod.top/internal/release"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: algodtop-sign keygen KEYFILE | sign KEYFILE DIR FILE... | verify DIR FILE...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		check(err)
		check(os.WriteFile(os.Args[2], []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600))
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
	case "sign":
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "usage: algodtop-sign sign KEYFILE DIR FILE...")
			os.Exit(2)
		}
		kb, err := os.ReadFile(os.Args[2])
		check(err)
		priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(kb)))
		check(err)
		dir := os.Args[3]
		sums, err := release.Checksums(dir, os.Args[4:])
		check(err)
		check(os.WriteFile(filepath.Join(dir, "checksums.txt"), sums, 0o644))
		check(os.WriteFile(filepath.Join(dir, "checksums.txt.sig"), release.Sign(ed25519.PrivateKey(priv), sums), 0o644))
		fmt.Printf("signed %d files in %s\n", len(os.Args)-4, dir)
	case "verify":
		dir := os.Args[2]
		sums, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
		check(err)
		sig, err := os.ReadFile(filepath.Join(dir, "checksums.txt.sig"))
		check(err)
		for _, name := range os.Args[3:] {
			bin, err := os.ReadFile(filepath.Join(dir, name))
			check(err)
			check(release.Verify(sums, sig, name, bin))
		}
		fmt.Printf("verified %d files in %s against the release key\n", len(os.Args)-3, dir)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
