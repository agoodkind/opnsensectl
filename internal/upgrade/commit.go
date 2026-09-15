package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Commit finalizes a successful upgrade or a clean rollback by
// deleting the prepare-phase snapshot per design section 4.5 and
// resolved decision 11.8. With KeepSnapshot the snapshot is retained
// and renamed by leaving it on the VM for the gc subcommand to sweep
// later, or for the operator to rename to keep- prefix manually.
func Commit(ctx context.Context, deps Deps, opts Options) (State, error) {
	if err := validateOptions(opts); err != nil {
		slog.ErrorContext(ctx, "upgrade.Commit: invalid options", "err", err)
		return emptyState(), err
	}
	if deps.Snap == nil {
		err := errors.New("upgrade.Commit: deps.Snap is required")
		slog.ErrorContext(ctx, "upgrade.Commit: deps.Snap missing", "err", err)
		return emptyState(), err
	}
	clk := clockOrDefault(deps.Clock)

	cur, err := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if err != nil {
		return emptyState(), err
	}
	if cur.Phase == PhaseCommitted {
		return cur, nil
	}
	if !commitAllowedFrom(cur.Phase) {
		txErr := TransitionNotAllowedError{From: cur.Phase, To: PhaseCommitted}
		slog.ErrorContext(ctx, "upgrade.Commit: refusing transition",
			"err", txErr, "from", cur.Phase)
		return cur, txErr
	}

	target := opts.Snapshot
	if target == "" {
		target = cur.Snapshot
	}

	if !opts.KeepSnapshot && target != "" {
		if err := deps.Snap.VMDelSnapshot(ctx, opts.VMID, target); err != nil {
			slog.ErrorContext(ctx, "upgrade.Commit: VMDelSnapshot",
				"err", err, "snapshot", target)
			return cur, fmt.Errorf("upgrade.Commit: VMDelSnapshot %q: %w", target, err)
		}
	}

	cur.Phase = PhaseCommitted
	if err := saveStateCtx(ctx, opts.StateDir, cur, clk.Now()); err != nil {
		return cur, err
	}
	emit(ctx, deps.Notifier, slog.LevelInfo, KindCommit, opts.VMID,
		"opnsense-upgrade commit: snapshot released",
		slog.String("vmid", opts.VMID),
		slog.String("snapshot", target),
		slog.Bool("kept", opts.KeepSnapshot),
	)
	resolve(ctx, deps.Notifier, KindRollback, opts.VMID,
		"opnsense-upgrade rollback: cleared after commit")
	resolve(ctx, deps.Notifier, KindValidate, opts.VMID,
		"opnsense-upgrade validate: cleared after commit")
	slog.InfoContext(ctx, "upgrade.Commit: complete",
		"vmid", opts.VMID, "snapshot", target, "kept", opts.KeepSnapshot)
	return cur, nil
}

// commitAllowedFrom enforces the design 4.5 precondition: commit is
// reachable from PhaseValidatedPass and PhaseRolledBack only. Partial
// validation requires an explicit operator override which the CLI
// layer surfaces as a separate flag, not a default-allowed transition.
func commitAllowedFrom(p Phase) bool {
	return p == PhaseValidatedPass || p == PhaseRolledBack
}
