package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

// Execute applies the pending OPNsense firmware change through the
// Executor. It compares the firmware state prepare captured with what
// the repository offers, applies the change the way the web interface
// does, reboots only when OPNsense requires it, and then checks that
// every pending change landed. With DryRunExecute it writes the plan
// and stops before installing or rebooting anything.
func Execute(ctx context.Context, deps Deps, opts Options) (State, error) {
	if err := validateOptions(opts); err != nil {
		slog.ErrorContext(ctx, "upgrade.Execute: invalid options", "err", err)
		return emptyState(), err
	}
	if deps.Exec == nil {
		err := errors.New("upgrade.Execute: deps.Exec is required")
		slog.ErrorContext(ctx, "upgrade.Execute: deps.Exec missing", "err", err)
		return emptyState(), err
	}
	clk := clockOrDefault(deps.Clock)
	now := clk.Now()

	cur, err := loadStateCtx(ctx, opts.StateDir, opts.VMID)
	if err != nil {
		return emptyState(), err
	}
	if err := EnforceTransition(cur.Phase, PhaseExecuting); err != nil {
		slog.ErrorContext(ctx, "upgrade.Execute: refusing transition",
			"err", err, "from", cur.Phase, "to", PhaseExecuting)
		return cur, err
	}

	timeout := opts.UpgradeTimeout
	if timeout <= 0 {
		timeout = DefaultUpgradeTimeout
	}

	executingState := cur
	executingState.Phase = PhaseExecuting
	if err := saveStateCtx(ctx, opts.StateDir, executingState, now); err != nil {
		return emptyState(), err
	}

	deployDir := deployPathFor(opts.StateDir, opts.VMID, cur.DeployID)
	run := executeRun{
		deps:      deps,
		opts:      opts,
		clk:       clk,
		timeout:   timeout,
		state:     executingState,
		deployDir: deployDir,
		runner: &guestRunner{
			exec:    deps.Exec,
			vmid:    opts.VMID,
			logPath: filepath.Join(deployDir, artefactUpgradeLog),
			log:     nil,
		},
	}
	return run.firmwareChange(ctx)
}

// executeRun carries one execute invocation's inputs so the steps and
// their failure handling share them.
type executeRun struct {
	deps      Deps
	opts      Options
	clk       Clock
	timeout   time.Duration
	state     State
	deployDir string
	runner    *guestRunner
}

func (e executeRun) firmwareChange(ctx context.Context) (State, error) {
	pre, err := readFirmwareState(ctx, filepath.Join(e.deployDir, ArtefactFirmwarePre))
	if err != nil {
		return e.fail(ctx, nil, "read pre-upgrade firmware state", err)
	}

	execCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	plan, err := planFirmwareChange(execCtx, e.runner, pre, e.opts)
	if err != nil {
		return e.fail(ctx, execCtx, "find pending update", err)
	}
	if err := writeFirmwarePlan(ctx, filepath.Join(e.deployDir, ArtefactFirmwarePlan), plan); err != nil {
		return e.fail(ctx, nil, "write firmware plan", err)
	}
	if e.opts.DryRunExecute {
		return e.finish(ctx, "opnsense-upgrade execute: dry run found the pending update, nothing applied",
			planAttrs(plan)...)
	}

	result, err := applyFirmwareChange(execCtx, e.runner, plan)
	if err != nil {
		return e.fail(ctx, execCtx, "apply update", err)
	}
	if result.RebootRequired {
		if err := rebootGuest(ctx, e.clk, e.runner, defaultRebootWait()); err != nil {
			return e.fail(ctx, nil, "reboot", err)
		}
	}
	post, err := captureFirmwareState(ctx, e.runner)
	if err != nil {
		return e.fail(ctx, nil, "read post-update firmware state", err)
	}
	result.Post = post
	if err := writeFirmwareResult(ctx, filepath.Join(e.deployDir, ArtefactFirmwarePost), result); err != nil {
		slog.WarnContext(ctx, "upgrade.Execute: write firmware result failed", "err", err)
	}
	if err := verifyFirmware(plan, result); err != nil {
		return e.fail(ctx, nil, "verify update", err)
	}
	attrs := append(planAttrs(plan),
		slog.Bool("rebooted", result.RebootRequired),
		slog.String("post_core_version", post.CoreVersion),
		slog.String("post_base_version", post.BaseVersion),
		slog.String("post_kernel_version", post.KernelVersion),
	)
	return e.finish(ctx, "opnsense-upgrade execute: update applied", attrs...)
}

