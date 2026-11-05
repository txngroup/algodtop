package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Snapshot writes a full backup into <dir>/snapshots/<time>/. Immutable
// block files are hard-linked; files that grow in place (the WAL, the
// series index, event and session logs) are copied after a commit, so the
// copy holds the heads as WAL records. Call it from the writer goroutine.
func (db *DB) Snapshot() (string, error) {
	if err := db.commit(); err != nil {
		return "", err
	}
	stamp := db.now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(db.dir, "snapshots", stamp)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", err
	}
	err := filepath.WalkDir(db.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(db.dir, p)
		if rel == "." {
			return nil
		}
		top := strings.Split(filepath.ToSlash(rel), "/")[0]
		// LOCK belongs to the running daemon, and Windows refuses to read
		// a file another handle has locked. A restored store makes its own.
		if rel == "LOCK" {
			return nil
		}
		if top == "snapshots" || strings.HasSuffix(rel, ".sock") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		if top == "chunks" || top == "peers" {
			if err := os.Link(p, target); err == nil {
				return nil
			}
		}
		return copyFile(p, target)
	})
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var _ = time.Second
