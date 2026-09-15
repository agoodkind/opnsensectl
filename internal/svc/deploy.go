package svc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Deploy paths and constants.
const (
	DefaultBinaryDir  = "/usr/local/sbin"
	BinaryName        = "mwan-opnsense"
	BinarySymlink     = "mwan-opnsense"
	BinaryCurrent     = "mwan-opnsense.current"
	BinaryPrevious    = "mwan-opnsense.previous"
	BinaryStaged      = "mwan-opnsense.staged"
	BinaryStagedTmp   = "mwan-opnsense.staged.tmp"
	DeployStatePath   = "/var/run/mwan-opnsense.deploy.state"
	PendingVerifyPath = "/var/run/mwan-opnsense.pending-verify"
	StateFileMode     = 0o644
	BinaryFileMode    = 0o755
	StagedTmpFileMode = 0o600
	PendingFileMode   = 0o644
	HealthOK          = "ok"
	HealthPending     = "pending"
	MarkerFreshDeploy = "fresh-deploy"
)

// DeployConfig parameterizes filesystem locations for the deploy
// machinery. Defaults are used when fields are zero-valued. Tests
// override these with tmpdirs.
type DeployConfig struct {
	BinaryDir      string // dir holding .current, .previous, .staged
	StatePath      string // colon-delim flag file
	PendingPath    string // marker dropped before swap
	WriteStateFile func(path string, content []byte, mode os.FileMode) error
	NowFn          func() time.Time
}

// DeployManager owns the binary swap + state-file lifecycle on the
// daemon side. It is safe for concurrent calls; serial.go ensures
// only one Deploy/Revert is in flight at a time via the server mutex,
// but DeployManager also takes its own mutex defensively.
type DeployManager struct {
	cfg DeployConfig
	log *slog.Logger
	mu  sync.Mutex
}

// NewDeployManager constructs a DeployManager. Empty fields in cfg
// fall back to package defaults.
func NewDeployManager(log *slog.Logger, cfg DeployConfig) *DeployManager {
	if cfg.BinaryDir == "" {
		cfg.BinaryDir = DefaultBinaryDir
	}
	if cfg.StatePath == "" {
		cfg.StatePath = DeployStatePath
	}
	if cfg.PendingPath == "" {
		cfg.PendingPath = PendingVerifyPath
	}
	if cfg.WriteStateFile == nil {
		cfg.WriteStateFile = atomicWriteFile
	}
	if cfg.NowFn == nil {
		cfg.NowFn = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &DeployManager{cfg: cfg, log: log, mu: sync.Mutex{}}
}

// pathOf returns the absolute path for a binary slot.
func (d *DeployManager) pathOf(name string) string {
	return filepath.Join(d.cfg.BinaryDir, name)
}

// CurrentSHA256 returns the hex-encoded sha256 of .current. Returns
// empty string and nil error if the file doesn't exist (cold boot).
func (d *DeployManager) CurrentSHA256() (string, error) {
	return fileSHA256(d.pathOf(BinaryCurrent))
}

// PreviousSHA256 returns the hex-encoded sha256 of .previous. Returns
// empty string and nil error if the file doesn't exist (no prior deploy).
func (d *DeployManager) PreviousSHA256() (string, error) {
	return fileSHA256(d.pathOf(BinaryPrevious))
}

// BinaryDir returns the absolute directory holding .current/.previous/
// .staged. Exposed so the streaming Deploy handler can drop its temp
// file on the same filesystem (renames must be cross-mount-free).
func (d *DeployManager) BinaryDir() string { return d.cfg.BinaryDir }

// CleanStaleTemps removes orphaned deploy scratch files from BinaryDir.
// The daemon calls this once at startup, before opening the serial
// port. A hard kill mid-Deploy leaves a .staged (intermediate rename
// target) or .staged.tmp (intermediate write target); a hard kill mid
// TransferService upload into the binary dir leaves a ".<name>.upload.
// <id>" dotfile. None of these can be in flight at startup, so any that
// exist are orphans from a prior crash. The promotable .current and
// .previous slots are never matched, and finishSwap sha-verifies before
// the rename, so a removed orphan was never promoted. Returns the count
// of files removed.
func (d *DeployManager) CleanStaleTemps(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(d.cfg.BinaryDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("clean stale temps: read %s: %w", d.cfg.BinaryDir, err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isStaleDeployTemp(name) {
			continue
		}
		path := filepath.Join(d.cfg.BinaryDir, name)
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			d.log.WarnContext(ctx, "deploy: clean stale temp failed", "path", path, "err", rmErr)
			continue
		}
		d.log.DebugContext(ctx, "deploy: removed orphaned deploy temp", "path", path)
		removed++
	}
	return removed, nil
}

// isStaleDeployTemp reports whether name is an orphaned deploy scratch
// file: an intermediate staged slot, or an interrupted upload temp
// (".<base>.upload.<id>"). The .current and .previous slots and the
// symlink never match, so a promotable binary is never removed.
func isStaleDeployTemp(name string) bool {
	if name == BinaryStaged || name == BinaryStagedTmp {
		return true
	}
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".upload.")
}

