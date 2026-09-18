// Package upgrade implements the OPNsense upgrade and rollback flow as
// the Go subcommand `mwan opnsense-upgrade`. The package is composed of
// small per-phase functions (prepare, execute, validate, rollback,
// commit, gc) that share a single state file under
// /var/lib/mwan/upgrades/<vmid>/<deploy-id>/.
//
// The package is deliberately decoupled from the concrete RealOps,
// real PVE client, and real validator: every external surface goes
// through an interface defined in this file so the unit tests can stub them.
package upgrade

import (
	"context"
	"log/slog"
	"time"

	"goodkind.io/opnsensectl/internal/notify"
)

// Phase is the typed lifecycle state recorded in the state file.
type Phase string

// Phase values enumerate every documented state in the upgrade
// lifecycle. PhaseEmpty is the zero value. PhaseCommitted ends a cycle
// and a fresh prepare is the only move out of it. PhaseRollbackFailed
// is terminal.
const (
	// PhaseEmpty is the zero value indicating no upgrade in flight.
	PhaseEmpty Phase = ""
	// PhasePrepared is the state after a successful prepare.
	PhasePrepared Phase = "prepared"
	// PhaseExecuting is the transient state during the upgrade run.
	PhaseExecuting Phase = "executing"
	// PhaseExecuted records a clean exit from the upgrade command.
	PhaseExecuted Phase = "executed"
	// PhaseExecuteFailed records a non-zero exit from the upgrade command.
	PhaseExecuteFailed Phase = "execute_failed"
	// PhaseExecuteHung records a watchdog timeout during execute.
	PhaseExecuteHung Phase = "execute_hung"
	// PhaseValidatedPass records a fully-passing validate run.
	PhaseValidatedPass Phase = "validated_pass"
	// PhaseValidatedPartial records a mixed pass/fail validate run with operator opt-in.
	PhaseValidatedPartial Phase = "validated_partial"
	// PhaseValidatedFail records a validate run with at least one failed check.
	PhaseValidatedFail Phase = "validated_fail"
	// PhaseRolledBack records a successful rollback to the prepare-phase snapshot.
	PhaseRolledBack Phase = "rolled_back"
	// PhaseRollbackFailed records a rollback that did not restore a healthy guest.
	PhaseRollbackFailed Phase = "rollback_failed"
	// PhaseCommitted records a finalized upgrade or rollback with snapshot released.
	PhaseCommitted Phase = "committed"
)

// Snapshotter is the Proxmox guest snapshot and lifecycle surface the
// upgrade package needs. [QmSnapshotter] satisfies this interface in
// production; tests stub it.
type Snapshotter interface {
	VMSnapshot(ctx context.Context, vmid, snapName string) error
	VMRollback(ctx context.Context, vmid, snap string) error
	VMSnapshots(ctx context.Context, vmid string) ([]byte, error)
	VMDelSnapshot(ctx context.Context, vmid, snapName string) error
	VMStart(ctx context.Context, vmid string) error
	VMStatus(ctx context.Context, vmid string) (bool, error)
}

// GuestExecResult mirrors [ops.GuestExecResult] but is duplicated here
// so the Executor interface does not pull a hard dep on internal/ops.
// Production code passes the value through unchanged.
type GuestExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Executor is the in-guest execution channel. The execute phase calls
// this with a Proxmox QGA path in production. Tests stub this with a
// recording fake. The QGA call shape (argv list, returning exit code
// plus stdout) is captured here so a real QGA wrapper can be plugged
// in by another slice without touching this package.
type Executor interface {
	GuestExec(ctx context.Context, vmid string, args ...string) (GuestExecResult, error)
}

// Validator decides whether the upgraded router is healthy.
// [HealthValidator] satisfies it in production. The Result type is
// intentionally simple so the unit tests in this package can pass canned
// results without coupling to concrete validator internals.
type Validator interface {
	Validate(ctx context.Context, ctxArgs ValidateContext) (ValidationResult, error)
}

