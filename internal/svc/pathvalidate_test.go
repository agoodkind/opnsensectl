package svc

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// newValidatorForTest builds a PathValidator with separate read and
// write allowlists rooted at tempdirs created by the caller. The
// returned function cleans up the validator.
func newValidatorForTest(t *testing.T, readDirs, writeDirs []string) (*PathValidator, func()) {
	t.Helper()
	pv := NewPathValidator(slog.Default(), readDirs, writeDirs)
	return pv, func() { _ = pv.Close() }
}

func TestPathValidator_AllowedBaseHits(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "hello.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	pv, cleanup := newValidatorForTest(t, []string{base}, nil)
	defer cleanup()

	file, err := pv.OpenForRead(target)
	if err != nil {
		t.Fatalf("OpenForRead: %v", err)
	}
	defer func() { _ = file.Close() }()
	got := make([]byte, 16)
	n, _ := file.Read(got)
	if string(got[:n]) != "hi" {
		t.Fatalf("read=%q want %q", got[:n], "hi")
	}
}

func TestPathValidator_PathTraversalRefused(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "base")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	pv, cleanup := newValidatorForTest(t, []string{base}, nil)
	defer cleanup()

	// Concatenate instead of filepath.Join, which would clean the ".."
	// away before the validator sees it.
	separator := string(filepath.Separator)
	traversal := base + separator + ".." + separator + "secret.txt"
	file, err := pv.OpenForRead(traversal)
	if err == nil {
		_ = file.Close()
		t.Fatalf("expected refusal for traversal path %q", traversal)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("traversal target exists, so the error must be a refusal: %v", err)
	}
}

func TestPathValidator_SymlinkRefused(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	link := filepath.Join(base, "evil")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	pv, cleanup := newValidatorForTest(t, []string{base}, nil)
	defer cleanup()

	if _, err := pv.OpenForRead(link); err == nil {
		t.Fatalf("expected error opening symlink escape, got nil")
	}
}

func TestPathValidator_NoFollowOnFinalComponent(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real.txt")
	if err := os.WriteFile(real, []byte("ok"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(base, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	pv, cleanup := newValidatorForTest(t, []string{base}, nil)
	defer cleanup()

	if _, err := pv.OpenForRead(link); err == nil {
		t.Fatalf("expected error opening symlink final component, got nil")
	}
}

func TestPathValidator_DirectionAllowlists(t *testing.T) {
	readOnly := t.TempDir()
	writeOnly := t.TempDir()
	readFile := filepath.Join(readOnly, "r.txt")
	if err := os.WriteFile(readFile, []byte("r"), 0o600); err != nil {
		t.Fatalf("seed read file: %v", err)
	}

	pv, cleanup := newValidatorForTest(t, []string{readOnly}, []string{writeOnly})
	defer cleanup()

	if _, err := pv.OpenForRead(readFile); err != nil {
		t.Fatalf("read in read allowlist must succeed: %v", err)
	}
	if _, _, err := pv.ResolveWrite(readFile); err == nil {
		t.Fatalf("write into read-only allowlist must fail")
	}

	writeTarget := filepath.Join(writeOnly, "w.txt")
	if _, _, err := pv.ResolveWrite(writeTarget); err != nil {
		t.Fatalf("write in write allowlist must resolve: %v", err)
	}
	// Write roots are implicitly readable.
	if err := os.WriteFile(writeTarget, []byte("w"), 0o600); err != nil {
		t.Fatalf("seed write file: %v", err)
	}
	writeFile, err := pv.OpenForRead(writeTarget)
	if err != nil {
		t.Fatalf("read in write allowlist must succeed: %v", err)
	}
	_ = writeFile.Close()
}