// DeployFromPath is the streaming entry point. The caller has already
// written the binary to srcPath (typically a temp file produced by
// the streaming Deploy handler). DeployFromPath verifies sha256
// against the on-disk content, renames into the staged slot, swaps
// .current and .previous, drops pending-verify, and arms re-exec. On
// any error before the rename, srcPath is left in place so the caller
// can decide whether to remove or inspect it. On success, srcPath has
// been consumed (renamed into the staged slot).
func (d *DeployManager) DeployFromPath(ctx context.Context, srcPath, sha256Hex, versionStr string) (previousPath string, stagedSHA256 string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.log.InfoContext(ctx, "deploy: begin from path",
		"src", srcPath,
		"sha256_hex", sha256Hex,
		"version", versionStr)

	if srcPath == "" {
		err = errors.New("deploy: empty src path")
		d.log.ErrorContext(ctx, "deploy: invalid", "err", err)
		return "", "", err
	}
	if sha256Hex == "" {
		err = errors.New("deploy: sha256_hex required")
		d.log.ErrorContext(ctx, "deploy: invalid", "err", err)
		return "", "", err
	}

	info, statErr := os.Stat(srcPath)
	if statErr != nil {
		d.log.ErrorContext(ctx, "deploy: stat src failed", "err", statErr, "path", srcPath)
		return "", "", fmt.Errorf("stat src %s: %w", srcPath, statErr)
	}
	if info.Size() == 0 {
		err = errors.New("deploy: empty binary payload")
		d.log.ErrorContext(ctx, "deploy: invalid", "err", err)
		return "", "", err
	}

	computedHex, hashErr := fileSHA256(srcPath)
	if hashErr != nil {
		d.log.ErrorContext(ctx, "deploy: hash src failed", "err", hashErr)
		return "", "", fmt.Errorf("hash src: %w", hashErr)
	}
	if !strings.EqualFold(computedHex, sha256Hex) {
		err = fmt.Errorf("deploy: sha256 mismatch: got %s, want %s", computedHex, sha256Hex)
		d.log.ErrorContext(ctx, "deploy: sha256 mismatch", "err", err, "got", computedHex, "want", sha256Hex)
		return "", "", err
	}
	stagedSHA256 = computedHex

	staged := d.pathOf(BinaryStaged)
	current := d.pathOf(BinaryCurrent)
	previous := d.pathOf(BinaryPrevious)
	previousPath = previous

	if renameErr := os.Rename(srcPath, staged); renameErr != nil {
		d.log.ErrorContext(ctx, "deploy: stage rename failed", "err", renameErr, "from", srcPath, "to", staged)
		return "", "", fmt.Errorf("stage rename: %w", renameErr)
	}
	if chmodErr := os.Chmod(staged, BinaryFileMode); chmodErr != nil {
		d.log.ErrorContext(ctx, "deploy: stage chmod failed", "err", chmodErr, "path", staged)
		_ = os.Remove(staged)
		return "", "", fmt.Errorf("chmod staged: %w", chmodErr)
	}

	if err := d.finishSwap(ctx, staged, current, previous, versionStr, computedHex); err != nil {
		return "", "", err
	}
	return previousPath, stagedSHA256, nil
}

