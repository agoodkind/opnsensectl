package upgrade

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// steppingClock advances by step on every read, so a wait's deadline
// passes after a known number of reads without real time passing.
type steppingClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.now
	c.now = c.now.Add(c.step)
	return current
}

func testRebootWait() rebootWait {
	return rebootWait{timeout: DefaultPostRebootTimeout, interval: time.Millisecond, probeTimeout: time.Second}
}

func newRebootRunner(x *fakeExec) *guestRunner {
	return &guestRunner{exec: x, vmid: "101", logPath: "", log: nil}
}

// bootIDProbesAfterShutdown counts the kern.boot_id reads issued after
// shutdown -r +0.
func bootIDProbesAfterShutdown(x *fakeExec) int {
	argvs := x.argvs()
	shutdownAt := slices.Index(argvs, guestShutdown+" -r +0")
	if shutdownAt < 0 {
		return 0
	}
	count := 0
	for _, argv := range argvs[shutdownAt+1:] {
		if argv == fakeBootIDArgv {
			count++
		}
	}
	return count
}

func TestRebootGuestWaitsForBootIDToChange(t *testing.T) {
	t.Parallel()
	_, _, _, x, _ := newDeps(t)
	x.firmware.stagedRelease = "27.1"
	x.firmware.oldBootCommands = 3
	x.firmware.unreachableCommands = 2
	x.firmware.unseededBootIDReads = 1
	clk := &steppingClock{now: time.Unix(1_700_000_000, 0), step: time.Second}
	runner := newRebootRunner(x)

	if err := rebootGuest(context.Background(), clk, runner, testRebootWait()); err != nil {
		t.Fatalf("rebootGuest: %v", err)
	}
	if x.firmware.bootID == testbedBootID {
		t.Fatalf("boot id = %s, want the new boot's id", x.firmware.bootID)
	}
	if probes := bootIDProbesAfterShutdown(x); probes != 7 {
		t.Fatalf("boot id probes after shutdown = %d, want 7 (3 old boot, 2 unreachable, 1 unseeded, 1 new boot)", probes)
	}
	post, err := captureFirmwareState(context.Background(), runner)
	if err != nil {
		t.Fatalf("captureFirmwareState after reboot: %v", err)
	}
	if post.CoreVersion != "27.1" {
		t.Fatalf("core version after reboot = %q, want the staged 27.1, not the old boot's state", post.CoreVersion)
	}
}

func TestRebootGuestTimesOutWhenBootIDNeverChanges(t *testing.T) {
	t.Parallel()
	_, _, _, x, _ := newDeps(t)
	x.firmware.rebootIgnored = true
	clk := &steppingClock{now: time.Unix(1_700_000_000, 0), step: time.Minute}

	err := rebootGuest(context.Background(), clk, newRebootRunner(x), testRebootWait())
	if err == nil {
		t.Fatalf("rebootGuest succeeded although the guest never left boot %s", x.firmware.bootID)
	}
	if !strings.Contains(err.Error(), "did not change") {
		t.Fatalf("error %q does not report the unchanged boot id", err)
	}
	if x.firmware.reboots != 1 {
		t.Fatalf("reboots = %d, want 1", x.firmware.reboots)
	}
	if probes := bootIDProbesAfterShutdown(x); probes < 2 {
		t.Fatalf("boot id probes after shutdown = %d, want several before the timeout", probes)
	}
}

func TestRebootGuestDoesNotPassOnClockStepWithoutReboot(t *testing.T) {
	t.Parallel()
	_, _, _, x, _ := newDeps(t)
	x.firmware.rebootIgnored = true
	x.firmware.clockStepSeconds = 3600
	clk := &steppingClock{now: time.Unix(1_700_000_000, 0), step: time.Minute}

	err := rebootGuest(context.Background(), clk, newRebootRunner(x), testRebootWait())
	if x.firmware.bootSeconds <= testbedBootSeconds {
		t.Fatalf("kern.boottime = %d, want the clock step to move it past %d", x.firmware.bootSeconds, testbedBootSeconds)
	}
	if err == nil {
		t.Fatalf("rebootGuest passed on the old boot %s after a clock step moved its boot time later", x.firmware.bootID)
	}
	if !strings.Contains(err.Error(), "did not change") {
		t.Fatalf("error %q does not report the unchanged boot id", err)
	}
}

func TestExecuteDoesNotRebootWhenBootIDCaptureFails(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	// Each clock read passes a whole reboot timeout, so a reboot issued by
	// mistake ends in one probe instead of waiting on a frozen clock.
	deps.Clock = &steppingClock{now: time.Unix(1_700_000_000, 0), step: DefaultPostRebootTimeout}
	x.firmware.coreAvailable = "26.7.4"
	x.firmware.updaterAvailable = "26.7.4"
	x.byArgv[fakeBootIDArgv] = GuestExecResult{
		ExitCode: 1, Stdout: "", Stderr: "sysctl: kern.boot_id: Device not configured\n",
	}
	opts := newOpts(t, "101")

	st, _, err := prepareAndExecute(t, deps, opts)
	if err == nil {
		t.Fatalf("Execute succeeded although the boot id could not be read")
	}
	if st.Phase != PhaseExecuteFailed {
		t.Fatalf("phase = %q, want execute_failed", st.Phase)
	}
	if !strings.Contains(err.Error(), "read boot id before reboot") {
		t.Fatalf("error %q does not name the boot id read", err)
	}
	if x.firmware.reboots != 0 || slices.Contains(x.argvs(), guestShutdown+" -r +0") {
		t.Fatalf("rebooted without a boot id to compare: %v", x.argvs())
	}
}
