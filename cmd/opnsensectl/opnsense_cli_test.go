package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	opnsensecfg "goodkind.io/opnsensectl/internal/config"
)

// writeTempTOML writes content to a tempdir-scoped config.toml and
// returns the path. The OPNSENSECTL_CONFIG env var is set so
// loadOpnsenseConfig reads the temp file rather than the host's files.
func writeTempTOML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Setenv(opnsensecfg.PathEnv, path)
	return path
}

// TestFilePushErrorsOnEmptyProbeTarget verifies that requireProbeTarget
// returns a clear error citing the missing TOML key when
// [opnsense.probe].target is empty. The file push verb funnels every
// call through this helper before it dials anything, so an empty target
// surfaces as a TOML-keyed error instead of a generic dial failure.
func TestFilePushErrorsOnEmptyProbeTarget(t *testing.T) {
	path := writeTempTOML(t, `
[opnsense.probe]
target = ""
timeout = "5s"
upload_chunk_bytes = 16384
`)

	cfg, err := loadOpnsenseConfig()
	if err != nil {
		t.Fatalf("loadOpnsenseConfig: %v", err)
	}
	if _, err := requireProbeTarget(cfg); err == nil {
		t.Fatalf("requireProbeTarget returned nil error for empty target")
	} else if !strings.Contains(err.Error(), "[opnsense.probe].target") {
		t.Fatalf("error %q does not name the TOML key", err.Error())
	} else if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the file it read, %s", err.Error(), path)
	}
}

// TestUpgradePrepareErrorsOnMissingUpgradeSection verifies that
// resolveUpgradeInputs rejects a TOML missing the [opnsense.upgrade]
// fields with a TOML-keyed error message. We blank out the values the
// schema's defaultConfig() pre-populates so the resolver must report
// the missing key.
func TestUpgradePrepareErrorsOnMissingUpgradeSection(t *testing.T) {
	writeTempTOML(t, `
hostname = "host-test"

[opnsense.upgrade]
vmid = 0
state_dir = ""
env_grpc_target = ""
exec_timeout = ""
upgrade_timeout = ""
post_rollback_wait = ""
gc_older_than = ""
`)

	_, err := resolveUpgradeInputs()
	if err == nil {
		t.Fatalf("resolveUpgradeInputs returned nil error for empty upgrade section")
	}
	msg := err.Error()
	// The first failing field is vmid; subsequent runs should see a
	// path that names [opnsense.upgrade].
	if !strings.Contains(msg, "[opnsense.upgrade]") {
		t.Fatalf("error %q does not name an [opnsense.upgrade] key", msg)
	}
	// errors.Is on a wrapped fmt.Errorf is too coarse here; the assertion
	// on the message text is the contract.
	_ = errors.New("placeholder")
}

// TestRequireDrainListen covers unix:// URL rejection, relative path rejection,
// and acceptance of an absolute path.
func TestRequireDrainListen(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		wantErr bool
	}{
		{
			name:    "unix:// URL is rejected",
			listen:  "unix:///var/run/mwan.sock",
			wantErr: true,
		},
		{
			name:    "relative path is rejected",
			listen:  "var/run/mwan.sock",
			wantErr: true,
		},
		{
			name:    "absolute path is accepted",
			listen:  "/var/run/mwan.sock",
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestFile(t, t.TempDir(), "config.toml",
				"hostname = \"drain-test\"\n\n[opnsense.drain]\nlisten = \""+tc.listen+"\"\n")
			cfg, err := loadServiceConfig(path)
			if err != nil {
				t.Fatalf("loadServiceConfig: %v", err)
			}
			got, err := requireDrainListen(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for listen=%q, got nil", tc.listen)
				}
				if !strings.Contains(err.Error(), "[opnsense.drain].listen") {
					t.Fatalf("error %q does not name [opnsense.drain].listen", err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.listen {
					t.Fatalf("got %q, want %q", got, tc.listen)
				}
			}
		})
	}
}
