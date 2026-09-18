package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// resetFixture wires a Deps backed by fakeSnap (defined in
// upgrade_test.go) together with a fresh state directory and an
// initial snapshot listing. Tests build a fixture, optionally write a
// state.json, then call Reset or ResetExecute.
type resetFixture struct {
	t        *testing.T
	deps     Deps
	snap     *fakeSnap
	stateDir string
	vmid     string
}

func newResetFixture(t *testing.T, listing string) *resetFixture {
	t.Helper()
	deps, _, snap, _, _ := newDeps(t)
	snap.listing = []byte(listing)
	return &resetFixture{
		t:        t,
		deps:     deps,
		snap:     snap,
		stateDir: t.TempDir(),
		vmid:     "102",
	}
}

// writeState records an interrupted cycle, the phase most reset tests
// start from.
func (f *resetFixture) writeState(t *testing.T, snapshot string, deployID string) {
	t.Helper()
	f.writeStateInPhase(t, snapshot, deployID, PhaseExecuteFailed)
}

func (f *resetFixture) writeStateInPhase(t *testing.T, snapshot, deployID string, phase Phase) {
	t.Helper()
	st := State{
		VMID:         f.vmid,
		DeployID:     deployID,
		Target:       "26.7",
		Snapshot:     snapshot,
		Phase:        phase,
		UpdatedAt:    time.Unix(1_700_000_000, 0),
		FailingCheck: nil,
	}
	if err := saveStateCtx(context.Background(), f.stateDir, st, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("saveStateCtx: %v", err)
	}
}

// listingWith builds a qm-listsnapshot-shaped payload from a list of
// snapshot names. The shape is the indented box-drawing form
// parseSnapshotNames already handles.
func listingWith(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(" `-> " + n + " 0 desc\n")
	}
	b.WriteString(" `-> current\n")
	return b.String()
}

func TestResetDryRunPrintsPlan(t *testing.T) {
	t.Parallel()
	baseline := "pre-upgrade-26x-1700000000"
	orphan := "pre-upgrade-26x-1700000999"
	f := newResetFixture(t, listingWith(baseline, orphan))
	f.writeState(t, baseline, "deploy-abc")

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if plan.NothingToDo {
		t.Fatalf("plan.NothingToDo unexpectedly true: %+v", plan)
	}
	if plan.RollbackTarget != baseline {
		t.Fatalf("plan.RollbackTarget = %q want %q", plan.RollbackTarget, baseline)
	}
	if !reflect.DeepEqual(plan.SnapshotsToDelete, []string{orphan}) {
		t.Fatalf("plan.SnapshotsToDelete = %v want [%q]", plan.SnapshotsToDelete, orphan)
	}
	wantPath := filepath.Join(f.stateDir, f.vmid, "state.json")
	if plan.StatePath != wantPath {
		t.Fatalf("plan.StatePath = %q want %q", plan.StatePath, wantPath)
	}
	if plan.DeployID != "deploy-abc" {
		t.Fatalf("plan.DeployID = %q want %q", plan.DeployID, "deploy-abc")
	}

	// Dry run must not mutate anything.
	if len(f.snap.deletes) != 0 {
		t.Fatalf("dry-run Reset called VMDelSnapshot: %v", f.snap.deletes)
	}
	if len(f.snap.rollbacks) != 0 {
		t.Fatalf("dry-run Reset called VMRollback: %v", f.snap.rollbacks)
	}
	if _, err := os.Stat(plan.StatePath); err != nil {
		t.Fatalf("dry-run Reset removed state file: %v", err)
	}
}

