package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// renderShim writes a sourceable copy of the embedded preflight shim with its
// `main "$@"` tail removed, so a test can source it under /bin/sh to define the
// functions and default paths, then override the paths and call `preflight` or
// `main` directly.
func renderShim(t *testing.T, dir string) string {
	t.Helper()
	rendered := string(runShim)
	neutralized := strings.Replace(rendered, "\nmain \"$@\"\n", "\n", 1)
	if neutralized == rendered {
		t.Fatal("shim does not contain the expected main tail")
	}
	scriptPath := filepath.Join(dir, "mwan-opnsense-run")
	if err := os.WriteFile(scriptPath, []byte(neutralized), 0o700); err != nil {
		t.Fatalf("write temp shim: %v", err)
	}
	return scriptPath
}

func shimWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// runShimMain sources the shim with its state paths under dir and runs its
// main with args. It returns the combined output and the exit code.
func runShimMain(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	shim := renderShim(t, dir)
	commandText := strings.Join([]string{
		"set -u",
		`. "${SHIM}"`,
		`sbin_dir="${DIR}"`,
		`pending="${DIR}/pending"`,
		`state="${DIR}/state"`,
		`attempt="${DIR}/attempt"`,
		`logger() { :; }`,
		`main "$@"`,
	}, "\n")
	command := exec.CommandContext(t.Context(), "/bin/sh", append([]string{"-c", commandText, "mwan-opnsense-run"}, args...)...)
	command.Env = append(os.Environ(), "SHIM="+shim, "DIR="+dir)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(output), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("run shim: %v\n%s", err, output)
	}
	return string(output), 0
}

// TestShimExecsTheCommandItIsGiven proves the shim runs exactly the daemon
// command the rc.d script passes it, argv[0] included.
func TestShimExecsTheCommandItIsGiven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	daemonStub := filepath.Join(dir, "mwan-opnsense")
	stubText := "#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\" > \"" + argvLog + "\"\n"
	if err := os.WriteFile(daemonStub, []byte(stubText), 0o700); err != nil {
		t.Fatalf("write daemon stub: %v", err)
	}

	command := []string{daemonStub, "daemon", "serve", "--config", "/usr/local/etc/opnsensectl.conf"}
	output, exitCode := runShimMain(t, dir, command...)
	if exitCode != 0 {
		t.Fatalf("shim exit code = %d, want 0\n%s", exitCode, output)
	}
	argvData, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("daemon stub did not run: %v\n%s", err, output)
	}
	gotArgv := strings.Split(strings.TrimSuffix(string(argvData), "\n"), "\n")
	if !slices.Equal(gotArgv, command) {
		t.Errorf("daemon argv = %q, want %q", gotArgv, command)
	}
}

// TestShimRefusesToRunWithoutACommand proves the shim never starts anything on
// its own: with no command it fails with the usage exit code before the
// preflight touches any state.
func TestShimRefusesToRunWithoutACommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	attempt := filepath.Join(dir, "attempt")
	shimWrite(t, attempt, "")

	output, exitCode := runShimMain(t, dir)
	if exitCode != 64 {
		t.Errorf("shim exit code = %d, want 64\n%s", exitCode, output)
	}
	if !strings.Contains(output, "no daemon command given") {
		t.Errorf("output = %q, want the missing command named", output)
	}
	if _, err := os.Stat(attempt); err != nil {
		t.Errorf("the preflight ran without a command: attempt marker gone (%v)", err)
	}
}

// TestShimPreflight exercises the attempt-bounded preflight state machine.
// The load-bearing case is first_spawn_runs_new (a freshly deployed binary
// must run once before any revert) vs crash_respawn_reverts (only a second
// spawn before health=ok reverts).
func TestShimPreflight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		pending     bool
		health      string // "" = no state file; else the health value
		attempt     bool
		previous    bool
		wantCurrent string // expected .current content after preflight
		wantPending bool
		wantAttempt bool
	}{
		{name: "steady_no_pending", pending: false, attempt: true, previous: true, wantCurrent: "new", wantPending: false, wantAttempt: false},
		{name: "healthy_clears", pending: true, health: "ok", attempt: true, previous: true, wantCurrent: "new", wantPending: false, wantAttempt: false},
		{name: "first_spawn_runs_new", pending: true, health: "pending", attempt: false, previous: true, wantCurrent: "new", wantPending: true, wantAttempt: true},
		{name: "crash_respawn_reverts", pending: true, health: "pending", attempt: true, previous: true, wantCurrent: "old", wantPending: false, wantAttempt: false},
		{name: "no_previous_failsafe", pending: true, health: "pending", attempt: true, previous: false, wantCurrent: "new", wantPending: false, wantAttempt: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			shim := renderShim(t, dir)

			sbin := filepath.Join(dir, "sbin")
			if err := os.Mkdir(sbin, 0o755); err != nil {
				t.Fatalf("mkdir sbin: %v", err)
			}
			shimWrite(t, filepath.Join(sbin, "mwan-opnsense.current"), "new")
			if tc.previous {
				shimWrite(t, filepath.Join(sbin, "mwan-opnsense.previous"), "old")
			}

			pending := filepath.Join(dir, "pending")
			state := filepath.Join(dir, "state")
			attempt := filepath.Join(dir, "attempt")
			if tc.pending {
				shimWrite(t, pending, "fresh-deploy\n")
			}
			if tc.health != "" {
				shimWrite(t, state, "active:abc\nhealth:"+tc.health+"\n")
			}
			if tc.attempt {
				shimWrite(t, attempt, "")
			}

			commandText := strings.Join([]string{
				"set -u",
				`. "${SHIM}"`,
				`sbin_dir="${SBIN}"`,
				`pending="${PENDING}"`,
				`state="${STATE}"`,
				`attempt="${ATTEMPT}"`,
				`preflight`,
			}, "\n")
			cmd := exec.Command("/bin/sh", "-c", commandText)
			cmd.Env = append(os.Environ(),
				"SHIM="+shim,
				"SBIN="+sbin,
				"PENDING="+pending,
				"STATE="+state,
				"ATTEMPT="+attempt,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run preflight: %v\n%s", err, out)
			}

			cur, readErr := os.ReadFile(filepath.Join(sbin, "mwan-opnsense.current"))
			if readErr != nil {
				t.Fatalf("read .current: %v", readErr)
			}
			if string(cur) != tc.wantCurrent {
				t.Errorf(".current = %q, want %q", cur, tc.wantCurrent)
			}
			if _, statErr := os.Stat(pending); (statErr == nil) != tc.wantPending {
				t.Errorf("pending exists = %v, want %v", statErr == nil, tc.wantPending)
			}
			if _, statErr := os.Stat(attempt); (statErr == nil) != tc.wantAttempt {
				t.Errorf("attempt exists = %v, want %v", statErr == nil, tc.wantAttempt)
			}
		})
	}
}
