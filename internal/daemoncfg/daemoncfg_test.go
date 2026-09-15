package daemoncfg

import (
	"errors"
	"io/fs"
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

// wantDefaults is the config the rc.d script templated from its compiled
// fallbacks before install owned the file.
var wantDefaults = DaemonSection{
	SerialPath:    "/dev/ttyV0.1",
	Baud:          115200,
	ConfigXMLPath: "/conf/config.xml",
	BackupDir:     "/conf/backup",
	Logfile:       "/var/log/mwan-opnsense.log",
	StateDir:      "/var/lib/mwan/transfers",
}

func writeTOML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opnsensectl.conf")
	writeTOML(t, path, happyTOML)

	cfg, err := loadFirstPresent(path, filepath.Join(dir, "absent.toml"))
	if err != nil {
		t.Fatalf("loadFirstPresent: %v", err)
	}
	if cfg.Daemon != wantDefaults {
		t.Errorf("daemon = %+v, want %+v", cfg.Daemon, wantDefaults)
	}
}

// TestDefaultFileLoads proves the file install writes starts the daemon with
// the same values the rc.d script used to template.
func TestDefaultFileLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opnsensectl.conf")
	writeTOML(t, path, string(DefaultFile))

	cfg, err := loadFirstPresent(path, filepath.Join(dir, "absent.toml"))
	if err != nil {
		t.Fatalf("load DefaultFile: %v", err)
	}
	if cfg.Daemon != wantDefaults {
		t.Errorf("DefaultFile daemon = %+v, want %+v", cfg.Daemon, wantDefaults)
	}
}

func TestLoadFirstPresent(t *testing.T) {
	legacyTOML := strings.Replace(happyTOML, "baud = 115200", "baud = 921600", 1)

	t.Run("primary wins over legacy", func(t *testing.T) {
		dir := t.TempDir()
		primary := filepath.Join(dir, "opnsensectl.conf")
		legacy := filepath.Join(dir, "daemon.toml")
		writeTOML(t, primary, happyTOML)
		writeTOML(t, legacy, legacyTOML)

		cfg, err := loadFirstPresent(primary, legacy)
		if err != nil {
			t.Fatalf("loadFirstPresent: %v", err)
		}
		if cfg.Daemon.Baud != 115200 {
			t.Errorf("baud = %d, want the primary file's 115200", cfg.Daemon.Baud)
		}
	})

	t.Run("legacy read while primary absent", func(t *testing.T) {
		dir := t.TempDir()
		legacy := filepath.Join(dir, "daemon.toml")
		writeTOML(t, legacy, legacyTOML)

		cfg, err := loadFirstPresent(filepath.Join(dir, "opnsensectl.conf"), legacy)
		if err != nil {
			t.Fatalf("loadFirstPresent: %v", err)
		}
		if cfg.Daemon.Baud != 921600 {
			t.Errorf("baud = %d, want the legacy file's 921600", cfg.Daemon.Baud)
		}
	})

	t.Run("broken primary is not masked by legacy", func(t *testing.T) {
		dir := t.TempDir()
		primary := filepath.Join(dir, "opnsensectl.conf")
		legacy := filepath.Join(dir, "daemon.toml")
		writeTOML(t, primary, strings.Replace(happyTOML, `state_dir = "/var/lib/mwan/transfers"`, "", 1))
		writeTOML(t, legacy, legacyTOML)

		_, err := loadFirstPresent(primary, legacy)
		if err == nil || !strings.Contains(err.Error(), "state_dir") {
			t.Fatalf("err = %v, want the primary file's missing state_dir", err)
		}
	})

	t.Run("both absent names primary and install", func(t *testing.T) {
		dir := t.TempDir()
		primary := filepath.Join(dir, "opnsensectl.conf")

		_, err := loadFirstPresent(primary, filepath.Join(dir, "daemon.toml"))
		if err == nil {
			t.Fatal("loadFirstPresent with no files: want error, got nil")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want fs.ErrNotExist in the chain", err)
		}
		if !strings.Contains(err.Error(), primary) || !strings.Contains(err.Error(), "opnsensectl install") {
			t.Errorf("err = %q, want it to name %s and opnsensectl install", err, primary)
		}
	})
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
			path := filepath.Join(dir, "opnsensectl.conf")
			writeTOML(t, path, tc.toml)
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