// finishSwap performs the .current<->.previous swap, state-file
// update, pending-verify marker drop, and re-exec arming for
// DeployFromPath.
func (d *DeployManager) finishSwap(ctx context.Context, staged, current, previous, versionStr, computedHex string) error {
	if _, statErr := os.Stat(current); statErr == nil {
		if copyErr := copyFile(current, previous); copyErr != nil {
			d.log.ErrorContext(ctx, "deploy: copy current to previous failed", "err", copyErr)
			_ = os.Remove(staged)
			return fmt.Errorf("copy current to previous: %w", copyErr)
		}
		d.log.InfoContext(ctx, "deploy: previous slot updated", "path", previous)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		d.log.ErrorContext(ctx, "deploy: stat current failed", "err", statErr, "path", current)
		_ = os.Remove(staged)
		return fmt.Errorf("stat current: %w", statErr)
	}

	if err := os.Rename(staged, current); err != nil {
		d.log.ErrorContext(ctx, "deploy: current swap failed", "err", err, "from", staged, "to", current)
		_ = os.Remove(staged)
		return fmt.Errorf("rename staged to current: %w", err)
	}
	d.log.InfoContext(ctx, "deploy: current binary swapped", "path", current, "sha256", computedHex)

	previousSHA, _ := fileSHA256(previous)
	if writeErr := d.writeState(deployState{
		Active:     computedHex,
		Previous:   previousSHA,
		Version:    versionStr,
		DeployedAt: d.cfg.NowFn().Unix(),
		Health:     HealthPending,
	}); writeErr != nil {
		d.log.ErrorContext(ctx, "deploy: write state failed", "err", writeErr)
	}

	if writeErr := d.cfg.WriteStateFile(d.cfg.PendingPath, []byte(MarkerFreshDeploy+"\n"), PendingFileMode); writeErr != nil {
		d.log.ErrorContext(ctx, "deploy: write pending marker failed", "err", writeErr)
	}

	d.log.InfoContext(ctx, "deploy: staged binary swapped into active slot",
		"sha256", computedHex)
	return nil
}

// MarkHealthy clears the pending-verify marker and stamps health=ok.
// Called from the DeployStatus RPC handler on MARK_HEALTHY.
func (d *DeployManager) MarkHealthy(ctx context.Context) (active, previous string, deployedAt int64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.log.InfoContext(ctx, "deploy: mark healthy")

	state, err := d.readStateLocked()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.ErrorContext(ctx, "deploy: read state failed", "err", err)
		return "", "", 0, fmt.Errorf("read state: %w", err)
	}
	state.Health = HealthOK
	if state.Active == "" {
		// State file missing or stale: derive from disk.
		current, _ := d.CurrentSHA256()
		state.Active = current
	}
	if state.Previous == "" {
		prev, _ := d.PreviousSHA256()
		state.Previous = prev
	}
	if state.DeployedAt == 0 {
		state.DeployedAt = d.cfg.NowFn().Unix()
	}

	if err := d.writeState(state); err != nil {
		d.log.ErrorContext(ctx, "deploy: write state failed", "err", err)
		return "", "", 0, fmt.Errorf("write state: %w", err)
	}
	if err := os.Remove(d.cfg.PendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.ErrorContext(ctx, "deploy: remove pending marker failed", "err", err, "path", d.cfg.PendingPath)
		return "", "", 0, fmt.Errorf("remove pending marker: %w", err)
	}
	d.log.InfoContext(ctx, "deploy: healthy", "active", state.Active, "previous", state.Previous)
	return state.Active, state.Previous, state.DeployedAt, nil
}