// fail records the failure phase and notifies. hungCtx is the watchdog
// context of the step that failed; when its deadline passed the phase is
// execute_hung so Run rolls back. Steps outside the watchdog pass nil.
func (e executeRun) fail(ctx, hungCtx context.Context, stage string, cause error) (State, error) {
	st := e.state
	if hungCtx != nil && errors.Is(hungCtx.Err(), context.DeadlineExceeded) {
		st.Phase = PhaseExecuteHung
		e.save(ctx, st)
		emit(ctx, e.deps.Notifier, slog.LevelError, KindExecute, e.opts.VMID,
			"opnsense-upgrade execute: hung after watchdog timeout",
			slog.Duration("timeout", e.timeout),
			slog.String("vmid", e.opts.VMID),
			slog.String("stage", stage),
		)
		hungErr := fmt.Errorf("upgrade.Execute: hung after %s during %s", e.timeout, stage)
		slog.ErrorContext(ctx, "upgrade.Execute: hung", "err", hungErr, "vmid", e.opts.VMID, "timeout", e.timeout)
		return st, hungErr
	}
	st.Phase = PhaseExecuteFailed
	e.save(ctx, st)
	emit(ctx, e.deps.Notifier, slog.LevelError, KindExecute, e.opts.VMID,
		"opnsense-upgrade execute: "+stage+" failed",
		slog.String("vmid", e.opts.VMID),
		slog.String("err", cause.Error()),
	)
	failErr := fmt.Errorf("upgrade.Execute: %s: %w", stage, cause)
	slog.ErrorContext(ctx, "upgrade.Execute: failed", "err", failErr, "vmid", e.opts.VMID, "stage", stage)
	return st, failErr
}

func (e executeRun) save(ctx context.Context, st State) {
	if err := saveStateCtx(ctx, e.opts.StateDir, st, e.clk.Now()); err != nil {
		slog.WarnContext(ctx, "upgrade.Execute: save failed state failed", "err", err)
	}
}

func (e executeRun) finish(ctx context.Context, msg string, attrs ...slog.Attr) (State, error) {
	st := e.state
	st.Phase = PhaseExecuted
	if err := saveStateCtx(ctx, e.opts.StateDir, st, e.clk.Now()); err != nil {
		return emptyState(), err
	}
	fields := append([]slog.Attr{
		slog.String("vmid", e.opts.VMID),
		slog.Bool("dry_run", e.opts.DryRunExecute),
	}, attrs...)
	emit(ctx, e.deps.Notifier, slog.LevelInfo, KindExecute, e.opts.VMID, msg, fields...)
	slog.LogAttrs(ctx, slog.LevelInfo, "upgrade.Execute: "+msg, fields...)
	return st, nil
}

func planAttrs(plan firmwarePlan) []slog.Attr {
	return []slog.Attr{
		slog.String("mode", string(plan.Mode)),
		slog.String("core_package", plan.Installed.CorePackage),
		slog.String("core_installed", plan.Installed.CoreVersion),
		slog.String("core_available", plan.CoreAvailable),
		slog.Bool("core_pending", plan.CorePending),
		slog.String("sets_available", plan.SetsAvailable),
		slog.Bool("base_pending", plan.BasePending),
		slog.Bool("kernel_pending", plan.KernelPending),
		slog.Bool("reboot_required", plan.RebootRequired),
	}
}

// waitForGuest is a small helper used by rollback to poll for QGA
// liveness. It polls every 2 seconds up to deadline. Any answer proves the
// restored boot, because qm rollback stops the VM before restoring it, so
// no guest from before the rollback is left to answer.
func waitForGuest(ctx context.Context, deps Deps, vmid string, deadline time.Duration) error {
	if deps.Exec == nil {
		err := errors.New("waitForGuest: deps.Exec is required")
		slog.ErrorContext(ctx, "upgrade.waitForGuest: deps.Exec missing", "err", err)
		return err
	}
	pollCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		res, err := deps.Exec.GuestExec(pollCtx, vmid, "true")
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		select {
		case <-pollCtx.Done():
			timedErr := fmt.Errorf("waitForGuest: timed out after %s", deadline)
			slog.WarnContext(ctx, "upgrade.waitForGuest: timed out",
				"err", timedErr, "vmid", vmid, "deadline", deadline)
			return timedErr
		case <-time.After(2 * time.Second):
		}
	}
}
