package svc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
)

// fixedClock is a Clock that returns a stable Now value plus a
// configurable offset, used by the GC test to set state-file mtimes
// in the past.
type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func newTestManager(t *testing.T) (*TransferManager, *PathValidator, string) {
	t.Helper()
	stateDir := t.TempDir()
	writeDir := t.TempDir()
	pv := NewPathValidator(slog.Default(), nil, []string{writeDir, stateDir})
	tm, err := NewTransferManager(slog.Default(), pv, stateDir, fixedClock{now: time.Unix(1_700_000_000, 0)})
	if err != nil {
		t.Fatalf("NewTransferManager: %v", err)
	}
	t.Cleanup(func() { _ = pv.Close() })
	return tm, pv, writeDir
}

// writeOneChunk drives a synthetic transfer pipeline: open a fresh
// write state, write a single chunk, and snapshot the hasher state.
// It returns the assigned transfer id, the temp path, the rolling
// committed offset, and the bytes written so the test can later
// attempt to resume.
func writeOneChunk(t *testing.T, tm *TransferManager, target, parent string, payload []byte) (string, string) {
	t.Helper()
	header := &mwanv1.TransferHeader{
		Path:       target,
		Direction:  mwanv1.TransferDirection_TRANSFER_DIRECTION_WRITE,
		FinishStep: mwanv1.FinishStep_FINISH_STEP_REPLACE,
		TotalSize:  int64(len(payload)),
	}
	wt, _, err := tm.openWriteState(context.Background(), header, target, parent)
	if err != nil {
		t.Fatalf("openWriteState: %v", err)
	}
	chunk := &mwanv1.TransferDataChunk{Offset: 0, Data: payload}
	if err := wt.writeChunk(chunk); err != nil {
		t.Fatalf("writeChunk: %v", err)
	}
	if err := wt.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := wt.temp.Close(); err != nil {
		t.Fatalf("temp close: %v", err)
	}
	wt.temp = nil
	return wt.state.TransferID, wt.tempPath
}

func TestTransfer_RollingHashCheckpointResume(t *testing.T) {
	tm, _, writeDir := newTestManager(t)
	target := filepath.Join(writeDir, "rolling.bin")
	payload := []byte("first half-")
	id, _ := writeOneChunk(t, tm, target, writeDir, payload)

	rest := []byte("second half!")
	resumeHeader := &mwanv1.TransferHeader{
		Path:             target,
		Direction:        mwanv1.TransferDirection_TRANSFER_DIRECTION_WRITE,
		FinishStep:       mwanv1.FinishStep_FINISH_STEP_REPLACE,
		ResumeTransferId: id,
		ResumeFromOffset: int64(len(payload)),
		TotalSize:        int64(len(payload) + len(rest)),
	}
	wt, ack, err := tm.openWriteState(context.Background(), resumeHeader, target, writeDir)
	if err != nil {
		t.Fatalf("resume openWriteState: %v", err)
	}
	if !ack.GetResumeAccepted() {
		t.Fatalf("resume_accepted=false; ack=%+v", ack)
	}
	if err := wt.writeChunk(&mwanv1.TransferDataChunk{Offset: int64(len(payload)), Data: rest}); err != nil {
		t.Fatalf("resume writeChunk: %v", err)
	}
	if err := wt.temp.Sync(); err != nil {
		t.Fatalf("temp sync: %v", err)
	}
	gotSum := hex.EncodeToString(wt.hasher.Sum(nil))

	full := append([]byte{}, payload...)
	full = append(full, rest...)
	expected := sha256.Sum256(full)
	if gotSum != hex.EncodeToString(expected[:]) {
		t.Fatalf("rolling sha mismatch: got %s want %s", gotSum, hex.EncodeToString(expected[:]))
	}
	wt.cleanup()
}

func TestTransfer_CancelDeletesState(t *testing.T) {
	tm, _, writeDir := newTestManager(t)
	target := filepath.Join(writeDir, "cancel.bin")
	id, tempPath := writeOneChunk(t, tm, target, writeDir, []byte("partial"))

	statePath := tm.statePath(id)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file should exist before cancel: %v", err)
	}
	was, err := tm.Cancel(context.Background(), id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !was {
		t.Fatalf("Cancel.was_present=false; want true")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file should be gone after cancel: err=%v", err)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("temp file should be gone after cancel: err=%v", err)
	}
}

func TestTransfer_StartupGCSweepsOldStateFiles(t *testing.T) {
	stateDir := t.TempDir()
	writeDir := t.TempDir()
	pv := NewPathValidator(slog.Default(), nil, []string{writeDir})
	t.Cleanup(func() { _ = pv.Close() })

	// Pre-seed two state files: one stale (8 days old) and one fresh.
	old := filepath.Join(stateDir, "stale.json")
	fresh := filepath.Join(stateDir, "fresh.json")
	dummy := &TransferStateFile{TransferID: "x", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	content, _ := json.Marshal(dummy)
	if err := os.WriteFile(old, content, 0o600); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := os.WriteFile(fresh, content, 0o600); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	tm, err := NewTransferManager(slog.Default(), pv, stateDir, nil)
	if err != nil {
		t.Fatalf("NewTransferManager: %v", err)
	}
	swept, err := tm.GC()
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	_ = swept

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale state file should be swept; err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh state file should remain; err=%v", err)
	}
}
