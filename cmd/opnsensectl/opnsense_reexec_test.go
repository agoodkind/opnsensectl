package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/opnsensectl/internal/svc"
)

// TestReExecCurrentExecsActiveSlot verifies the restart hook re-execs
// onto the active binary slot (.current) with argv[0] rewritten to that
// path, which is how a deploy or revert lands the new binary without a
// stop. The exec function is faked so the test process is not replaced.
func TestReExecCurrentExecsActiveSlot(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, svc.BinaryCurrent)
	if err := os.WriteFile(current, []byte("\x7fELF-stub"), 0o755); err != nil {
		t.Fatalf("write fake current: %v", err)
	}

	var gotArgv0 string
	var gotArgv []string
	var gotEnvLen int
	fakeExec := func(argv0 string, argv []string, envv []string) error {
		gotArgv0 = argv0
		gotArgv = argv
		gotEnvLen = len(envv)
		return nil
	}

	if err := reExecCurrent(slog.Default(), dir, fakeExec); err != nil {
		t.Fatalf("reExecCurrent: unexpected error: %v", err)
	}
	if gotArgv0 != current {
		t.Errorf("argv0 = %q, want %q", gotArgv0, current)
	}
	if len(gotArgv) == 0 {
		t.Fatalf("argv is empty")
	}
	if gotArgv[0] != current {
		t.Errorf("argv[0] = %q, want %q", gotArgv[0], current)
	}
	if gotEnvLen == 0 {
		t.Errorf("env was not passed through")
	}
}

// TestReExecCurrentMissingSlotErrorsWithoutExec verifies that when the
// active binary slot is absent, reExecCurrent returns an error and never
// calls exec, so the caller can fall back to a clean exit rather than
// exec a missing path.
func TestReExecCurrentMissingSlotErrorsWithoutExec(t *testing.T) {
	dir := t.TempDir() // no .current written

	called := false
	fakeExec := func(string, []string, []string) error {
		called = true
		return nil
	}

	if err := reExecCurrent(slog.Default(), dir, fakeExec); err == nil {
		t.Fatalf("reExecCurrent: expected error for missing .current, got nil")
	}
	if called {
		t.Errorf("exec must not be called when .current is missing")
	}
}
