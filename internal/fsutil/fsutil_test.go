package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// entries lists the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	for _, perm := range []os.FileMode{0o600, 0o640} {
		data := []byte("mode " + perm.String())
		if err := WriteFileAtomic(p, data, perm); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != string(data) {
			t.Errorf("read back %q, %v", got, err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != perm {
			t.Errorf("mode %v, want %v", fi.Mode().Perm(), perm)
		}
		if names := entries(t, dir); len(names) != 1 || names[0] != "f" {
			t.Errorf("dir holds %q, want only f", names)
		}
	}
}

// A failed rename (the target is a non-empty directory) returns the error and
// leaves no temp file behind.
func TestWriteFileAtomicFailureRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.MkdirAll(filepath.Join(p, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("x"), 0o600); err == nil {
		t.Fatal("want rename error")
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "f" {
		t.Errorf("dir holds %q, want only f", names)
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	if err := WriteFileAtomic(filepath.Join(t.TempDir(), "no", "f"), []byte("x"), 0o600); err == nil {
		t.Fatal("want error")
	}
}

// A bare filename is written in the current directory, not $TMPDIR, so the
// rename stays on one filesystem and atomic. $TMPDIR points at a directory
// that does not exist: a temp file created there would fail the write.
func TestWriteFileAtomicBareFilename(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", filepath.Join(dir, "no-such-tmpdir"))
	if err := WriteFileAtomic("bare", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, dir); len(got) != 1 || got[0] != "bare" {
		t.Fatalf("dir entries = %q, want [bare]", got)
	}
}
