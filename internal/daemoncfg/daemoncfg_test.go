package daemoncfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const happyTOML = `
[daemon]
serial_path = "/dev/ttyV0.1"
baud = 115200
config_xml_path = "/conf/config.xml"
backup_dir = "/conf/backup"
logfile = "/var/log/mwan-opnsense.log"
state_dir = "/var/lib/mwan/transfers"
`

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.toml")
	if err := os.WriteFile(path, []byte(happyTOML), 0o600); err != nil {
		t.Fatalf("write tmp toml: %v", err)
	}

	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}

	wantSerial := "/dev/ttyV0.1"
	if cfg.Daemon.SerialPath != wantSerial {
		t.Errorf("serial_path: got %q, want %q", cfg.Daemon.SerialPath, wantSerial)
	}
	if cfg.Daemon.Baud != 115200 {
		t.Errorf("baud: got %d, want 115200", cfg.Daemon.Baud)
	}
	if cfg.Daemon.ConfigXMLPath != "/conf/config.xml" {
		t.Errorf("config_xml_path: got %q", cfg.Daemon.ConfigXMLPath)
	}
	if cfg.Daemon.BackupDir != "/conf/backup" {
		t.Errorf("backup_dir: got %q", cfg.Daemon.BackupDir)
	}
	if cfg.Daemon.Logfile != "/var/log/mwan-opnsense.log" {
		t.Errorf("logfile: got %q", cfg.Daemon.Logfile)
	}
	if cfg.Daemon.StateDir != "/var/lib/mwan/transfers" {
		t.Errorf("state_dir: got %q", cfg.Daemon.StateDir)
	}
}

func TestLoadMissingDefaultFileMessage(t *testing.T) {
	// If /var/lib/mwan/daemon.toml happens to exist on the test host (it
	// will not in CI, but a developer running tests on a FreeBSD VM could
	// have it), this test would not be checking what we think it checks.
	if _, statErr := os.Stat(DefaultPath); statErr == nil {
		t.Skipf("DefaultPath %s exists on host; skipping missing-file assertion", DefaultPath)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("Load with missing DefaultPath: want error, got nil")
	}
	want := "daemoncfg: /var/lib/mwan/daemon.toml not found; " +
		"the rc.d script must template this file before starting the daemon"
	if err.Error() != want {
		t.Errorf("missing-file error message:\n got: %q\nwant: %q", err.Error(), want)
	}
}

func TestLoadRequiredFieldMissing(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		wantSub string
	}{
		{
			name: "serial_path empty",
			toml: `
[daemon]
serial_path = ""
baud = 115200
config_xml_path = "/conf/config.xml"
backup_dir = "/conf/backup"
logfile = "/var/log/mwan-opnsense.log"
state_dir = "/var/lib/mwan/transfers"
`,
			wantSub: "serial_path",
		},
		{
			name: "baud zero",
			toml: `
[daemon]
serial_path = "/dev/ttyV0.1"
baud = 0
config_xml_path = "/conf/config.xml"
backup_dir = "/conf/backup"
logfile = "/var/log/mwan-opnsense.log"
state_dir = "/var/lib/mwan/transfers"
`,
			wantSub: "baud",
		},
		{
			name: "config_xml_path missing",
			toml: `
[daemon]
serial_path = "/dev/ttyV0.1"
baud = 115200
backup_dir = "/conf/backup"
logfile = "/var/log/mwan-opnsense.log"
state_dir = "/var/lib/mwan/transfers"
`,
			wantSub: "config_xml_path",
		},
		{
			name: "backup_dir missing",
			toml: `
[daemon]
serial_path = "/dev/ttyV0.1"
baud = 115200
config_xml_path = "/conf/config.xml"
logfile = "/var/log/mwan-opnsense.log"
state_dir = "/var/lib/mwan/transfers"
`,
			wantSub: "backup_dir",
		},
		{
			name: "logfile missing",
			toml: `
[daemon]
serial_path = "/dev/ttyV0.1"
baud = 115200
config_xml_path = "/conf/config.xml"
backup_dir = "/conf/backup"
state_dir = "/var/lib/mwan/transfers"
`,
			wantSub: "logfile",
		},
		{
			name: "state_dir missing",
			toml: `
[daemon]
serial_path = "/dev/ttyV0.1"
baud = 115200
config_xml_path = "/conf/config.xml"
backup_dir = "/conf/backup"
logfile = "/var/log/mwan-opnsense.log"
`,
			wantSub: "state_dir",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatalf("write tmp toml: %v", err)
			}
			_, err := loadFrom(path)
			if err == nil {
				t.Fatalf("loadFrom: want error mentioning %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}
