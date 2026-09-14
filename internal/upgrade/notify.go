package upgrade

import (
	"context"
	"log/slog"

	"goodkind.io/opnsensectl/internal/notify"
)

// Notify kinds carry the kebab-case `opnsense-upgrade-` prefix per
// resolved decision 11.7. Each phase has its own kind so notify
// state-change suppression treats them as independent alerts.
const (
	// KindPrepare is the notify kind for prepare-phase events.
	KindPrepare = "opnsense-upgrade-prepare"
	// KindExecute is the notify kind for execute-phase events.
	KindExecute = "opnsense-upgrade-execute"
	// KindValidate is the notify kind for validate-phase events.
	KindValidate = "opnsense-upgrade-validate"
	// KindRollback is the notify kind for rollback-phase events.
	KindRollback = "opnsense-upgrade-rollback"
	// KindCommit is the notify kind for commit-phase events.
	KindCommit = "opnsense-upgrade-commit"
	// KindRollbackFailed is the notify kind for the loud-alert state
	// where rollback itself did not restore a healthy guest.
	KindRollbackFailed = "opnsense-upgrade-rollback-failed"
	// KindRunComplete is the notify kind for the terminal run-pipeline event.
	KindRunComplete = "opnsense-upgrade-run-complete"
)

// emit sends a notify event with the given kind at the given level.
// The vmid is the alert key so two concurrent upgrade runs against
// different vmids do not collide. fields carries arbitrary structured
// metadata for the email body.
func emit(
	ctx context.Context,
	n notify.Notifier,
	level slog.Level,
	kind string,
	vmid string,
	msg string,
	fields ...slog.Attr,
) {
	if n == nil {
		return
	}
	n.Notify(ctx, notify.Event{
		Level:   level,
		Kind:    kind,
		Key:     vmid,
		Message: msg,
		Fields:  fields,
	})
}

// resolve clears an active alert. Used at commit time so a previously
// failed validate or rollback transitions back to a quiet state in
// notify and the operator inbox does not accumulate stale entries.
func resolve(ctx context.Context, n notify.Notifier, kind, vmid, msg string) {
	if n == nil {
		return
	}
	n.Resolve(ctx, kind, vmid, msg)
}
