package fsx

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The tailer holds node.log open while algod renames it at LogSizeLimit.
// On Windows that needs FILE_SHARE_DELETE, which os.Open omits.
func TestRenameWhileOpen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node.log")
	os.WriteFile(p, []byte("line 1\n"), 0o600)
	f, err := OpenShared(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, filepath.Join(dir, "node.archive.log")); err != nil {
		t.Fatalf("rename while the tailer holds the file: %v", err)
	}
	b, _ := io.ReadAll(f)
	if string(b) != "line 1\n" {
		t.Fatalf("read after rename: %q", b)
	}
	if err := os.WriteFile(p, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAtomicAndLock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b.json")
	if err := WriteAtomic(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Fatal("content")
	}
	l, err := Lock(filepath.Join(dir, "LOCK"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(filepath.Join(dir, "LOCK")); err != ErrLocked {
		t.Fatalf("second lock: %v", err)
	}
	l.Close()
	if l2, err := Lock(filepath.Join(dir, "LOCK")); err != nil {
		t.Fatalf("relock: %v", err)
	} else {
		l2.Close()
	}
}
