package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostFile is the shape the Proxmox host deploy renders: the bridge, the
// upgrade target, and the drainer. Every other key comes from the defaults.
const hostFile = `
[opnsense.host]
upstream = "unix:///run/test-drain.sock"
listen = "/run/test-bridge.sock"
reconnect = "3s"
heartbeat_interval = "40s"
heartbeat_timeout = "12s"

[opnsense.upgrade]
vmid = 4242

[opnsense.drain]
chardev = "unix:///var/run/qemu-server/4242.mwanrpc"
listen = "/run/test-drain.sock"
`

// gatewayFile is a gateway configuration that still carries the [opnsense.*]
// tables beside the gateway's own sections.
const gatewayFile = `
hostname = "hypervisor-test"
mwan_vmid = "4100"

[email]
alert_email = "alerts@example.com"

[watchdog]
deploy_window_minutes = 30

[opnsense.host]
upstream = "unix:///run/legacy-drain.sock"
listen = "/run/legacy-bridge.sock"
reconnect = "2s"
heartbeat_interval = "30s"
heartbeat_timeout = "10s"

[opnsense.upgrade]
vmid = 4343

[opnsense.drain]
chardev = "unix:///var/run/qemu-server/4343.mwanrpc"
listen = "/run/legacy-drain.sock"
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadFileOverridesRenderedKeysAndDefaultsTheRest(t *testing.T) {
	path := writeFile(t, t.TempDir(), "config.toml", hostFile)

	cfg, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if cfg.Source != path {
		t.Errorf("Source = %q, want %q", cfg.Source, path)
	}
	host := cfg.OPNsense.Host
	if host.Upstream != "unix:///run/test-drain.sock" || host.Listen != "/run/test-bridge.sock" {
		t.Errorf("host sockets = %q %q, want the rendered values", host.Upstream, host.Listen)
	}
	if host.ReconnectDuration != "3s" || host.HeartbeatIntervalDuration != "40s" || host.HeartbeatTimeoutDuration != "12s" {
		t.Errorf("host durations = %q %q %q, want 3s 40s 12s",
			host.ReconnectDuration, host.HeartbeatIntervalDuration, host.HeartbeatTimeoutDuration)
	}
	if cfg.OPNsense.Upgrade.VMID != 4242 {
		t.Errorf("upgrade vmid = %d, want 4242", cfg.OPNsense.Upgrade.VMID)
	}
	if cfg.OPNsense.Drain.Chardev != "unix:///var/run/qemu-server/4242.mwanrpc" {
		t.Errorf("drain chardev = %q, want the rendered value", cfg.OPNsense.Drain.Chardev)
	}

	probe := cfg.OPNsense.Probe
	if probe.Target != "unix:///var/run/mwan-opnsense.sock" || probe.TimeoutDuration != "10s" ||
		probe.UploadChunkBytes != 16384 || probe.TransferStallDuration != "30s" {
		t.Errorf("probe = %+v, want the defaults", probe)
	}
	upgrade := cfg.OPNsense.Upgrade
	if upgrade.EnvGRPCTarget != "unix:///var/run/mwan-opnsense.sock" || upgrade.StateDir != "/var/lib/mwan/upgrades" ||
		upgrade.ExecTimeoutDuration != "60m" || upgrade.UpgradeTimeoutDuration != "30m" ||
		upgrade.PostRollbackWaitDuration != "5m" || upgrade.GCOlderThan != "168h" {
		t.Errorf("upgrade = %+v, want the defaults beside the rendered vmid", upgrade)
	}
	// The validate phase pings the targets the mwan watchdog pings.
	if upgrade.Validate.PingTargetIPv4 != "1.1.1.1" || upgrade.Validate.PingTargetIPv6 != "2606:4700:4700::1111" {
		t.Errorf("upgrade validate ping targets = %q %q, want the watchdog's targets",
			upgrade.Validate.PingTargetIPv4, upgrade.Validate.PingTargetIPv6)
	}
}

func TestLoadFileDefaultsEverySectionForAnEmptyFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "config.toml", "")

	cfg, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if cfg.OPNsense.Host.Upstream != "unix:///var/run/mwan-opnsense-drain.sock" {
		t.Errorf("host upstream = %q, want the drain relay socket", cfg.OPNsense.Host.Upstream)
	}
	if cfg.OPNsense.Drain.Listen != "/var/run/mwan-opnsense-drain.sock" {
		t.Errorf("drain listen = %q, want the drain relay socket", cfg.OPNsense.Drain.Listen)
	}
	if cfg.OPNsense.Host.HeartbeatIntervalDuration != "30s" {
		t.Errorf("host heartbeat interval = %q, want 30s", cfg.OPNsense.Host.HeartbeatIntervalDuration)
	}
}

func TestLoadReadsTheFilePathEnvNames(t *testing.T) {
	path := writeFile(t, t.TempDir(), "override.toml", hostFile)
	t.Setenv(PathEnv, path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Source != path {
		t.Errorf("Source = %q, want %q", cfg.Source, path)
	}
	if cfg.OPNsense.Upgrade.VMID != 4242 {
		t.Errorf("upgrade vmid = %d, want 4242 from the override file", cfg.OPNsense.Upgrade.VMID)
	}
}

func TestLoadFailsWhenThePathEnvFileIsMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	t.Setenv(PathEnv, missing)

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded for a missing override file")
	}
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want a not-exist error naming %s", err, missing)
	}
}

func TestLoadFileRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "vmid is not an integer", body: "[opnsense.upgrade]\nvmid = \"router\"\n"},
		{name: "upload chunk is not an integer", body: "[opnsense.probe]\nupload_chunk_bytes = \"big\"\n"},
		{name: "duration is not a string", body: "[opnsense.host]\nheartbeat_timeout = 10\n"},
		{name: "boolean is not a boolean", body: "[opnsense.upgrade]\nreset_confirm = \"yes\"\n"},
		{name: "malformed table header", body: "[opnsense.host\nlisten = \"/run/x.sock\"\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "config.toml", testCase.body)

			_, err := loadFile(path)
			if err == nil {
				t.Fatal("loadFile accepted an invalid value")
			}
			if !strings.Contains(err.Error(), "parse "+path) {
				t.Errorf("error = %q, want a parse error naming %s", err, path)
			}
		})
	}
}

func TestLoadFirstPresentPrefersThePrimaryFile(t *testing.T) {
	dir := t.TempDir()
	primary := writeFile(t, dir, "opnsensectl.toml", hostFile)
	legacy := writeFile(t, dir, "mwan.toml", gatewayFile)

	cfg, err := loadFirstPresent(primary, legacy)
	if err != nil {
		t.Fatalf("loadFirstPresent: %v", err)
	}

	if cfg.Source != primary || cfg.OPNsense.Upgrade.VMID != 4242 {
		t.Errorf("Source = %q vmid = %d, want %q and 4242", cfg.Source, cfg.OPNsense.Upgrade.VMID, primary)
	}
}

func TestLoadFirstPresentReadsTheGatewayFileWhenThePrimaryIsAbsent(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "opnsensectl.toml")
	legacy := writeFile(t, dir, "mwan.toml", gatewayFile)

	cfg, err := loadFirstPresent(primary, legacy)
	if err != nil {
		t.Fatalf("loadFirstPresent: %v", err)
	}

	if cfg.Source != legacy {
		t.Errorf("Source = %q, want %q", cfg.Source, legacy)
	}
	if cfg.OPNsense.Upgrade.VMID != 4343 || cfg.OPNsense.Drain.Chardev != "unix:///var/run/qemu-server/4343.mwanrpc" {
		t.Errorf("vmid = %d chardev = %q, want the gateway file's values",
			cfg.OPNsense.Upgrade.VMID, cfg.OPNsense.Drain.Chardev)
	}
	if cfg.OPNsense.Host.Listen != "/run/legacy-bridge.sock" {
		t.Errorf("host listen = %q, want the gateway file's value", cfg.OPNsense.Host.Listen)
	}
	if cfg.Email.AlertEmail != "alerts@example.com" {
		t.Errorf("email alert_email = %q, want the gateway file's value", cfg.Email.AlertEmail)
	}
}

// emailFile is the [email] table the Proxmox host deploy renders beside the
// [opnsense.*] tables, with min_level left to its default.
const emailFile = `
[email]
smtp2go_api_key = "key-from-file"
alert_email = "opnsense-alerts@example.test"
from = "opnsense-upgrade@example.test"
subject_prefix = "[OPNsense-test]"
bind_iface = "oob0"
`

func TestLoadFileReadsTheEmailSectionAndDefaultsMinLevel(t *testing.T) {
	t.Setenv(SMTP2GOEnv, "")
	path := writeFile(t, t.TempDir(), "config.toml", emailFile)

	cfg, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	want := EmailSection{
		SMTP2GOAPIKey: "key-from-file",
		AlertEmail:    "opnsense-alerts@example.test",
		From:          "opnsense-upgrade@example.test",
		SubjectPrefix: "[OPNsense-test]",
		BindIface:     "oob0",
		MinLevel:      "ERROR",
	}
	if cfg.Email != want {
		t.Errorf("email = %+v, want %+v", cfg.Email, want)
	}
}

func TestLoadFileLetsTheSMTP2GOEnvironmentVariableReplaceTheFileKey(t *testing.T) {
	t.Setenv(SMTP2GOEnv, "  key-from-environment  ")
	path := writeFile(t, t.TempDir(), "config.toml", emailFile)

	cfg, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	if cfg.Email.SMTP2GOAPIKey != "key-from-environment" {
		t.Errorf("smtp2go_api_key = %q, want the environment value", cfg.Email.SMTP2GOAPIKey)
	}
}

func TestLoadFirstPresentDoesNotMaskABrokenPrimaryFile(t *testing.T) {
	dir := t.TempDir()
	primary := writeFile(t, dir, "opnsensectl.toml", "[opnsense.upgrade]\nvmid = \"router\"\n")
	legacy := writeFile(t, dir, "mwan.toml", gatewayFile)

	_, err := loadFirstPresent(primary, legacy)
	if err == nil {
		t.Fatal("loadFirstPresent read the gateway file past a broken primary file")
	}
	if !strings.Contains(err.Error(), primary) {
		t.Errorf("error = %q, want it to name %s", err, primary)
	}
}

func TestLoadFirstPresentNamesThePrimaryFileWhenNeitherExists(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "opnsensectl.toml")
	legacy := filepath.Join(dir, "mwan.toml")

	_, err := loadFirstPresent(primary, legacy)
	if err == nil {
		t.Fatal("loadFirstPresent succeeded with neither file present")
	}
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), primary) {
		t.Errorf("error = %v, want a not-exist error naming %s", err, primary)
	}
}
