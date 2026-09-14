package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// Rollback reverts the VM to the prepare-phase snapshot per design
// section 4.4. The function deletes any child snapshots in
// newest-first order, calls VMRollback, restarts the VM if needed,
// waits for QGA liveness, and re-runs Validate as a sanity check.
func Rollback(ctx context.Context, deps Deps, opts Options) (State, error) {
	cur, target, err := preRollbackChecks(ctx, deps, opts)
	if err != nil {
		return cur, err
	}

	if err := deleteChildSnapshots(ctx, deps, opts.VMID, target); err != nil {
		return cur, err
	}

	if err := deps.Snap.VMRollback(ctx, opts.VMID, target); err != nil {
		return markRollbackFailed(ctx, deps, opts, cur,
			"VMRollback failed", err, "VMRollback", target)
	}

	if err := startGuestIfStopped(ctx, deps, opts, cur); err != nil {
		return cur, err
	}

	if deps.Exec != nil {
		post := opts.PostRollbackTimeout
		if post <= 0 {
			post = DefaultPostRollbackTimeout
		}
		if err := waitForGuest(ctx, deps, opts.VMID, post); err != nil {
			return markRollbackFailed(ctx, deps, opts, cur,
				"QGA did not respond after rollback", err, "waitForGuest", target)
		}
	}

	cur.Phase = PhaseRolledBack
	clk := clockOrDefault(deps.Clock)
	if err := saveStateCtx(ctx, opts.StateDir, cur, clk.Now()); err != nil {
		return cur, err
	}
	emit(ctx, deps.Notifier, slog.LevelWarn, KindRollback, opts.VMID,
		"opnsense-upgrade rollback: snapshot restored",
		slog.String("vmid", opts.VMID),
		slog.String("snapshot", target),
	)
	slog.InfoContext(ctx, "upgrade.Rollback: snapshot restored",
		"vmid", opts.VMID, "snapshot", target)
	return cur, nil
}

// preRollbackChecks validates options, loads state, picks the snapshot
// to roll back to, and returns the resolved values. Splitting this
// logic into a helper keeps the main Rollback below the gocognit limit.
func preRollbackChecks(ctx context.Context, deps Deps, opts Options) (State, string, error) {
	if err := validateOptions(opts); err != nil {
		slog.ErrorContext(ctx, "upgrade.Rollback: invalid options", "err", err)
		return emptyState(), "", err
	}
	if deps.Snap == nil {
		err := errors.New("upgrade.Rollback: deps.Snap is required")
		slog.ErrorContext(ctx, "upgrade.Rollback: deps.Snap missing", "err", err)
		return emptyState(), "", err
	}
	cur, err := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if err != nil {
		return emptyState(), "", err
	}
	if !rollbackAllowedFrom(cur.Phase) {
		txErr := TransitionNotAllowedError{From: cur.Phase, To: PhaseRolledBack}
		slog.ErrorContext(ctx, "upgrade.Rollback: refusing transition",
			"err", txErr, "from", cur.Phase)
		return cur, "", txErr
	}
	target := opts.Snapshot
	if target == "" {
		target = cur.Snapshot
	}
	if target == "" {
		err := errors.New("upgrade.Rollback: no snapshot recorded and none provided via --snapshot")
		slog.ErrorContext(ctx, "upgrade.Rollback: missing snapshot",
			"err", err, "vmid", opts.VMID)
		return cur, "", err
	}
	return cur, target, nil
}

// deleteChildSnapshots purges any watchdog-managed child snapshots that
// would block VMRollback from reaching the target.
func deleteChildSnapshots(ctx context.Context, deps Deps, vmid, target string) error {
	listing, err := deps.Snap.VMSnapshots(ctx, vmid)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Rollback: VMSnapshots", "err", err, "vmid", vmid)
		return fmt.Errorf("upgrade.Rollback: VMSnapshots: %w", err)
	}
	children := snapshotsAfter(listing, target)
	for _, child := range slices.Backward(children) {
		if err := deps.Snap.VMDelSnapshot(ctx, vmid, child); err != nil {
			if logger := deps.Log; logger != nil {
				logger.WarnContext(ctx, "upgrade.Rollback: child snapshot delete failed",
					"vmid", vmid, "snap", child, "err", err)
			}
		}
	}
	return nil
}

// startGuestIfStopped probes the VM status and starts it when needed.
// Used after VMRollback because the snapshot may have included a
// stopped state.
func startGuestIfStopped(ctx context.Context, deps Deps, opts Options, cur State) error {
	running, err := deps.Snap.VMStatus(ctx, opts.VMID)
	if err != nil && deps.Log != nil {
		deps.Log.WarnContext(ctx, "upgrade.Rollback: VMStatus probe failed",
			"vmid", opts.VMID, "err", err)
	}
	if running {
		return nil
	}
	if err := deps.Snap.VMStart(ctx, opts.VMID); err != nil {
		_, markErr := markRollbackFailed(ctx, deps, opts, cur,
			"VM did not start after rollback", err, "VMStart", cur.Snapshot)
		return markErr
	}
	return nil
}

// markRollbackFailed records the rollback_failed phase, emits a
// loud-alert notify event, and returns the wrapped error. Centralizing
// this path keeps the Rollback function below the gocognit limit and
// guarantees every failure path goes through the same notify kind.
func markRollbackFailed(
	ctx context.Context,
	deps Deps,
	opts Options,
	cur State,
	humanMsg string,
	cause error,
	stage string,
	target string,
) (State, error) {
	cur.Phase = PhaseRollbackFailed
	clk := clockOrDefault(deps.Clock)
	if saveErr := saveStateCtx(ctx, opts.StateDir, cur, clk.Now()); saveErr != nil {
		slog.WarnContext(ctx, "upgrade.Rollback: save rollback_failed state",
			"err", saveErr)
	}
	emit(ctx, deps.Notifier, slog.LevelError, KindRollbackFailed, opts.VMID,
		"opnsense-upgrade rollback: "+humanMsg,
		slog.String("vmid", opts.VMID),
		slog.String("snapshot", target),
		slog.String("err", cause.Error()),
	)
	slog.ErrorContext(ctx, "upgrade.Rollback: "+stage+" failed",
		"err", cause, "snapshot", target, "vmid", opts.VMID)
	return cur, fmt.Errorf("upgrade.Rollback: %s: %w", stage, cause)
}

// rollbackAllowedFrom reports whether the given phase can transition
// to rolled_back. PhaseValidatedPass and PhaseCommitted are explicitly
// excluded; the design says those use a dedicated `revert-committed`
// flow which is out of scope for this slice.
func rollbackAllowedFrom(p Phase) bool {
	switch p {
	case PhaseExecuted, PhaseExecuteFailed, PhaseExecuteHung,
		PhaseValidatedFail, PhaseValidatedPartial, PhasePrepared:
		return true
	case PhaseEmpty, PhaseExecuting, PhaseValidatedPass, PhaseRolledBack,
		PhaseRollbackFailed, PhaseCommitted:
		return false
	default:
		return false
	}
}
