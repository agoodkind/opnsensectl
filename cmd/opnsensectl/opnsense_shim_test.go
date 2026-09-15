package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderShim writes a sourceable copy of the preflight shim with its tail
// (the `preflight` call and the `exec` of the daemon) removed, so a test can
// source it under /bin/sh to define the functions and default paths, then
// override the paths and call `preflight` directly.
func renderShim(t *testing.T, dir string) string {
	t.Helper()
	srcPath := filepath.Join("opnsense-src", "usr", "local", "libexec", "mwan-opnsense-run")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	rendered := string(data)
	neutralized := strings.Replace(rendered, "preflight\nexec \"${daemon_bin}\"\n", "", 1)
	if neutralized == rendered {
		t.Fatal("shim does not contain the expected preflight+exec tail")
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

// TestRCDStartUsesRestartAndShim guards the supervision contract: the rc.d
// start must launch daemon(8) with -r (auto-restart) against the preflight
// shim, not the daemon binary directly, and must no longer run an inline
// preflight (that logic moved into the shim).
func TestRCDStartUsesRestartAndShim(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("opnsense-src", "etc", "rc.d", "mwan_opnsense"))
	if err != nil {
		t.Fatalf("read rc.d script: %v", err)
	}
	script := string(data)
	wants := []string{
		`run_shim="/usr/local/libexec/mwan-opnsense-run"`,
		`/usr/sbin/daemon -r -P "${pidfile}" -p "${child_pidfile}" -o "${mwan_opnsense_logfile}" "${run_shim}"`,
	}
	for _, want := range wants {
		if !strings.Contains(script, want) {
			t.Errorf("rc.d script missing %q", want)
		}
	}
	if strings.Contains(script, "mwan_opnsense_preflight") {
		t.Error("rc.d script still references mwan_opnsense_preflight; preflight moved to the shim")
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
				`daemon_bin="${SBIN}/mwan-opnsense"`,
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
