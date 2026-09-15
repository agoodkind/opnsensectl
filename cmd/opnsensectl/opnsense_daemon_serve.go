package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"goodkind.io/opnsensectl/internal/daemoncfg"
	"goodkind.io/opnsensectl/internal/svc"
)

const (
	rcEnabledCheckTimeout = 5 * time.Second

	// defaultRCName / defaultRCSubr are the rc.d service name and the
	// rc.subr path the is-enabled check resolves. Both live on FreeBSD
	// hosts and have stable conventional paths. The serial path, baud,
	// config.xml path, backup dir, state dir, and rendered logfile path
	// are recorded in the daemon-side TOML (/var/lib/mwan/daemon.toml);
	// only rc.d supervision details stay compiled in here.
	defaultRCName = "mwan_opnsense"
	defaultRCSubr = "/etc/rc.subr"

	// daemonStopTimeout bounds the in-Serve graceful shutdown: a stuck
	// in-flight RPC is force-stopped after this, so Serve always returns on a
	// stop. The fd close in Serve makes the idle case return well inside the
	// bound. The guaranteed last resort for a wedged process is the rc.d
	// process-group SIGKILL in the mwan_opnsense service script, not an
	// in-process [os.Exit] (which the os_exit_outside_main gate forbids in a
	// watcher goroutine).
	daemonStopTimeout = 8 * time.Second
)

// runOPNsenseDaemonServe starts the MWN1 dispatcher daemon with the
// virtio-serial-pci listener. There is exactly one listener and exactly
// one peer; auth is unix socket permissions on the host side (root-only)
// so the daemon does not authenticate at the application layer.
//
// The serve verb takes no flags. The serial path, baud, config.xml path,
// backup dir, and transfer state dir come from /var/lib/mwan/daemon.toml
// (templated by the rc.d script). That file also records the rc.d-owned logfile
// path so the runtime contract is complete even though the serve process does
// not open the logfile.
// The verb still accepts an empty arg slice or a help token for forward
// compatibility.
func runOPNsenseDaemonServe(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stdout, "usage: mwan opnsense daemon serve")
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "Run the in-VM dispatcher daemon. No flags; runtime config")
			fmt.Fprintln(os.Stdout, "is loaded from /var/lib/mwan/daemon.toml (templated by the")
			fmt.Fprintln(os.Stdout, "rc.d script from rc.conf.d-overridable variables).")
			return 0
		}
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "mwan opnsense daemon serve: unexpected arguments: %v\n", args)
		return 2
	}

	cfg, cfgErr := daemoncfg.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "daemon serve: load config: %v\n", cfgErr)
		return 1
	}

	serialPath := cfg.Daemon.SerialPath
	configPath := cfg.Daemon.ConfigXMLPath
	backupDir := cfg.Daemon.BackupDir
	baud := cfg.Daemon.Baud
	stateDir := cfg.Daemon.StateDir

	logger := slog.Default()
	srv := svc.NewServer(logger, configPath, backupDir)
	validator := svc.NewPathValidator(logger, svc.DefaultReadAllowlist, svc.DefaultWriteAllowlist)
	transferMgr, transferErr := svc.NewTransferManager(logger, validator, stateDir, nil)
	if transferErr != nil {
		fmt.Fprintf(os.Stderr, "daemon serve: build transfer manager: %v\n", transferErr)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// We do not `defer cancel()` because we explicitly call it before
	// returning from this subcommand.
	srv.SetRestartHook(func() {
		logger.Info("mwan-opnsense: RestartDaemon hook firing, re-exec onto active binary")
		if err := reExecCurrent(logger, svc.DefaultBinaryDir, syscall.Exec); err != nil {
			logger.Error("mwan-opnsense: re-exec failed; falling back to ctx cancel", "err", err)
			cancel()
		}
	})

	openSerial := func(path string) (io.ReadWriteCloser, error) {
		return svc.OpenVirtioSerial(path, baud, logger)
	}

	opts := svc.ServeOpts{
		SerialPath:   serialPath,
		OpenSerial:   openSerial,
		Server:       srv,
		Log:          logger,
		OnSerialOpen: nil,
		OnGRPCAccept: nil,
		Transfer:     transferMgr,
		StopTimeout:  daemonStopTimeout,
	}

	// Sweep orphaned deploy scratch (.staged/.staged.tmp/upload temps)
	// from a prior crash before the serial port opens, so an interrupted
	// deploy or upload never wedges a stale temp into the binary dir.
	if removed, cleanErr := srv.CleanStaleDeployTemps(ctx); cleanErr != nil {
		logger.Warn("daemon serve: clean stale deploy temps", "err", cleanErr)
	} else if removed > 0 {
		logger.Info("daemon serve: removed orphaned deploy temps", "removed", removed)
	}

	slog.Info("mwan-opnsense: serving",
		"serial_path", serialPath,
		"baud", baud)

	serveErr := svc.Serve(ctx, opts)
	cancel()
	if serveErr != nil {
		slog.Error("daemon serve: terminated", "err", serveErr)
		return 1
	}
	slog.Info("mwan-opnsense: stopped")
	return 0
}

// runOPNsenseDaemonIsEnabled returns exit 0 when the rc.d service is
// enabled in /etc/rc.conf, 1 when it is disabled, 2 on error. No flags;
// the name and rc.subr path are compiled constants.
func runOPNsenseDaemonIsEnabled(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stdout, "usage: mwan opnsense daemon is-enabled")
			return 0
		}
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "mwan opnsense daemon is-enabled: unexpected arguments: %v\n", args)
		return 2
	}
	enabled, err := checkRCEnabled(defaultRCName, defaultRCSubr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mwan opnsense daemon is-enabled: %v\n", err)
		return 2
	}
	if enabled {
		fmt.Fprintln(os.Stdout, "mwan-opnsense is enabled")
		return 0
	}
	fmt.Fprintln(os.Stderr, "mwan-opnsense is disabled")
	return 1
}

func checkRCEnabled(name, rcSubr string) (bool, error) {
	script := strings.Join([]string{
		`rc_subr_path="$1"`,
		`service_name="$2"`,
		`if [ ! -r "${rc_subr_path}" ]; then exit 2; fi`,
		`. "${rc_subr_path}"`,
		`name="${service_name}"`,
		`rcvar="${name}_enable"`,
		`load_rc_config "${name}" >/dev/null 2>&1`,
		`checkyesno "${rcvar}"`,
	}, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), rcEnabledCheckTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", script, "mwan-opnsense-is-enabled", rcSubr, name)
	output, err := command.CombinedOutput()
	if err == nil {
		return true, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 1:
			return false, nil
		case 2:
			return false, fmt.Errorf("rc.subr not readable: %s", rcSubr)
		default:
			message := strings.TrimSpace(string(output))
			if message == "" {
				message = "no output"
			}
			return false, fmt.Errorf("rc.subr check failed with exit %d: %s", exitErr.ExitCode(), message)
		}
	}
	slog.Error("daemon is-enabled: rc.subr check failed", "err", err, "rc_subr", rcSubr)
	return false, fmt.Errorf("run rc.subr check: %w", err)
}
