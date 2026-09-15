package upgrade

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
)

// The probes are keyed by binary and target, so a probe that sends a target
// to the other family's binary does not answer.
const (
	pingIPv4Probe = "ping 1.1.1.1"
	pingIPv6Probe = "ping6 2606:4700:4700::1111"
)

// fakeHostPinger is the host behind the Pinger seam. down keeps a probe from
// ever answering, and answersFrom makes a probe answer only from that call on.
type fakeHostPinger struct {
	mu          sync.Mutex
	down        map[string]bool
	answersFrom map[string]int
	calls       map[string]int
}

func newFakeHostPinger() *fakeHostPinger {
	return &fakeHostPinger{
		down:        map[string]bool{},
		answersFrom: map[string]int{},
		calls:       map[string]int{},
	}
}

func (p *fakeHostPinger) Ping(_ context.Context, bin, target string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	probe := bin + " " + target
	p.calls[probe]++
	if probe != pingIPv4Probe && probe != pingIPv6Probe {
		return false
	}
	if p.down[probe] {
		return false
	}
	return p.calls[probe] >= p.answersFrom[probe]
}

// fakeDaemon is the host bridge behind DaemonRPCClient.
type fakeDaemon struct {
	versionErr error
	execResult *ExecResult
	execErr    error
	execArgv   []string
	closed     bool
}

func (d *fakeDaemon) Version(_ context.Context, _ *mwanv1.VersionRequest) (*mwanv1.VersionResponse, error) {
	if d.versionErr != nil {
		return nil, d.versionErr
	}
	return &mwanv1.VersionResponse{Version: "0.0.0-test", BuildCommit: "7a86cca"}, nil
}

func (d *fakeDaemon) Exec(
	_ context.Context, command string, args []string, _ bool, _ int32, _ []byte,
) (*ExecResult, error) {
	d.execArgv = append([]string{command}, args...)
	return d.execResult, d.execErr
}

func hostnameAnswer() *ExecResult {
	return &ExecResult{Stdout: []byte("router.testbed\n"), ExitCode: 0}
}

func newTestHealthValidator(pinger Pinger, daemon *fakeDaemon) *HealthValidator {
	return &HealthValidator{
		dial: func(context.Context) (DaemonRPCClient, func() error, error) {
			return daemon, func() error {
				daemon.closed = true
				return nil
			}, nil
		},
		pinger:       pinger,
		clock:        &steppingClock{now: time.Unix(1_700_000_000, 0), step: time.Second},
		targetIPv4:   "1.1.1.1",
		targetIPv6:   "2606:4700:4700::1111",
		egressWindow: 5 * time.Second,
		pollInterval: time.Millisecond,
	}
}

