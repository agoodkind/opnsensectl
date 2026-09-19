package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	opnsensecfg "goodkind.io/opnsensectl/internal/config"
)

// wrapErr is the shared "log + wrap" helper used by the opnsense
// subverbs that stream internal-error sites are noisy enough that
// every wrap call already carries diagnostic context. Returning
// [fmt.Errorf] without a paired [slog.Error] trips the staticcheck-extra
// slogged-wrap rule, so this helper centralizes the pairing.
func wrapErr(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "mwan opnsense: "+op, "err", err)
	return fmt.Errorf("%s: %w", op, err)
}

// loadOpnsenseConfig loads the OPNsense tooling config (see
// [opnsensecfg.Load] for the file it picks) and returns it. The operator
// verbs that need TOML values funnel through this so the error surface is
// uniform: a missing or unreadable config file is rejected up-front with a
// clear message and the toml path in scope. The long-running services read
// the file their --config names through loadServiceConfig instead.
func loadOpnsenseConfig() (*opnsensecfg.Config, error) {
	cfg, err := opnsensecfg.Load()
	if err != nil {
		slog.Error("opnsense: load config", "err", err)
		return nil, fmt.Errorf("opnsense: load config: %w", err)
	}
	return cfg, nil
}

// loadServiceConfig loads exactly path, the file a service's --config names,
// with no defaults and no environment override (see [opnsensecfg.LoadFile]).
func loadServiceConfig(path string) (*opnsensecfg.Config, error) {
	cfg, err := opnsensecfg.LoadFile(path)
	if err != nil {
		slog.Error("opnsense: load service config", "err", err, "path", path)
		return nil, fmt.Errorf("opnsense: load config: %w", err)
	}
	return cfg, nil
}

// requireProbeTarget returns [opnsense.probe].target or a wrapped error
// that names the missing TOML key. Probe-driven verbs (file push/pull,
// config *, daemon push/stage/restart/revert/gc/state, exec, selftest,
// version) call this first so the operator never gets a generic dial
// failure when the real problem is empty TOML.
func requireProbeTarget(cfg *opnsensecfg.Config) (string, error) {
	target := strings.TrimSpace(cfg.OPNsense.Probe.Target)
	if target == "" {
		return "", fmt.Errorf("opnsense: [opnsense.probe].target is required in %s", cfg.Source)
	}
	return target, nil
}

