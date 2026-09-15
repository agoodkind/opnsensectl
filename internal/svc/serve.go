package svc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/hashicorp/yamux"
	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Serve opens the virtio-serial device, wraps it in a yamux server
// session so the single byte stream carries many gRPC connections,
// registers OpnsenseService and TransferService against a new gRPC
// server, and serves until ctx is cancelled. WriteBufferSize is set to
// 0 so HTTP/2 frames are not coalesced past the FreeBSD tty input
// queue ceiling. yamux keep-alive is disabled because the virtio-
// serial line has no out-of-band channel and any keep-alive frame
// would race real RPC traffic on the same byte stream.
func Serve(ctx context.Context, opts ServeOpts) error {
	if opts.Server == nil {
		return errors.New("Serve: Server required")
	}
	if opts.SerialPath == "" {
		return errors.New("Serve: SerialPath required")
	}
	if opts.OpenSerial == nil {
		return errors.New("Serve: OpenSerial required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.Transfer != nil {
		opts.Server.AttachTransferManager(opts.Transfer)
	}

	rwc, err := opts.OpenSerial(opts.SerialPath)
	if err != nil {
		return logWrappedErrorContext(ctx, log,
			"opnsensesvc: open serial", "opnsensesvc: open serial", err,
			slog.String("path", opts.SerialPath))
	}
	defer func() { _ = rwc.Close() }()
	if opts.OnSerialOpen != nil {
		opts.OnSerialOpen(opts.SerialPath)
	}
	log.InfoContext(ctx, "opnsensesvc: serial opened", "path", opts.SerialPath)

	// The yamux read loop parks in a blocking serial read that the FreeBSD
	// virtio_console driver never wakes on its own (no host-disconnect
	// signal, and a fd close from another goroutine does not interrupt the
	// parked read), so Serve does not return cleanly on a true stop. That is
	// handled outside this process: a terminal SIGTERM relies on the rc.d
	// process-group SIGKILL plus daemon -r respawn, and a binary update
	// re-execs in place (the RestartDaemon hook) so there is no stop at all.
	// serialStream.Close stays a no-op so the fd outlives individual yamux
	// sessions while the daemon is up.

	// One yamux session at a time runs over the shared chardev. If the
	// session ends (peer disconnect, protocol-version mismatch from
	// leftover bytes after a restart, transport error), build a fresh
	// session and resume serving. The chardev byte stream is reused;
	// only the yamux+gRPC layer is rebuilt.
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := serveOneSession(ctx, log, rwc, opts.Server, opts.StopTimeout); err != nil {
			log.WarnContext(ctx, "opnsensesvc: session ended, restarting", "err", err)
			if !sleepOK(ctx, time.Second) {
				return nil
			}
			continue
		}
		return nil
	}
}

// serveOneSession builds a yamux session over rwc, hands it to a
// fresh gRPC server, and serves until the session terminates or ctx
// is cancelled. Any returned error is non-fatal at the daemon level
// so the outer loop can build a new session.
func serveOneSession(ctx context.Context, log *slog.Logger, rwc io.ReadWriteCloser, srv *Server, stopTimeout time.Duration) error {
	yamuxCfg := yamux.DefaultConfig()
	yamuxCfg.EnableKeepAlive = false
	yamuxCfg.LogOutput = io.Discard
	yamuxCfg.MaxStreamWindowSize = 16 * 1024 * 1024
	session, err := yamux.Server(serialStream{rwc: rwc}, yamuxCfg)
	if err != nil {
		return fmt.Errorf("yamux server: %w", err)
	}
	defer func() { _ = session.Close() }()

	grpcServer := grpc.NewServer(
		grpc.WriteBufferSize(0),
		grpc.Creds(insecure.NewCredentials()),
	)
	mwanv1.RegisterOpnsenseServiceServer(grpcServer, srv)
	mwanv1.RegisterTransferServiceServer(grpcServer, srv)

	stopCtx, stopCancel := context.WithCancel(ctx)
	defer stopCancel()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(stopCtx, "opnsensesvc: stop watcher panic",
					"panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		<-stopCtx.Done()
		// Bound the graceful stop. GracefulStop waits for in-flight RPCs,
		// which a long Exec or Transfer can stretch; force grpcServer.Stop()
		// if it does not finish within stopTimeout so the gRPC layer is
		// always torn down within the bound. The subsequent session.Close
		// still parks on the un-interruptible serial read, so a true stop
		// ultimately completes via the rc.d process-group kill, not here.
		gracefulDone := make(chan struct{})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(stopCtx, "opnsensesvc: graceful-stop panic",
						"panic", r, "err", fmt.Errorf("panic: %v", r))
				}
			}()
			grpcServer.GracefulStop()
			close(gracefulDone)
		}()
		if stopTimeout > 0 {
			t := time.NewTimer(stopTimeout)
			defer t.Stop()
			select {
			case <-gracefulDone:
			case <-t.C:
				grpcServer.Stop()
			}
		} else {
			<-gracefulDone
		}
		_ = session.Close()
	}()

	serveErr := grpcServer.Serve(session)
	if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return fmt.Errorf("grpc serve: %w", serveErr)
	}
	return nil
}

// sleepOK waits for d or returns false if ctx is cancelled first.
func sleepOK(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
