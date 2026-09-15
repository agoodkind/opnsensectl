package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
)

// Validate invokes the Validator interface and translates the result into the
// next Phase plus a notify event.
func Validate(ctx context.Context, deps Deps, opts Options) (State, ValidationResult, error) {
	if err := validateOptions(opts); err != nil {
		slog.ErrorContext(ctx, "upgrade.Validate: invalid options", "err", err)
		return emptyState(), emptyValidation(), err
	}
	if deps.Validate == nil {
		err := errors.New("upgrade.Validate: deps.Validate is required")
		slog.ErrorContext(ctx, "upgrade.Validate: deps.Validate missing", "err", err)
		return emptyState(), emptyValidation(), err
	}
	clk := clockOrDefault(deps.Clock)

	cur, err := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if err != nil {
		return emptyState(), emptyValidation(), err
	}
	if cur.Phase != PhaseExecuted && cur.Phase != PhaseExecuteFailed && cur.Phase != PhaseRolledBack {
		err := TransitionNotAllowedError{From: cur.Phase, To: PhaseValidatedPass}
		slog.ErrorContext(ctx, "upgrade.Validate: refusing transition",
			"err", err, "from", cur.Phase)
		return cur, emptyValidation(), err
	}

	result, err := deps.Validate.Validate(ctx, ValidateContext{
		VMID:     opts.VMID,
		Target:   opts.Target,
		StateDir: opts.StateDir,
		DeployID: cur.DeployID,
		Logger:   deps.Log,
	})
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Validate: validator", "err", err, "vmid", opts.VMID)
		return cur, result, fmt.Errorf("upgrade.Validate: validator: %w", err)
	}
	// Re-aggregate the booleans from Checks. A validator that returned
	// an inconsistent (AllPass/AnyFail/Partial) triple gets normalized
	// here so downstream phase selection is deterministic.
	result = AggregateChecks(result.Checks)

	deployDir := deployPathFor(opts.StateDir, opts.VMID, cur.DeployID)
	if err := writeValidationReport(ctx, filepath.Join(deployDir, "validate.json"), result); err != nil {
		slog.WarnContext(ctx, "upgrade.Validate: write validation report failed", "err", err)
	}

	failingNames := failingCheckNames(result)
	target := selectValidatePhase(result, opts.AcceptPartial)

	if cur.Phase == PhaseRolledBack && target != PhaseValidatedPass {
		postErr := errors.New("upgrade.Validate: post-rollback validation did not pass")
		slog.ErrorContext(ctx, "upgrade.Validate: post-rollback validation did not pass",
			"err", postErr, "vmid", opts.VMID, "phase", cur.Phase)
		return cur, result, postErr
	}

	cur.Phase = target
	cur.FailingCheck = failingNames
	if err := saveStateCtx(ctx, opts.StateDir, cur, clk.Now()); err != nil {
		return cur, result, err
	}

	level, msg := validateLevelAndMessage(target, len(failingNames))
	emit(ctx, deps.Notifier, level, KindValidate, opts.VMID,
		msg,
		slog.String("vmid", opts.VMID),
		slog.String("phase", string(target)),
		slog.Int("failing_count", len(failingNames)),
	)
	if target == PhaseValidatedPass {
		resolve(ctx, deps.Notifier, KindValidate, opts.VMID,
			"opnsense-upgrade validate: all checks passed")
	}
	slog.InfoContext(ctx, "upgrade.Validate: complete",
		"vmid", opts.VMID, "phase", target, "failing", len(failingNames))
	return cur, result, nil
}

// writeValidationReport marshals the validation result and writes the
// JSON file under the deploy directory. The signature uses a concrete
// type rather than `any` to satisfy the lint rule that bans empty
// interfaces in function signatures.
func writeValidationReport(ctx context.Context, path string, r ValidationResult) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: marshal validation report", "err", err, "path", path)
		return fmt.Errorf("upgrade: marshal validation report %q: %w", path, err)
	}
	return WriteFileBytes(ctx, path, data)
}

// selectValidatePhase decides which validated_* phase the run lands
// in. The default is strict: any failure means PhaseValidatedFail.
// AcceptPartial promotes a partial result to PhaseValidatedPartial,
// which is the explicit operator-decision state called out in design
// section 5.
func selectValidatePhase(result ValidationResult, acceptPartial bool) Phase {
	switch {
	case result.AllPass:
		return PhaseValidatedPass
	case result.Partial && acceptPartial:
		return PhaseValidatedPartial
	default:
		return PhaseValidatedFail
	}
}

// validateLevelAndMessage maps the resulting Phase to the slog level
// and human-facing notify message used in the email body.
func validateLevelAndMessage(target Phase, failingCount int) (slog.Level, string) {
	switch target {
	case PhaseValidatedPass:
		return slog.LevelInfo, "opnsense-upgrade validate: all checks passed"
	case PhaseValidatedPartial:
		return slog.LevelWarn,
			fmt.Sprintf("opnsense-upgrade validate: partial pass, %d check(s) failed", failingCount)
	case PhaseEmpty, PhasePrepared, PhaseExecuting, PhaseExecuted,
		PhaseExecuteFailed, PhaseExecuteHung, PhaseValidatedFail,
		PhaseRolledBack, PhaseRollbackFailed, PhaseCommitted:
		fallthrough
	default:
		return slog.LevelError,
			fmt.Sprintf("opnsense-upgrade validate: %d check(s) failed", failingCount)
	}
}

// failingCheckNames extracts the Name field from each failing check.
func failingCheckNames(result ValidationResult) []string {
	if result.AllPass {
		return nil
	}
	names := make([]string, 0, len(result.Checks))
	for _, c := range result.Checks {
		if !c.Pass {
			names = append(names, c.Name)
		}
	}
	return names
}

// AggregateChecks computes the AllPass, AnyFail, Partial booleans
// from a slice of CheckResult. Callers building a ValidationResult
// inline use this so the booleans stay consistent.
func AggregateChecks(checks []CheckResult) ValidationResult {
	if len(checks) == 0 {
		return ValidationResult{Checks: checks, AllPass: false, AnyFail: false, Partial: false}
	}
	pass := 0
	fail := 0
	for _, c := range checks {
		if c.Pass {
			pass++
		} else {
			fail++
		}
	}
	return ValidationResult{
		Checks:  checks,
		AllPass: fail == 0,
		AnyFail: fail > 0,
		Partial: pass > 0 && fail > 0,
	}
}

// emptyValidation returns a fully-zero ValidationResult so callers
// always return a fully-populated value (exhaustruct compliance) on
// the error path.
func emptyValidation() ValidationResult {
	return ValidationResult{Checks: nil, AllPass: false, AnyFail: false, Partial: false}
}
