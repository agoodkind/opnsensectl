package version

import (
	"os"
	"path/filepath"
	"testing"

	gklogversion "goodkind.io/gklog/version"
)

func TestGitDirty_MapsGklogStamps(t *testing.T) {
	orig := gklogversion.Dirty
	defer func() { gklogversion.Dirty = orig }()
	cases := map[string]string{
		"true": "dirty", "dirty": "dirty",
		"false": "clean", "clean": "clean",
		"": "unknown", "unknown": "unknown", "maybe": "unknown",
	}
	for stamp, want := range cases {
		gklogversion.Dirty = stamp
		if got := GitDirty(); got != want {
			t.Fatalf("GitDirty() with Dirty=%q = %q, want %q", stamp, got, want)
		}
	}
}

func TestGitCommit_ReadsGklogStamp(t *testing.T) {
	orig := gklogversion.Commit
	defer func() { gklogversion.Commit = orig }()
	gklogversion.Commit = "03cf29ac"
	if got := GitCommit(); got != "03cf29ac" {
		t.Fatalf("GitCommit() = %q, want the gklog stamp", got)
	}
	gklogversion.Commit = ""
	if got := GitCommit(); got != "unknown" {
		t.Fatalf("GitCommit() with empty stamp = %q, want unknown", got)
	}
}

func TestBuildVersionString_ReportsStampedIdentity(t *testing.T) {
	origCommit := gklogversion.Commit
	origDirty := gklogversion.Dirty
	defer func() {
		gklogversion.Commit = origCommit
		gklogversion.Dirty = origDirty
	}()
	gklogversion.Commit = "03cf29ac"
	gklogversion.Dirty = "true"
	want := "commit=03cf29ac dirty=dirty binhash=" + BinaryHash()
	if got := BuildVersionString(); got != want {
		t.Fatalf("BuildVersionString() = %q, want %q", got, want)
	}
}

func TestBinaryHash_HashesRunningExecutable(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := binaryHashFrom(executable)
	if want == unknown {
		t.Fatalf("binaryHashFrom(%q) = unknown, want a hash of the test binary", executable)
	}
	if got := BinaryHash(); got != want {
		t.Fatalf("BinaryHash() = %q, want %q", got, want)
	}
}

func TestBinaryHashFrom_KnownFile(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The first 12 hex characters of SHA-256("hello").
	const want = "2cf24dba5fb0"
	if got := binaryHashFrom(p); got != want {
		t.Fatalf("binaryHashFrom = %q, want %q", got, want)
	}
}

func TestBinaryHashFrom_MissingFile(t *testing.T) {
	t.Parallel()
	h := binaryHashFrom(filepath.Join(t.TempDir(), "nope"))
	if h != "unknown" {
		t.Fatalf("got %q want unknown", h)
	}
}

func TestBinaryHashFrom_Directory(t *testing.T) {
	t.Parallel()
	// os.Open succeeds on a directory, but reading it fails with EISDIR.
	h := binaryHashFrom(t.TempDir())
	if h != "unknown" {
		t.Fatalf("got %q want unknown", h)
	}
}
