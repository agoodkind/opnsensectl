package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"goodkind.io/opnsensectl/internal/opnsense"
)

// fileVerb enumerates `mwan opnsense file <verb>` sub-verbs.
type fileVerb string

const (
	fileVerbPush fileVerb = "push"
	fileVerbPull fileVerb = "pull"
)

func fileUsage(out *os.File) {
	fmt.Fprintln(out, "usage: mwan opnsense file <verb> [args...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Verbs:")
	fmt.Fprintln(out, "  push SOURCE PATH   upload local SOURCE to remote PATH on the OPNsense guest")
	fmt.Fprintln(out, "  pull PATH DEST     download remote PATH to local DEST")
}

func runOPNsenseFile(args []string) int {
	if len(args) < 1 {
		fileUsage(os.Stderr)
		return 2
	}
	verb := fileVerb(args[0])
	rest := args[1:]
	switch verb {
	case fileVerbPush:
		return runFilePush(rest)
	case fileVerbPull:
		return runFilePull(rest)
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense file: unknown verb %q\n", string(verb))
		fileUsage(os.Stderr)
		return 2
	}
}

// transferWatchdog bounds a transfer by lack of progress instead of total
// wall-clock time. The owning loop calls markProgress on each chunk; if no
// progress is seen for stall, the watchdog cancels the transfer context. A
// transfer then succeeds for any size as long as bytes keep flowing, and fails
// fast only on a genuine stall. A fixed whole-transfer deadline could never be
// reliable, because any large enough file exceeds any fixed timeout.
type transferWatchdog struct {
	last    atomic.Int64 // unix nanos of last progress
	stalled atomic.Bool
	once    sync.Once
	done    chan struct{}
	stall   time.Duration
}

// startTransferWatchdog arms a watchdog that cancels via cancel when no progress
// is reported for stall. Only the transfer loop writes last (via markProgress)
// and only the watchdog reads it, so the atomics need no mutex. The goroutine
// exits on stop or after firing; cancel is idempotent, so a watchdog cancel plus
// the caller's deferred cancel is safe.
func startTransferWatchdog(cancel context.CancelFunc, stall time.Duration) *transferWatchdog {
	w := &transferWatchdog{done: make(chan struct{}), stall: stall}
	w.last.Store(time.Now().UnixNano())
	// Tick at stall/4 for prompt detection, capped at 1s so a long stall still
	// samples often; min (not max) keeps the 1s cap from delaying a short stall.
	// The 10ms floor prevents a NewTicker(0) panic on a misconfigured tiny stall.
	tick := min(stall/4, time.Second)
	if tick <= 0 {
		tick = 10 * time.Millisecond
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				w.handleWatchdogPanic(cancel, fmt.Errorf("panic: %v", r))
			}
		}()
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, w.last.Load())) >= w.stall {
					w.stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return w
}

func (w *transferWatchdog) markProgress() { w.last.Store(time.Now().UnixNano()) }

// handleWatchdogPanic records the stall and cancels the transfer context after a
// panic in the watchdog goroutine so the caller fails closed rather than hanging.
// The caller is responsible for converting the recover() value to an error.
func (w *transferWatchdog) handleWatchdogPanic(cancel context.CancelFunc, panicErr error) {
	slog.Error("mwan opnsense: transfer watchdog panic", "err", panicErr)
	w.stalled.Store(true)
	cancel()
}

func (w *transferWatchdog) stop() { w.once.Do(func() { close(w.done) }) }

// fired reports whether the watchdog tripped the cancel due to a stall.
func (w *transferWatchdog) fired() bool { return w.stalled.Load() }

// failure returns a clear stall error when the watchdog tripped the cancel, and
// otherwise wraps the underlying transport error. Centralizing this keeps the
// stall-versus-real-error decision identical across every transfer loop, and a
// genuinely dead channel (a transport error with no stall) still surfaces fast.
func (w *transferWatchdog) failure(ctx context.Context, op string, err error) error {
	if w.fired() {
		stallErr := fmt.Errorf("transfer stalled: no progress for %s", w.stall)
		slog.ErrorContext(ctx, "mwan opnsense: "+op+" stalled", "stall", w.stall.String(), "err", stallErr)
		return fmt.Errorf("%s: %w", op, stallErr)
	}
	return wrapErr(ctx, op, err)
}

