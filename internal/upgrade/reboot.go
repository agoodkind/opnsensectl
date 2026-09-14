package upgrade

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// The reboot wait proves the guest came back on a new boot by reading the
// kernel's boot ID. FreeBSD shutdown forks and its parent exits 0 before
// rc.shutdown runs (sbin/shutdown/shutdown.c:249-253), so a guest that
// answers right after shutdown -r can still be the boot that is going down.
// The kernel picks the boot ID at random once per boot
// (sys/kern/kern_mib.c:519-540). kern.boottime cannot stand in for it,
// because a clock step recomputes the boot time (sys/kern/kern_tc.c:1293-1311)
// and can move the old boot's value later without a reboot.
const (
	guestSysctl  = "/sbin/sysctl"
	sysctlBootID = "kern.boot_id"
	// sysctlHexDump makes sysctl print the opaque boot ID as hex; -n alone
	// prints nothing for it.
	sysctlHexDump = "-nx"
	// rebootPollInterval is the pause between boot ID probes.
	rebootPollInterval = 2 * time.Second
	// rebootProbeTimeout bounds one probe, so a guest that stops answering
	// mid-call cannot hold the wait past its deadline.
	rebootProbeTimeout = 30 * time.Second
)

// bootIDPattern matches sysctl -nx kern.boot_id output, for example
// "Format: Length:16 Dump:0x2b1d732b2c1b8134cf3b7161650b01d8".
var bootIDPattern = regexp.MustCompile(`^Format: Length:16 Dump:0x([0-9a-f]{32})$`)

// rebootWait bounds the wait for a new boot: the whole wait, the pause
// between probes, and each probe.
type rebootWait struct {
	timeout      time.Duration
	interval     time.Duration
	probeTimeout time.Duration
}

func defaultRebootWait() rebootWait {
	return rebootWait{
		timeout:      DefaultPostRebootTimeout,
		interval:     rebootPollInterval,
		probeTimeout: rebootProbeTimeout,
	}
}

// rebootGuest reboots the guest and waits until it answers with a boot ID
// other than the one read before the reboot. The guest closes the exec
// channel as it shuts down, so the shutdown command's own error is
// expected and only recorded in upgrade.log.
func rebootGuest(ctx context.Context, clk Clock, r *guestRunner, wait rebootWait) error {
	before, err := readBootID(ctx, r)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade.Execute: read boot id before reboot failed", "err", err, "vmid", r.vmid)
		return fmt.Errorf("read boot id before reboot: %w", err)
	}
	_, _ = r.run(ctx, "shutdown", "-r", "+0")
	slog.InfoContext(ctx, "upgrade.Execute: reboot issued, waiting for a new boot",
		"vmid", r.vmid, "boot_id", before)
	if _, err := waitForReboot(ctx, clk, r.exec, r.vmid, before, wait); err != nil {
		slog.ErrorContext(ctx, "upgrade.Execute: guest did not return after reboot", "err", err, "vmid", r.vmid)
		return fmt.Errorf("post-reboot wait: %w", err)
	}
	return nil
}

// readBootID reads the guest's boot ID through the logged runner. The
// reboot step reads it before shutdown, so a failure there stops the
// reboot instead of leaving nothing to compare against.
func readBootID(ctx context.Context, r *guestRunner) (string, error) {
	out, err := r.value(ctx, guestSysctl, sysctlHexDump, sysctlBootID)
	if err != nil {
		return "", err
	}
	return parseBootID(ctx, out)
}

// parseBootID extracts the hex boot ID from sysctl -nx kern.boot_id output.
func parseBootID(ctx context.Context, out string) (string, error) {
	match := bootIDPattern.FindStringSubmatch(strings.TrimSpace(out))
	if match == nil {
		err := fmt.Errorf("%s: unrecognised output %q", sysctlBootID, out)
		slog.DebugContext(ctx, "upgrade: boot id output not recognised", "err", err)
		return "", err
	}
	return match[1], nil
}

// waitForReboot polls the guest's boot ID until it differs from before,
// and returns the new boot ID. A failed probe or the old boot ID means the
// guest has not finished rebooting yet. The deadline comes from clk; it
// returns an error once the deadline passes or ctx ends.
func waitForReboot(
	ctx context.Context,
	clk Clock,
	exec Executor,
	vmid string,
	before string,
	wait rebootWait,
) (string, error) {
	start := clk.Now()
	deadline := start.Add(wait.timeout)
	var bootID string
	attempt := 0
	for {
		attempt++
		probed, answered := probeBootID(ctx, exec, vmid, attempt, wait.probeTimeout)
		if answered && probed != before {
			bootID = probed
			break
		}
		if answered {
			slog.DebugContext(ctx, "upgrade: guest still reports the boot before the reboot",
				"vmid", vmid, "attempt", attempt, "boot_id", probed)
		}
		if !clk.Now().Before(deadline) {
			timedErr := fmt.Errorf("boot id did not change from %s within %s", before, wait.timeout)
			slog.WarnContext(ctx, "upgrade: guest did not reach a new boot",
				"err", timedErr, "vmid", vmid, "attempts", attempt)
			return "", timedErr
		}
		select {
		case <-ctx.Done():
			cancelErr := fmt.Errorf("wait for new boot: %w", ctx.Err())
			slog.WarnContext(ctx, "upgrade: reboot wait cancelled",
				"err", cancelErr, "vmid", vmid, "attempts", attempt)
			return "", cancelErr
		case <-time.After(wait.interval):
		}
	}
	slog.InfoContext(ctx, "upgrade: guest is back on a new boot",
		"vmid", vmid, "attempts", attempt, "boot_id_before", before,
		"boot_id_after", bootID, "waited", clk.Now().Sub(start))
	return bootID, nil
}

// probeBootID reads the boot ID once without writing upgrade.log, since the
// wait can probe hundreds of times while the guest is down. It reports
// false, after logging why at debug level, when the guest did not answer
// with a boot ID. That includes a new boot whose random pool is not seeded
// yet, where the read fails with ENXIO (sys/kern/kern_mib.c:527-529).
func probeBootID(
	ctx context.Context, exec Executor, vmid string, attempt int, probeTimeout time.Duration,
) (string, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res, err := exec.GuestExec(probeCtx, vmid, guestSysctl, sysctlHexDump, sysctlBootID)
	if err != nil {
		slog.DebugContext(ctx, "upgrade: boot id probe failed",
			"err", err, "vmid", vmid, "attempt", attempt)
		return "", false
	}
	if res.ExitCode != 0 {
		slog.DebugContext(ctx, "upgrade: boot id probe exited non-zero",
			"vmid", vmid, "attempt", attempt, "exit", res.ExitCode,
			"stderr", strings.TrimSpace(res.Stderr))
		return "", false
	}
	bootID, err := parseBootID(ctx, res.Stdout)
	if err != nil {
		return "", false
	}
	return bootID, true
}
