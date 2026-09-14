package svc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestDeployManager(t *testing.T) (*DeployManager, string) {
	t.Helper()
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "sbin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := DeployConfig{
		BinaryDir:      binDir,
		StatePath:      filepath.Join(tmp, "deploy.state"),
		PendingPath:    filepath.Join(tmp, "pending-verify"),
		WriteStateFile: atomicWriteFile,
		NowFn:          func() time.Time { return time.Unix(1715000000, 0) },
	}
	dm := NewDeployManager(slog.Default(), cfg)
	return dm, binDir
}

func writeBinary(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseStateFile_RoundTrip(t *testing.T) {
	want := deployState{
		Active:     "abc123",
		Previous:   "def456",
		Version:    "v1.2.3",
		DeployedAt: 1715000000,
		Health:     HealthOK,
	}
	encoded := encodeStateFile(want)
	got := parseStateFile(encoded)
	if got != want {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestParseStateFile_IgnoresBlankAndComments(t *testing.T) {
	input := `# top comment

active:abc

# mid
previous:def
health:ok
`
	got := parseStateFile(input)
	if got.Active != "abc" || got.Previous != "def" || got.Health != HealthOK {
		t.Errorf("blank/comment handling broken: %+v", got)
	}
}

func TestParseStateFile_TolerantToUnknownKeys(t *testing.T) {
	input := "active:abc\nunknown_field:value\nhealth:ok\n"
	got := parseStateFile(input)
	if got.Active != "abc" || got.Health != HealthOK {
		t.Errorf("unknown-key tolerance broken: %+v", got)
	}
}

// stageUpload writes payload into the staged slot, the path the
// StageBinary handler hands to DeployFromPath.
func stageUpload(t *testing.T, binDir string, payload []byte) string {
	t.Helper()
	staged := filepath.Join(binDir, BinaryStaged)
	writeBinary(t, staged, payload)
	return staged
}

func TestDeployFromPath_RejectsEmptyBinary(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	staged := stageUpload(t, binDir, nil)
	_, _, err := dm.DeployFromPath(context.Background(), staged, "abc", "v1")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want empty-binary error, got %v", err)
	}
}

func TestDeployFromPath_RejectsMissingSHA(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	staged := stageUpload(t, binDir, []byte("payload"))
	_, _, err := dm.DeployFromPath(context.Background(), staged, "", "v1")
	if err == nil || !strings.Contains(err.Error(), "sha256_hex required") {
		t.Errorf("want missing-sha error, got %v", err)
	}
}

func TestDeployFromPath_RejectsSHAMismatch(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	staged := stageUpload(t, binDir, []byte("payload"))
	_, _, err := dm.DeployFromPath(context.Background(), staged, "deadbeef", "v1")
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("want sha mismatch error, got %v", err)
	}
}

func TestDeployFromPath_FirstDeployNoCurrent(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	payload := []byte("new-elf")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])
	staged := stageUpload(t, binDir, payload)

	_, stagedSHA, err := dm.DeployFromPath(context.Background(), staged, sumHex, "v1.0")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if stagedSHA != sumHex {
		t.Errorf("stagedSHA=%s want %s", stagedSHA, sumHex)
	}
	currentBytes, readErr := os.ReadFile(filepath.Join(binDir, BinaryCurrent))
	if readErr != nil {
		t.Fatalf("read current: %v", readErr)
	}
	if !bytesEqual(currentBytes, payload) {
		t.Errorf("current does not match payload")
	}
	// previous should NOT exist on first deploy.
	if _, statErr := os.Stat(filepath.Join(binDir, BinaryPrevious)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("previous should be absent on first deploy, statErr=%v", statErr)
	}

	// pending marker dropped
	if _, err := os.Stat(dm.cfg.PendingPath); err != nil {
		t.Errorf("pending marker missing: %v", err)
	}

	// state file: health=pending, active=sumHex
	stateBytes, _ := os.ReadFile(dm.cfg.StatePath)
	state := parseStateFile(string(stateBytes))
	if state.Active != sumHex {
		t.Errorf("state.Active=%s want %s", state.Active, sumHex)
	}
	if state.Health != HealthPending {
		t.Errorf("state.Health=%s want %s", state.Health, HealthPending)
	}
}

func TestDeployFromPath_PreservesPreviousOnSecondDeploy(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	original := []byte("v1-elf")
	originalSum := sha256.Sum256(original)
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), original)

	updated := []byte("v2-elf")
	updatedSum := sha256.Sum256(updated)
	updatedSumHex := hex.EncodeToString(updatedSum[:])
	staged := stageUpload(t, binDir, updated)

	_, _, err := dm.DeployFromPath(context.Background(), staged, updatedSumHex, "v2.0")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// .previous holds the original
	prevBytes, _ := os.ReadFile(filepath.Join(binDir, BinaryPrevious))
	if !bytesEqual(prevBytes, original) {
		t.Errorf(".previous does not match original")
	}
	// .current holds the new
	curBytes, _ := os.ReadFile(filepath.Join(binDir, BinaryCurrent))
	if !bytesEqual(curBytes, updated) {
		t.Errorf(".current does not match updated payload")
	}

	state, _ := dm.readStateLocked()
	if state.Previous != hex.EncodeToString(originalSum[:]) {
		t.Errorf("state.Previous=%s want %s", state.Previous, hex.EncodeToString(originalSum[:]))
	}
}

