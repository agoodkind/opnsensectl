package svc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicWrite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	want := []byte("hello atomic")

	if err := AtomicWriteFile(context.Background(), target, want, 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content=%q want %q", got, want)
	}
}

// TestAtomicWrite_CleanupOnError makes the final rename fail after
// renameio has already written its temp file, by pointing the target at
// an existing non-empty directory, and checks that no temp file
// survives. renameio places the temp file in os.TempDir when that shares
// a filesystem with the target and in the target's directory otherwise,
// so TMPDIR is redirected to an empty directory and both are checked.
func TestAtomicWrite_CleanupOnError(t *testing.T) {
	tempRoot := t.TempDir()
	parent := t.TempDir()
	target := filepath.Join(parent, "out.bin")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	t.Setenv("TMPDIR", tempRoot)

	if err := AtomicWriteFile(context.Background(), target, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error replacing a directory with a file")
	}
	for _, dir := range []string{tempRoot, parent} {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatalf("read dir %s: %v", dir, readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".out.bin") {
				t.Fatalf("leftover temp file after failed write: %s", filepath.Join(dir, entry.Name()))
			}
		}
	}
}
