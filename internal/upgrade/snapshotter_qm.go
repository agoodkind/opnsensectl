package upgrade

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Proxmox takes a configuration lock on the guest for the whole of a
// snapshot, a snapshot delete, or a rollback, and releases it only once every
// step succeeded. Proxmox cleans up after its own failures, but only while its
// process lives, so killing the qm process partway leaves the guest locked
// with a half-written snapshot entry. The lock-holding operations below
// therefore run qm in a detached scope with a wait that outlasts Proxmox's
// own limits.

const (
	timeoutQmStatus       = 10 * time.Second
	timeoutQmStart        = 60 * time.Second
	timeoutQmListSnapshot = 10 * time.Second
)

// timeoutQmLockHolding is how long the caller waits on an operation that
// holds a Proxmox guest lock. It bounds the wait, not the operation: a
// detached operation keeps running after the wait expires. Proxmox allows a
// guest filesystem freeze 60 minutes before failing it and unwinding its own
// lock and thaw, so the budget sits above that ceiling.
const timeoutQmLockHolding = 75 * time.Minute

// qmRunner is the Proxmox guest management command.
const qmRunner = "qm"

// scopeRunner starts a transient systemd scope. A scope is its own unit, so
// the process it starts runs outside the control group of whatever started
// it.
const scopeRunner = "systemd-run"

// QmSnapshotter implements [Snapshotter] with the Proxmox qm command on the
// hypervisor that hosts the OPNsense guest.
type QmSnapshotter struct {
	log *slog.Logger
}

// NewQmSnapshotter returns a [QmSnapshotter] that logs through logger, or
// through the default logger when logger is nil.
func NewQmSnapshotter(logger *slog.Logger) *QmSnapshotter {
	if logger == nil {
		logger = slog.Default()
	}
	return &QmSnapshotter{log: logger.With("component", "upgrade-snapshotter")}
}

var _ Snapshotter = (*QmSnapshotter)(nil)

// runQm runs qm with a context-bound timeout.
func (s *QmSnapshotter) runQm(
	ctx context.Context,
	timeout time.Duration,
	args ...string,
) ([]byte, error) {
	s.log.DebugContext(ctx, "upgrade: runQm", "args", args, "timeout", timeout)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, qmRunner, args...).CombinedOutput()
	if err != nil {
		s.log.WarnContext(ctx, "upgrade: qm failed",
			"args", args, "err", err,
			"output", strings.TrimSpace(string(out)))
		return out, fmt.Errorf("qm %s: %w", args[0], err)
	}
	return out, nil
}

// runQmDetached runs qm inside a transient systemd scope, so that stopping
// the calling service cannot interrupt it. Stopping a systemd service signals
// its whole control group, and the scope takes qm out of that group, so the
// Proxmox worker finishes or fails on its own terms.
//
// Hosts without systemd-run fall back to a direct call. Only the Proxmox
// hypervisors run this code and they all run systemd, so the fallback keeps
// unit tests and non-systemd hosts working rather than describing a
// supported deployment.
func (s *QmSnapshotter) runQmDetached(ctx context.Context, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(scopeRunner); err != nil {
		s.log.WarnContext(ctx,
			"upgrade: systemd-run not found; running qm in this control group",
			"args", args, "err", err)
		return s.runQm(ctx, timeoutQmLockHolding, args...)
	}
	s.log.DebugContext(ctx, "upgrade: runQmDetached",
		"args", args, "wait", timeoutQmLockHolding)
	cctx, cancel := context.WithTimeout(ctx, timeoutQmLockHolding)
	defer cancel()
	scopeArgs := append(
		[]string{"--scope", "--quiet", "--collect", qmRunner}, args...,
	)
	out, err := exec.CommandContext(cctx, scopeRunner, scopeArgs...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("systemd-run scope qm %s: %w", args[0], err)
	}
	return out, nil
}

// VMStatus reports whether the VM with the given vmid is currently running
// according to `qm status`.
func (s *QmSnapshotter) VMStatus(ctx context.Context, vmid string) (bool, error) {
	out, err := s.runQm(ctx, timeoutQmStatus, "status", vmid)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "running"), nil
}

// VMStart starts the VM with the given vmid via `qm start`.
func (s *QmSnapshotter) VMStart(ctx context.Context, vmid string) error {
	_, err := s.runQm(ctx, timeoutQmStart, "start", vmid)
	return err
}

// VMSnapshots returns the raw output of `qm listsnapshot` for the given vmid.
func (s *QmSnapshotter) VMSnapshots(ctx context.Context, vmid string) ([]byte, error) {
	return s.runQm(ctx, timeoutQmListSnapshot, "listsnapshot", vmid)
}

// VMRollback rolls the VM back to the named snapshot via `qm rollback`.
func (s *QmSnapshotter) VMRollback(ctx context.Context, vmid, snap string) error {
	out, err := s.runQmDetached(ctx, "rollback", vmid, snap)
	if err != nil {
		s.log.ErrorContext(ctx, "qm rollback failed",
			"vmid", vmid, "snapshot", snap, "err", err,
			"output", strings.TrimSpace(string(out)))
		return fmt.Errorf(
			"qm rollback %s %s: %w: %s",
			vmid, snap, err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}

// VMSnapshot creates a new snapshot named snapName on the given VM via
// `qm snapshot`.
func (s *QmSnapshotter) VMSnapshot(ctx context.Context, vmid, snapName string) error {
	out, err := s.runQmDetached(ctx, "snapshot", vmid, snapName)
	if err != nil {
		s.log.ErrorContext(ctx, "qm snapshot failed",
			"vmid", vmid, "snapshot", snapName, "err", err,
			"output", strings.TrimSpace(string(out)))
		return fmt.Errorf(
			"qm snapshot %s %s: %w: %s",
			vmid, snapName, err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}

// VMDelSnapshot deletes the snapshot named snapName via `qm delsnapshot`.
// A failure carries the command output in the error, because Proxmox
// reports why the delete failed there rather than in the exit status.
func (s *QmSnapshotter) VMDelSnapshot(ctx context.Context, vmid, snapName string) error {
	out, err := s.runQmDetached(ctx, "delsnapshot", vmid, snapName)
	if err != nil {
		s.log.ErrorContext(ctx, "qm delsnapshot failed",
			"vmid", vmid, "snapshot", snapName, "err", err,
			"output", strings.TrimSpace(string(out)))
		return fmt.Errorf(
			"qm delsnapshot %s %s: %w: %s",
			vmid, snapName, err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}
