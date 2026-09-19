package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/opnsensectl/internal/upgrade"
)

const (
	resetTestVMID     = 104
	resetTestBaseline = "pre-upgrade-26x-1757500000"
)

// qmListingStub stands in for the Proxmox qm command. It records its argv
// and answers listsnapshot with the listing in QM_STUB_LISTING, the shape
// `qm listsnapshot` prints.
const qmListingStub = `#!/bin/sh
printf '%s\n' "$*" >>"${QM_STUB_RECORD}"
if [ "$1" = "listsnapshot" ]; then
    printf '%s' "${QM_STUB_LISTING}"
fi
`

// resetCommandFixture is one `upgrade reset` invocation against a temporary
// config, state directory, and qm stub.
type resetCommandFixture struct {
	stateDir string
	qmRecord string
}

// newResetCommandFixture writes a config naming a temporary state directory,
// records a committed cycle in that directory, and puts a qm stub that lists
// snapshots on PATH alone, so the handler touches nothing on the host.
func newResetCommandFixture(t *testing.T, snapshots []string) resetCommandFixture {
	t.Helper()
	stateDir := t.TempDir()
	writeTempTOML(t, "[opnsense.upgrade]\nvmid = "+strconv.Itoa(resetTestVMID)+
		"\nstate_dir = \""+stateDir+"\"\n")

	vmid := strconv.Itoa(resetTestVMID)
	committed := upgrade.State{
		VMID:         vmid,
		DeployID:     "deploy-committed",
		Target:       "26.7",
		Snapshot:     resetTestBaseline,
		Phase:        upgrade.PhaseCommitted,
		UpdatedAt:    time.Unix(1_757_500_000, 0),
		FailingCheck: nil,
	}
	body, err := json.Marshal(committed)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, vmid), 0o750); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, vmid, "state.json"), body, 0o600); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	var listing strings.Builder
	for _, name := range snapshots {
		listing.WriteString(" `-> " + name + " 2026-09-10 10:26:40     no-description\n")
	}
	listing.WriteString("  `-> current                                             You are here!\n")

	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "qm"), []byte(qmListingStub), 0o700); err != nil {
		t.Fatalf("write qm stub: %v", err)
	}
	qmRecord := filepath.Join(stubDir, "qm.argv")
	t.Setenv("PATH", stubDir)
	t.Setenv("QM_STUB_RECORD", qmRecord)
	t.Setenv("QM_STUB_LISTING", listing.String())
	return resetCommandFixture{stateDir: stateDir, qmRecord: qmRecord}
}

// runResetCapturingStdout runs `upgrade reset` through its command handler
// and returns the exit code and everything it printed to stdout.
func runResetCapturingStdout(t *testing.T) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	exitCode := runOPNsenseUpgradeCmd([]string{string(upgradePhaseReset)})
	os.Stdout = original
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	return exitCode, string(output)
}

// TestUpgradeResetOnCleanCommittedCycleExitsZero covers the committed cycle
// whose commit deleted its baseline. Reset reports nothing to do, exits 0,
// and leaves the committed state file for the next prepare.
func TestUpgradeResetOnCleanCommittedCycleExitsZero(t *testing.T) {
	fixture := newResetCommandFixture(t, nil)

	exitCode, output := runResetCapturingStdout(t)

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stdout:\n%s", exitCode, output)
	}
	if strings.TrimSpace(output) != "nothing to do" {
		t.Fatalf("stdout = %q, want %q", output, "nothing to do")
	}
	statePath := filepath.Join(fixture.stateDir, strconv.Itoa(resetTestVMID), "state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.json should stay after a no-op reset: %v", err)
	}
	record, err := os.ReadFile(fixture.qmRecord)
	if err != nil {
		t.Fatalf("read qm record: %v", err)
	}
	if got := strings.TrimSpace(string(record)); got != "listsnapshot "+strconv.Itoa(resetTestVMID) {
		t.Fatalf("qm argv = %q, want only the snapshot listing", got)
	}
}

// TestUpgradeResetOnCommittedCycleWithKeptBaselineNeedsConfirm covers a commit
// that kept its baseline. Reset still plans that deletion, so without
// reset_confirm it prints the plan and exits 2.
func TestUpgradeResetOnCommittedCycleWithKeptBaselineNeedsConfirm(t *testing.T) {
	newResetCommandFixture(t, []string{resetTestBaseline})

	exitCode, output := runResetCapturingStdout(t)

	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2; stdout:\n%s", exitCode, output)
	}
	for _, want := range []string{"- " + resetTestBaseline, "rollback target: (none)", "reset_confirm = true"} {
		if !strings.Contains(output, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, output)
		}
	}
}
