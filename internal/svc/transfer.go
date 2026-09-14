package svc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	// TransferStateDir is the daemon-side directory holding per-transfer
	// JSON state files. It must sit beneath the write allowlist.
	TransferStateDir = "/var/lib/mwan/transfers"

	// TransferBackupSubdir is the per-target-directory subdirectory
	// where SNAPSHOT_THEN_REPLACE places the prior file content.
	TransferBackupSubdir = "backup"

	// TransferStagedSuffix is appended to the target path for
	// FINISH_STEP_STAGE transfers.
	TransferStagedSuffix = ".staged"

	// TransferGCAge is the retention window after which a stale
	// state file is swept on startup or via the transfer-gc op.
	TransferGCAge = 7 * 24 * time.Hour

	// TransferStateCheckpointInterval is the byte interval at which the
	// on-disk transfer state file is rewritten so a crash mid-transfer
	// leaves committed_offset roughly in sync with the temp file.
	TransferStateCheckpointInterval = 16 << 20

	// TransferChunkSize is the application-layer chunk size used by
	// READ-direction transfers.
	TransferChunkSize = 16 << 10

	// TransferIDBytes is the byte length of a transfer id before hex
	// encoding. 16 bytes yields a 128-bit random id.
	TransferIDBytes = 16
)

// TransferStateFile is the JSON representation of an in-progress
// transfer. The file lives at TransferStateDir/<id>.json and is
// rewritten atomically on every checkpoint.
type TransferStateFile struct {
	TransferID      string    `json:"transfer_id"`
	Path            string    `json:"path"`
	Direction       string    `json:"direction"`
	FinishStep      string    `json:"finish_step"`
	Label           string    `json:"label"`
	TotalSize       int64     `json:"total_size"`
	CommittedOffset int64     `json:"committed_offset"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	TempPath        string    `json:"temp_path"`
}

// TransferManager owns the per-transfer state, the rolling hashers,
// and the staged temp files. One TransferManager is shared across
// all gRPC handlers.
type TransferManager struct {
	mu        sync.Mutex
	log       *slog.Logger
	validator *PathValidator
	stateDir  string
	clock     Clock
}

// NewTransferManager builds a TransferManager, ensures stateDir exists,
// and runs a startup garbage-collection sweep of stale state files.
func NewTransferManager(log *slog.Logger, validator *PathValidator, stateDir string, clock Clock) (*TransferManager, error) {
	if log == nil {
		log = slog.Default()
	}
	if validator == nil {
		return nil, errors.New("transfer: PathValidator required")
	}
	if stateDir == "" {
		stateDir = TransferStateDir
	}
	if clock == nil {
		clock = realClock{}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("transfer: mkdir %s: %w", stateDir, err)
	}
	tm := &TransferManager{
		mu:        sync.Mutex{},
		log:       log,
		validator: validator,
		stateDir:  stateDir,
		clock:     clock,
	}
	if swept, err := tm.GC(); err != nil {
		log.Warn("transfer: startup gc failed", "err", err)
	} else if swept > 0 {
		log.Info("transfer: startup gc complete", "swept", swept)
	}
	return tm, nil
}

// GC sweeps state files older than TransferGCAge. Returns the number
// of files removed.
func (m *TransferManager) GC() (int, error) {
	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("transfer: read state dir: %w", err)
	}
	cutoff := m.clock.Now().Add(-TransferGCAge)
	swept := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(m.stateDir, entry.Name())
		if removeErr := os.Remove(path); removeErr != nil {
			m.log.Warn("transfer: gc remove failed", "path", path, "err", removeErr)
			continue
		}
		swept++
	}
	return swept, nil
}

func (m *TransferManager) statePath(id string) string {
	return filepath.Join(m.stateDir, id+".json")
}

// loadStateContext reads and parses the transfer state file for id.
// The caller passes the gRPC handler context so the slog event chains
// to the same request span.
func (m *TransferManager) loadStateContext(ctx context.Context, id string) (*TransferStateFile, error) {
	content, err := os.ReadFile(m.statePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.log.WarnContext(ctx, "opnsensesvc: transfer load state not found",
				"transfer_id", id, "err", err)
			return nil, fmt.Errorf("transfer: read state %s: %w", id, err)
		}
		return nil, logWrappedErrorContext(ctx, m.log,
			"opnsensesvc: transfer load state",
			"transfer: read state "+id, err, slog.String("transfer_id", id))
	}
	var st TransferStateFile
	if unmarshalErr := json.Unmarshal(content, &st); unmarshalErr != nil {
		return nil, logWrappedErrorContext(ctx, m.log,
			"opnsensesvc: transfer parse state",
			"transfer: parse state "+id, unmarshalErr,
			slog.String("transfer_id", id))
	}
	return &st, nil
}

func (m *TransferManager) saveStateContext(ctx context.Context, st *TransferStateFile) error {
	st.UpdatedAt = m.clock.Now()
	content, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return logWrappedErrorContext(ctx, m.log,
			"opnsensesvc: transfer marshal state",
			"transfer: marshal state "+st.TransferID, err,
			slog.String("transfer_id", st.TransferID))
	}
	m.log.DebugContext(ctx, "opnsensesvc: TransferManager.saveStateContext",
		"transfer_id", st.TransferID, "bytes", len(content))
	return AtomicWriteFile(ctx, m.statePath(st.TransferID), content, 0o600)
}

// deleteStateContext removes the state file for id; ErrNotExist is not
// an error.
func (m *TransferManager) deleteStateContext(ctx context.Context, id string) error {
	m.log.DebugContext(ctx, "opnsensesvc: TransferManager.deleteStateContext",
		"transfer_id", id)
	err := os.Remove(m.statePath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return logWrappedErrorContext(ctx, m.log,
			"opnsensesvc: transfer delete state",
			"transfer: remove state "+id, err,
			slog.String("transfer_id", id))
	}
	return nil
}

// Status returns the current state for transfer_id, or exists=false
// when no record is found.
func (m *TransferManager) Status(ctx context.Context, id string) (*mwanv1.StatusResponse, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "transfer: id required")
	}
	st, err := m.loadStateContext(ctx, id)
	if errors.Is(err, os.ErrNotExist) {
		return &mwanv1.StatusResponse{
			TransferId: id, Exists: false,
			CommittedOffset: 0, TotalBytes: 0, Path: "",
		}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "transfer: load state: %v", err)
	}
	return &mwanv1.StatusResponse{
		TransferId:      st.TransferID,
		Exists:          true,
		CommittedOffset: st.CommittedOffset,
		TotalBytes:      st.TotalSize,
		Path:            st.Path,
	}, nil
}

// Cancel deletes the transfer state and any staged temp file.
func (m *TransferManager) Cancel(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, status.Error(codes.InvalidArgument, "transfer: id required")
	}
	st, err := m.loadStateContext(ctx, id)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if st.TempPath != "" {
		if removeErr := os.Remove(st.TempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			m.log.WarnContext(ctx, "transfer: cancel remove temp failed", "path", st.TempPath, "err", removeErr)
		}
	}
	if delErr := m.deleteStateContext(ctx, id); delErr != nil {
		return true, delErr
	}
	return true, nil
}

// ReadNewID returns a 128-bit random transfer id encoded as 32 hex
// bytes.
func ReadNewID(ctx context.Context) (string, error) {
	buf := make([]byte, TransferIDBytes)
	if _, err := rand.Read(buf); err != nil {
		slog.ErrorContext(ctx, "transfer: rand", "err", err)
		return "", fmt.Errorf("transfer: rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Upload implements the bidi resumable transfer stream. The first
// message must be a TransferHeader; subsequent messages depend on the
// direction. READ streams data from server to client and ends with
// a Terminal. WRITE streams data from client to server and ends with
// a Final, after which the server emits a Terminal.
func (s *Server) Upload(stream mwanv1.TransferService_UploadServer) error {
	if s.transfer == nil {
		return status.Error(codes.FailedPrecondition, "transfer: manager not configured")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "transfer: recv header: %v", err)
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "transfer: first message must be header")
	}
	switch header.GetDirection() {
	case mwanv1.TransferDirection_TRANSFER_DIRECTION_READ:
		return s.transfer.serveRead(stream, header)
	case mwanv1.TransferDirection_TRANSFER_DIRECTION_WRITE:
		return s.transfer.serveWrite(stream, header)
	case mwanv1.TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED:
		return status.Error(codes.InvalidArgument, "transfer: direction unspecified")
	default:
		return status.Error(codes.InvalidArgument, "transfer: unknown direction")
	}
}

// Status is the unary lookup for an in-progress transfer.
func (s *Server) Status(ctx context.Context, req *mwanv1.StatusRequest) (*mwanv1.StatusResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	if s.transfer == nil {
		return nil, status.Error(codes.FailedPrecondition, "transfer: manager not configured")
	}
	return s.transfer.Status(ctx, req.GetTransferId())
}

// Cancel deletes the transfer's daemon-side state.
func (s *Server) Cancel(ctx context.Context, req *mwanv1.CancelRequest) (*mwanv1.CancelResponse, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	if s.transfer == nil {
		return nil, status.Error(codes.FailedPrecondition, "transfer: manager not configured")
	}
	was, err := s.transfer.Cancel(ctx, req.GetTransferId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "transfer: cancel: %v", err)
	}
	return &mwanv1.CancelResponse{WasPresent: was}, nil
}

func (m *TransferManager) serveRead(stream mwanv1.TransferService_UploadServer, header *mwanv1.TransferHeader) error {
	ctx := stream.Context()
	clean, resolveErr := m.validator.ResolveRead(header.GetPath())
	if resolveErr != nil {
		return status.Errorf(codes.PermissionDenied, "transfer: %v", resolveErr)
	}
	file, openErr := m.validator.OpenForRead(clean)
	if openErr != nil {
		return status.Errorf(codes.NotFound, "transfer: open %s: %v", clean, openErr)
	}
	defer func() { _ = file.Close() }()

	info, statErr := file.Stat()
	if statErr != nil {
		return status.Errorf(codes.Internal, "transfer: stat %s: %v", clean, statErr)
	}
	total := info.Size()
	id, idErr := ReadNewID(ctx)
	if idErr != nil {
		return status.Errorf(codes.Internal, "transfer: id: %v", idErr)
	}
	if ackErr := stream.Send(&mwanv1.UploadResponse{
		Body: &mwanv1.UploadResponse_Ack{
			Ack: &mwanv1.TransferAck{
				TransferId:      id,
				TotalBytes:      total,
				ResumeAccepted:  false,
				CommittedOffset: 0,
			},
		},
	}); ackErr != nil {
		m.log.ErrorContext(ctx, "opnsensesvc: serveRead send ack", "err", ackErr)
		return fmt.Errorf("transfer: send ack: %w", ackErr)
	}

	hasher := sha256.New()
	offset := int64(0)
	buf := make([]byte, TransferChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return status.Errorf(codes.Canceled, "transfer: ctx: %v", err)
		}
		n, readErr := file.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, hashWriteErr := hasher.Write(chunk); hashWriteErr != nil {
				return status.Errorf(codes.Internal, "transfer: hash: %v", hashWriteErr)
			}
			if sendErr := stream.Send(&mwanv1.UploadResponse{
				Body: &mwanv1.UploadResponse_Data{
					Data: &mwanv1.TransferDataChunk{Offset: offset, Data: append([]byte(nil), chunk...)},
				},
			}); sendErr != nil {
				m.log.ErrorContext(ctx, "opnsensesvc: serveRead send data", "err", sendErr)
				return fmt.Errorf("transfer: send data: %w", sendErr)
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "transfer: read: %v", readErr)
		}
	}

	finalSum := hex.EncodeToString(hasher.Sum(nil))
	m.log.InfoContext(ctx, "opnsensesvc: Upload READ audit",
		"transfer_id", id, "path", clean, "bytes", offset, "sha256", finalSum)
	if err := stream.Send(&mwanv1.UploadResponse{
		Body: &mwanv1.UploadResponse_Terminal{
			Terminal: &mwanv1.TransferTerminal{
				Sha256Hex:     finalSum,
				TotalBytes:    offset,
				BackupPath:    "",
				StatusCode:    int32(codes.OK),
				StatusMessage: "ok",
				StagedPath:    "",
			},
		},
	}); err != nil {
		m.log.ErrorContext(ctx, "opnsensesvc: serveRead send terminal", "err", err)
		return fmt.Errorf("transfer: send terminal: %w", err)
	}
	return nil
}

func (m *TransferManager) serveWrite(stream mwanv1.TransferService_UploadServer, header *mwanv1.TransferHeader) error {
	ctx := stream.Context()
	clean, parent, resolveErr := m.validator.ResolveWrite(header.GetPath())
	if resolveErr != nil {
		return status.Errorf(codes.PermissionDenied, "transfer: %v", resolveErr)
	}

	writeState, ackResp, openErr := m.openWriteState(ctx, header, clean, parent)
	if openErr != nil {
		return openErr
	}
	if sendErr := stream.Send(&mwanv1.UploadResponse{Body: &mwanv1.UploadResponse_Ack{Ack: ackResp}}); sendErr != nil {
		writeState.cleanup()
		m.log.ErrorContext(ctx, "opnsensesvc: serveWrite send ack", "err", sendErr)
		return fmt.Errorf("transfer: send ack: %w", sendErr)
	}

	for {
		if err := ctx.Err(); err != nil {
			writeState.cleanup()
			return status.Errorf(codes.Canceled, "transfer: ctx: %v", err)
		}
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			_ = writeState.persist()
			return status.Errorf(codes.Canceled, "transfer: recv: %v", recvErr)
		}
		switch body := msg.GetBody().(type) {
		case *mwanv1.UploadRequest_Data:
			if writeErr := writeState.writeChunk(body.Data); writeErr != nil {
				writeState.cleanup()
				return status.Errorf(codes.Internal, "transfer: write: %v", writeErr)
			}
			// Application-level stop-and-wait pacing. The client
			// blocks on this ack before sending the next chunk so
			// the in-flight byte count never exceeds one chunk.
			if ackErr := stream.Send(&mwanv1.UploadResponse{
				Body: &mwanv1.UploadResponse_DataAck{
					DataAck: &mwanv1.TransferDataAck{CommittedOffset: writeState.state.CommittedOffset},
				},
			}); ackErr != nil {
				writeState.cleanup()
				m.log.ErrorContext(ctx, "opnsensesvc: serveWrite send data ack", "err", ackErr)
				return fmt.Errorf("transfer: send data ack: %w", ackErr)
			}
		case *mwanv1.UploadRequest_Final:
			return m.finalizeWrite(stream, writeState, body.Final, header)
		case *mwanv1.UploadRequest_Cancel:
			writeState.cleanup()
			return status.Error(codes.Canceled, "transfer: client cancel")
		default:
			writeState.cleanup()
			return status.Error(codes.InvalidArgument, "transfer: unexpected body")
		}
	}
}

type writeTransfer struct {
	mgr       *TransferManager
	state     *TransferStateFile
	target    string
	parent    string
	tempPath  string
	temp      *os.File
	hasher    hash.Hash
	lastChkpt int64
}

func (w *writeTransfer) writeChunk(chunk *mwanv1.TransferDataChunk) error {
	ctx := context.Background()
	if chunk.GetOffset() != w.state.CommittedOffset {
		err := fmt.Errorf("offset %d != committed %d", chunk.GetOffset(), w.state.CommittedOffset)
		w.mgr.log.ErrorContext(ctx, "opnsensesvc: writeChunk offset mismatch",
			"err", err,
			"chunk_offset", chunk.GetOffset(), "committed", w.state.CommittedOffset)
		return err
	}
	n, err := w.temp.Write(chunk.GetData())
	if err != nil {
		w.mgr.log.ErrorContext(ctx, "opnsensesvc: writeChunk temp write", "err", err)
		return fmt.Errorf("transfer: temp write: %w", err)
	}
	if _, hashErr := w.hasher.Write(chunk.GetData()[:n]); hashErr != nil {
		w.mgr.log.ErrorContext(ctx, "opnsensesvc: writeChunk hash write", "err", hashErr)
		return fmt.Errorf("transfer: hash write: %w", hashErr)
	}
	w.state.CommittedOffset += int64(n)
	// Persist committed_offset periodically so a crash mid-transfer
	// leaves the temp file and the state file approximately in sync.
	// Hash state is not stored on disk; on resume the server re-hashes
	// the temp file from byte 0 to committed_offset.
	if w.state.CommittedOffset-w.lastChkpt >= TransferStateCheckpointInterval {
		if err := w.persist(); err != nil {
			return err
		}
		w.lastChkpt = w.state.CommittedOffset
	}
	return nil
}

func (w *writeTransfer) persist() error {
	ctx := context.Background()
	if w.temp != nil {
		if err := w.temp.Sync(); err != nil {
			w.mgr.log.ErrorContext(ctx, "opnsensesvc: persist temp sync", "err", err)
			return fmt.Errorf("transfer: temp sync: %w", err)
		}
	}
	w.mgr.log.DebugContext(ctx, "opnsensesvc: writeTransfer.persist",
		"transfer_id", w.state.TransferID,
		"committed_offset", w.state.CommittedOffset)
	return w.mgr.saveStateContext(ctx, w.state)
}

func (w *writeTransfer) cleanup() {
	ctx := context.Background()
	w.mgr.log.DebugContext(ctx, "opnsensesvc: writeTransfer.cleanup",
		"transfer_id", w.state.TransferID, "temp_path", w.tempPath)
	if w.temp != nil {
		_ = w.temp.Close()
	}
	if w.tempPath != "" {
		if err := os.Remove(w.tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			w.mgr.log.Warn("transfer: cleanup temp failed", "path", w.tempPath, "err", err)
		}
	}
	if err := w.mgr.deleteStateContext(context.Background(), w.state.TransferID); err != nil {
		w.mgr.log.Warn("transfer: cleanup state failed", "id", w.state.TransferID, "err", err)
	}
}

func (m *TransferManager) openWriteState(ctx context.Context, header *mwanv1.TransferHeader, target, parent string) (*writeTransfer, *mwanv1.TransferAck, error) {
	m.log.DebugContext(ctx, "opnsensesvc: openWriteState", "target", target, "parent", parent)
	if header.GetResumeTransferId() != "" {
		return m.resumeWriteState(ctx, header, target, parent)
	}
	id, err := ReadNewID(ctx)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "transfer: id: %v", err)
	}
	tempPath := filepath.Join(parent, "."+filepath.Base(target)+".upload."+id)
	file, openErr := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if openErr != nil {
		return nil, nil, status.Errorf(codes.Internal, "transfer: create temp: %v", openErr)
	}
	now := m.clock.Now()
	state := &TransferStateFile{
		TransferID:      id,
		Path:            target,
		Direction:       "write",
		FinishStep:      header.GetFinishStep().String(),
		Label:           header.GetLabel(),
		TotalSize:       header.GetTotalSize(),
		CommittedOffset: 0,
		CreatedAt:       now,
		UpdatedAt:       now,
		TempPath:        tempPath,
	}
	wt := &writeTransfer{
		mgr:       m,
		state:     state,
		target:    target,
		parent:    parent,
		tempPath:  tempPath,
		temp:      file,
		hasher:    sha256.New(),
		lastChkpt: 0,
	}
	if saveErr := m.saveStateContext(ctx, state); saveErr != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return nil, nil, status.Errorf(codes.Internal, "transfer: save state: %v", saveErr)
	}
	ack := &mwanv1.TransferAck{
		TransferId:      id,
		TotalBytes:      header.GetTotalSize(),
		ResumeAccepted:  false,
		CommittedOffset: 0,
	}
	return wt, ack, nil
}

func (m *TransferManager) resumeWriteState(ctx context.Context, header *mwanv1.TransferHeader, target, parent string) (*writeTransfer, *mwanv1.TransferAck, error) {
	id := header.GetResumeTransferId()
	state, err := m.loadStateContext(ctx, id)
	if err != nil {
		return nil, nil, status.Errorf(codes.NotFound, "transfer: resume %s: %v", id, err)
	}
	if state.Path != target {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "transfer: resume path mismatch")
	}
	// The server is authoritative for committed_offset. The client's
	// hint in header.resume_from_offset is accepted only as a sanity
	// check; on mismatch we trust our own committed_offset and let
	// the client adjust on the returned TransferAck.
	file, openErr := os.OpenFile(state.TempPath, os.O_RDWR, 0o600)
	if openErr != nil {
		return nil, nil, status.Errorf(codes.Internal, "transfer: reopen temp: %v", openErr)
	}
	if truncErr := file.Truncate(state.CommittedOffset); truncErr != nil {
		_ = file.Close()
		return nil, nil, status.Errorf(codes.Internal, "transfer: truncate to committed: %v", truncErr)
	}
	hasher := sha256.New()
	if state.CommittedOffset > 0 {
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			_ = file.Close()
			return nil, nil, status.Errorf(codes.Internal, "transfer: seek to start: %v", seekErr)
		}
		if _, copyErr := io.CopyN(hasher, file, state.CommittedOffset); copyErr != nil {
			_ = file.Close()
			return nil, nil, status.Errorf(codes.Internal, "transfer: replay temp into hasher: %v", copyErr)
		}
	}
	if _, seekErr := file.Seek(state.CommittedOffset, io.SeekStart); seekErr != nil {
		_ = file.Close()
		return nil, nil, status.Errorf(codes.Internal, "transfer: seek: %v", seekErr)
	}
	wt := &writeTransfer{
		mgr:       m,
		state:     state,
		target:    target,
		parent:    parent,
		tempPath:  state.TempPath,
		temp:      file,
		hasher:    hasher,
		lastChkpt: state.CommittedOffset,
	}
	ack := &mwanv1.TransferAck{
		TransferId:      state.TransferID,
		TotalBytes:      state.TotalSize,
		ResumeAccepted:  true,
		CommittedOffset: state.CommittedOffset,
	}
	return wt, ack, nil
}

func (m *TransferManager) finalizeWrite(stream mwanv1.TransferService_UploadServer, w *writeTransfer, final *mwanv1.TransferFinal, header *mwanv1.TransferHeader) error {
	ctx := stream.Context()
	got := hex.EncodeToString(w.hasher.Sum(nil))
	if !strings.EqualFold(got, final.GetSha256Hex()) {
		w.cleanup()
		return status.Errorf(codes.DataLoss, "transfer: sha256 mismatch: got %s want %s", got, final.GetSha256Hex())
	}
	if err := w.temp.Sync(); err != nil {
		w.cleanup()
		return status.Errorf(codes.Internal, "transfer: sync: %v", err)
	}
	if err := w.temp.Close(); err != nil {
		w.cleanup()
		return status.Errorf(codes.Internal, "transfer: close: %v", err)
	}
	w.temp = nil

	var backupPath, stagedPath string
	var finalErr error
	switch header.GetFinishStep() {
	case mwanv1.FinishStep_FINISH_STEP_REPLACE:
		finalErr = AtomicRenameFile(ctx, w.tempPath, w.target)
	case mwanv1.FinishStep_FINISH_STEP_SNAPSHOT_THEN_REPLACE:
		backupPath, finalErr = m.snapshotThenReplace(ctx, w.target, w.parent, header.GetLabel(), w.tempPath)
	case mwanv1.FinishStep_FINISH_STEP_STAGE:
		stagedPath = w.target + TransferStagedSuffix
		finalErr = AtomicRenameFile(ctx, w.tempPath, stagedPath)
	case mwanv1.FinishStep_FINISH_STEP_UNSPECIFIED:
		finalErr = errors.New("transfer: finish_step unspecified")
	default:
		finalErr = errors.New("transfer: finish_step unknown")
	}
	if finalErr != nil {
		w.cleanup()
		return status.Errorf(codes.Internal, "transfer: finalize: %v", finalErr)
	}
	if delErr := m.deleteStateContext(context.Background(), w.state.TransferID); delErr != nil {
		m.log.Warn("transfer: delete state after finalize", "id", w.state.TransferID, "err", delErr)
	}
	m.log.InfoContext(ctx, "opnsensesvc: Upload WRITE audit",
		"transfer_id", w.state.TransferID,
		"path", w.target,
		"finish_step", header.GetFinishStep().String(),
		"bytes", w.state.CommittedOffset,
		"sha256", got,
		"backup_path", backupPath,
		"staged_path", stagedPath)
	if err := stream.Send(&mwanv1.UploadResponse{
		Body: &mwanv1.UploadResponse_Terminal{
			Terminal: &mwanv1.TransferTerminal{
				Sha256Hex:     got,
				TotalBytes:    w.state.CommittedOffset,
				BackupPath:    backupPath,
				StagedPath:    stagedPath,
				StatusCode:    int32(codes.OK),
				StatusMessage: "ok",
			},
		},
	}); err != nil {
		m.log.ErrorContext(ctx, "opnsensesvc: finalize send terminal", "err", err)
		return fmt.Errorf("transfer: send terminal: %w", err)
	}
	return nil
}

func (m *TransferManager) snapshotThenReplace(ctx context.Context, target, parent, label, tempPath string) (string, error) {
	backupDir := filepath.Join(parent, TransferBackupSubdir)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", logWrappedErrorContext(ctx, m.log,
			"opnsensesvc: snapshot mkdir backup",
			"mkdir backup", err,
			slog.String("backup_dir", backupDir))
	}
	base := filepath.Base(target)
	ts := m.clock.Now().UTC().Format("20060102T150405Z")
	suffix := ""
	if label != "" {
		suffix = "-" + label
	}
	backupPath := filepath.Join(backupDir, fmt.Sprintf("%s.%s%s.bak", base, ts, suffix))
	if _, err := os.Stat(target); err == nil {
		content, readErr := os.ReadFile(target)
		if readErr != nil {
			return "", logWrappedErrorContext(ctx, m.log,
				"opnsensesvc: snapshot read target",
				"read target for snapshot", readErr,
				slog.String("target", target))
		}
		if writeErr := AtomicWriteFile(ctx, backupPath, content, 0o600); writeErr != nil {
			return "", writeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", logWrappedErrorContext(ctx, m.log,
			"opnsensesvc: snapshot stat target",
			"stat target", err,
			slog.String("target", target))
	} else {
		backupPath = ""
	}
	if err := AtomicRenameFile(ctx, tempPath, target); err != nil {
		return backupPath, err
	}
	return backupPath, nil
}
