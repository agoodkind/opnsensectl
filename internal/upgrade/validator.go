package upgrade

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
)

// The validate phase trusts the checks the rest of the system already relies
// on: the dual-stack host egress probe the mwan watchdog and the deploy gate
// run, the host bridge answering the daemon's Version RPC, and the router
// running a command over the Exec RPC.
const (
	checkEgressIPv4    = "egress_ipv4"
	checkEgressIPv6    = "egress_ipv6"
	checkDaemonVersion = "daemon_version"
	checkExecHostname  = "exec_hostname"

	pingBinaryIPv4 = "ping"
	pingBinaryIPv6 = "ping6"
	// hostPingTimeout caps one ping binary run, the same cap the watchdog's
	// host probe uses.
	hostPingTimeout = 20 * time.Second
	// defaultEgressWindow bounds the egress retries, matching the watchdog's
	// 60 second connectivity timeout.
	defaultEgressWindow = 60 * time.Second
	// egressPollInterval is the pause between egress probe cycles.
	egressPollInterval = 2 * time.Second
	// daemonRPCTimeout bounds each host bridge call.
	daemonRPCTimeout = 30 * time.Second
	// execRoundTripCommand is the command `mwan opnsense exec` runs to prove
	// the channel end to end.
	execRoundTripCommand = "/bin/hostname"
	// execRoundTripTimeoutSeconds is the daemon-side timeout for that command.
	execRoundTripTimeoutSeconds int32 = 30
)

// Pinger probes one target from the Proxmox host. bin is ping or ping6.
type Pinger interface {
	Ping(ctx context.Context, bin, target string) bool
}

// ExecPinger runs the host ping binary with two packets and a three second
// wait per packet, the probe the mwan watchdog runs, and reports whether the
// binary exited 0.
type ExecPinger struct{}

// Ping runs bin against target.
func (ExecPinger) Ping(ctx context.Context, bin, target string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, hostPingTimeout)
	defer cancel()
	if err := exec.CommandContext(probeCtx, bin, "-c", "2", "-W", "3", target).Run(); err != nil {
		slog.DebugContext(ctx, "upgrade: host ping failed", "err", err, "bin", bin, "target", target)
		return false
	}
	return true
}

// DaemonRPCClient is the host bridge surface the validate phase needs: the
// Version RPC that `mwan opnsense daemon version` calls and the Exec RPC.
// *opnsense.RPC satisfies it.
type DaemonRPCClient interface {
	OPNsenseRPCClient
	Version(ctx context.Context, req *mwanv1.VersionRequest) (*mwanv1.VersionResponse, error)
}

// DaemonDialer opens a host bridge client for one validate run and returns the
// function that closes it.
type DaemonDialer func(ctx context.Context) (DaemonRPCClient, func() error, error)

// HealthValidator satisfies [Validator] with four checks: IPv4 and IPv6 egress
// from the Proxmox host, the daemon's version answer, and an exec round trip.
// Each check is one [CheckResult], so a failure names what is down.
type HealthValidator struct {
	dial         DaemonDialer
	pinger       Pinger
	clock        Clock
	targetIPv4   string
	targetIPv6   string
	egressWindow time.Duration
	pollInterval time.Duration
}

var _ Validator = (*HealthValidator)(nil)

// NewHealthValidator returns the production validator. It pings targetIPv4
// and targetIPv6 with the host ping binaries and dials the host bridge with
// dial on every run.
func NewHealthValidator(dial DaemonDialer, targetIPv4, targetIPv6 string) *HealthValidator {
	return &HealthValidator{
		dial:         dial,
		pinger:       ExecPinger{},
		clock:        realClock{},
		targetIPv4:   targetIPv4,
		targetIPv6:   targetIPv6,
		egressWindow: defaultEgressWindow,
		pollInterval: egressPollInterval,
	}
}

// Validate runs every check. A check that fails is a failed CheckResult, not
// an error, so a router that does not answer drives the phase to
// validated_fail and Run rolls it back.
func (v *HealthValidator) Validate(ctx context.Context, vctx ValidateContext) (ValidationResult, error) {
	ipv4OK, ipv6OK, cycles := v.probeEgress(ctx)
	checks := []CheckResult{
		egressCheck(checkEgressIPv4, "IPv4", v.targetIPv4, ipv4OK, cycles, v.egressWindow),
		egressCheck(checkEgressIPv6, "IPv6", v.targetIPv6, ipv6OK, cycles, v.egressWindow),
	}
	checks = append(checks, v.daemonChecks(ctx, vctx.VMID)...)
	result := AggregateChecks(checks)
	slog.InfoContext(ctx, "upgrade: health checks complete",
		"vmid", vctx.VMID, "all_pass", result.AllPass, "egress_cycles", cycles)
	return result, nil
}

