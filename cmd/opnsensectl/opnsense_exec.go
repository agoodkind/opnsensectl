package main

import (
	"crypto/rand"
	"fmt"
	"os"
)

const (
	probeMaxCommandTimeoutSec = int32(300)
	selftestDefaultSize       = 1024
)

// runOPNsenseExec implements `mwan opnsense exec CMD [ARGS...]`. The
// CMD path is the first positional, every remaining positional is an
// argv token. Sudo wrapping is controlled by [opnsense.probe].sudo
// today; this PR keeps that off because the proto's Exec request takes
// an explicit sudo bit and the CLI has no other knob to flip without
// reintroducing a flag. The exec timeout comes from
// [opnsense.probe].timeout (the dial+RPC timeout drives both).
func runOPNsenseExec(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense exec CMD [ARGS...]")
		return 2
	}
	cmd := args[0]
	argv := args[1:]
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("exec", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()

	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return printAndExit("exec", err)
	}
	timeout, err := requireProbeTimeout(cfg)
	if err != nil {
		return printAndExit("exec", err)
	}
	timeoutSec := min(int32(timeout.Seconds()), probeMaxCommandTimeoutSec)
	res, err := cli.RPC().Exec(ctx, cmd, argv, false, timeoutSec, nil)
	if err != nil {
		return printAndExit("exec", err)
	}
	fmt.Printf("exit_code=%d duration_ms=%d timed_out=%t\n",
		res.ExitCode, res.DurationMs, res.TimedOut)
	if len(res.Stdout) > 0 {
		fmt.Printf("---stdout---\n%s\n", res.Stdout)
	}
	if len(res.Stderr) > 0 {
		fmt.Printf("---stderr---\n%s\n", res.Stderr)
	}
	return 0
}

// runOPNsenseSelftest sends a small random payload through /usr/bin/wc
// on the guest. Used as an end-to-end sanity check that the daemon and
// the host bridge are both healthy.
func runOPNsenseSelftest(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: mwan opnsense selftest")
		return 2
	}
	cli, ctx, cancel, err := dialProbe()
	if err != nil {
		return printAndExit("selftest", err)
	}
	defer cancel()
	defer func() { _ = cli.Close() }()
	payload := make([]byte, selftestDefaultSize)
	if _, err := rand.Read(payload); err != nil {
		return printAndExit("selftest", fmt.Errorf("rand: %w", err))
	}
	res, err := cli.RPC().Exec(ctx, "/usr/bin/wc", []string{"-c"}, false, 30, payload)
	if err != nil {
		return printAndExit("selftest", err)
	}
	fmt.Printf("selftest size=%d exit=%d stdout_bytes=%d stderr_bytes=%d duration_ms=%d\n",
		selftestDefaultSize, res.ExitCode, len(res.Stdout), len(res.Stderr), res.DurationMs)
	return 0
}
