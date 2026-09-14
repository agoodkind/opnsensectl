package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	opnsensecfg "goodkind.io/opnsensectl/internal/config"
	opnsensenotify "goodkind.io/opnsensectl/internal/notify"
	"goodkind.io/opnsensectl/internal/opnsense"
	"goodkind.io/opnsensectl/internal/upgrade"
)

// upgradePhase enumerates `mwan opnsense upgrade <phase>` actions.
type upgradePhase string

const (
	upgradePhasePrepare  upgradePhase = "prepare"
	upgradePhaseExecute  upgradePhase = "execute"
	upgradePhaseValidate upgradePhase = "validate"
	upgradePhaseRollback upgradePhase = "rollback"
	upgradePhaseCommit   upgradePhase = "commit"
	upgradePhaseRun      upgradePhase = "run"
	upgradePhaseGC       upgradePhase = "gc"
	upgradePhaseReset    upgradePhase = "reset"
)

func upgradeUsage(out *os.File) {
	fmt.Fprintln(out, "usage: mwan opnsense upgrade <phase>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Phases: prepare, execute, validate, rollback, commit, run, gc, reset")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Every input comes from [opnsense.upgrade] and [email] in "+opnsensecfg.DefaultPath+".")
}

func runOPNsenseUpgradeCmd(args []string) int {
	if len(args) < 1 {
		upgradeUsage(os.Stderr)
		return 2
	}
	verb := upgradePhase(args[0])
	rest := args[1:]
	if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help" || rest[0] == "help") {
		upgradeUsage(os.Stdout)
		return 0
	}
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "mwan opnsense upgrade %s: unexpected arguments: %v\n", verb, rest)
		return 2
	}
	switch verb {
	case upgradePhasePrepare,
		upgradePhaseExecute,
		upgradePhaseValidate,
		upgradePhaseRollback,
		upgradePhaseCommit,
		upgradePhaseRun,
		upgradePhaseGC:
		return runUpgradePhase(verb)
	case upgradePhaseReset:
		return runUpgradeReset()
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense upgrade: unknown phase %q\n", string(verb))
		upgradeUsage(os.Stderr)
		return 2
	}
}

// upgradeInputs is the resolved set of TOML-derived values every phase
// needs. Loading it once and validating every required field up-front
// guarantees the operator sees one clear error message instead of a
// half-completed run.
type upgradeInputs struct {
	VMID               string
	StateDir           string
	GRPCTarget         string
	Target             string
	ExecTimeout        time.Duration
	UpgradeTimeout     time.Duration
	PostRollbackWait   time.Duration
	DryRunExecute      bool
	UseBootEnvironment bool
	AcceptPartial      bool
	KeepSnapshot       bool
	GCOlderThan        time.Duration
	ResetConfirm       bool
	PingTargetIPv4     string
	PingTargetIPv6     string
	Email              opnsensecfg.EmailSection
}

// buildRedial returns a context-free closure suitable for the
// upgrade.GRPCExecutor.Redial field. The closure runs in a goroutine
// that has no caller ctx (the executor invokes it on demand after a
// rollback drops the gRPC channel), so plain opnsense.Dial is the
// right primitive here.
func buildRedial(target string) func() (upgrade.OPNsenseRPCClient, error) {
	return func() (upgrade.OPNsenseRPCClient, error) {
		c, err := opnsense.Dial(target)
		if err != nil {
			slog.Error("opnsense upgrade: redial", "err", err, "target", target)
			return nil, fmt.Errorf("dial %s: %w", target, err)
		}
		return c.RPC(), nil
	}
}

func resolveUpgradeInputs() (upgradeInputs, error) {
	var ui upgradeInputs
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return ui, err
	}
	vmid, err := requireUpgradeVMID(cfg)
	if err != nil {
		return ui, err
	}
	stateDir, err := requireUpgradeStateDir(cfg)
	if err != nil {
		return ui, err
	}
	grpcTarget, err := requireUpgradeGRPCTarget(cfg)
	if err != nil {
		return ui, err
	}
	execTimeout, err := parseRequiredDuration(cfg, cfg.OPNsense.Upgrade.ExecTimeoutDuration, "[opnsense.upgrade].exec_timeout")
	if err != nil {
		return ui, err
	}
	upgradeTimeout, err := parseRequiredDuration(cfg, cfg.OPNsense.Upgrade.UpgradeTimeoutDuration, "[opnsense.upgrade].upgrade_timeout")
	if err != nil {
		return ui, err
	}
	postRollbackWait, err := parseRequiredDuration(cfg, cfg.OPNsense.Upgrade.PostRollbackWaitDuration, "[opnsense.upgrade].post_rollback_wait")
	if err != nil {
		return ui, err
	}
	gcOlderThan, err := parseRequiredDuration(cfg, cfg.OPNsense.Upgrade.GCOlderThan, "[opnsense.upgrade].gc_older_than")
	if err != nil {
		return ui, err
	}
	ui = upgradeInputs{
		VMID:               vmid,
		StateDir:           stateDir,
		GRPCTarget:         grpcTarget,
		Target:             cfg.OPNsense.Upgrade.Target,
		ExecTimeout:        execTimeout,
		UpgradeTimeout:     upgradeTimeout,
		PostRollbackWait:   postRollbackWait,
		DryRunExecute:      cfg.OPNsense.Upgrade.DryRunExecute,
		UseBootEnvironment: cfg.OPNsense.Upgrade.UseBootEnvironment,
		AcceptPartial:      cfg.OPNsense.Upgrade.AcceptPartial,
		KeepSnapshot:       cfg.OPNsense.Upgrade.KeepSnapshot,
		GCOlderThan:        gcOlderThan,
		ResetConfirm:       cfg.OPNsense.Upgrade.ResetConfirm,
		PingTargetIPv4:     cfg.OPNsense.Upgrade.Validate.PingTargetIPv4,
		PingTargetIPv6:     cfg.OPNsense.Upgrade.Validate.PingTargetIPv6,
		Email:              cfg.Email,
	}
	return ui, nil
}