// Status returns the current deploy state for the DeployStatus RPC.
// Returns derived state if the state file is absent.
func (d *DeployManager) Status(ctx context.Context) (active, previous, health string, deployedAt int64, err error) {
	state, readErr := d.readStateLocked()
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		d.log.ErrorContext(ctx, "deploy: status read state failed", "err", readErr)
		return "", "", "", 0, fmt.Errorf("read state: %w", readErr)
	}
	// Always reconcile against on-disk reality.
	current, _ := d.CurrentSHA256()
	prev, _ := d.PreviousSHA256()
	if state.Active == "" {
		state.Active = current
	}
	if state.Previous == "" {
		state.Previous = prev
	}
	if state.Health == "" {
		state.Health = HealthPending
	}
	return state.Active, state.Previous, state.Health, state.DeployedAt, nil
}

// Revert copies .previous over .current, drops pending-verify marker,
// and re-execs. Errors if .previous is absent.
func (d *DeployManager) Revert(ctx context.Context) (revertedTo string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.log.InfoContext(ctx, "revert: begin")

	current := d.pathOf(BinaryCurrent)
	previous := d.pathOf(BinaryPrevious)

	if _, statErr := os.Stat(previous); statErr != nil {
		d.log.ErrorContext(ctx, "revert: previous absent", "err", statErr, "path", previous)
		return "", fmt.Errorf("previous absent: %w", statErr)
	}

	revertedTo, sumErr := fileSHA256(previous)
	if sumErr != nil {
		d.log.ErrorContext(ctx, "revert: sha256 failed", "err", sumErr, "path", previous)
		return "", fmt.Errorf("sha256 previous: %w", sumErr)
	}

	if copyErr := copyFile(previous, current); copyErr != nil {
		d.log.ErrorContext(ctx, "revert: copy failed", "err", copyErr)
		return "", fmt.Errorf("copy previous to current: %w", copyErr)
	}

	if err := d.writeState(deployState{
		Active:     revertedTo,
		Previous:   revertedTo, // after revert, both slots match
		Version:    "reverted",
		DeployedAt: d.cfg.NowFn().Unix(),
		Health:     HealthPending,
	}); err != nil {
		d.log.ErrorContext(ctx, "revert: write state failed", "err", err)
		// not fatal
	}

	if writeErr := d.cfg.WriteStateFile(d.cfg.PendingPath, []byte(MarkerFreshDeploy+"\n"), PendingFileMode); writeErr != nil {
		d.log.ErrorContext(ctx, "revert: write pending marker failed", "err", writeErr)
	}

	d.log.InfoContext(ctx, "revert: completed", "reverted_to_sha256", revertedTo)
	return revertedTo, nil
}

// deployState is the structured form of the state file.
type deployState struct {
	Active     string
	Previous   string
	Version    string
	DeployedAt int64
	Health     string
}

// writeState atomically rewrites the state file.
func (d *DeployManager) writeState(s deployState) error {
	content := encodeStateFile(s)
	return d.cfg.WriteStateFile(d.cfg.StatePath, []byte(content), StateFileMode)
}

// readStateLocked reads and parses the state file. Caller must hold mu.
// [os.ErrNotExist] is reported but not slogged at error level since absent
// state is the normal cold-boot condition; callers gate on [errors.Is].
func (d *DeployManager) readStateLocked() (deployState, error) {
	data, err := os.ReadFile(d.cfg.StatePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.log.Error("readStateLocked: read failed", "path", d.cfg.StatePath, "err", err)
		}
		return deployState{}, fmt.Errorf("read state file %s: %w", d.cfg.StatePath, err)
	}
	return parseStateFile(string(data)), nil
}

// stateKey enumerates the recognized keys in the colon-delimited
// state file. Unknown keys in input are ignored.
type stateKey string

const (
	stateKeyActive     stateKey = "active"
	stateKeyPrevious   stateKey = "previous"
	stateKeyVersion    stateKey = "version"
	stateKeyDeployedAt stateKey = "deployed_at"
	stateKeyHealth     stateKey = "health"
)