// ValidateContext is the input to [Validator.Validate]. It carries the
// fields a check matrix needs to run: vmid, target version, the
// directory holding pre-upgrade artifacts, and a logger for trace.
type ValidateContext struct {
	VMID     string
	Target   string
	StateDir string
	DeployID string
	Logger   *slog.Logger
}

// CheckResult is the outcome of a single named check inside the
// validator. Pass=true means the check succeeded. Note holds a free
// form human-facing summary for the email body and the audit log.
type CheckResult struct {
	Name string `json:"name"`
	Pass bool   `json:"pass"`
	Note string `json:"note,omitempty"`
}

// ValidationResult aggregates all check results plus the overall
// outcome class. AllPass is true iff every check passed; AnyFail is
// true iff at least one check failed; Partial is true when some passed
// and some failed.
type ValidationResult struct {
	Checks  []CheckResult `json:"checks"`
	AllPass bool          `json:"all_pass"`
	AnyFail bool          `json:"any_fail"`
	Partial bool          `json:"partial"`
}

// Clock injects time for deterministic tests. realClock returns the
// wall clock; tests pass a stub. The clock is named once-per-call and
// not cached across phases so a long upgrade run does not see frozen
// timestamps.
type Clock interface {
	Now() time.Time
}

// Deps is the dependency bundle threaded through every phase entry
// point. It owns the Notifier, the Snapshotter, the Executor, the
// Validator, the Clock, and the Logger. Tests construct one with
// stubs; production constructs one with QmSnapshotter and the typed RPC.
type Deps struct {
	Snap     Snapshotter
	Exec     Executor
	Validate Validator
	Notifier notify.Notifier
	Clock    Clock
	Log      *slog.Logger
}

// Options controls one invocation of a phase. Not every field applies
// to every phase: the per-phase entry points read only the fields that
// matter to them, so a caller that only runs `prepare` does not need
// to populate `AcceptPartial` or `KeepSnapshot`.
type Options struct {
	VMID                string
	Target              string
	StateDir            string
	DeployID            string
	Snapshot            string
	AcceptPartial       bool
	DryRunExecute       bool
	DryRunGC            bool
	UseBootEnvironment  bool
	KeepSnapshot        bool
	OlderThan           time.Duration
	UpgradeTimeout      time.Duration
	PostRollbackTimeout time.Duration
}

// DefaultStateDir is the documented state directory per resolved
// decision 11.6. Sub-paths are <state-dir>/<vmid>/<deploy-id>/.
const DefaultStateDir = "/var/lib/mwan/upgrades"

// DefaultUpgradeTimeout caps the execute phase. Per the design (4.2)
// the testbed measurement is pending; 30 minutes is the documented
// default until that lands.
const DefaultUpgradeTimeout = 30 * time.Minute

// DefaultPostRollbackTimeout caps the QGA-liveness probe loop after
// rollback. Design 4.4 step 6 sets this at 60 seconds.
const DefaultPostRollbackTimeout = 60 * time.Second

// DefaultPostRebootTimeout caps the QGA-liveness probe loop after the
// post-execute reboot. A major-version OPNsense boot (package scripts,
// FRR start, mwan-opnsense start) can take several minutes; 10 minutes
// is the documented safe ceiling.
const DefaultPostRebootTimeout = 10 * time.Minute

// DefaultExecTimeout is the per-RPC Exec timeout passed to the
// mwan-opnsense daemon by the gRPC executor. The outer --upgrade-timeout
// still bounds the whole execute phase; this value bounds one Exec.
const DefaultExecTimeout = 30 * time.Minute

// DefaultGCThreshold is 7 days.
const DefaultGCThreshold = 7 * 24 * time.Hour

// SnapshotPrefix is the prefix used for upgrade snapshots. The upgrade flow
// uses its own prefix so watchdog and upgrade snapshots stay disjoint.
const SnapshotPrefix = "pre-upgrade-26x-"

// KeepPrefix is the rename prefix that protects a snapshot from gc.
const KeepPrefix = "keep-pre-upgrade-26x-"
