package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"goodkind.io/opnsensectl/internal/opnsense"
	"goodkind.io/opnsensectl/internal/version"
)

// daemonVerb is the typed enum of `mwan opnsense daemon <verb>`
// sub-verbs. The verb tree separates in-VM-daemon controls (serve,
// is-enabled) from probe-driven RPC calls against an already-running
// daemon (state, push, stage, restart, revert, gc, version).
type daemonVerb string

const (
	daemonVerbServe       daemonVerb = "serve"
	daemonVerbIsEnabled   daemonVerb = "is-enabled"
	daemonVerbVersion     daemonVerb = "version"
	daemonVerbState       daemonVerb = "state"
	daemonVerbPush        daemonVerb = "push"
	daemonVerbStage       daemonVerb = "stage"
	daemonVerbRestart     daemonVerb = "restart"
	daemonVerbRevert      daemonVerb = "revert"
	daemonVerbGC          daemonVerb = "gc"
	daemonVerbMarkHealthy daemonVerb = "mark-healthy"
)

func daemonUsage(out *os.File) {
	fmt.Fprintln(out, "usage: mwan opnsense daemon <verb> [args...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Verbs:")
	fmt.Fprintln(out, "  serve --config PATH         run the in-VM daemon (the rc.d service command)")
	fmt.Fprintln(out, "  is-enabled                  exit 0 if rc.d service is enabled")
	fmt.Fprintln(out, "  version                     print build identity from the running daemon")
	fmt.Fprintln(out, "  state                       print deploy state (active sha, previous, health, deployed_at)")
	fmt.Fprintln(out, "  push BINARY                 upload BINARY to the daemon staging path")
	fmt.Fprintln(out, "  stage SHA                   verify+swap a previously pushed binary by sha256")
	fmt.Fprintln(out, "  restart                     trigger the daemon to exit so rc.d respawns it")
	fmt.Fprintln(out, "  revert                      restore the previous staged binary")
	fmt.Fprintln(out, "  gc                          probe the GC surface (daemon runs sweep on startup)")
	fmt.Fprintln(out, "  mark-healthy                clear pending-verify and stamp health=ok for the active binary")
}

func runOPNsenseDaemon(args []string) int {
	if len(args) < 1 {
		daemonUsage(os.Stderr)
		return 2
	}
	verb := daemonVerb(args[0])
	rest := args[1:]
	switch verb {
	case daemonVerbServe:
		return runOPNsenseDaemonServe(rest)
	case daemonVerbIsEnabled:
		return runOPNsenseDaemonIsEnabled(rest)
	case daemonVerbVersion:
		return runDaemonRPCVersion()
	case daemonVerbState:
		return runDaemonState()
	case daemonVerbPush:
		return runDaemonPush(rest)
	case daemonVerbStage:
		return runDaemonStage(rest)
	case daemonVerbRestart:
		return runDaemonRestart()
	case daemonVerbRevert:
		return runDaemonRevert()
	case daemonVerbGC:
		return runDaemonGC()
	case daemonVerbMarkHealthy:
		return runDaemonMarkHealthy()
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense daemon: unknown verb %q\n", string(verb))
		daemonUsage(os.Stderr)
		return 2
	}
}

