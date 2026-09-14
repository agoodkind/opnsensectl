package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// RunOutcome describes the terminal state of a `run` invocation. The
// CLI converts this to a non-zero exit when AutoRollback fired or the
// pipeline did not reach committed.
type RunOutcome struct {
	State        State
	Validation   ValidationResult
	AutoRollback bool
	Reached      Phase
}

// Run is the unattended pipeline per design section 4.6. It chains
// prepare -> execute -> validate, then auto-rollback on
// validate-fail. Partial-pass is treated as the explicit
// "manual decision required" state from design section 5: Run does
// not auto-rollback or commit; the caller inspects the outcome and
// drives the next step.
func Run(ctx context.Context, deps Deps, opts Options) (RunOutcome, error) {
	preparedState, err := Prepare(ctx, deps, opts)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Run: prepare", "err", err, "vmid", opts.VMID)
		return RunOutcome{State: preparedState, Validation: emptyValidation(), AutoRollback: false, Reached: PhaseEmpty}, err
	}

	loaded, err := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if err != nil {
		return RunOutcome{State: preparedState, Validation: emptyValidation(), AutoRollback: false, Reached: preparedState.Phase}, err
	}
	if loaded.DeployID != "" {
		opts.DeployID = loaded.DeployID
	}

	exState, execErr := Execute(ctx, deps, opts)
	if execErr != nil {
		if logger := deps.Log; logger != nil {
			logger.WarnContext(ctx, "upgrade.Run: execute returned error, continuing to validate",
				"vmid", opts.VMID, "phase", exState.Phase, "err", execErr)
		}
	}

	postExec, _ := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if postExec.Phase == PhaseExecuteHung {
		rb, rbErr := Rollback(ctx, deps, opts)
		out := RunOutcome{State: rb, Validation: emptyValidation(), AutoRollback: true, Reached: rb.Phase}
		if rbErr != nil {
			slog.ErrorContext(ctx, "upgrade.Run: hung-exec rollback", "err", rbErr)
			return out, fmt.Errorf("upgrade.Run: hung-exec rollback: %w", rbErr)
		}
		emitRunComplete(ctx, deps, opts, slog.LevelError,
			"opnsense-upgrade run: hung exec rolled back")
		return out, nil
	}

	validateState, result, validateErr := Validate(ctx, deps, opts)
	out := RunOutcome{State: validateState, Validation: result, AutoRollback: false, Reached: validateState.Phase}

	var dummy TransitionNotAllowedError
	switch {
	case validateErr != nil && !errors.As(validateErr, &dummy):
		emitRunComplete(ctx, deps, opts, slog.LevelError,
			"opnsense-upgrade run: validator returned error")
		slog.ErrorContext(ctx, "upgrade.Run: validator", "err", validateErr)
		return out, validateErr
	case validateState.Phase == PhaseValidatedPass:
		emitRunComplete(ctx, deps, opts, slog.LevelInfo,
			"opnsense-upgrade run: validated pass")
		return out, nil
	case validateState.Phase == PhaseValidatedPartial:
		emitRunComplete(ctx, deps, opts, slog.LevelWarn,
			"opnsense-upgrade run: validated partial, manual decision required")
		return out, nil
	case validateState.Phase == PhaseValidatedFail:
		rb, rbErr := Rollback(ctx, deps, opts)
		out.State = rb
		out.AutoRollback = true
		out.Reached = rb.Phase
		if rbErr != nil {
			emitRunComplete(ctx, deps, opts, slog.LevelError,
				"opnsense-upgrade run: validate failed, rollback failed")
			slog.ErrorContext(ctx, "upgrade.Run: validate-fail rollback", "err", rbErr)
			return out, fmt.Errorf("upgrade.Run: validate-fail rollback: %w", rbErr)
		}
		emitRunComplete(ctx, deps, opts, slog.LevelError,
			"opnsense-upgrade run: validate failed, rolled back")
		return out, nil
	default:
		emitRunComplete(ctx, deps, opts, slog.LevelError,
			"opnsense-upgrade run: ended in unexpected phase")
		err := fmt.Errorf("upgrade.Run: unexpected phase %q", validateState.Phase)
		slog.ErrorContext(ctx, "upgrade.Run: unexpected phase", "err", err, "phase", validateState.Phase)
		return out, err
	}
}

func emitRunComplete(ctx context.Context, deps Deps, opts Options, level slog.Level, msg string) {
	emit(ctx, deps.Notifier, level, KindRunComplete, opts.VMID, msg,
		slog.String("vmid", opts.VMID),
		slog.String("target", opts.Target),
	)
}
