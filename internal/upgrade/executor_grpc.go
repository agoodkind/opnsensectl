package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/opnsensectl/internal/opnsense"
)

// ExecResult mirrors opnsense.ExecResult so test doubles can satisfy
// OPNsenseRPCClient without importing the parent package.
type ExecResult = opnsense.ExecResult

// OPNsenseRPCClient is the narrow surface the gRPC Executor needs from
// the mwan-opnsense daemon. It is satisfied by *opnsense.RPC in
// production and by an in-memory mock in tests.
type OPNsenseRPCClient interface {
	// Exec runs a binary inside the OPNsense guest via the bidi Exec
	// stream. The wrapper drives the stream synchronously and returns
	// a buffered result.
	Exec(ctx context.Context, command string, args []string, sudo bool, timeoutSeconds int32, stdin []byte) (*ExecResult, error)
}

// GRPCExecutor implements [Executor] by routing each GuestExec call to
// the mwan-opnsense daemon over the persistent virtio-serial gRPC
// channel. This is the OOB path used when QGA is unavailable on the
// guest, for example on the testbed where there is no internet egress
// to install the os-qemu-guest-agent package. The validate phase's exec
// check rides the same RPC, so prepare, execute, validate, rollback, and
// commit all run over the gRPC channel.
type GRPCExecutor struct {
	// RPC is the typed mwan-opnsense client. Required.
	RPC OPNsenseRPCClient

	// ExecTimeoutSeconds caps each Exec RPC's per-call timeout. Zero
	// falls back to the daemon's default (30s); negative is rejected
	// by the daemon at call time. The maximum honoured by the daemon
	// is bounded by maxExecTimeout in internal/opnsense/svc/exec.go.
	ExecTimeoutSeconds int32

	// Redial reconnects the gRPC client and returns a fresh
	// OPNsenseRPCClient. It is invoked when an Exec call returns a
	// closed-client error, which can happen after qm rollback restarts QEMU.
	// When Redial is nil the executor never reconnects and surfaces the original
	// error verbatim. The closed state is detected via isClientClosedErr so
	// callers do not need to import the opnsense package for the sentinel.
	Redial func() (OPNsenseRPCClient, error)
}

// guard against drift: GRPCExecutor must satisfy Executor.
var _ Executor = (*GRPCExecutor)(nil)

// GuestExec dispatches an in-guest exec to the daemon's Exec RPC. The
// vmid is recorded in slog context for forensics but is not forwarded
// to the daemon: the daemon serves a single VM identified by its
// virtio-serial socket path on the Proxmox host, so the binding is
// implicit at dial time.
func (g *GRPCExecutor) GuestExec(
	ctx context.Context, vmid string, args ...string,
) (GuestExecResult, error) {
	if g.RPC == nil {
		err := errors.New("GRPCExecutor: RPC client not set")
		slog.ErrorContext(ctx, "upgrade GRPCExecutor missing RPC client",
			"err", err.Error(), "vmid", vmid)
		return GuestExecResult{}, err
	}
	if len(args) == 0 {
		err := errors.New("GRPCExecutor: empty argv")
		slog.ErrorContext(ctx, "upgrade GRPCExecutor empty argv",
			"err", err.Error(), "vmid", vmid)
		return GuestExecResult{}, err
	}
	command := args[0]
	tail := append([]string(nil), args[1:]...)
	resp, err := g.RPC.Exec(ctx, command, tail, false, g.ExecTimeoutSeconds, nil)
	if err != nil && isClientClosedErr(err) && g.Redial != nil {
		slog.WarnContext(ctx, "upgrade GRPCExecutor: client closed, redialing",
			"err", err.Error(), "vmid", vmid, "command", command)
		newRPC, dialErr := g.Redial()
		if dialErr != nil {
			slog.ErrorContext(ctx, "upgrade GRPCExecutor: redial failed",
				"err", dialErr.Error(), "vmid", vmid, "command", command,
				"orig_err", err.Error())
			return GuestExecResult{}, fmt.Errorf("grpc Exec %s redial: %w", command, dialErr)
		}
		g.RPC = newRPC
		resp, err = g.RPC.Exec(ctx, command, tail, false, g.ExecTimeoutSeconds, nil)
	}
	if err != nil {
		slog.ErrorContext(ctx, "upgrade GRPCExecutor Exec RPC failed",
			"err", err.Error(), "vmid", vmid, "command", command)
		return GuestExecResult{}, fmt.Errorf("grpc Exec %s: %w", command, err)
	}
	if resp == nil {
		err := errors.New("GRPCExecutor: nil ExecResult from daemon")
		slog.ErrorContext(ctx, "upgrade GRPCExecutor nil response",
			"err", err.Error(), "vmid", vmid, "command", command)
		return GuestExecResult{}, err
	}
	stderrText := string(resp.Stderr)
	if resp.TimedOut {
		stderrText = appendDiag(stderrText, "[grpc-executor: command timed out]")
	}
	if resp.StdoutTruncated {
		stderrText = appendDiag(stderrText, "[grpc-executor: stdout truncated at 10 MB]")
	}
	if resp.StderrTruncated {
		stderrText = appendDiag(stderrText, "[grpc-executor: stderr truncated at 10 MB]")
	}
	return GuestExecResult{
		ExitCode: int(resp.ExitCode),
		Stdout:   string(resp.Stdout),
		Stderr:   stderrText,
	}, nil
}

// isClientClosedErr reports whether err indicates the underlying gRPC
// client is closed. The sentinel from internal/opnsense surfaces as
// "opnsense: client is closed"; matching by message keeps the upgrade
// package free of the opnsense package import. This is the post-rollback
// signal that drives the redial-once path in GuestExec.
func isClientClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "client is closed")
}

// appendDiag joins a diagnostic line onto an existing stderr blob
// without colliding with trailing whitespace.
func appendDiag(stderr, diag string) string {
	if stderr == "" {
		return diag
	}
	if strings.HasSuffix(stderr, "\n") {
		return stderr + diag
	}
	return stderr + "\n" + diag
}
