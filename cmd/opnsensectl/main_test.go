package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvokedAsOPNsenseDaemon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv string
		want bool
	}{
		{name: "symlink", argv: "/usr/local/sbin/mwan-opnsense", want: true},
		{name: "current", argv: "/usr/local/sbin/mwan-opnsense.current", want: true},
		{name: "host bridge", argv: "/usr/local/bin/mwan-opnsense-host", want: false},
		{name: "monolith", argv: "/usr/local/bin/mwan", want: false},
		{name: "cli", argv: "/usr/local/bin/opnsensectl", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := invokedAsOPNsenseDaemon(tt.argv); got != tt.want {
				t.Fatalf("invokedAsOPNsenseDaemon(%q) = %v, want %v", tt.argv, got, tt.want)
			}
		})
	}
}

// buildRouterLayout builds this command into a temp dir the way the router
// holds it: the binary at mwan-opnsense.current, a mwan-opnsense symlink to it,
// and an opnsensectl symlink for the plain CLI name. It returns the dir.
func buildRouterLayout(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find the go toolchain: %v", err)
	}
	dir := t.TempDir()
	current := filepath.Join(dir, "mwan-opnsense.current")
	build := exec.CommandContext(t.Context(), goBinary, "build", "-o", current, ".")
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build: %v\n%s", buildErr, output)
	}
	for _, name := range []string{"mwan-opnsense", "opnsensectl"} {
		if linkErr := os.Symlink(current, filepath.Join(dir, name)); linkErr != nil {
			t.Fatalf("link %s: %v", name, linkErr)
		}
	}
	return dir
}

// TestMainDispatchUnderDaemonName runs the built binary under each router name
// with the argv every caller passes. The run shim execs mwan-opnsense with no
// arguments, and the restart hook re-execs mwan-opnsense.current with that same
// empty argument list, so both must still reach the daemon, which fails on the
// missing /usr/local/etc/opnsensectl.conf. A first argument of install must
// reach the install verb instead; its help text and argument check prove that
// without a write to the host.
func TestMainDispatchUnderDaemonName(t *testing.T) {
	t.Parallel()
	dir := buildRouterLayout(t)

	tests := []struct {
		name       string
		binary     string
		args       []string
		wantExit   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "run shim start",
			binary:     "mwan-opnsense",
			args:       nil,
			wantExit:   1,
			wantStderr: "daemon serve: load config",
		},
		{
			name:       "restart re-exec",
			binary:     "mwan-opnsense.current",
			args:       nil,
			wantExit:   1,
			wantStderr: "daemon serve: load config",
		},
		{
			name:       "other verb stays daemon",
			binary:     "mwan-opnsense",
			args:       []string{"version"},
			wantExit:   2,
			wantStderr: "mwan opnsense daemon serve: unexpected arguments: [version]",
		},
		{
			name:       "install after another argument stays daemon",
			binary:     "mwan-opnsense",
			args:       []string{"serve", "install"},
			wantExit:   2,
			wantStderr: "mwan opnsense daemon serve: unexpected arguments: [serve install]",
		},
		{
			name:       "daemon name install help",
			binary:     "mwan-opnsense",
			args:       []string{"install", "--help"},
			wantExit:   0,
			wantStdout: "usage: opnsensectl install",
		},
		{
			name:       "daemon slot install help",
			binary:     "mwan-opnsense.current",
			args:       []string{"install", "--help"},
			wantExit:   0,
			wantStdout: "usage: opnsensectl install",
		},
		{
			name:       "daemon name install rejects extra arguments",
			binary:     "mwan-opnsense",
			args:       []string{"install", "extra"},
			wantExit:   2,
			wantStderr: "opnsensectl install: unexpected arguments: [extra]",
		},
		{
			name:       "cli install help",
			binary:     "opnsensectl",
			args:       []string{"install", "--help"},
			wantExit:   0,
			wantStdout: "usage: opnsensectl install",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			command := exec.CommandContext(t.Context(), filepath.Join(dir, tt.binary), tt.args...)
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			runErr := command.Run()

			exitCode := 0
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else if runErr != nil {
				t.Fatalf("run %s: %v", tt.binary, runErr)
			}
			if exitCode != tt.wantExit {
				t.Fatalf("exit_code=%d, want %d\nstdout:\n%s\nstderr:\n%s",
					exitCode, tt.wantExit, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tt.wantStdout, stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tt.wantStderr, stderr.String())
			}
		})
	}
}
