package svc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// execWaitDelay bounds how long cmd.Run may block on inherited stdout/stderr
// pipes after the per-call timeout has killed the process group. It is a
// safety net only: the process-group kill below already terminates the whole
// command tree at the deadline, so this is reached only if some process still
// holds the pipe open.
const execWaitDelay = 2 * time.Second

const (
	defaultExecTimeout = 30 * time.Second
	// maxExecTimeout caps the per-Exec RPC duration. Long package-fetch operations
	// need more than the default per-call timeout, but the orchestrator's outer
	// upgrade timeout still bounds the overall execute phase.
	maxExecTimeout = 60 * time.Minute
	maxOutputBytes = 10 * 1024 * 1024 // 10 MB stdout/stderr cap each
)

// ExecArgs is the input shape used by the gRPC handler. The proto
// types live in the gen/ tree and the server wraps this struct around
// them so the core exec logic is testable without proto deps.
type ExecArgs struct {
	Command        string
	Args           []string
	Sudo           bool
	TimeoutSeconds int32
	StdinBytes     []byte
	Clock          Clock
	Log            *slog.Logger
}

// ExecResult is the output shape mirrored back into the gRPC response.
type ExecResult struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int32
	DurationMS      int64
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
}

// runExec is the pure execution implementation. ctx provides the
// outer deadline; the per-call timeout is layered on top.
func runExec(ctx context.Context, args ExecArgs) (*ExecResult, error) {
	slog.DebugContext(ctx, "opnsensesvc: runExec",
		"command", args.Command,
		"args_len", len(args.Args),
		"sudo", args.Sudo,
		"timeout_seconds", args.TimeoutSeconds)
	if err := validateExecArgs(args); err != nil {
		return nil, err
	}

	timeout := time.Duration(args.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	if timeout > maxExecTimeout {
		timeout = maxExecTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmdName, cmdArgs := args.Command, args.Args
	if args.Sudo {
		cmdName = "sudo"
		cmdArgs = append([]string{"-n", args.Command}, args.Args...)
	}
	cmd := exec.CommandContext(cctx, cmdName, cmdArgs...)
	// Run the child in its own process group and kill the whole group when
	// the per-call timeout fires. Without this, a grandchild (e.g. the
	// `sleep` under `sh -c "sleep 5"`) inherits the stdout/stderr pipe and
	// keeps cmd.Run() blocked until it exits on its own, so the timeout is
	// detected but not enforced and the grandchild is left orphaned. Killing
	// the negative PID (the process group) terminates the whole tree at the
	// deadline. WaitDelay is the I/O safety net for the rare case where a
	// process still holds the pipe after the group kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return logWrappedErrorContext(cctx, args.Log,
				"opnsensesvc: kill process group failed", "runExec cancel", err,
				slog.Int("pid", cmd.Process.Pid))
		}
		return nil
	}
	cmd.WaitDelay = execWaitDelay
	if len(args.StdinBytes) > 0 {
		cmd.Stdin = bytes.NewReader(args.StdinBytes)
	}

	stdoutBuf := &cappedBuffer{cap: maxOutputBytes, buf: bytes.Buffer{}, truncated: false, bytesWrote: 0}
	stderrBuf := &cappedBuffer{cap: maxOutputBytes, buf: bytes.Buffer{}, truncated: false, bytesWrote: 0}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	activeClock := clockOrReal(args.Clock)
	start := activeClock.Now()
	runErr := cmd.Run()
	dur := activeClock.Now().Sub(start)

	res := &ExecResult{
		Stdout:          stdoutBuf.Bytes(),
		Stderr:          stderrBuf.Bytes(),
		ExitCode:        0,
		DurationMS:      dur.Milliseconds(),
		StdoutTruncated: stdoutBuf.Truncated(),
		StderrTruncated: stderrBuf.Truncated(),
		TimedOut:        errors.Is(cctx.Err(), context.DeadlineExceeded),
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = clampToInt32(exitErr.ExitCode())
			return res, nil
		}
		if res.TimedOut {
			res.ExitCode = -1
			return res, nil
		}
		return res, logWrappedErrorContext(ctx, args.Log,
			"opnsensesvc: exec command failed", "runExec", runErr,
			slog.String("command", args.Command))
	}

	return res, nil
}

func validateExecArgs(args ExecArgs) error {
	if args.Command == "" {
		return errors.New("exec: empty command")
	}
	if strings.ContainsRune(args.Command, 0) {
		return errors.New("exec: command contains null byte")
	}
	if len(args.Command) > 4096 {
		return errors.New("exec: command too long")
	}
	for i, a := range args.Args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("exec: arg[%d] contains null byte", i)
		}
		if len(a) > 4096 {
			return fmt.Errorf("exec: arg[%d] too long", i)
		}
	}
	return nil
}

// cappedBuffer is an [io.Writer] that drops bytes after `cap` and
// records the fact via Truncated().
type cappedBuffer struct {
	cap        int
	buf        bytes.Buffer
	truncated  bool
	bytesWrote int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	want := len(p)
	remaining := c.cap - c.bytesWrote
	if remaining <= 0 {
		c.truncated = true
		return want, nil // pretend we accepted; subprocess keeps running
	}
	if len(p) > remaining {
		c.truncated = true
		p = p[:remaining]
	}
	n, err := c.buf.Write(p)
	c.bytesWrote += n
	if err != nil {
		slog.Error("opnsensesvc: capped buffer write failed", "err", err)
		return n, fmt.Errorf("capped buffer write: %w", err)
	}
	return want, nil
}

func (c *cappedBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

func (c *cappedBuffer) Truncated() bool {
	return c.truncated
}

// ensure cappedBuffer satisfies [io.Writer]
var _ io.Writer = (*cappedBuffer)(nil)