func TestMarkHealthy_ClearsPendingAndStampsOK(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	payload := []byte("v-elf")
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), payload)
	// Pre-condition: pending marker present, health=pending
	if err := os.WriteFile(dm.cfg.PendingPath, []byte(MarkerFreshDeploy), PendingFileMode); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	currentSum := sha256.Sum256(payload)
	if err := dm.writeState(deployState{
		Active:     hex.EncodeToString(currentSum[:]),
		Previous:   "",
		Version:    "v1.0",
		DeployedAt: 1715000000,
		Health:     HealthPending,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	active, _, _, err := dm.MarkHealthy(context.Background())
	if err != nil {
		t.Fatalf("mark healthy: %v", err)
	}
	if active != hex.EncodeToString(currentSum[:]) {
		t.Errorf("active=%s want %s", active, hex.EncodeToString(currentSum[:]))
	}
	if _, statErr := os.Stat(dm.cfg.PendingPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("pending marker should be gone after MarkHealthy: %v", statErr)
	}
	state, _ := dm.readStateLocked()
	if state.Health != HealthOK {
		t.Errorf("state.Health=%s want %s", state.Health, HealthOK)
	}
}

func TestMarkHealthy_TolerantToMissingState(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	payload := []byte("v-elf")
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), payload)
	// No state file, no pending marker. MarkHealthy should still succeed.
	_, _, _, err := dm.MarkHealthy(context.Background())
	if err != nil {
		t.Fatalf("mark healthy on missing state: %v", err)
	}
	state, _ := dm.readStateLocked()
	if state.Health != HealthOK {
		t.Errorf("state.Health=%s want %s", state.Health, HealthOK)
	}
}

func TestStatus_DerivesFromDiskWhenStateMissing(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	payload := []byte("v-elf")
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), payload)
	prev := []byte("v-prev")
	writeBinary(t, filepath.Join(binDir, BinaryPrevious), prev)

	active, previous, health, _, err := dm.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	wantActive := sha256Hex(payload)
	wantPrev := sha256Hex(prev)
	if active != wantActive {
		t.Errorf("active=%s want %s", active, wantActive)
	}
	if previous != wantPrev {
		t.Errorf("previous=%s want %s", previous, wantPrev)
	}
	if health != HealthPending {
		t.Errorf("health=%s want %s (default when state absent)", health, HealthPending)
	}
}

func TestRevert_RequiresPrevious(t *testing.T) {
	dm, _ := newTestDeployManager(t)
	_, err := dm.Revert(context.Background())
	if err == nil || !strings.Contains(err.Error(), "previous absent") {
		t.Errorf("want previous-absent error, got %v", err)
	}
}

func TestRevert_CopiesPreviousAndArmsReExec(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	current := []byte("v2-elf")
	previous := []byte("v1-elf")
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), current)
	writeBinary(t, filepath.Join(binDir, BinaryPrevious), previous)

	revertedTo, err := dm.Revert(context.Background())
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if revertedTo != sha256Hex(previous) {
		t.Errorf("revertedTo=%s want %s", revertedTo, sha256Hex(previous))
	}
	curBytes, _ := os.ReadFile(filepath.Join(binDir, BinaryCurrent))
	if !bytesEqual(curBytes, previous) {
		t.Errorf("current after revert should match previous")
	}
	if _, statErr := os.Stat(dm.cfg.PendingPath); statErr != nil {
		t.Errorf("pending marker should be present after revert: %v", statErr)
	}
}

func TestAtomicWriteFile_ReplacesContent(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "x")
	if err := atomicWriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := atomicWriteFile(target, []byte("world"), 0o644); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "world" {
		t.Errorf("content=%q want %q", got, "world")
	}
}

func TestFileSHA256_MissingReturnsEmpty(t *testing.T) {
	got, err := fileSHA256(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("err on missing: %v", err)
	}
	if got != "" {
		t.Errorf("want empty sha for missing file, got %q", got)
	}
}

func TestCleanStaleTemps_RemovesOrphansKeepsSlots(t *testing.T) {
	dm, binDir := newTestDeployManager(t)
	// Orphan scratch from an interrupted deploy/upload: must be removed.
	writeBinary(t, filepath.Join(binDir, BinaryStaged), []byte("staged"))
	writeBinary(t, filepath.Join(binDir, BinaryStagedTmp), []byte("staged-tmp"))
	uploadTemp := filepath.Join(binDir, "."+BinaryCurrent+".upload.deadbeef")
	writeBinary(t, uploadTemp, []byte("upload"))
	// Promotable slots and the symlink name: must be kept.
	writeBinary(t, filepath.Join(binDir, BinaryCurrent), []byte("current"))
	writeBinary(t, filepath.Join(binDir, BinaryPrevious), []byte("previous"))

	removed, err := dm.CleanStaleTemps(context.Background())
	if err != nil {
		t.Fatalf("CleanStaleTemps: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed=%d, want 3", removed)
	}
	for _, gone := range []string{BinaryStaged, BinaryStagedTmp} {
		if _, statErr := os.Stat(filepath.Join(binDir, gone)); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s should be removed, statErr=%v", gone, statErr)
		}
	}
	if _, statErr := os.Stat(uploadTemp); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("upload temp should be removed, statErr=%v", statErr)
	}
	for _, kept := range []string{BinaryCurrent, BinaryPrevious} {
		if _, statErr := os.Stat(filepath.Join(binDir, kept)); statErr != nil {
			t.Errorf("%s should be kept, statErr=%v", kept, statErr)
		}
	}
}

func TestCleanStaleTemps_MissingDirNoError(t *testing.T) {
	cfg := DeployConfig{BinaryDir: filepath.Join(t.TempDir(), "does-not-exist")}
	dm := NewDeployManager(slog.Default(), cfg)
	removed, err := dm.CleanStaleTemps(context.Background())
	if err != nil {
		t.Fatalf("want nil err for missing dir, got %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0", removed)
	}
}

// helpers

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// _ ensures sync is referenced if a future test needs it; harmless.
var _ = sync.Mutex{}