// probeEgress pings both targets every cycle until both have answered or the
// window closes. A family that answered once inside the window counts as up.
func (v *HealthValidator) probeEgress(ctx context.Context) (bool, bool, int) {
	clk := clockOrDefault(v.clock)
	deadline := clk.Now().Add(v.egressWindow)
	ipv4OK := false
	ipv6OK := false
	cycles := 0
	for {
		cycles++
		if v.pinger.Ping(ctx, pingBinaryIPv6, v.targetIPv6) {
			ipv6OK = true
		}
		if v.pinger.Ping(ctx, pingBinaryIPv4, v.targetIPv4) {
			ipv4OK = true
		}
		if ipv4OK && ipv6OK {
			return ipv4OK, ipv6OK, cycles
		}
		if !clk.Now().Before(deadline) {
			return ipv4OK, ipv6OK, cycles
		}
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "upgrade: egress probe cancelled", "err", ctx.Err(), "cycles", cycles)
			return ipv4OK, ipv6OK, cycles
		case <-time.After(v.pollInterval):
		}
	}
}

func egressCheck(
	name, family, target string, answered bool, cycles int, window time.Duration,
) CheckResult {
	if answered {
		return CheckResult{
			Name: name,
			Pass: true,
			Note: fmt.Sprintf("%s target %s answered a ping from the host", family, target),
		}
	}
	return CheckResult{
		Name: name,
		Pass: false,
		Note: fmt.Sprintf("%s target %s did not answer a ping from the host within %s (%d probe cycles)",
			family, target, window, cycles),
	}
}

// daemonChecks dials the host bridge and runs the version and exec checks.
// A failed dial fails both checks.
func (v *HealthValidator) daemonChecks(ctx context.Context, vmid string) []CheckResult {
	client, closeClient, err := v.dial(ctx)
	if err != nil {
		note := fmt.Sprintf("dial host bridge: %v", err)
		slog.WarnContext(ctx, "upgrade: host bridge dial failed", "err", err, "vmid", vmid)
		return []CheckResult{
			{Name: checkDaemonVersion, Pass: false, Note: note},
			{Name: checkExecHostname, Pass: false, Note: note},
		}
	}
	defer func() {
		if closeErr := closeClient(); closeErr != nil {
			slog.WarnContext(ctx, "upgrade: close host bridge client", "err", closeErr, "vmid", vmid)
		}
	}()
	return []CheckResult{
		daemonVersionCheck(ctx, client, vmid),
		execRoundTripCheck(ctx, client, vmid),
	}
}

func daemonVersionCheck(ctx context.Context, client DaemonRPCClient, vmid string) CheckResult {
	callCtx, cancel := context.WithTimeout(ctx, daemonRPCTimeout)
	defer cancel()
	resp, err := client.Version(callCtx, &mwanv1.VersionRequest{})
	if err != nil {
		slog.WarnContext(ctx, "upgrade: daemon version failed", "err", err, "vmid", vmid)
		return CheckResult{Name: checkDaemonVersion, Pass: false, Note: fmt.Sprintf("Version RPC: %v", err)}
	}
	if resp == nil {
		return CheckResult{Name: checkDaemonVersion, Pass: false, Note: "Version RPC returned no response"}
	}
	return CheckResult{
		Name: checkDaemonVersion,
		Pass: true,
		Note: fmt.Sprintf("version=%s commit=%s dirty=%t binhash=%s",
			resp.GetVersion(), resp.GetBuildCommit(), resp.GetBuildDirty(), resp.GetBuildBinhash()),
	}
}

func execRoundTripCheck(ctx context.Context, client DaemonRPCClient, vmid string) CheckResult {
	callCtx, cancel := context.WithTimeout(ctx, daemonRPCTimeout)
	defer cancel()
	res, err := client.Exec(callCtx, execRoundTripCommand, nil, false, execRoundTripTimeoutSeconds, nil)
	if err != nil {
		slog.WarnContext(ctx, "upgrade: exec round trip failed", "err", err, "vmid", vmid)
		return CheckResult{Name: checkExecHostname, Pass: false, Note: fmt.Sprintf("Exec %s: %v", execRoundTripCommand, err)}
	}
	if res == nil {
		return CheckResult{Name: checkExecHostname, Pass: false, Note: "Exec RPC returned no result"}
	}
	if res.TimedOut {
		return CheckResult{Name: checkExecHostname, Pass: false, Note: execRoundTripCommand + " timed out on the router"}
	}
	if res.ExitCode != 0 {
		return CheckResult{
			Name: checkExecHostname,
			Pass: false,
			Note: fmt.Sprintf("%s exited %d: %s", execRoundTripCommand, res.ExitCode, strings.TrimSpace(string(res.Stderr))),
		}
	}
	return CheckResult{Name: checkExecHostname, Pass: true, Note: strings.TrimSpace(string(res.Stdout))}
}
