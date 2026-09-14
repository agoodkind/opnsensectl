package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// metadata is the per-deploy metadata.json shape. Captures enough to
// reconstruct what the upgrade was supposed to do without re-reading
// the state file.
type metadata struct {
	DeployID   string    `json:"deploy_id"`
	VMID       string    `json:"vmid"`
	Target     string    `json:"target"`
	Snapshot   string    `json:"snapshot"`
	StartedAt  time.Time `json:"started_at"`
	UseBootEnv bool      `json:"use_boot_environment"`
	DryRunExec bool      `json:"dry_run_execute"`
}

// Prepare runs the prepare phase per design section 4.1. It captures
// pre-upgrade artifacts under <state_dir>/<vmid>/<deploy-id>/, takes a
// Proxmox snapshot, and writes the state file to PhasePrepared. On any
// failure before the snapshot lands, the state file is not touched.
func Prepare(ctx context.Context, deps Deps, opts Options) (State, error) {
	if err := validateOptions(opts); err != nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: invalid options", "err", err)
		return emptyState(), err
	}
	if deps.Snap == nil {
		err := errors.New("upgrade.Prepare: deps.Snap is required")
		slog.ErrorContext(ctx, "upgrade.Prepare: deps.Snap missing", "err", err)
		return emptyState(), err
	}
	clk := clockOrDefault(deps.Clock)
	now := clk.Now()

	deployID := opts.DeployID
	if deployID == "" {
		generated, err := newDeployID()
		if err != nil {
			slog.ErrorContext(ctx, "upgrade.Prepare: deploy id", "err", err)
			return emptyState(), fmt.Errorf("upgrade.Prepare: deploy id: %w", err)
		}
		deployID = generated
	}

	cur, err := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: load state", "err", err, "vmid", opts.VMID)
		return emptyState(), err
	}
	if cur.Phase != PhaseEmpty && cur.Phase != PhasePrepared {
		err := TransitionNotAllowedError{From: cur.Phase, To: PhasePrepared}
		slog.ErrorContext(ctx, "upgrade.Prepare: refusing transition",
			"err", err, "from", cur.Phase, "to", PhasePrepared)
		return cur, err
	}

	deployDir := deployPathFor(opts.StateDir, opts.VMID, deployID)
	if err := os.MkdirAll(deployDir, 0o750); err != nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: mkdir deploy dir failed", "err", err, "path", deployDir)
		return emptyState(), fmt.Errorf("upgrade.Prepare: mkdir deploy dir: %w", err)
	}

	snap := SnapshotName(now)

	if err := capturePreUpgradeArtefacts(ctx, deps, opts, deployDir); err != nil {
		emit(ctx, deps.Notifier, slog.LevelError, KindPrepare, opts.VMID,
			"opnsense-upgrade prepare: pre-upgrade artefact capture failed",
			slog.String("vmid", opts.VMID),
			slog.String("deploy_id", deployID),
			slog.String("err", err.Error()),
		)
		slog.ErrorContext(ctx, "upgrade.Prepare: capture artefacts failed",
			"err", err, "vmid", opts.VMID, "deploy_id", deployID)
		return emptyState(), err
	}

	if opts.UseBootEnvironment && deps.Exec != nil {
		bectlOK := captureBootEnvironment(ctx, deps, opts, deployDir, clk)
		if logger := deps.Log; logger != nil {
			logger.InfoContext(ctx, "upgrade.Prepare: boot environment capture",
				"vmid", opts.VMID, "deploy_id", deployID, "ok", bectlOK)
		}
	}

	// metadata.json is written after the snapshot lands so that a retry
	// with the same deploy_id does not find a stale file referencing a
	// snapshot that was never created. The snapshot name is known before
	// takeSnapshotAndPersistState is called because SnapshotName is
	// deterministic for a given timestamp.
	meta := metadata{
		DeployID:   deployID,
		VMID:       opts.VMID,
		Target:     opts.Target,
		Snapshot:   snap,
		StartedAt:  now,
		UseBootEnv: opts.UseBootEnvironment,
		DryRunExec: opts.DryRunExecute,
	}
	return takeSnapshotAndPersistState(ctx, deps, opts, deployID, snap, now, meta)
}