// runFilePush uploads SOURCE to PATH on the OPNsense guest. The upload chunk
// size comes from [opnsense.probe].upload_chunk_bytes and the target socket from
// the same section. The transfer is bounded by a progress stall, not a fixed
// deadline (see streamUpload).
func runFilePush(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense file push SOURCE PATH")
		return 2
	}
	source := filepath.Clean(args[0])
	remotePath := args[1]
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return printAndExit("file push", err)
	}
	chunk, err := requireProbeUploadChunk(cfg)
	if err != nil {
		return printAndExit("file push", err)
	}
	stall, err := requireProbeTransferStall(cfg)
	if err != nil {
		return printAndExit("file push", err)
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return printAndExit("file push", fmt.Errorf("read %s: %w", source, err))
	}
	cli, ctx, cancel, err := dialProbeTransfer()
	if err != nil {
		return printAndExit("file push", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()

	header := &mwanv1.TransferHeader{
		Path:             remotePath,
		Direction:        mwanv1.TransferDirection_TRANSFER_DIRECTION_WRITE,
		FinishStep:       mwanv1.FinishStep_FINISH_STEP_REPLACE,
		ResumeTransferId: "",
		ResumeFromOffset: 0,
		Label:            "",
		TotalSize:        int64(len(payload)),
	}
	term, err := streamUpload(ctx, cli, header, payload, chunk, stall)
	if err != nil {
		return printAndExit("file push", err)
	}
	fmt.Printf("file push: bytes=%d sha256=%s backup_path=%s\n",
		term.GetTotalBytes(), term.GetSha256Hex(), term.GetBackupPath())
	return 0
}

// streamUpload runs a WRITE-direction transfer over the TransferService Upload
// stream: open, send header, recv header ack, stream data chunks with stop-and-
// wait acks, send the final checksum, then read the terminal. It is the single
// shared writer behind both `file push` and `daemon push`; each caller supplies
// only its own header (path and finish step differ) and consumes the returned
// terminal. ctx must come from dialProbeTransfer because the transfer is bounded
// by a progress stall watchdog rather than a fixed deadline.
func streamUpload(ctx context.Context, cli *opnsense.Client, header *mwanv1.TransferHeader, data []byte, chunk int, stall time.Duration) (*mwanv1.TransferTerminal, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := startTransferWatchdog(cancel, stall)
	defer wd.stop()

	stream, err := cli.TransferClient().Upload(cctx)
	if err != nil {
		return nil, wd.failure(cctx, "upload: open stream", err)
	}
	if sendErr := stream.Send(&mwanv1.UploadRequest{
		Body: &mwanv1.UploadRequest_Header{Header: header},
	}); sendErr != nil {
		return nil, wd.failure(cctx, "upload: send header", sendErr)
	}
	ackMsg, recvErr := stream.Recv()
	if recvErr != nil {
		return nil, wd.failure(cctx, "upload: recv header ack", recvErr)
	}
	if ackMsg.GetAck() == nil {
		return nil, wrapErr(cctx, "upload: header ack missing", errors.New("first response was not an ack"))
	}

	hasher := sha256.New()
	if chunkErr := uploadChunks(cctx, stream, data, chunk, hasher, wd); chunkErr != nil {
		return nil, chunkErr
	}

	finalHex := hex.EncodeToString(hasher.Sum(nil))
	if sendErr := stream.Send(&mwanv1.UploadRequest{
		Body: &mwanv1.UploadRequest_Final{Final: &mwanv1.TransferFinal{Sha256Hex: finalHex}},
	}); sendErr != nil {
		return nil, wd.failure(cctx, "upload: send final", sendErr)
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return nil, wd.failure(cctx, "upload: close send", closeErr)
	}
	for {
		msg, termRecvErr := stream.Recv()
		if errors.Is(termRecvErr, io.EOF) {
			return nil, wrapErr(cctx, "upload: stream ended", errors.New("no terminal message"))
		}
		if termRecvErr != nil {
			return nil, wd.failure(cctx, "upload: recv terminal", termRecvErr)
		}
		wd.markProgress()
		if term := msg.GetTerminal(); term != nil {
			return term, nil
		}
	}
}

// uploadChunks streams data to the server in chunk-sized segments, blocking on
// the per-chunk ack (application-level stop-and-wait) before sending the next so
// the in-flight byte count stays bounded. It hashes as it goes and reports
// progress to the watchdog after each confirmed ack.
func uploadChunks(ctx context.Context, stream mwanv1.TransferService_UploadClient, data []byte, chunk int, hasher hash.Hash, wd *transferWatchdog) error {
	size := int64(len(data))
	offset := int64(0)
	for offset < size {
		end := min(offset+int64(chunk), size)
		segment := data[offset:end]
		if _, hashErr := hasher.Write(segment); hashErr != nil {
			return wrapErr(ctx, "upload: hash", hashErr)
		}
		if sendErr := stream.Send(&mwanv1.UploadRequest{
			Body: &mwanv1.UploadRequest_Data{Data: &mwanv1.TransferDataChunk{
				Offset: offset,
				Data:   segment,
			}},
		}); sendErr != nil {
			return wd.failure(ctx, "upload: send data", sendErr)
		}
		ackMsg, recvErr := stream.Recv()
		if recvErr != nil {
			return wd.failure(ctx, "upload: recv data ack", recvErr)
		}
		dataAck, ok := ackMsg.GetBody().(*mwanv1.UploadResponse_DataAck)
		if !ok {
			return wrapErr(ctx, "upload: expected data ack", fmt.Errorf("got %T at offset %d", ackMsg.GetBody(), end))
		}
		if dataAck.DataAck.GetCommittedOffset() != end {
			return wrapErr(ctx, "upload: data ack offset", fmt.Errorf("got %d want %d", dataAck.DataAck.GetCommittedOffset(), end))
		}
		wd.markProgress()
		offset = end
	}
	return nil
}

// runFilePull downloads PATH from the OPNsense guest and writes it to DEST
// locally. Like the push path, it is bounded by a progress stall watchdog rather
// than a fixed deadline, so a large download never fails just for being slow.
func runFilePull(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense file pull PATH DEST")
		return 2
	}
	remotePath := args[0]
	dest := filepath.Clean(args[1])
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return printAndExit("file pull", err)
	}
	stall, err := requireProbeTransferStall(cfg)
	if err != nil {
		return printAndExit("file pull", err)
	}
	cli, ctx, cancel, err := dialProbeTransfer()
	if err != nil {
		return printAndExit("file pull", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()

	wd := startTransferWatchdog(cancel, stall)
	defer wd.stop()

	stream, err := cli.TransferClient().Upload(ctx)
	if err != nil {
		return printAndExit("file pull", fmt.Errorf("open stream: %w", err))
	}
	if sendErr := stream.Send(&mwanv1.UploadRequest{
		Body: &mwanv1.UploadRequest_Header{Header: &mwanv1.TransferHeader{
			Path:             remotePath,
			Direction:        mwanv1.TransferDirection_TRANSFER_DIRECTION_READ,
			FinishStep:       mwanv1.FinishStep_FINISH_STEP_UNSPECIFIED,
			ResumeTransferId: "",
			ResumeFromOffset: 0,
			Label:            "",
			TotalSize:        0,
		}},
	}); sendErr != nil {
		return printAndExit("file pull", fmt.Errorf("send header: %w", sendErr))
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return printAndExit("file pull", fmt.Errorf("close send: %w", closeErr))
	}

	hasher := sha256.New()
	total := int64(0)
	out, err := os.Create(dest)
	if err != nil {
		return printAndExit("file pull", fmt.Errorf("create %s: %w", dest, err))
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil {
			slog.WarnContext(ctx, "file pull: close dest", "err", closeErr, "path", dest)
		}
	}()
	for {
		msg, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return printAndExit("file pull", wd.failure(ctx, "recv", recvErr))
		}
		if data := msg.GetData(); data != nil {
			buf := data.GetData()
			if _, hashErr := hasher.Write(buf); hashErr != nil {
				return printAndExit("file pull", fmt.Errorf("hash: %w", hashErr))
			}
			if _, writeErr := out.Write(buf); writeErr != nil {
				return printAndExit("file pull", fmt.Errorf("write %s: %w", dest, writeErr))
			}
			total += int64(len(buf))
			wd.markProgress()
		}
		if term := msg.GetTerminal(); term != nil {
			fmt.Printf("file pull: bytes=%d sha256=%s server_sha256=%s dest=%s\n",
				total, hex.EncodeToString(hasher.Sum(nil)), term.GetSha256Hex(), dest)
			return 0
		}
	}
	return 0
}
