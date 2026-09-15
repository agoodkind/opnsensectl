// Package daemoncfg loads the in-VM mwan-opnsense daemon's runtime
// configuration from /usr/local/etc/opnsensectl.conf.
//
// This file is daemon-side, owned by root, mode 0600. opnsensectl install
// writes it with the defaults in [DefaultFile] only when it is absent, so an
// operator's edits survive a reinstall. The daemon itself never writes it.
//
// The host-side configuration at /etc/opnsensectl/config.toml is intentionally
// a separate file with a different schema; daemoncfg does not read it and the
// two file paths never overlap.
//
// The package is cross-platform on purpose. It only does TOML parsing
// and file IO, both portable; keeping it free of a //go:build freebsd
// tag means cmd/opnsensectl/opnsense_daemon_serve.go (which is itself built on
// both Linux and FreeBSD) can call daemoncfg.Load without any
// build-tag gymnastics in callers or tests.
package daemoncfg

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
)

const (
	// DefaultPath is the on-disk location of the daemon-side TOML, which
	// opnsensectl install writes. The path is intentionally not
	// operator-tunable: it is a runtime contract between the install verb and
	// the daemon, not a user-facing knob.
	DefaultPath = "/usr/local/etc/opnsensectl.conf"
	// LegacyPath is the file the rc.d script templated before DefaultPath
	// existed. Load reads it only while DefaultPath is absent, so a daemon
	// binary that reaches a router before opnsensectl install runs still
	// starts with the values that router was rendered with.
	LegacyPath = "/var/lib/mwan/daemon.toml"
)

// DefaultFile is the content opnsensectl install writes to [DefaultPath]
// when that file is absent.
//
//go:embed opnsensectl.conf
var DefaultFile []byte

// DaemonSection is the [daemon] table in the daemon-side TOML.
// Every field is required; daemoncfg.Load rejects empty values rather
// than falling back to compiled defaults. Logfile records where the rc.d
// wrapper sends daemon output even though the daemon itself does not open
// it: the rc.d wrapper keeps stdout/stderr redirection and pidfile
// ownership in daemon(8), and the serve process only consumes the
// non-supervision runtime fields.
type DaemonSection struct {
	SerialPath    string `toml:"serial_path"`
	Baud          uint32 `toml:"baud"`
	ConfigXMLPath string `toml:"config_xml_path"`
	BackupDir     string `toml:"backup_dir"`
	Logfile       string `toml:"logfile"`
	StateDir      string `toml:"state_dir"`
}

// Config is the top-level TOML shape. Only the [daemon] table is
// defined today; new sections can be added without touching callers.
type Config struct {
	Daemon DaemonSection `toml:"daemon"`
}

// Load reads [DefaultPath], and reads [LegacyPath] only when DefaultPath does
// not exist, then validates that every required field is present. There is no
// fallback to compiled defaults: a missing file or missing field is a hard
// error so the daemon refuses to start with an under-specified config. A
// DefaultPath that exists but cannot be read, parsed, or validated is an error
// rather than a reason to read LegacyPath, so a broken new file is never
// masked.
func Load() (*Config, error) {
	return loadFirstPresent(DefaultPath, LegacyPath)
}

// loadFirstPresent is the path-injected variant used by tests; production
// callers should use Load(). When neither file exists the error names
// primary, the file opnsensectl install writes.
func loadFirstPresent(primary, legacy string) (*Config, error) {
	cfg, primaryErr := loadFrom(primary)
	if !errors.Is(primaryErr, fs.ErrNotExist) {
		return cfg, primaryErr
	}
	slog.Warn("daemoncfg: file absent, reading the legacy rc.d-templated file",
		"path", primary, "legacy_path", legacy)
	cfg, legacyErr := loadFrom(legacy)
	if !errors.Is(legacyErr, fs.ErrNotExist) {
		return cfg, legacyErr
	}
	slog.Error("daemoncfg: no config file", "path", primary, "legacy_path", legacy, "err", primaryErr)
	return nil, fmt.Errorf("daemoncfg: %s not found; run opnsensectl install to write it: %w",
		primary, primaryErr)
}

func loadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("daemoncfg: read failed", "path", path, "err", err)
		return nil, fmt.Errorf("daemoncfg: read %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		slog.Error("daemoncfg: parse failed", "path", path, "err", err)
		return nil, fmt.Errorf("daemoncfg: parse %s: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces that every field in the daemon TOML schema is present and
// non-empty. The error message cites the offending TOML key so operators can
// fix the file directly.
func validate(cfg *Config) error {
	d := &cfg.Daemon
	if d.SerialPath == "" {
		return errors.New("daemoncfg: [daemon] serial_path is required")
	}
	if d.Baud == 0 {
		return errors.New("daemoncfg: [daemon] baud is required and must be non-zero")
	}
	if d.ConfigXMLPath == "" {
		return errors.New("daemoncfg: [daemon] config_xml_path is required")
	}
	if d.BackupDir == "" {
		return errors.New("daemoncfg: [daemon] backup_dir is required")
	}
	if d.Logfile == "" {
		return errors.New("daemoncfg: [daemon] logfile is required")
	}
	if d.StateDir == "" {
		return errors.New("daemoncfg: [daemon] state_dir is required")
	}
	return nil
}