// requireProbeTimeout parses [opnsense.probe].timeout. An empty value is a
// hard error because the TOML value is the source of truth.
func requireProbeTimeout(cfg *opnsensecfg.Config) (time.Duration, error) {
	raw := strings.TrimSpace(cfg.OPNsense.Probe.TimeoutDuration)
	if raw == "" {
		return 0, fmt.Errorf("opnsense: [opnsense.probe].timeout is required in %s", cfg.Source)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Error("opnsense: parse probe timeout", "err", err, "value", raw)
		return 0, fmt.Errorf("opnsense: [opnsense.probe].timeout %q is not a Go duration: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("opnsense: [opnsense.probe].timeout %q must be positive", raw)
	}
	return d, nil
}

// probeTransferStallDefault is the fallback used when
// [opnsense.probe].transfer_stall_timeout is absent. Transfers are bounded by
// lack of progress, not total time, so this only needs to be long enough that a
// healthy channel never has a gap this large between chunks.
const probeTransferStallDefault = 30 * time.Second

// requireProbeTransferStall parses [opnsense.probe].transfer_stall_timeout.
// Unlike requireProbeTimeout, an empty value is NOT an error: it falls back to
// probeTransferStallDefault. A rendered or test config may omit this key (or the
// whole section), and a transfer must never fail to start over a missing knob. A
// present-but-malformed value still errors so operator typos are caught.
func requireProbeTransferStall(cfg *opnsensecfg.Config) (time.Duration, error) {
	raw := strings.TrimSpace(cfg.OPNsense.Probe.TransferStallDuration)
	if raw == "" {
		return probeTransferStallDefault, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Error("opnsense: parse probe transfer_stall_timeout", "err", err, "value", raw)
		return 0, fmt.Errorf("opnsense: [opnsense.probe].transfer_stall_timeout %q is not a Go duration: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("opnsense: [opnsense.probe].transfer_stall_timeout %q must be positive", raw)
	}
	return d, nil
}

// requireProbeUploadChunk returns [opnsense.probe].upload_chunk_bytes
// or a default when not set. Zero or negative values are rejected.
func requireProbeUploadChunk(cfg *opnsensecfg.Config) (int, error) {
	chunk := cfg.OPNsense.Probe.UploadChunkBytes
	if chunk <= 0 {
		return 0, fmt.Errorf("opnsense: [opnsense.probe].upload_chunk_bytes must be > 0 in %s", cfg.Source)
	}
	return chunk, nil
}

// requireHostUpstream returns [opnsense.host].upstream or an error.
func requireHostUpstream(cfg *opnsensecfg.Config) (string, error) {
	v := strings.TrimSpace(cfg.OPNsense.Host.Upstream)
	if v == "" {
		return "", fmt.Errorf("opnsense: [opnsense.host].upstream is required in %s", cfg.Source)
	}
	return v, nil
}

// requireHostListen returns [opnsense.host].listen or an error.
func requireHostListen(cfg *opnsensecfg.Config) (string, error) {
	v := strings.TrimSpace(cfg.OPNsense.Host.Listen)
	if v == "" {
		return "", fmt.Errorf("opnsense: [opnsense.host].listen is required in %s", cfg.Source)
	}
	return v, nil
}

// requireDrainChardev returns [opnsense.drain].chardev or an error.
func requireDrainChardev(cfg *opnsensecfg.Config) (string, error) {
	v := strings.TrimSpace(cfg.OPNsense.Drain.Chardev)
	if v == "" {
		return "", fmt.Errorf("opnsense: [opnsense.drain].chardev is required in %s", cfg.Source)
	}
	return v, nil
}

// requireDrainListen returns [opnsense.drain].listen or an error. The value
// must be an absolute filesystem path: unix:// URLs and relative paths are
// rejected so callers always get a path that [net.Listen]("unix", v) can use.
func requireDrainListen(cfg *opnsensecfg.Config) (string, error) {
	v := strings.TrimSpace(cfg.OPNsense.Drain.Listen)
	if v == "" {
		return "", fmt.Errorf("opnsense: [opnsense.drain].listen is required in %s", cfg.Source)
	}
	if strings.HasPrefix(v, "unix://") {
		return "", fmt.Errorf("opnsense: [opnsense.drain].listen %q must be an absolute path, not a unix:// URL", v)
	}
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("opnsense: [opnsense.drain].listen %q must be an absolute path", v)
	}
	return v, nil
}

// parseHostDurations returns reconnect, heartbeat-interval, and
// heartbeat-timeout from the [opnsense.host] section. An empty string
// means the field is absent from TOML, which is a hard error.
func parseHostDurations(cfg *opnsensecfg.Config) (reconnect, hbInterval, hbTimeout time.Duration, err error) {
	reconnect, err = parseRequiredDuration(cfg, cfg.OPNsense.Host.ReconnectDuration, "[opnsense.host].reconnect")
	if err != nil {
		return 0, 0, 0, err
	}
	hbInterval, err = parseRequiredDuration(cfg, cfg.OPNsense.Host.HeartbeatIntervalDuration, "[opnsense.host].heartbeat_interval")
	if err != nil {
		return 0, 0, 0, err
	}
	hbTimeout, err = parseRequiredDuration(cfg, cfg.OPNsense.Host.HeartbeatTimeoutDuration, "[opnsense.host].heartbeat_timeout")
	if err != nil {
		return 0, 0, 0, err
	}
	return reconnect, hbInterval, hbTimeout, nil
}

// parseRequiredDuration is the named-key wrapper around [time.ParseDuration]
// used by every TOML-driven verb. The key argument is interpolated into
// the error message so operators get the exact TOML coordinate, not a
// generic "invalid duration" message.
func parseRequiredDuration(cfg *opnsensecfg.Config, raw, key string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("opnsense: %s is required in %s", key, cfg.Source)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Error("opnsense: parse duration", "err", err, "key", key, "value", raw)
		return 0, fmt.Errorf("opnsense: %s %q is not a Go duration: %w", key, raw, err)
	}
	return d, nil
}

// requireUpgradeVMID returns [opnsense.upgrade].vmid as a string. A
// zero VMID is a hard error.
func requireUpgradeVMID(cfg *opnsensecfg.Config) (string, error) {
	if cfg.OPNsense.Upgrade.VMID <= 0 {
		return "", fmt.Errorf("opnsense: [opnsense.upgrade].vmid is required in %s", cfg.Source)
	}
	return strconv.Itoa(cfg.OPNsense.Upgrade.VMID), nil
}

// requireUpgradeStateDir returns [opnsense.upgrade].state_dir.
func requireUpgradeStateDir(cfg *opnsensecfg.Config) (string, error) {
	v := strings.TrimSpace(cfg.OPNsense.Upgrade.StateDir)
	if v == "" {
		return "", fmt.Errorf("opnsense: [opnsense.upgrade].state_dir is required in %s", cfg.Source)
	}
	return v, nil
}

// requireUpgradeGRPCTarget returns [opnsense.upgrade].env_grpc_target.
// The daemon socket is the single source of truth for the gRPC upgrade path.
func requireUpgradeGRPCTarget(cfg *opnsensecfg.Config) (string, error) {
	v := strings.TrimSpace(cfg.OPNsense.Upgrade.EnvGRPCTarget)
	if v == "" {
		return "", fmt.Errorf("opnsense: [opnsense.upgrade].env_grpc_target is required in %s", cfg.Source)
	}
	return v, nil
}
