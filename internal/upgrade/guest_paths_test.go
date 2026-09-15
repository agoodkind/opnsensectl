package upgrade

import (
	"context"
	"path"
	"testing"
)

// upgradeGuestCommands is every command the upgrade flow runs on the router.
// The walk below must run each one, so a new code path that the walk misses
// fails here instead of passing unchecked.
var upgradeGuestCommands = []string{
	guestCat, guestVersion, guestUpdater, guestPkg, guestPluginctl, guestWebGUI,
	guestIfconfig, guestNetstat, guestVtysh, guestBectl, guestSysctl, guestShutdown,
	guestTrue, execRoundTripCommand,
}

// commands returns argv[0] of every call the guest received.
func (e *fakeExec) commands() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.calls))
	for _, c := range e.calls {
		out = append(out, c.Args[0])
	}
	return out
}

// TestUpgradeFlowRunsEveryGuestCommandByAbsolutePath walks prepare, execute,
// validate, rollback, and commit through a base and kernel update and through
// a major upgrade, then checks that every command reached the router by
// absolute path. The router daemon resolves a bare name with its own PATH, and
// a daemon restarted through service(8) has no /usr/local/sbin on it, so a bare
// opnsense-version failed prepare on the testbed router.
func TestUpgradeFlowRunsEveryGuestCommandByAbsolutePath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	seen := map[string]bool{}

	// A package, base, and kernel update with the always-reboot setting on
	// and a boot environment fails validation, rolls back, passes validation,
	// and commits.
	deps, _, snap, update, _ := newDeps(t)
	snap.running = true
	update.byCommand[guestBectl] = GuestExecResult{ExitCode: 0, Stdout: "", Stderr: ""}
	update.firmware.coreAvailable = "26.7.4"
	update.firmware.updaterAvailable = "26.7.4"
	update.firmware.alwaysReboot = true
	pinger := newFakeHostPinger()
	daemon := &fakeDaemon{execResult: hostnameAnswer()}
	deps.Validate = newTestHealthValidator(pinger, daemon)
	opts := newOpts(t, "101")
	opts.UseBootEnvironment = true

	if st, err := Prepare(ctx, deps, opts); err != nil || st.Phase != PhasePrepared {
		t.Fatalf("Prepare = %q, %v; want %q", st.Phase, err, PhasePrepared)
	}
	if st, err := Execute(ctx, deps, opts); err != nil || st.Phase != PhaseExecuted {
		t.Fatalf("Execute = %q, %v; want %q", st.Phase, err, PhaseExecuted)
	}
	pinger.down[pingIPv6Probe] = true
	if st, _, err := Validate(ctx, deps, opts); err != nil || st.Phase != PhaseValidatedFail {
		t.Fatalf("Validate = %q, %v; want %q", st.Phase, err, PhaseValidatedFail)
	}
	if st, err := Rollback(ctx, deps, opts); err != nil || st.Phase != PhaseRolledBack {
		t.Fatalf("Rollback = %q, %v; want %q", st.Phase, err, PhaseRolledBack)
	}
	pinger.down[pingIPv6Probe] = false
	if st, _, err := Validate(ctx, deps, opts); err != nil || st.Phase != PhaseValidatedPass {
		t.Fatalf("Validate = %q, %v; want %q", st.Phase, err, PhaseValidatedPass)
	}
	if st, err := Commit(ctx, deps, opts); err != nil || st.Phase != PhaseCommitted {
		t.Fatalf("Commit = %q, %v; want %q", st.Phase, err, PhaseCommitted)
	}
	if update.firmware.reboots != 1 {
		t.Fatalf("reboots = %d, want 1 so the walk covers the reboot commands", update.firmware.reboots)
	}
	for _, command := range update.commands() {
		seen[command] = true
	}
	if len(daemon.execArgv) == 0 {
		t.Fatalf("validate never ran its exec round trip")
	}
	seen[daemon.execArgv[0]] = true

	// A major upgrade runs the unattended pipeline and commits.
	majorDeps, _, _, major, _ := newDeps(t)
	majorOpts := newOpts(t, "101")
	majorOpts.Target = "27.1"
	if out, err := Run(ctx, majorDeps, majorOpts); err != nil || out.Reached != PhaseValidatedPass {
		t.Fatalf("Run = %q, %v; want %q", out.Reached, err, PhaseValidatedPass)
	}
	if st, err := Commit(ctx, majorDeps, majorOpts); err != nil || st.Phase != PhaseCommitted {
		t.Fatalf("Commit = %q, %v; want %q", st.Phase, err, PhaseCommitted)
	}
	for _, command := range major.commands() {
		seen[command] = true
	}

	for command := range seen {
		if !path.IsAbs(command) {
			t.Errorf("guest command %q is not an absolute path", command)
		}
	}
	for _, command := range upgradeGuestCommands {
		if !seen[command] {
			t.Errorf("the walk never ran %s", command)
		}
	}
}
