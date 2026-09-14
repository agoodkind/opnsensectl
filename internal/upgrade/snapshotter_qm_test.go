package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	stubVMID         = "104"
	stubSnapshotName = "pre-upgrade-26x-1757500000"
)

// stubCommands holds the paths of the argv records the stub commands
// append to.
type stubCommands struct {
	qmRecord         string
	systemdRunRecord string
}

// installStubCommands puts a stub qm, and optionally a stub systemd-run, in a
// temporary directory and makes that directory the whole PATH, so the
// snapshotter runs the stubs and nothing from the host.
func installStubCommands(t *testing.T, withScopeRunner bool) stubCommands {
	t.Helper()
	dir := t.TempDir()
	installStub(t, "testdata/qm_stub.sh", filepath.Join(dir, qmRunner))
	stubs := stubCommands{
		qmRecord:         filepath.Join(dir, "qm.argv"),
		systemdRunRecord: filepath.Join(dir, "systemd-run.argv"),
	}
	if withScopeRunner {
		installStub(t, "testdata/systemd_run_stub.sh", filepath.Join(dir, scopeRunner))
	}
	t.Setenv("PATH", dir)
	t.Setenv("QM_STUB_RECORD", stubs.qmRecord)
	t.Setenv("SYSTEMD_RUN_STUB_RECORD", stubs.systemdRunRecord)
	return stubs
}

func installStub(t *testing.T, source, destination string) {
	t.Helper()
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.WriteFile(destination, body, 0o755); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
}

