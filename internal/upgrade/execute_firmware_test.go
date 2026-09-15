package upgrade

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// prepareAndExecute runs Prepare then Execute against the fake guest and
// returns the execute state, the deploy directory, and Execute's error.
func prepareAndExecute(t *testing.T, deps Deps, opts Options) (State, string, error) {
	t.Helper()
	prepared, err := Prepare(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	st, execErr := Execute(context.Background(), deps, opts)
	return st, filepath.Join(opts.StateDir, opts.VMID, prepared.DeployID), execErr
}

func readPlanArtefact(t *testing.T, deployDir string) firmwarePlan {
	t.Helper()
	body, err := readJSON(filepath.Join(deployDir, ArtefactFirmwarePlan))
	if err != nil {
		t.Fatalf("read firmware.plan.json: %v", err)
	}
	var plan firmwarePlan
	if err := json.Unmarshal(body, &plan); err != nil {
		t.Fatalf("parse firmware.plan.json: %v", err)
	}
	return plan
}

func TestExecuteDryRunReportsPackageHotfixWithoutChangingGuest(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	opts := newOpts(t, "101")
	opts.DryRunExecute = true

	st, deployDir, err := prepareAndExecute(t, deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
	if len(x.firmware.mutations) != 0 {
		t.Fatalf("dry run changed the guest: %v", x.firmware.mutations)
	}
	if x.firmware.coreVersion != "26.7.3_8" {
		t.Fatalf("core version = %q, dry run must leave 26.7.3_8 installed", x.firmware.coreVersion)
	}
	plan := readPlanArtefact(t, deployDir)
	if plan.Mode != firmwareModeUpdate || !plan.CorePending || plan.CoreAvailable != "26.7.3_11" {
		t.Fatalf("plan = %+v, want a pending core update to 26.7.3_11", plan)
	}
	if plan.BasePending || plan.KernelPending || plan.RebootRequired {
		t.Fatalf("plan = %+v, want no base, kernel, or reboot for a package hotfix", plan)
	}
}

func TestExecuteAppliesPackageHotfixWithoutReboot(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	opts := newOpts(t, "101")

	st, _, err := prepareAndExecute(t, deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
	if !slices.Contains(x.argvs(), guestUpdater+" -p -t opnsense") {
		t.Fatalf("package update did not run: %v", x.argvs())
	}
	if x.firmware.coreVersion != "26.7.3_11" {
		t.Fatalf("core version = %q, want 26.7.3_11", x.firmware.coreVersion)
	}
	if x.firmware.reboots != 0 || slices.Contains(x.firmware.mutations, "opnsense-update -bk") {
		t.Fatalf("package hotfix rebooted or installed sets: %v", x.firmware.mutations)
	}
}

func TestExecuteRebootsWhenBaseAndKernelUpdate(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.coreAvailable = "26.7.4"
	x.firmware.updaterAvailable = "26.7.4"
	opts := newOpts(t, "101")

	st, deployDir, err := prepareAndExecute(t, deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
	if x.firmware.reboots != 1 {
		t.Fatalf("reboots = %d, want 1 after a base and kernel update", x.firmware.reboots)
	}
	if x.firmware.baseVersion != "26.7.4" || x.firmware.kernelVersion != "26.7.4" {
		t.Fatalf("base %q kernel %q, want 26.7.4", x.firmware.baseVersion, x.firmware.kernelVersion)
	}
	if plan := readPlanArtefact(t, deployDir); !plan.RebootRequired {
		t.Fatalf("plan = %+v, want reboot_required", plan)
	}
}

func TestExecuteFailsWhenGuestReturnsOnOldSets(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.coreAvailable = "26.7.4"
	x.firmware.updaterAvailable = "26.7.4"
	x.firmware.bootsOldSets = true
	opts := newOpts(t, "101")

	st, _, err := prepareAndExecute(t, deps, opts)
	if err == nil {
		t.Fatalf("Execute succeeded although the guest came back on base %q", x.firmware.baseVersion)
	}
	if st.Phase != PhaseExecuteFailed {
		t.Fatalf("phase = %q, want execute_failed", st.Phase)
	}
	if !strings.Contains(err.Error(), "base 26.7.3, want 26.7.4") {
		t.Fatalf("error %q does not name the base that did not land", err)
	}
}

func TestExecuteMajorUpgradeStagesReleaseAndReboots(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	opts := newOpts(t, "101")
	opts.Target = "27.1"

	st, deployDir, err := prepareAndExecute(t, deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
	if !slices.Contains(x.argvs(), guestUpdater+" -u -r 27.1") {
		t.Fatalf("major upgrade command did not run: %v", x.argvs())
	}
	if slices.Contains(x.argvs(), guestUpdater+" -p -t opnsense") {
		t.Fatalf("major upgrade ran the package update path: %v", x.argvs())
	}
	if x.firmware.reboots != 1 || x.firmware.coreVersion != "27.1" {
		t.Fatalf("reboots %d core %q, want one reboot onto 27.1", x.firmware.reboots, x.firmware.coreVersion)
	}
	if plan := readPlanArtefact(t, deployDir); plan.Mode != firmwareModeMajor {
		t.Fatalf("plan mode = %q, want major", plan.Mode)
	}
}

func TestExecuteNothingPendingDoesNotReboot(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.coreAvailable = x.firmware.coreVersion
	opts := newOpts(t, "101")

	st, deployDir, err := prepareAndExecute(t, deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
	if x.firmware.reboots != 0 {
		t.Fatalf("reboots = %d, want 0 with nothing pending", x.firmware.reboots)
	}
	plan := readPlanArtefact(t, deployDir)
	if plan.CorePending || plan.BasePending || plan.KernelPending || plan.RebootRequired {
		t.Fatalf("plan = %+v, want nothing pending", plan)
	}
}

func TestExecuteFailsWhenPendingPackageDidNotApply(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.coreStuck = true
	opts := newOpts(t, "101")

	st, _, err := prepareAndExecute(t, deps, opts)
	if err == nil {
		t.Fatalf("Execute succeeded although core stayed at %q", x.firmware.coreVersion)
	}
	if st.Phase != PhaseExecuteFailed {
		t.Fatalf("phase = %q, want execute_failed", st.Phase)
	}
	if !strings.Contains(err.Error(), "core 26.7.3_8, want 26.7.3_11") {
		t.Fatalf("error %q does not name the core version that did not land", err)
	}
}

func TestExecuteAlwaysRebootSettingRebootsAfterPackageChange(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.alwaysReboot = true
	opts := newOpts(t, "101")

	st, _, err := prepareAndExecute(t, deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
	if x.firmware.reboots != 1 {
		t.Fatalf("reboots = %d, want 1 when the always-reboot setting is on", x.firmware.reboots)
	}
}