func TestResetExecuteDeletesThenRollsBackThenRemovesState(t *testing.T) {
	t.Parallel()
	baseline := "pre-upgrade-26x-1700000000"
	orphan1 := "pre-upgrade-26x-1700000500"
	orphan2 := "pre-upgrade-26x-1700000999"
	f := newResetFixture(t, listingWith(baseline, orphan1, orphan2))
	f.writeState(t, baseline, "deploy-xyz")

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if err := ResetExecute(context.Background(), f.deps, plan); err != nil {
		t.Fatalf("ResetExecute: %v", err)
	}

	gotDeletes := make([]string, 0, len(f.snap.deletes))
	for _, d := range f.snap.deletes {
		gotDeletes = append(gotDeletes, d.Snap)
	}
	wantDeletes := []string{orphan1, orphan2}
	sort.Strings(gotDeletes)
	sort.Strings(wantDeletes)
	if !reflect.DeepEqual(gotDeletes, wantDeletes) {
		t.Fatalf("deletes = %v want %v", gotDeletes, wantDeletes)
	}
	if len(f.snap.rollbacks) != 1 || f.snap.rollbacks[0].Snap != baseline {
		t.Fatalf("rollbacks = %v want one to %q", f.snap.rollbacks, baseline)
	}
	if _, err := os.Stat(plan.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json should have been removed: stat err = %v", err)
	}
}

// errSnap wraps the standard fakeSnap with a hook that returns an
// error on the Nth call to a chosen method. Used to verify ordering
// invariants: a failure in delete must prevent rollback, and a failure
// in rollback must prevent state.json removal.
type errSnap struct {
	*fakeSnap
	failDelete   bool
	failRollback bool
}

func (e *errSnap) VMDelSnapshot(ctx context.Context, vmid, name string) error {
	if e.failDelete {
		return errors.New("synthetic delete failure")
	}
	return e.fakeSnap.VMDelSnapshot(ctx, vmid, name)
}

func (e *errSnap) VMRollback(ctx context.Context, vmid, snap string) error {
	if e.failRollback {
		return errors.New("synthetic rollback failure")
	}
	return e.fakeSnap.VMRollback(ctx, vmid, snap)
}

func TestResetExecuteStopsWhenDeleteFails(t *testing.T) {
	t.Parallel()
	baseline := "pre-upgrade-26x-1700000000"
	orphan := "pre-upgrade-26x-1700000999"
	f := newResetFixture(t, listingWith(baseline, orphan))
	f.writeState(t, baseline, "deploy-xyz")

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}

	wrapped := &errSnap{fakeSnap: f.snap, failDelete: true, failRollback: false}
	deps := f.deps
	deps.Snap = wrapped
	if err := ResetExecute(context.Background(), deps, plan); err == nil {
		t.Fatalf("ResetExecute: want error, got nil")
	}
	if len(f.snap.rollbacks) != 0 {
		t.Fatalf("rollback ran despite delete failure: %v", f.snap.rollbacks)
	}
	if _, err := os.Stat(plan.StatePath); err != nil {
		t.Fatalf("state file removed despite delete failure: %v", err)
	}
}

func TestResetExecuteStopsWhenRollbackFails(t *testing.T) {
	t.Parallel()
	baseline := "pre-upgrade-26x-1700000000"
	f := newResetFixture(t, listingWith(baseline))
	f.writeState(t, baseline, "deploy-xyz")

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	wrapped := &errSnap{fakeSnap: f.snap, failDelete: false, failRollback: true}
	deps := f.deps
	deps.Snap = wrapped
	if err := ResetExecute(context.Background(), deps, plan); err == nil {
		t.Fatalf("ResetExecute: want error, got nil")
	}
	if _, err := os.Stat(plan.StatePath); err != nil {
		t.Fatalf("state file removed despite rollback failure: %v", err)
	}
}

func TestResetNothingToDoOnCleanState(t *testing.T) {
	t.Parallel()
	f := newResetFixture(t, listingWith("some-other-snap"))

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !plan.NothingToDo {
		t.Fatalf("expected NothingToDo on clean state, got %+v", plan)
	}
	if err := ResetExecute(context.Background(), f.deps, plan); err != nil {
		t.Fatalf("ResetExecute no-op: %v", err)
	}
	if len(f.snap.deletes) != 0 || len(f.snap.rollbacks) != 0 {
		t.Fatalf("clean-state reset mutated state: deletes=%v rollbacks=%v",
			f.snap.deletes, f.snap.rollbacks)
	}
}