// recordedArgv returns one entry per invocation recorded in path, or nil when
// the command never ran.
func recordedArgv(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

func newTestSnapshotter() *QmSnapshotter {
	return NewQmSnapshotter(slog.New(slog.DiscardHandler))
}

// TestQmSnapshotterRunsLockHoldingOperationsInScope proves that snapshot,
// rollback, and snapshot delete run qm through a transient systemd scope when
// systemd-run is available.
func TestQmSnapshotterRunsLockHoldingOperationsInScope(t *testing.T) {
	stubs := installStubCommands(t, true)
	snapshotter := newTestSnapshotter()
	ctx := t.Context()

	if err := snapshotter.VMSnapshot(ctx, stubVMID, stubSnapshotName); err != nil {
		t.Fatalf("VMSnapshot: %v", err)
	}
	if err := snapshotter.VMRollback(ctx, stubVMID, stubSnapshotName); err != nil {
		t.Fatalf("VMRollback: %v", err)
	}
	if err := snapshotter.VMDelSnapshot(ctx, stubVMID, stubSnapshotName); err != nil {
		t.Fatalf("VMDelSnapshot: %v", err)
	}

	wantScope := []string{
		"--scope --quiet --collect qm snapshot 104 " + stubSnapshotName,
		"--scope --quiet --collect qm rollback 104 " + stubSnapshotName,
		"--scope --quiet --collect qm delsnapshot 104 " + stubSnapshotName,
	}
	if got := recordedArgv(t, stubs.systemdRunRecord); !reflect.DeepEqual(got, wantScope) {
		t.Errorf("systemd-run argv = %q, want %q", got, wantScope)
	}
	wantQm := []string{
		"snapshot 104 " + stubSnapshotName,
		"rollback 104 " + stubSnapshotName,
		"delsnapshot 104 " + stubSnapshotName,
	}
	if got := recordedArgv(t, stubs.qmRecord); !reflect.DeepEqual(got, wantQm) {
		t.Errorf("qm argv = %q, want %q", got, wantQm)
	}
}

// TestQmSnapshotterRunsQmDirectlyWithoutScopeRunner proves that a host
// without systemd-run still runs the lock-holding operation through qm.
func TestQmSnapshotterRunsQmDirectlyWithoutScopeRunner(t *testing.T) {
	stubs := installStubCommands(t, false)
	snapshotter := newTestSnapshotter()

	if err := snapshotter.VMSnapshot(t.Context(), stubVMID, stubSnapshotName); err != nil {
		t.Fatalf("VMSnapshot: %v", err)
	}

	wantQm := []string{"snapshot 104 " + stubSnapshotName}
	if got := recordedArgv(t, stubs.qmRecord); !reflect.DeepEqual(got, wantQm) {
		t.Errorf("qm argv = %q, want %q", got, wantQm)
	}
	if got := recordedArgv(t, stubs.systemdRunRecord); got != nil {
		t.Errorf("systemd-run ran without being on PATH: %q", got)
	}
}

// TestQmSnapshotterReadsGuestState proves the read operations and start run
// directly through qm and parse what Proxmox prints.
func TestQmSnapshotterReadsGuestState(t *testing.T) {
	stubs := installStubCommands(t, true)
	snapshotter := newTestSnapshotter()
	ctx := t.Context()

	t.Setenv("QM_STUB_STATUS", "running")
	running, err := snapshotter.VMStatus(ctx, stubVMID)
	if err != nil {
		t.Fatalf("VMStatus running: %v", err)
	}
	if !running {
		t.Error("VMStatus = false for a running guest, want true")
	}

	t.Setenv("QM_STUB_STATUS", "stopped")
	running, err = snapshotter.VMStatus(ctx, stubVMID)
	if err != nil {
		t.Fatalf("VMStatus stopped: %v", err)
	}
	if running {
		t.Error("VMStatus = true for a stopped guest, want false")
	}

	if err := snapshotter.VMStart(ctx, stubVMID); err != nil {
		t.Fatalf("VMStart: %v", err)
	}

	listing, err := snapshotter.VMSnapshots(ctx, stubVMID)
	if err != nil {
		t.Fatalf("VMSnapshots: %v", err)
	}
	wantNames := []string{"keep-pre-upgrade-26x-1757000000", stubSnapshotName}
	if got := parseSnapshotNames(listing); !reflect.DeepEqual(got, wantNames) {
		t.Errorf("snapshot names = %q, want %q", got, wantNames)
	}

	wantQm := []string{"status 104", "status 104", "start 104", "listsnapshot 104"}
	if got := recordedArgv(t, stubs.qmRecord); !reflect.DeepEqual(got, wantQm) {
		t.Errorf("qm argv = %q, want %q", got, wantQm)
	}
	if got := recordedArgv(t, stubs.systemdRunRecord); got != nil {
		t.Errorf("read operations ran through systemd-run: %q", got)
	}
}

// TestQmSnapshotterFailureCarriesProxmoxOutput proves a failed lock-holding
// operation returns an error that names the command and carries the reason
// Proxmox printed, and that a failed status read is an error.
func TestQmSnapshotterFailureCarriesProxmoxOutput(t *testing.T) {
	installStubCommands(t, true)
	snapshotter := newTestSnapshotter()
	ctx := t.Context()
	reason := "VM 104 qmp command 'blockdev-snapshot-delete-internal-sync' failed - Snapshot with id 'null' and name '" +
		stubSnapshotName + "' does not exist"

	t.Setenv("QM_STUB_FAIL_VERB", "delsnapshot")
	t.Setenv("QM_STUB_FAIL_MESSAGE", reason)
	err := snapshotter.VMDelSnapshot(ctx, stubVMID, stubSnapshotName)
	if err == nil {
		t.Fatal("VMDelSnapshot succeeded, want an error")
	}
	for _, want := range []string{"qm delsnapshot 104 " + stubSnapshotName, "systemd-run scope qm delsnapshot", reason} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	t.Setenv("QM_STUB_FAIL_VERB", "status")
	t.Setenv("QM_STUB_FAIL_MESSAGE", "Configuration file 'nodes/pve/qemu-server/104.conf' does not exist")
	if _, err := snapshotter.VMStatus(ctx, stubVMID); err == nil {
		t.Error("VMStatus succeeded on a failed qm status, want an error")
	}
}