func (ui upgradeInputs) toOptions() upgrade.Options {
	return upgrade.Options{
		VMID:                ui.VMID,
		Target:              ui.Target,
		StateDir:            ui.StateDir,
		DeployID:            "",
		Snapshot:            "",
		DryRunExecute:       ui.DryRunExecute,
		DryRunGC:            false,
		UseBootEnvironment:  ui.UseBootEnvironment,
		AcceptPartial:       ui.AcceptPartial,
		KeepSnapshot:        ui.KeepSnapshot,
		OlderThan:           ui.GCOlderThan,
		UpgradeTimeout:      ui.UpgradeTimeout,
		PostRollbackTimeout: ui.PostRollbackWait,
	}
}

// buildDaemonDialer returns the dialer the validate phase uses to reach the
// host bridge. Each validate run dials afresh, so a client an earlier rollback
// closed never carries into the checks.
func buildDaemonDialer(target string) upgrade.DaemonDialer {
	return func(ctx context.Context) (upgrade.DaemonRPCClient, func() error, error) {
		client, err := opnsense.DialContext(ctx, target)
		if err != nil {
			slog.ErrorContext(ctx, "opnsense upgrade: validate dial", "err", err, "target", target)
			return nil, nil, fmt.Errorf("dial %s: %w", target, err)
		}
		return client.RPC(), client.Close, nil
	}
}

// buildUpgradeDeps wires the production Deps. The executor and the validator's
// daemon checks ride the gRPC channel, and the validator's egress checks ping
// from this host. The alert notifier mails through send-email with the [email]
// section of the OPNsense tooling config.
func buildUpgradeDeps(ui upgradeInputs) (upgrade.Deps, error) {
	logger := slog.Default()
	notifier, err := opnsensenotify.New(ui.Email, logger, "mwan-opnsense-upgrade")
	if err != nil {
		slog.Error("opnsense upgrade: build alert notifier", "err", err)
		return upgrade.Deps{}, fmt.Errorf("build alert notifier: %w", err)
	}
	snapshotter := upgrade.NewQmSnapshotter(logger)

	rpcCli, err := opnsense.Dial(ui.GRPCTarget)
	if err != nil {
		slog.Error("opnsense upgrade: dial", "err", err, "target", ui.GRPCTarget)
		return upgrade.Deps{}, fmt.Errorf("dial %s: %w", ui.GRPCTarget, err)
	}
	target := ui.GRPCTarget
	redial := buildRedial(target)
	exec := &upgrade.GRPCExecutor{
		RPC:                rpcCli.RPC(),
		ExecTimeoutSeconds: upgradeExecTimeoutSeconds(ui.ExecTimeout),
		Redial:             redial,
	}
	return upgrade.Deps{
		Snap:     snapshotter,
		Exec:     exec,
		Validate: upgrade.NewHealthValidator(buildDaemonDialer(target), ui.PingTargetIPv4, ui.PingTargetIPv6),
		Notifier: notifier,
		Clock:    nil,
		Log:      logger,
	}, nil
}

func upgradeExecTimeoutSeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	rounded := (d + time.Second - 1) / time.Second
	const int32Max = int32(2147483647)
	if rounded > time.Duration(int32Max) {
		return int32Max
	}
	return int32(rounded)
}