// TestResetRefusesWhenBaselineMissingOnVM keeps the refusal that still
// matters. An unfinished cycle names a baseline the VM does not hold,
// so a snapshot vanished under an interrupted upgrade and an operator
// reads the state file before anything is touched.
func TestResetRefusesWhenBaselineMissingOnVM(t *testing.T) {
	t.Parallel()
	// state.json claims baseline foo, but the VM has only bar.
	f := newResetFixture(t, listingWith("pre-upgrade-26x-1700000999"))
	f.writeState(t, "pre-upgrade-26x-1700000000", "deploy-xyz")

	_, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err == nil {
		t.Fatalf("Reset: want error on missing baseline, got nil")
	}
	if len(f.snap.deletes) != 0 {
		t.Fatalf("Reset deleted snapshots despite missing baseline: %v", f.snap.deletes)
	}
	if len(f.snap.rollbacks) != 0 {
		t.Fatalf("Reset rolled back despite missing baseline: %v", f.snap.rollbacks)
	}
}

// TestResetOnCommittedCycleDoesNotRollBack covers the destructive case.
// A committed cycle is a validated, finished upgrade. Its baseline is
// released, so reset sweeps it like any other orphan. Rolling the guest
// onto it would undo the upgrade, and Rollback refuses that same move
// from PhaseCommitted.
func TestResetOnCommittedCycleDoesNotRollBack(t *testing.T) {
	t.Parallel()
	baseline := "pre-upgrade-26x-1700000000"
	orphan := "pre-upgrade-26x-1700000999"
	// The operator committed with the snapshot kept, so the VM still
	// holds the baseline.
	f := newResetFixture(t, listingWith(baseline, orphan))
	f.writeStateInPhase(t, baseline, "deploy-committed", PhaseCommitted)

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if plan.RollbackTarget != "" {
		t.Fatalf("plan.RollbackTarget = %q, want empty on a committed cycle", plan.RollbackTarget)
	}
	wantDeletes := []string{baseline, orphan}
	sort.Strings(wantDeletes)
	if !reflect.DeepEqual(plan.SnapshotsToDelete, wantDeletes) {
		t.Fatalf("plan.SnapshotsToDelete = %v, want %v", plan.SnapshotsToDelete, wantDeletes)
	}

	if err := ResetExecute(context.Background(), f.deps, plan); err != nil {
		t.Fatalf("ResetExecute: %v", err)
	}
	if len(f.snap.rollbacks) != 0 {
		t.Fatalf("reset rolled a committed upgrade back: %v", f.snap.rollbacks)
	}
	if _, err := os.Stat(plan.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json should have been removed: stat err = %v", err)
	}
}

// TestResetOnCommittedCycleWithReleasedBaseline covers the state the
// production router reached. Commit deleted the baseline it recorded,
// so its absence is the expected result rather than a reason to refuse.
func TestResetOnCommittedCycleWithReleasedBaseline(t *testing.T) {
	t.Parallel()
	baseline := "pre-upgrade-26x-1700000000"
	// The VM holds no snapshots, because commit deleted this one.
	f := newResetFixture(t, listingWith())
	f.writeStateInPhase(t, baseline, "deploy-committed", PhaseCommitted)

	plan, err := Reset(context.Background(), f.deps, ResetOptions{
		VMID:     f.vmid,
		StateDir: f.stateDir,
		DeployID: "",
	})
	if err != nil {
		t.Fatalf("Reset: want no error when commit released the baseline, got %v", err)
	}
	if plan.RollbackTarget != "" {
		t.Fatalf("plan.RollbackTarget = %q, want empty on a committed cycle", plan.RollbackTarget)
	}
	if len(plan.SnapshotsToDelete) != 0 {
		t.Fatalf("plan.SnapshotsToDelete = %v, want none", plan.SnapshotsToDelete)
	}

	if err := ResetExecute(context.Background(), f.deps, plan); err != nil {
		t.Fatalf("ResetExecute: %v", err)
	}
	if len(f.snap.rollbacks) != 0 {
		t.Fatalf("reset rolled back with no baseline on the vm: %v", f.snap.rollbacks)
	}
	if len(f.snap.deletes) != 0 {
		t.Fatalf("reset deleted snapshots the vm does not hold: %v", f.snap.deletes)
	}
	if _, err := os.Stat(plan.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json should have been removed: stat err = %v", err)
	}
}