func TestHealthValidatorChecks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		setup       func(pinger *fakeHostPinger, daemon *fakeDaemon)
		wantFailing []string
		// wantIPv6Probes, when non-zero, is how many IPv6 probes must run.
		wantIPv6Probes int
	}{
		{
			name:        "all checks pass",
			setup:       func(*fakeHostPinger, *fakeDaemon) {},
			wantFailing: nil,
		},
		{
			name:        "IPv4 egress down",
			setup:       func(pinger *fakeHostPinger, _ *fakeDaemon) { pinger.down[pingIPv4Probe] = true },
			wantFailing: []string{checkEgressIPv4},
		},
		{
			name:        "IPv6 egress down",
			setup:       func(pinger *fakeHostPinger, _ *fakeDaemon) { pinger.down[pingIPv6Probe] = true },
			wantFailing: []string{checkEgressIPv6},
		},
		{
			name:           "IPv6 egress returns inside the window",
			setup:          func(pinger *fakeHostPinger, _ *fakeDaemon) { pinger.answersFrom[pingIPv6Probe] = 3 },
			wantFailing:    nil,
			wantIPv6Probes: 3,
		},
		{
			name: "daemon version fails",
			setup: func(_ *fakeHostPinger, daemon *fakeDaemon) {
				daemon.versionErr = errors.New("rpc error: code = Unavailable desc = connection refused")
			},
			wantFailing: []string{checkDaemonVersion},
		},
		{
			name: "exec transport fails",
			setup: func(_ *fakeHostPinger, daemon *fakeDaemon) {
				daemon.execResult = nil
				daemon.execErr = errors.New("rpc error: code = Unavailable desc = transport is closing")
			},
			wantFailing: []string{checkExecHostname},
		},
		{
			name: "exec exits non-zero",
			setup: func(_ *fakeHostPinger, daemon *fakeDaemon) {
				daemon.execResult = &ExecResult{Stderr: []byte("hostname: not found\n"), ExitCode: 127}
			},
			wantFailing: []string{checkExecHostname},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pinger := newFakeHostPinger()
			daemon := &fakeDaemon{execResult: hostnameAnswer()}
			tc.setup(pinger, daemon)

			result, err := newTestHealthValidator(pinger, daemon).Validate(
				context.Background(), ValidateContext{VMID: "101"})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}

			if got := failingCheckNames(result); !slices.Equal(got, tc.wantFailing) {
				t.Fatalf("failing checks = %v, want %v; checks = %+v", got, tc.wantFailing, result.Checks)
			}
			if result.AllPass != (len(tc.wantFailing) == 0) {
				t.Fatalf("AllPass = %t with failing checks %v", result.AllPass, tc.wantFailing)
			}
			if len(result.Checks) != 4 {
				t.Fatalf("checks = %+v, want four", result.Checks)
			}
			if argv := strings.Join(daemon.execArgv, " "); argv != "/bin/hostname" {
				t.Fatalf("exec argv = %q, want /bin/hostname", argv)
			}
			if !daemon.closed {
				t.Fatalf("host bridge client was not closed")
			}
			if tc.wantIPv6Probes != 0 && pinger.calls[pingIPv6Probe] != tc.wantIPv6Probes {
				t.Fatalf("IPv6 probes = %d, want %d", pinger.calls[pingIPv6Probe], tc.wantIPv6Probes)
			}
		})
	}
}

func TestHealthValidatorFailsDaemonChecksWhenTheBridgeDoesNotDial(t *testing.T) {
	t.Parallel()
	validator := newTestHealthValidator(newFakeHostPinger(), &fakeDaemon{execResult: hostnameAnswer()})
	validator.dial = func(context.Context) (DaemonRPCClient, func() error, error) {
		return nil, nil, errors.New("opnsense: only unix:// targets supported")
	}

	result, err := validator.Validate(context.Background(), ValidateContext{VMID: "101"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	want := []string{checkDaemonVersion, checkExecHostname}
	if got := failingCheckNames(result); !slices.Equal(got, want) {
		t.Fatalf("failing checks = %v, want %v", got, want)
	}
}

// TestRunRollsBackWhenHostEgressFails drives the unattended pipeline with the
// production validator and IPv6 egress down, so validate fails and Run must
// roll the router back.
func TestRunRollsBackWhenHostEgressFails(t *testing.T) {
	t.Parallel()
	deps, _, snap, _, _ := newDeps(t)
	snap.running = true
	pinger := newFakeHostPinger()
	pinger.down[pingIPv6Probe] = true
	deps.Validate = newTestHealthValidator(pinger, &fakeDaemon{execResult: hostnameAnswer()})
	opts := newOpts(t, "101")

	out, err := Run(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !out.AutoRollback {
		t.Fatalf("auto rollback did not fire; validation = %+v", out.Validation)
	}
	if out.Reached != PhaseRolledBack {
		t.Fatalf("reached = %q, want rolled_back", out.Reached)
	}
	if len(snap.rollbacks) != 1 {
		t.Fatalf("rollback calls = %d, want 1", len(snap.rollbacks))
	}
	if !slices.Equal(failingCheckNames(out.Validation), []string{checkEgressIPv6}) {
		t.Fatalf("failing checks = %v, want [%s]", failingCheckNames(out.Validation), checkEgressIPv6)
	}
}
