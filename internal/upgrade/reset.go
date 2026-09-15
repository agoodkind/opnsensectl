package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// ResetOptions controls one invocation of Reset. VMID is required;
// StateDir defaults to DefaultStateDir when empty; DeployID is
// optional and, when empty, is inferred from the most recently
// modified deploy directory under <StateDir>/<VMID>/.
type ResetOptions struct {
	VMID     string
	StateDir string
	DeployID string
}

// Plan describes the operations Reset would perform. The fields are
// exported so the CLI can render the dry-run plan to the operator and
// then hand the same value to ResetExecute.
type Plan struct {
	// VMID echoes the target VM so the CLI can render a self-contained
	// plan without re-reading flags.
	VMID string
	// DeployID is the resolved deploy identifier. Empty when no state
	// file was found and no deploy directory was discoverable.
	DeployID string
	// SnapshotsToDelete is the list of pre-upgrade-* snapshots that
	// reset will pass to VMDelSnapshot. The recorded baseline snapshot
	// (RollbackTarget) is intentionally excluded so the rollback step
	// has a target.
	SnapshotsToDelete []string
	// RollbackTarget is the recorded baseline snapshot name from
	// state.json. Empty when state.json is missing or carries no
	// snapshot, in which case ResetExecute skips the rollback step.
	RollbackTarget string
	// StatePath is the absolute path to the state.json file that reset
	// will remove. Empty when no state file exists.
	StatePath string
	// NothingToDo is true when reset has no work: no state file and no
	// orphan upgrade snapshots. The CLI prints a friendly message and
	// exits 0 without prompting for --confirm in this case.
	NothingToDo bool
}

// Reset builds a Plan describing what cleanup ResetExecute would
// perform on the given VM. It does not mutate anything. The function
// reads state.json (best effort), enumerates pre-upgrade-* snapshots
// via the Snapshotter, and validates that the recorded baseline still
// exists on the VM. The constraints encoded here match the operator-
// facing contract: on a clean VM with no state and no snapshots the
// returned Plan has NothingToDo=true; if state.json names a baseline absent
// from the VM, Reset returns an error and the operator must investigate
// manually.
func Reset(ctx context.Context, deps Deps, opts ResetOptions) (Plan, error) {
	if opts.VMID == "" {
		err := errors.New("upgrade.Reset: VMID is required")
		slog.ErrorContext(ctx, "upgrade.Reset: VMID missing", "err", err)
		return Plan{}, err
	}
	if deps.Snap == nil {
		err := errors.New("upgrade.Reset: deps.Snap is required")
		slog.ErrorContext(ctx, "upgrade.Reset: deps.Snap missing", "err", err)
		return Plan{}, err
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = DefaultStateDir
	}

	stateExists, st, statePath, err := loadResetState(ctx, stateDir, opts.VMID)
	if err != nil {
		return Plan{}, err
	}

	resolvedDeployID := opts.DeployID
	if resolvedDeployID == "" {
		if stateExists && st.DeployID != "" {
			resolvedDeployID = st.DeployID
		} else {
			inferred, ierr := mostRecentDeployDir(ctx, stateDir, opts.VMID)
			if ierr != nil {
				return Plan{}, ierr
			}
			resolvedDeployID = inferred
		}
	}

	listing, err := deps.Snap.VMSnapshots(ctx, opts.VMID)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Reset: VMSnapshots", "err", err, "vmid", opts.VMID)
		return Plan{}, fmt.Errorf("upgrade.Reset: VMSnapshots: %w", err)
	}
	names := parseSnapshotNames(listing)
	upgradeSnaps := make([]string, 0, len(names))
	for _, n := range names {
		if IsUpgradeSnapshot(n) {
			upgradeSnaps = append(upgradeSnaps, n)
		}
	}

	rollbackTarget := ""
	if stateExists && st.Snapshot != "" {
		rollbackTarget = st.Snapshot
		if !slices.Contains(names, rollbackTarget) {
			err := fmt.Errorf("upgrade.Reset: recorded baseline snapshot %q not present on vm %s; refusing to delete anything, investigate manually",
				rollbackTarget, opts.VMID)
			slog.ErrorContext(ctx, "upgrade.Reset: missing baseline", "err", err,
				"vmid", opts.VMID, "snapshot", rollbackTarget)
			return Plan{}, err
		}
	}

	toDelete := make([]string, 0, len(upgradeSnaps))
	for _, n := range upgradeSnaps {
		if n == rollbackTarget {
			continue
		}
		toDelete = append(toDelete, n)
	}
	sort.Strings(toDelete)

	plan := Plan{
		VMID:              opts.VMID,
		DeployID:          resolvedDeployID,
		SnapshotsToDelete: toDelete,
		RollbackTarget:    rollbackTarget,
		StatePath:         "",
		NothingToDo:       false,
	}
	if stateExists {
		plan.StatePath = statePath
	}
	plan.NothingToDo = !stateExists && len(toDelete) == 0 && rollbackTarget == ""
	return plan, nil
}

