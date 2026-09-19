package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/opnsensectl/internal/svc"
)

// reExecCurrent replaces the running process image with the active
// binary slot (.current) via execFn. This is how a deploy or revert
// lands the new binary: the slot and symlink already point at the new
// code, so the re-exec runs it without ever stopping the daemon.
//
// Why re-exec rather than exit-and-respawn: execve keeps the same pid,
// so the rc.d daemon -r supervisor sees no exit and runs no
// preflight-revert cycle on a normal update. It also obliterates every
// goroutine, including the one parked in the un-interruptible
// virtio-serial read, so there is no clean-stop problem to solve.
//
// The serial fd is O_CLOEXEC (os.OpenFile in OpenVirtioSerial), so
// execve closes it and the new image re-opens the device; the host
// bridge detects the brief blip and rebuilds its yamux session. A
// future change can preserve the fd across exec to remove that blip.
//
// The new image receives the running process's argv unchanged, so it runs
// the same explicit `daemon serve --config PATH` command. The binary selects
// nothing by file name, so argv[0] stays what the process was started with.
//
// execFn is [syscall.Exec] in production and a fake in tests.
// [syscall.Exec] returns only on failure, so a non-nil return means the
// exec did not happen and the caller should fall back to a clean exit.
func reExecCurrent(log *slog.Logger, binaryDir string, execFn func(argv0 string, argv []string, envv []string) error) error {
	if log == nil {
		log = slog.Default()
	}
	target := filepath.Join(binaryDir, svc.BinaryCurrent)
	if _, err := os.Stat(target); err != nil {
		log.Error("re-exec: stat active binary failed", "target", target, "err", err)
		return fmt.Errorf("re-exec: stat active binary %s: %w", target, err)
	}
	if len(os.Args) < 2 {
		err := fmt.Errorf("re-exec: the running argv %q carries no command to reuse", os.Args)
		log.Error("re-exec: refusing to re-exec without the running command", "err", err)
		return err
	}
	argv := append([]string(nil), os.Args...)
	log.Info("mwan-opnsense: re-exec onto active binary", "target", target, "argv", argv)
	return execFn(target, argv, os.Environ())
}