// takeSnapshotAndPersistState issues the Proxmox snapshot, writes
// metadata.json, and writes the prepared state file. Split out of
// [Prepare] so the surrounding orchestration stays under the funlen
// ceiling. On any error before the state file is written this returns
// emptyState; the caller returns the same.
//
// metadata.json is written here, after the snapshot succeeds, so that
// a retry with the same deploy_id cannot find a stale file whose
// snapshot name points to a snapshot that was never created.
func takeSnapshotAndPersistState(
	ctx context.Context,
	deps Deps,
	opts Options,
	deployID, snap string,
	now time.Time,
	meta metadata,
) (State, error) {
	if err := deps.Snap.VMSnapshot(ctx, opts.VMID, snap); err != nil {
		emit(ctx, deps.Notifier, slog.LevelError, KindPrepare, opts.VMID,
			"opnsense-upgrade prepare: snapshot creation failed",
			slog.String("vmid", opts.VMID),
			slog.String("snapshot", snap),
			slog.String("err", err.Error()),
		)
		slog.ErrorContext(ctx, "upgrade.Prepare: VMSnapshot failed",
			"err", err, "vmid", opts.VMID, "snapshot", snap)
		return emptyState(), fmt.Errorf("upgrade.Prepare: VMSnapshot: %w", err)
	}

	deployDir := deployPathFor(opts.StateDir, opts.VMID, deployID)
	if err := writeMetadata(ctx, filepath.Join(deployDir, "metadata.json"), meta); err != nil {
		return emptyState(), err
	}

	st := State{
		VMID:         opts.VMID,
		DeployID:     deployID,
		Target:       opts.Target,
		Snapshot:     snap,
		Phase:        PhasePrepared,
		UpdatedAt:    time.Time{},
		FailingCheck: nil,
	}
	if err := saveStateCtx(ctx, opts.StateDir, st, now); err != nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: save state", "err", err, "vmid", opts.VMID)
		return emptyState(), err
	}
	emit(ctx, deps.Notifier, slog.LevelInfo, KindPrepare, opts.VMID,
		"opnsense-upgrade prepare: snapshot taken",
		slog.String("vmid", opts.VMID),
		slog.String("deploy_id", deployID),
		slog.String("snapshot", snap),
		slog.String("target", opts.Target),
	)
	slog.InfoContext(ctx, "upgrade.Prepare: snapshot taken",
		"vmid", opts.VMID, "snapshot", snap, "deploy_id", deployID)
	return st, nil
}

// captureBootEnvironment runs `bectl create` inside the guest. Returns
// true on success. The exit code is treated as advisory: a non-zero
// exit on a non-ZFS guest is expected and documented as best-effort,
// not an error.
func captureBootEnvironment(ctx context.Context, deps Deps, opts Options, deployDir string, clk Clock) bool {
	beName := fmt.Sprintf("pre-mwan152-%d", clk.Now().Unix())
	res, err := deps.Exec.GuestExec(ctx, opts.VMID, "bectl", "create", beName)
	if err != nil {
		_ = WriteFileBytes(ctx, filepath.Join(deployDir, "bectl.err"), []byte(err.Error()))
		return false
	}
	if res.ExitCode != 0 {
		out := fmt.Appendf(nil, "exit=%d stdout=%s stderr=%s", res.ExitCode, res.Stdout, res.Stderr)
		_ = WriteFileBytes(ctx, filepath.Join(deployDir, "bectl.err"), out)
		return false
	}
	_ = WriteFileBytes(ctx, filepath.Join(deployDir, "bectl.ok"), []byte(beName+"\n"))
	return true
}

// validateOptions enforces the cross-cutting Options invariants every
// phase entry point relies on. Each guard has its own line so the
// error message points at the missing field directly.
func validateOptions(opts Options) error {
	if opts.VMID == "" {
		return errors.New("upgrade: VMID is required")
	}
	if opts.StateDir == "" {
		return errors.New("upgrade: StateDir is required")
	}
	return nil
}

// newDeployID returns a 16-byte hex deploy id. Hex rather than uuid so
// the package does not pull a uuid dep; the value is used purely as a
// directory name and ad hoc identifier.
func newDeployID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		slog.Error("upgrade: rand.Read", "err", err)
		return "", fmt.Errorf("upgrade: rand.Read: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// writeMetadata marshals the metadata struct and writes it to the
// given path. Keeping the type concrete (no any) satisfies the
// "deeply enumerated named type" lint rule.
func writeMetadata(ctx context.Context, path string, m metadata) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: marshal metadata", "err", err, "path", path)
		return fmt.Errorf("upgrade: marshal metadata %q: %w", path, err)
	}
	return WriteFileBytes(ctx, path, data)
}

// WriteFileBytes is a small helper that wraps [os.WriteFile] with a
// consistent permission mask (0600). Callers do not need to remember
// the perm bits. Exported so the rare cross-package caller can land
// artifacts under the same upgrade tree.
func WriteFileBytes(ctx context.Context, path string, content []byte) error {
	if err := os.WriteFile(path, content, 0o600); err != nil {
		slog.ErrorContext(ctx, "upgrade: write file", "err", err, "path", path)
		return fmt.Errorf("upgrade: write %q: %w", path, err)
	}
	return nil
}

// clockOrDefault returns deps.Clock when set, otherwise realClock.
func clockOrDefault(c Clock) Clock {
	if c == nil {
		return realClock{}
	}
	return c
}

// emptyState returns a fully-zero State so callers always return a
// fully-populated value (exhaustruct compliance) on the error path.
func emptyState() State {
	return State{
		VMID: "", DeployID: "", Target: "", Snapshot: "",
		Phase: PhaseEmpty, UpdatedAt: time.Time{}, FailingCheck: nil,
	}
}