func runUpgradePhase(phase upgradePhase) int {
	ui, err := resolveUpgradeInputs()
	if err != nil {
		return printAndExit("upgrade "+string(phase), err)
	}
	deps, err := buildUpgradeDeps(ui)
	if err != nil {
		return printAndExit("upgrade "+string(phase), err)
	}
	ctx := context.Background()
	opts := ui.toOptions()
	switch phase {
	case upgradePhasePrepare:
		st, err := upgrade.Prepare(ctx, deps, opts)
		if err != nil {
			return printAndExit("upgrade prepare", err)
		}
		fmt.Fprintf(os.Stdout, "phase=%s deploy_id=%s snapshot=%s\n", st.Phase, st.DeployID, st.Snapshot)
	case upgradePhaseExecute:
		st, err := upgrade.Execute(ctx, deps, opts)
		if err != nil {
			return printAndExit("upgrade execute", err)
		}
		fmt.Fprintf(os.Stdout, "phase=%s\n", st.Phase)
	case upgradePhaseValidate:
		return runUpgradeValidatePhase(ctx, deps, opts)
	case upgradePhaseRollback:
		st, err := upgrade.Rollback(ctx, deps, opts)
		if err != nil {
			return printAndExit("upgrade rollback", err)
		}
		fmt.Fprintf(os.Stdout, "phase=%s snapshot=%s\n", st.Phase, st.Snapshot)
	case upgradePhaseCommit:
		st, err := upgrade.Commit(ctx, deps, opts)
		if err != nil {
			return printAndExit("upgrade commit", err)
		}
		fmt.Fprintf(os.Stdout, "phase=%s\n", st.Phase)
	case upgradePhaseRun:
		out, err := upgrade.Run(ctx, deps, opts)
		if err != nil {
			return printAndExit("upgrade run", err)
		}
		fmt.Fprintf(os.Stdout, "reached=%s auto_rollback=%t\n", out.Reached, out.AutoRollback)
	case upgradePhaseReset:
		// runUpgradePhase never receives upgradePhaseReset; the outer
		// dispatch in runOPNsenseUpgradeCmd routes reset to its own
		// helper. The case exists to satisfy the exhaustive linter.
		return printAndExit("upgrade", fmt.Errorf("internal: reset routed to phase switch"))
	case upgradePhaseGC:
		res, err := upgrade.GC(ctx, deps, opts)
		if err != nil {
			return printAndExit("upgrade gc", err)
		}
		fmt.Fprintf(os.Stdout, "deleted=%s skipped=%s\n",
			strings.Join(res.Deleted, ","), strings.Join(res.Skipped, ","))
	default:
		return printAndExit("upgrade", fmt.Errorf("internal: unhandled phase %q", phase))
	}
	return 0
}

// runUpgradeValidatePhase runs the orchestrator's validate step and prints
// the resulting phase and failing checks.
func runUpgradeValidatePhase(ctx context.Context, deps upgrade.Deps, opts upgrade.Options) int {
	st, res, err := upgrade.Validate(ctx, deps, opts)
	if err != nil {
		return printAndExit("upgrade validate", err)
	}
	fmt.Fprintf(os.Stdout, "phase=%s all_pass=%t partial=%t failing=%s\n",
		st.Phase, res.AllPass, res.Partial, strings.Join(st.FailingCheck, ","))
	return 0
}

// runUpgradeReset is the only phase whose semantics differ from a
// straight phase transition: it computes a plan, prints it, and only
// applies it when [opnsense.upgrade].reset_confirm is true. Reading the
// confirm bit from TOML means an operator who wants to apply a reset
// flips it in their config and re-runs, mirroring the old --confirm
// flag behaviour.
func runUpgradeReset() int {
	ui, err := resolveUpgradeInputs()
	if err != nil {
		return printAndExit("upgrade reset", err)
	}
	// Reset only needs the Snapshotter, not the validator or executor.
	deps := upgrade.Deps{
		Snap:     upgrade.NewQmSnapshotter(slog.Default()),
		Exec:     nil,
		Validate: nil,
		Notifier: nil,
		Clock:    nil,
		Log:      slog.Default(),
	}
	plan, err := upgrade.Reset(context.Background(), deps, upgrade.ResetOptions{
		VMID:     ui.VMID,
		StateDir: ui.StateDir,
		DeployID: "",
	})
	if err != nil {
		return printAndExit("upgrade reset", err)
	}
	if plan.NothingToDo {
		fmt.Fprintln(os.Stdout, "nothing to do")
		return 0
	}
	if !ui.ResetConfirm {
		printResetPlan(os.Stdout, plan)
		fmt.Fprintln(os.Stdout, "")
		fmt.Fprintln(os.Stdout, "set [opnsense.upgrade].reset_confirm = true in the opnsense config to apply.")
		return 2
	}
	if err := upgrade.ResetExecute(context.Background(), deps, plan); err != nil {
		return printAndExit("upgrade reset", err)
	}
	fmt.Fprintln(os.Stdout, "reset complete")
	return 0
}

func printResetPlan(w *os.File, plan upgrade.Plan) {
	fmt.Fprintf(w, "reset plan for vmid=%s deploy_id=%s (dry run):\n", plan.VMID, plan.DeployID)
	if len(plan.SnapshotsToDelete) == 0 {
		fmt.Fprintln(w, "  snapshots to delete: (none)")
	} else {
		fmt.Fprintln(w, "  snapshots to delete:")
		for _, s := range plan.SnapshotsToDelete {
			fmt.Fprintf(w, "    - %s\n", s)
		}
	}
	if plan.RollbackTarget == "" {
		fmt.Fprintln(w, "  rollback target: (none)")
	} else {
		fmt.Fprintf(w, "  rollback target: %s\n", plan.RollbackTarget)
	}
	if plan.StatePath == "" {
		fmt.Fprintln(w, "  state.json to remove: (none)")
	} else {
		fmt.Fprintf(w, "  state.json to remove: %s\n", plan.StatePath)
	}
}