// ResetExecute applies the Plan: delete each orphan snapshot, perform
// the rollback to the recorded baseline (when one is present), and
// remove state.json. The operations run strictly in order; the first
// failure short-circuits with an error so downstream steps do not
// touch ambiguous state. A Plan with NothingToDo=true is a no-op and
// returns nil.
func ResetExecute(ctx context.Context, deps Deps, plan Plan) error {
	if deps.Snap == nil {
		err := errors.New("upgrade.ResetExecute: deps.Snap is required")
		slog.ErrorContext(ctx, "upgrade.ResetExecute: deps.Snap missing", "err", err)
		return err
	}
	if plan.NothingToDo {
		slog.InfoContext(ctx, "upgrade.ResetExecute: nothing to do", "vmid", plan.VMID)
		return nil
	}
	if plan.VMID == "" {
		err := errors.New("upgrade.ResetExecute: plan.VMID is required")
		slog.ErrorContext(ctx, "upgrade.ResetExecute: VMID missing", "err", err)
		return err
	}
	deleted := make([]string, 0, len(plan.SnapshotsToDelete))
	for _, snap := range plan.SnapshotsToDelete {
		if err := deps.Snap.VMDelSnapshot(ctx, plan.VMID, snap); err != nil {
			slog.ErrorContext(ctx, "upgrade.ResetExecute: VMDelSnapshot",
				"err", err, "vmid", plan.VMID, "snapshot", snap,
				"deleted_before_failure", deleted)
			return fmt.Errorf("upgrade.ResetExecute: delete %q: %w", snap, err)
		}
		deleted = append(deleted, snap)
	}
	if len(deleted) > 0 {
		slog.InfoContext(ctx, "upgrade.ResetExecute: snapshots deleted",
			"vmid", plan.VMID, "count", len(deleted), "snapshots", deleted)
	}
	if plan.RollbackTarget != "" {
		if err := deps.Snap.VMRollback(ctx, plan.VMID, plan.RollbackTarget); err != nil {
			slog.ErrorContext(ctx, "upgrade.ResetExecute: VMRollback",
				"err", err, "vmid", plan.VMID, "snapshot", plan.RollbackTarget)
			return fmt.Errorf("upgrade.ResetExecute: rollback to %q: %w", plan.RollbackTarget, err)
		}
		slog.InfoContext(ctx, "upgrade.ResetExecute: rolled back",
			"vmid", plan.VMID, "snapshot", plan.RollbackTarget)
	}
	if plan.StatePath != "" {
		if err := os.Remove(plan.StatePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.ErrorContext(ctx, "upgrade.ResetExecute: remove state",
				"err", err, "path", plan.StatePath)
			return fmt.Errorf("upgrade.ResetExecute: remove state %q: %w", plan.StatePath, err)
		}
		slog.InfoContext(ctx, "upgrade.ResetExecute: state removed",
			"vmid", plan.VMID, "path", plan.StatePath)
	}
	return nil
}

// loadResetState reads state.json for the given VMID. It distinguishes
// "file does not exist" (stateExists=false, no error) from a true read
// or parse failure (returned as an error). The returned statePath is
// the canonical location regardless of whether the file was found, so
// callers can stash it for later removal.
func loadResetState(ctx context.Context, stateDir, vmid string) (bool, State, string, error) {
	path := statePathFor(stateDir, vmid)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, emptyState(), path, nil
		}
		slog.ErrorContext(ctx, "upgrade.Reset: stat state", "err", err, "path", path)
		return false, emptyState(), path, fmt.Errorf("upgrade.Reset: stat %q: %w", path, err)
	}
	st, err := loadStateCtx(ctx, stateDir, vmid)
	if err != nil {
		return false, emptyState(), path, err
	}
	return true, st, path, nil
}

// mostRecentDeployDir returns the name of the most recently modified
// subdirectory under <stateDir>/<vmid>/. An empty string with nil
// error signals "no deploy directories present", which is a normal
// condition on a clean VM. Filesystem errors other than "not exist"
// surface to the caller.
func mostRecentDeployDir(ctx context.Context, stateDir, vmid string) (string, error) {
	root := filepath.Join(stateDir, vmid)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		slog.ErrorContext(ctx, "upgrade.Reset: read deploy dir", "err", err, "path", root)
		return "", fmt.Errorf("upgrade.Reset: read %q: %w", root, err)
	}
	var newestName string
	var newestMtimeNs int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, ierr := entry.Info()
		if ierr != nil {
			continue
		}
		modNs := info.ModTime().UnixNano()
		if newestName == "" || modNs > newestMtimeNs {
			newestName = entry.Name()
			newestMtimeNs = modNs
		}
	}
	return newestName, nil
}