// encodeStateFile renders deployState as colon-delimited key:value
// lines in stable order.
func encodeStateFile(s deployState) string {
	var b strings.Builder
	b.WriteString(string(stateKeyActive))
	b.WriteByte(':')
	b.WriteString(s.Active)
	b.WriteByte('\n')
	b.WriteString(string(stateKeyPrevious))
	b.WriteByte(':')
	b.WriteString(s.Previous)
	b.WriteByte('\n')
	b.WriteString(string(stateKeyVersion))
	b.WriteByte(':')
	b.WriteString(s.Version)
	b.WriteByte('\n')
	b.WriteString(string(stateKeyDeployedAt))
	b.WriteByte(':')
	b.WriteString(strconv.FormatInt(s.DeployedAt, 10))
	b.WriteByte('\n')
	b.WriteString(string(stateKeyHealth))
	b.WriteByte(':')
	b.WriteString(s.Health)
	b.WriteByte('\n')
	return b.String()
}

// parseStateFile parses colon-delimited key:value lines. Unknown
// keys are ignored. Missing keys yield zero values.
func parseStateFile(content string) deployState {
	var s deployState
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		key := stateKey(line[:idx])
		val := line[idx+1:]
		switch key {
		case stateKeyActive:
			s.Active = val
		case stateKeyPrevious:
			s.Previous = val
		case stateKeyVersion:
			s.Version = val
		case stateKeyDeployedAt:
			ts, err := strconv.ParseInt(val, 10, 64)
			if err == nil {
				s.DeployedAt = ts
			}
		case stateKeyHealth:
			s.Health = val
		}
	}
	return s
}

// atomicWriteFile writes content to a tmpfile then renames into place.
// Default backing for DeployConfig.WriteStateFile.
func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		slog.Error("atomicWriteFile: open failed", "path", tmp, "err", err)
		return fmt.Errorf("open %s: %w", tmp, err)
	}
	if _, writeErr := file.Write(content); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		slog.Error("atomicWriteFile: write failed", "path", tmp, "err", writeErr)
		return fmt.Errorf("write %s: %w", tmp, writeErr)
	}
	if syncErr := file.Sync(); syncErr != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		slog.Error("atomicWriteFile: sync failed", "path", tmp, "err", syncErr)
		return fmt.Errorf("sync %s: %w", tmp, syncErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		slog.Error("atomicWriteFile: close failed", "path", tmp, "err", closeErr)
		return fmt.Errorf("close %s: %w", tmp, closeErr)
	}
	if renameErr := os.Rename(tmp, path); renameErr != nil {
		_ = os.Remove(tmp)
		slog.Error("atomicWriteFile: rename failed", "from", tmp, "to", path, "err", renameErr)
		return fmt.Errorf("rename %s into %s: %w", tmp, path, renameErr)
	}
	return nil
}

// copyFile copies src to dst, preserving executable mode.
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = source.Close() }()

	tmp := dst + ".tmp"
	target, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, BinaryFileMode)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}
	if _, copyErr := io.Copy(target, source); copyErr != nil {
		_ = target.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy: %w", copyErr)
	}
	if syncErr := target.Sync(); syncErr != nil {
		_ = target.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync %s: %w", tmp, syncErr)
	}
	if closeErr := target.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		slog.Error("copyFile: close target failed", "path", tmp, "err", closeErr)
		return fmt.Errorf("close %s: %w", tmp, closeErr)
	}
	if renameErr := os.Rename(tmp, dst); renameErr != nil {
		_ = os.Remove(tmp)
		slog.Error("copyFile: rename failed", "from", tmp, "to", dst, "err", renameErr)
		return fmt.Errorf("rename %s into %s: %w", tmp, dst, renameErr)
	}
	return nil
}

// fileSHA256 returns the hex-encoded sha256 of a file. Empty string
// + nil error if path doesn't exist.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		slog.Error("fileSHA256: open failed", "path", path, "err", err)
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	if _, copyErr := io.Copy(hasher, file); copyErr != nil {
		slog.Error("fileSHA256: hash failed", "path", path, "err", copyErr)
		return "", fmt.Errorf("hash %s: %w", path, copyErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
