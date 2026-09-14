// Package daemoncfg loads the in-VM mwan-opnsense daemon's runtime
// configuration from /var/lib/mwan/daemon.toml.
//
// This file is daemon-side, owned by root, mode 0600, and is templated
// by the rc.d script (etc/rc.d/mwan_opnsense) from rc.conf.d-overridable
// variables before the daemon starts. The daemon itself never writes it.
//
// The host-side configuration at /etc/mwan/config.toml is intentionally
// a separate file with a different schema; daemoncfg does not read it
// and the two file paths never overlap.
//
// The package is cross-platform on purpose. It only does TOML parsing
// and file IO, both portable; keeping it free of a //go:build freebsd
// tag means cmd/mwan/opnsense_daemon_serve.go (which is itself built on
// both Linux and FreeBSD) can call daemoncfg.Load without any
// build-tag gymnastics in callers or tests.
package daemoncfg

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
)

// DefaultPath is the on-disk location of the daemon-side TOML. The
// rc.d script writes this file before starting the daemon. The path is
// intentionally not operator-tunable: it is a runtime contract between
// the rc.d script and the daemon, not a user-facing knob.
const DefaultPath = "/var/lib/mwan/daemon.toml"

// missingFileMessage is the exact error string returned when the daemon-side
// TOML is absent. The rc.d script is responsible for templating the TOML before
// exec'ing the daemon, so a missing file always indicates a packaging or rc.d
// bug, never a normal startup.
const missingFileMessage = "daemoncfg: /var/lib/mwan/daemon.toml not found; " +
	"the rc.d script must template this file before starting the daemon"

// DaemonSection is the [daemon] table in /var/lib/mwan/daemon.toml.
// Every field is required; daemoncfg.Load rejects empty values rather
// than falling back to compiled defaults. Logfile is recorded here for
// contract completeness even though the daemon itself does not open it:
// the rc.d wrapper keeps stdout/stderr redirection and pidfile
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

// Load reads /var/lib/mwan/daemon.toml, validates that every required
// field is present, and returns the parsed config. There is no fallback
// to compiled defaults: a missing file or missing field is a hard error
// so the daemon refuses to start with an under-specified config.
func Load() (*Config, error) {
	return loadFrom(DefaultPath)
}

// loadFrom is the path-injected variant used by tests; production
// callers should use Load().
func loadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && path == DefaultPath {
			slog.Error("daemoncfg: default file missing", "path", path, "err", err)
			return nil, errors.New(missingFileMessage)
		}
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
// fix the rc.d-templated file directly.
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