// dialProbe is the shared helper used by every daemon/file/config/exec
// verb. It loads TOML, resolves the probe target+timeout, dials, and
// returns the client plus a derived context. The caller must Close the
// client and cancel the context.
func dialProbe() (*opnsense.Client, context.Context, context.CancelFunc, error) {
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	target, err := requireProbeTarget(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	timeout, err := requireProbeTimeout(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	cli, err := opnsense.Dial(target)
	if err != nil {
		slog.Error("opnsense: dial", "err", err, "target", target)
		return nil, nil, nil, fmt.Errorf("opnsense: dial %s: %w", target, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return cli, ctx, cancel, nil
}

// dialProbeTransfer is the dial helper for file transfers. It is identical to
// dialProbe except the returned context has no overall deadline. A whole-
// transfer wall-clock deadline cannot be 100% reliable, because any file large
// enough exceeds any fixed timeout; transfers are instead bounded by a progress
// stall watchdog (see transferWatchdog). The caller must Close the client and
// cancel the context.
func dialProbeTransfer() (*opnsense.Client, context.Context, context.CancelFunc, error) {
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	target, err := requireProbeTarget(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	cli, err := opnsense.Dial(target)
	if err != nil {
		slog.Error("opnsense: dial", "err", err, "target", target)
		return nil, nil, nil, fmt.Errorf("opnsense: dial %s: %w", target, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return cli, ctx, cancel, nil
}

// printAndExit prints an error to stderr in the conventional
// "mwan opnsense <path>: <err>" form and returns 1.
func printAndExit(path string, err error) int {
	fmt.Fprintf(os.Stderr, "mwan opnsense %s: %v\n", path, err)
	return 1
}

func runOPNsenseVersion(_ []string) int {
	fmt.Fprintln(os.Stdout, version.BuildVersionString())
	return 0
}

func runDaemonRPCVersion() int {
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon version", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().Version(ctx, &mwanv1.VersionRequest{})
	if err != nil {
		return printAndExit("daemon version", err)
	}
	fmt.Printf("version=%s commit=%s dirty=%t binhash=%s\n",
		resp.GetVersion(), resp.GetBuildCommit(), resp.GetBuildDirty(), resp.GetBuildBinhash())
	return 0
}

func runDaemonState() int {
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon state", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().DeployStatus(ctx, &mwanv1.DeployStatusRequest{
		Mark: mwanv1.DeployStatusRequest_MARK_UNSPECIFIED,
	})
	if err != nil {
		return printAndExit("daemon state", err)
	}
	fmt.Printf("active=%s previous=%s health=%s deployed_at=%d\n",
		resp.GetActiveSha256(), resp.GetPreviousSha256(), resp.GetHealth(), resp.GetDeployedAt())
	return 0
}

// runDaemonMarkHealthy stamps the active binary healthy: it clears the
// pending-verify marker and sets health=ok in the deploy state, so the
// rc.d preflight does not revert the deploy on a later respawn. This is
// the step that completes a self-deploy round-trip; without it the
// preflight treats every deploy as unverified.
func runDaemonMarkHealthy() int {
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon mark-healthy", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().DeployStatus(ctx, &mwanv1.DeployStatusRequest{
		Mark: mwanv1.DeployStatusRequest_MARK_HEALTHY,
	})
	if err != nil {
		return printAndExit("daemon mark-healthy", err)
	}
	fmt.Printf("active=%s previous=%s health=%s deployed_at=%d\n",
		resp.GetActiveSha256(), resp.GetPreviousSha256(), resp.GetHealth(), resp.GetDeployedAt())
	return 0
}

// runDaemonPush stages BINARY through the shared TransferService Upload path
// with FINISH_STEP_STAGE. It reuses streamUpload (the same writer as `file
// push`) so the upload protocol and the progress-stall deadline live in one
// place. The daemon writes the staged file to its standard staging path and
// returns the canonical sha256 in the terminal, which a follow-up `daemon
// stage` refers to.
func runDaemonPush(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense daemon push BINARY")
		return 2
	}
	binaryPath := filepath.Clean(args[0])
	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return printAndExit("daemon push", err)
	}
	chunk, err := requireProbeUploadChunk(cfg)
	if err != nil {
		return printAndExit("daemon push", err)
	}
	stall, err := requireProbeTransferStall(cfg)
	if err != nil {
		return printAndExit("daemon push", err)
	}
	content, err := os.ReadFile(binaryPath)
	if err != nil {
		return printAndExit("daemon push", fmt.Errorf("read %s: %w", binaryPath, err))
	}
	cli, ctx, cancel, err := dialProbeTransfer()
	if err != nil {
		return printAndExit("daemon push", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()

	header := &mwanv1.TransferHeader{
		Path:             filepath.Join("/usr/local/sbin", "mwan-opnsense"),
		Direction:        mwanv1.TransferDirection_TRANSFER_DIRECTION_WRITE,
		FinishStep:       mwanv1.FinishStep_FINISH_STEP_STAGE,
		ResumeTransferId: "",
		ResumeFromOffset: 0,
		Label:            "",
		TotalSize:        int64(len(content)),
	}
	term, err := streamUpload(ctx, cli, header, content, chunk, stall)
	if err != nil {
		return printAndExit("daemon push", err)
	}
	fmt.Printf("daemon push: bytes=%d sha256=%s staged_path=%s\n",
		term.GetTotalBytes(), term.GetSha256Hex(), term.GetStagedPath())
	fmt.Printf("pushed sha256=%s\n", term.GetSha256Hex())
	return 0
}

func runDaemonStage(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense daemon stage SHA")
		return 2
	}
	stagedSHA := args[0]
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon stage", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.OpnsenseClient().StageBinary(ctx, &mwanv1.StageBinaryRequest{
		StagedSha256: stagedSHA,
		VersionStr:   "",
	})
	if err != nil {
		return printAndExit("daemon stage", err)
	}
	fmt.Printf("staged active=%s previous=%s\n",
		resp.GetActiveSha256(), resp.GetPreviousPath())
	return 0
}

func runDaemonRestart() int {
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon restart", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	if _, err := cli.OpnsenseClient().RestartDaemon(ctx, &mwanv1.RestartDaemonRequest{}); err != nil {
		return printAndExit("daemon restart", err)
	}
	fmt.Println("daemon restart: initiated")
	return 0
}

func runDaemonRevert() int {
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon revert", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	resp, err := cli.RPC().Revert(ctx, &mwanv1.RevertRequest{})
	if err != nil {
		return printAndExit("daemon revert", err)
	}
	fmt.Printf("reverted_to=%s\n", resp.GetRevertedToSha256())
	return 0
}

func runDaemonGC() int {
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("daemon gc", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	if _, err := cli.TransferClient().Cancel(ctx, &mwanv1.CancelRequest{TransferId: ""}); err != nil {
		slog.WarnContext(ctx, "daemon gc: cancel surface", "err", err)
	}
	fmt.Println("daemon gc: server runs GC on startup; nothing to drive from probe")
	return 0
}
