package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/opnsensectl/internal/svc"
)

// TestReExecCurrentReusesTheRunningArgv verifies the restart hook re-execs
// onto the active binary slot (.current) with the running process's argv
// unchanged, which is how a deploy or revert lands the new binary without a
// stop and without losing the explicit service command. The exec function is
// faked so the test process is not replaced.
func TestReExecCurrentReusesTheRunningArgv(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, svc.BinaryCurrent)
	if err := os.WriteFile(current, []byte("\x7fELF-stub"), 0o755); err != nil {
		t.Fatalf("write fake current: %v", err)
	}
	runningArgv := os.Args
	t.Cleanup(func() { os.Args = runningArgv })
	os.Args = []string{"/usr/local/sbin/mwan-opnsense", "daemon", "serve", "--config", "/usr/local/etc/opnsensectl.conf"}

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
		t.Errorf("exec path = %q, want %q", gotArgv0, current)
	}
	if !slices.Equal(gotArgv, os.Args) {
		t.Errorf("argv = %q, want the running argv %q", gotArgv, os.Args)
	}
	if gotEnvLen == 0 {
		t.Errorf("env was not passed through")
	}
}

// TestReExecCurrentRefusesAnArgvWithoutACommand verifies that a process whose
// argv carries no command is never re-executed, because the new image would
// print help and exit instead of serving.
func TestReExecCurrentRefusesAnArgvWithoutACommand(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, svc.BinaryCurrent)
	if err := os.WriteFile(current, []byte("\x7fELF-stub"), 0o755); err != nil {
		t.Fatalf("write fake current: %v", err)
	}
	runningArgv := os.Args
	t.Cleanup(func() { os.Args = runningArgv })
	os.Args = []string{"/usr/local/sbin/mwan-opnsense"}

	called := false
	fakeExec := func(string, []string, []string) error {
		called = true
		return nil
	}

	if err := reExecCurrent(slog.Default(), dir, fakeExec); err == nil {
		t.Fatal("reExecCurrent: expected an error for an argv without a command, got nil")
	}
	if called {
		t.Error("exec must not be called without the running command")
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
