// Package daemoncfg loads the in-VM mwan-opnsense daemon's runtime
// configuration from the file `opnsensectl daemon serve --config PATH` names.
// The rc.d service names [InstallPath].
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
	"fmt"
	"log/slog"
	"os"

	"github.com/BurntSushi/toml"
)

// InstallPath is where opnsensectl install writes the daemon-side TOML and the
// file the installed rc.d service passes to `daemon serve --config`. The daemon
// never reads it unless its command line names it.
const InstallPath = "/usr/local/etc/opnsensectl.conf"

// DefaultFile is the content opnsensectl install writes to [InstallPath]
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

// Load reads exactly path and validates that every required field is present.
// There is no other file and no fallback to compiled defaults: a missing file
// or missing field is a hard error that names it, so the daemon refuses to
// start with an under-specified config.
func Load(path string) (*Config, error) {
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

	if err := validate(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces that every field in the daemon TOML schema is present and
// non-empty. The error message cites the offending TOML key and the file so
// operators can fix the file directly.
func validate(path string, cfg *Config) error {
	d := &cfg.Daemon
	if d.SerialPath == "" {
		return fmt.Errorf("daemoncfg: [daemon] serial_path is required in %s", path)
	}
	if d.Baud == 0 {
		return fmt.Errorf("daemoncfg: [daemon] baud is required and must be non-zero in %s", path)
	}
	if d.ConfigXMLPath == "" {
		return fmt.Errorf("daemoncfg: [daemon] config_xml_path is required in %s", path)
	}
	if d.BackupDir == "" {
		return fmt.Errorf("daemoncfg: [daemon] backup_dir is required in %s", path)
	}
	if d.Logfile == "" {
		return fmt.Errorf("daemoncfg: [daemon] logfile is required in %s", path)
	}
	if d.StateDir == "" {
		return fmt.Errorf("daemoncfg: [daemon] state_dir is required in %s", path)
	}
	return nil
}
