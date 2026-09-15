package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/opnsensectl/internal/notify"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type recordedNotify struct {
	Kind  string
	Key   string
	Level slog.Level
	Msg   string
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []recordedNotify
}

func (f *fakeNotifier) Notify(_ context.Context, ev notify.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedNotify{Kind: ev.Kind, Key: ev.Key, Level: ev.Level, Msg: ev.Message})
}

func (f *fakeNotifier) Resolve(_ context.Context, kind, key, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedNotify{Kind: kind, Key: key, Msg: msg})
}

func (f *fakeNotifier) snapshot() []recordedNotify {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedNotify, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeNotifier) kinds() []string {
	out := make([]string, 0, len(f.events))
	for _, e := range f.snapshot() {
		out = append(out, e.Kind)
	}
	return out
}

type snapshotCall struct {
	VMID string
	Snap string
}

type fakeSnap struct {
	mu sync.Mutex

	snapErr     error
	rollbackErr error
	startErr    error
	delErr      error
	listing     []byte
	running     bool
	statusErr   error

	snapshots   []snapshotCall
	rollbacks   []snapshotCall
	deletes     []snapshotCall
	starts      []string
	statusCalls []string
}

func (s *fakeSnap) VMSnapshot(_ context.Context, vmid, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, snapshotCall{VMID: vmid, Snap: name})
	return s.snapErr
}

func (s *fakeSnap) VMRollback(_ context.Context, vmid, snap string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollbacks = append(s.rollbacks, snapshotCall{VMID: vmid, Snap: snap})
	return s.rollbackErr
}

func (s *fakeSnap) VMSnapshots(_ context.Context, _ string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listing, nil
}

func (s *fakeSnap) VMDelSnapshot(_ context.Context, vmid, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, snapshotCall{VMID: vmid, Snap: name})
	return s.delErr
}

func (s *fakeSnap) VMStart(_ context.Context, vmid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, vmid)
	return s.startErr
}

func (s *fakeSnap) VMStatus(_ context.Context, vmid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCalls = append(s.statusCalls, vmid)
	return s.running, s.statusErr
}

type execCall struct {
	VMID string
	Args []string
}

// fakeExec is the guest behind the Executor seam. The prepare captures
// (cat, ifconfig, netstat, vtysh) answer from byCommand, and every
// firmware command answers from the firmware model.
type fakeExec struct {
	mu sync.Mutex

	calls []execCall

	// byArgv overrides the result for one exact argv joined by spaces.
	byArgv map[string]GuestExecResult
	// byCommand overrides the result for every argv whose argv[0]
	// matches.
	byCommand map[string]GuestExecResult
	// errByCommand mirrors byCommand for the error return.
	errByCommand map[string]error
	// firmware answers every other command the way an OPNsense guest does.
	firmware *fakeFirmware
}

func (e *fakeExec) GuestExec(_ context.Context, vmid string, args ...string) (GuestExecResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, execCall{VMID: vmid, Args: append([]string(nil), args...)})
	if len(args) == 0 {
		return GuestExecResult{}, errors.New("fakeExec: empty argv")
	}
	if res, ok := e.byArgv[strings.Join(args, " ")]; ok {
		return res, nil
	}
	if res, ok := e.byCommand[args[0]]; ok {
		return res, e.errByCommand[args[0]]
	}
	return e.firmware.exec(args)
}

func (e *fakeExec) argvs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.calls))
	for _, c := range e.calls {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// fakeFirmware models the firmware commands of an OPNsense guest. Outputs
// and exit codes follow opnsense/update src/update/opnsense-update.sh.in
// and the opnsense/core firmware scripts, and each command that installs
// software or reboots changes the modelled state the way the guest would.
type fakeFirmware struct {
	corePackage      string
	coreVersion      string
	coreAvailable    string
	updaterVersion   string
	updaterAvailable string
	baseVersion      string
	kernelVersion    string
	alwaysReboot     bool

	// packageUpdateExit is the exit code of opnsense-update -p. A failed
	// pkg upgrade exits 1 (opnsense-update.sh.in:786-790).
	packageUpdateExit int
	// coreStuck leaves the core package at its installed version even
	// though the package update exits 0.
	coreStuck bool
	// bootsOldSets brings the guest back on the base and kernel it ran
	// before opnsense-update -bk installed new ones.
	bootsOldSets bool

	// bootID is the running boot's kern.boot_id as sysctl -x dumps it.
	bootID string
	// bootSeconds is the kern.boottime seconds of the running boot.
	bootSeconds int64
	// unseededBootIDReads is how many kern.boot_id reads fail on a new boot
	// before the kernel's random pool is seeded, since the handler returns
	// ENXIO until then (sys/kern/kern_mib.c:527-529).
	unseededBootIDReads int
	// clockStepSeconds steps the old boot's clock forward when shutdown -r
	// +0 runs, as ntpd -g can. A clock step recomputes kern.boottime
	// (sys/kern/kern_tc.c:1293-1311), so the old boot reports a later boot
	// time without rebooting.
	clockStepSeconds int64
	// oldBootCommands is how many commands the guest still answers from
	// the old boot after shutdown -r +0 returns, because FreeBSD shutdown
	// exits before rc.shutdown stops the exec daemon.
	oldBootCommands int
	// unreachableCommands is how many commands fail after that while the
	// guest is down.
	unreachableCommands int
	// rebootIgnored keeps the guest on its old boot after shutdown -r +0.
	rebootIgnored bool

	rebooting       bool
	oldBootLeft     int
	unreachableLeft int
	unseededLeft    int
	stagedRelease   string
	oldBase         string
	oldKernel       string
	mutations       []string
	reboots         int
}

// The testbed router's kern.boottime and kern.boot_id as read on
// 2026-09-14, and the argv that reads each.
const (
	testbedBootSeconds      = 1786126956
	testbedBootMicroseconds = 209257
	testbedBootID           = "2b1d732b2c1b8134cf3b7161650b01d8"
	bootSecondsPerReboot    = 600
	fakeBootTimeArgv        = "/sbin/sysctl -n kern.boottime"
	fakeBootIDArgv          = "/sbin/sysctl -nx kern.boot_id"
)

func bootTimeOutput(seconds int64) string {
	return fmt.Sprintf("{ sec = %d, usec = %d } %s\n", seconds, testbedBootMicroseconds,
		time.Unix(seconds, 0).UTC().Format("Mon Jan _2 15:04:05 2006"))
}

// productionHotfix is the production router on 2026-09-14: core package
// 26.7.3_8 installed, 26.7.3_11 offered, and no new base or kernel.
func productionHotfix() *fakeFirmware {
	return &fakeFirmware{
		corePackage:      "opnsense",
		coreVersion:      "26.7.3_8",
		coreAvailable:    "26.7.3_11",
		updaterVersion:   "26.7.3",
		updaterAvailable: "26.7.3",
		baseVersion:      "26.7.3",
		kernelVersion:    "26.7.3",
		bootID:           testbedBootID,
		bootSeconds:      testbedBootSeconds,
	}
}

const pkgCatalogueOutput = "Updating OPNsense repository catalogue...\n" +
	"Fetching meta.conf: . done\n" +
	"Fetching data.pkg: .......... done\n" +
	"Processing entries: .......... done\n" +
	"OPNsense repository update completed. 874 packages processed.\n" +
	"All repositories are up to date.\n"

func fwOK(stdout string) (GuestExecResult, error) {
	return GuestExecResult{ExitCode: 0, Stdout: stdout, Stderr: ""}, nil
}

func (f *fakeFirmware) exec(args []string) (GuestExecResult, error) {
	command := strings.Join(args, " ")
	if f.rebooting {
		switch {
		case f.oldBootLeft > 0:
			f.oldBootLeft--
		case f.unreachableLeft > 0:
			f.unreachableLeft--
			return GuestExecResult{}, errors.New("grpc Exec " + args[0] + ": rpc error: code = Unavailable desc = connection refused")
		default:
			f.boot()
		}
	}
	switch command {
	case guestTrue:
		return fwOK("")
	case fakeBootTimeArgv:
		return fwOK(bootTimeOutput(f.bootSeconds))
	case fakeBootIDArgv:
		return f.readBootID()
	case guestVersion:
		return fwOK("OPNsense " + f.coreVersion + " (amd64)\n")
	case guestVersion + " -n":
		return fwOK(f.corePackage + "\n")
	case guestPkg + " update":
		return fwOK(pkgCatalogueOutput)
	case guestPkg + " query %v " + f.corePackage:
		return fwOK(f.coreVersion + "\n")
	case guestPkg + " rquery %v " + f.corePackage:
		return fwOK(f.coreAvailable + "\n")
	case guestPkg + " rquery %v opnsense-update":
		return fwOK(f.updaterAvailable + "\n")
	case guestPkg + " query %n-%v":
		return fwOK(fmt.Sprintf("opnsense-%s\nopnsense-update-%s\nos-frr-1.45\n", f.coreVersion, f.updaterVersion))
	case guestPluginctl + " -g system.firmware.reboot":
		if f.alwaysReboot {
			return fwOK("1\n")
		}
		return fwOK("\n")
	case guestUpdater + " -v":
		return fwOK(stripPackageRevision(f.updaterVersion) + "\n")
	case guestUpdater + " -vb":
		return fwOK(f.baseVersion + "\n")
	case guestUpdater + " -vk":
		return fwOK(f.kernelVersion + "\n")
	case guestUpdater + " -bk -c":
		return f.checkSets()
	case guestUpdater + " -bk":
		return f.installSets()
	case guestUpdater + " -p -t " + f.corePackage:
		return f.updatePackages()
	case guestWebGUI:
		f.mutations = append(f.mutations, command)
		return fwOK("")
	case guestShutdown + " -r +0":
		return f.reboot()
	}
	if len(args) == 5 && args[0] == guestPkg && args[1] == "version" && args[2] == "-t" {
		return fwOK(pkgVersionOrder(args[3], args[4]) + "\n")
	}
	if len(args) == 4 && args[0] == guestUpdater && args[1] == "-u" && args[2] == "-r" {
		return f.stageRelease(args[3])
	}
	return GuestExecResult{ExitCode: 127, Stdout: "", Stderr: args[0] + ": not found\n"},
		fmt.Errorf("fakeFirmware: unexpected command %q", command)
}

// checkSets mirrors opnsense-update -bk -c: exit 0 when the base or
// kernel release differs from the installed one, 1 otherwise, with no
// output (opnsense-update.sh.in:686-728).
func (f *fakeFirmware) checkSets() (GuestExecResult, error) {
	release := stripPackageRevision(f.updaterVersion)
	if release != f.baseVersion || release != f.kernelVersion {
		return fwOK("")
	}
	return GuestExecResult{ExitCode: 1, Stdout: "", Stderr: ""}, nil
}

// installSets mirrors opnsense-update -bk outside an upgrade: fetch,
// install kernel then base, clean obsolete files, ask for a reboot
// (opnsense-update.sh.in:917-942, 1119-1122, 1181-1221, 1240-1242).
func (f *fakeFirmware) installSets() (GuestExecResult, error) {
	f.mutations = append(f.mutations, "opnsense-update -bk")
	release := stripPackageRevision(f.updaterVersion)
	f.oldBase = f.baseVersion
	f.oldKernel = f.kernelVersion
	f.baseVersion = release
	f.kernelVersion = release
	return fwOK(fmt.Sprintf("Fetching base-%[1]s-amd64.txz: ..... done\n"+
		"Fetching kernel-%[1]s-amd64.txz: ..... done\n"+
		"!!!!!!!!!!!! ATTENTION !!!!!!!!!!!!!!!\n"+
		"! A critical upgrade is in progress. !\n"+
		"! Please do not turn off the system. !\n"+
		"!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n"+
		"Installing kernel-%[1]s-amd64.txz... done\n"+
		"Installing base-%[1]s-amd64.txz... done\n"+
		"Cleaning obsolete files... done\n"+
		"Please reboot.\n", release))
}

// updatePackages mirrors opnsense-update -p: pkg update and pkg upgrade,
// exit 1 on failure (opnsense-update.sh.in:768-805).
func (f *fakeFirmware) updatePackages() (GuestExecResult, error) {
	f.mutations = append(f.mutations, "opnsense-update -p")
	if f.packageUpdateExit != 0 {
		return GuestExecResult{
			ExitCode: f.packageUpdateExit,
			Stdout: "Updating OPNsense repository catalogue...\n" +
				"pkg-static: https://pkg.opnsense.org/FreeBSD:14:amd64/26.7/latest/meta.txz: No address record\n" +
				"Unable to update repository OPNsense\n" +
				"Error updating repositories!\n" +
				"Flushing temporary package files... done\n",
			Stderr: "",
		}, nil
	}
	from := f.coreVersion
	if !f.coreStuck {
		f.coreVersion = f.coreAvailable
	}
	f.updaterVersion = f.updaterAvailable
	return fwOK(fmt.Sprintf("Updating OPNsense repository catalogue...\n"+
		"OPNsense repository is up to date.\n"+
		"All repositories are up to date.\n"+
		"Checking for upgrades (2 candidates): .. done\n"+
		"Processing candidates (2 candidates): .. done\n"+
		"Installed packages to be UPGRADED:\n"+
		"\topnsense: %[1]s -> %[2]s [OPNsense]\n\n"+
		"[1/1] Upgrading opnsense from %[1]s to %[2]s...\n"+
		"Checking integrity... done (0 conflicting)\n"+
		"Flushing temporary package files... done\n", from, f.coreAvailable))
}

// stageRelease mirrors opnsense-update -u -r: fetch and stage the
// release sets for the next boot (opnsense-update.sh.in:1125-1179).
func (f *fakeFirmware) stageRelease(release string) (GuestExecResult, error) {
	f.mutations = append(f.mutations, "opnsense-update -u -r "+release)
	f.stagedRelease = release
	return fwOK(fmt.Sprintf("Fetching packages-%[1]s-amd64.tar: ..... done\n"+
		"Fetching base-%[1]s-amd64.txz: ..... done\n"+
		"Fetching kernel-%[1]s-amd64.txz: ..... done\n"+
		"Flushing temporary package files... done\n"+
		"Extracting packages-%[1]s-amd64.tar... done\n"+
		"Extracting base-%[1]s-amd64.txz... done\n"+
		"Extracting kernel-%[1]s-amd64.txz... done\n"+
		"Please reboot.\n", release))
}

// reboot starts the shutdown and drops the exec channel, which surfaces as
// a transport error on the shutdown call. The guest boots once the old
// boot and unreachable commands are used up.
func (f *fakeFirmware) reboot() (GuestExecResult, error) {
	f.mutations = append(f.mutations, "shutdown -r +0")
	f.reboots++
	f.bootSeconds += f.clockStepSeconds
	if !f.rebootIgnored {
		f.rebooting = true
		f.oldBootLeft = f.oldBootCommands
		f.unreachableLeft = f.unreachableCommands
	}
	return GuestExecResult{}, errors.New("grpc Exec shutdown: rpc error: code = Unavailable desc = transport is closing")
}

// boot starts a new boot with a new boot ID and a later boot time, and
// applies staged release sets.
func (f *fakeFirmware) boot() {
	f.rebooting = false
	f.bootID = fmt.Sprintf("%032x", f.reboots)
	f.unseededLeft = f.unseededBootIDReads
	f.bootSeconds += bootSecondsPerReboot
	if f.stagedRelease != "" {
		f.coreVersion = f.stagedRelease
		f.updaterVersion = f.stagedRelease
		f.baseVersion = f.stagedRelease
		f.kernelVersion = f.stagedRelease
		f.stagedRelease = ""
	}
	if f.bootsOldSets && f.oldBase != "" {
		f.baseVersion = f.oldBase
		f.kernelVersion = f.oldKernel
	}
}

// readBootID mirrors sysctl -nx kern.boot_id: a hex dump of the 16-byte
// boot ID, or a failure before the new boot's random pool is seeded.
func (f *fakeFirmware) readBootID() (GuestExecResult, error) {
	if f.unseededLeft > 0 {
		f.unseededLeft--
		return GuestExecResult{ExitCode: 1, Stdout: "", Stderr: "sysctl: kern.boot_id: Device not configured\n"}, nil
	}
	return fwOK("Format: Length:16 Dump:0x" + f.bootID + "\n")
}

// pkgVersionOrder compares two package versions numerically by their
// dot and underscore separated components, the way pkg version -t
// prints "<", "=", or ">".
func pkgVersionOrder(left, right string) string {
	split := func(v string) []string {
		return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' })
	}
	leftParts := split(left)
	rightParts := split(right)
	for i := 0; i < len(leftParts) || i < len(rightParts); i++ {
		leftNum := 0
		rightNum := 0
		if i < len(leftParts) {
			leftNum, _ = strconv.Atoi(leftParts[i])
		}
		if i < len(rightParts) {
			rightNum, _ = strconv.Atoi(rightParts[i])
		}
		if leftNum < rightNum {
			return "<"
		}
		if leftNum > rightNum {
			return ">"
		}
	}
	return "="
}

type fakeValidator struct {
	result ValidationResult
	err    error
	calls  int
}

func (v *fakeValidator) Validate(_ context.Context, _ ValidateContext) (ValidationResult, error) {
	v.calls++
	return v.result, v.err
}

type fixedClock struct {
	t time.Time
}

func (c fixedClock) Now() time.Time { return c.t }

func newDeps(t *testing.T) (Deps, *fakeNotifier, *fakeSnap, *fakeExec, *fakeValidator) {
	t.Helper()
	n := &fakeNotifier{}
	s := &fakeSnap{}
	x := &fakeExec{
		byArgv: map[string]GuestExecResult{},
		byCommand: map[string]GuestExecResult{
			guestCat:      {ExitCode: 0, Stdout: "<config/>"},
			guestIfconfig: {ExitCode: 0, Stdout: "lo0: flags=...\n"},
			guestNetstat:  {ExitCode: 0, Stdout: "Routing tables\n"},
			guestVtysh:    {ExitCode: 0, Stdout: "{}\n"},
		},
		errByCommand: map[string]error{},
		firmware:     productionHotfix(),
	}
	v := &fakeValidator{result: AggregateChecks([]CheckResult{{Name: "qga_responsive", Pass: true}})}
	deps := Deps{
		Snap:     s,
		Exec:     x,
		Validate: v,
		Notifier: n,
		Clock:    fixedClock{t: time.Unix(1_700_000_000, 0)},
		Log:      slog.New(slog.NewTextHandler(testWriter{t: t}, nil)),
	}
	return deps, n, s, x, v
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func newOpts(t *testing.T, vmid string) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		VMID:                vmid,
		Target:              "26.7",
		StateDir:            dir,
		UpgradeTimeout:      5 * time.Second,
		PostRollbackTimeout: 1 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// state machine tests
// ---------------------------------------------------------------------------

func TestCanTransitionAllowsDocumentedEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from Phase
		to   Phase
		want bool
	}{
		{PhaseEmpty, PhasePrepared, true},
		{PhasePrepared, PhaseExecuting, true},
		{PhaseExecuting, PhaseExecuted, true},
		{PhaseExecuted, PhaseValidatedPass, true},
		{PhaseValidatedPass, PhaseCommitted, true},
		{PhaseValidatedFail, PhaseRolledBack, true},
		{PhaseRolledBack, PhaseCommitted, true},
		{PhaseValidatedPass, PhaseRolledBack, false},
		{PhaseCommitted, PhasePrepared, false},
		{PhaseRollbackFailed, PhaseRolledBack, false},
	}
	for _, tc := range cases {
		got := CanTransition(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("CanTransition(%s -> %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestEnforceTransitionReturnsTypedError(t *testing.T) {
	t.Parallel()
	err := EnforceTransition(PhaseCommitted, PhasePrepared)
	var typed TransitionNotAllowedError
	if !errors.As(err, &typed) {
		t.Fatalf("expected TransitionNotAllowedError, got %v", err)
	}
	if typed.From != PhaseCommitted || typed.To != PhasePrepared {
		t.Fatalf("error fields = %+v", typed)
	}
}

func TestSnapshotNameAndIsUpgradeSnapshot(t *testing.T) {
	t.Parallel()
	name := SnapshotName(time.Unix(1_700_000_000, 0))
	if !strings.HasPrefix(name, SnapshotPrefix) {
		t.Fatalf("snapshot name %q missing prefix", name)
	}
	if !IsUpgradeSnapshot(name) {
		t.Fatalf("IsUpgradeSnapshot(%q) = false", name)
	}
	if IsUpgradeSnapshot("pre-deploy-1700000000") {
		t.Fatalf("watchdog snapshot accepted as upgrade snapshot")
	}
	if IsUpgradeSnapshot(KeepPrefix + "1700000000") {
		t.Fatalf("kept snapshot must not match IsUpgradeSnapshot")
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := State{
		VMID: "101", DeployID: "abc", Target: "26.7", Snapshot: "pre-upgrade-26x-1",
		Phase: PhasePrepared, UpdatedAt: time.Time{}, FailingCheck: nil,
	}
	if err := saveStateCtx(context.Background(), dir, st, time.Unix(1, 0)); err != nil {
		t.Fatalf("saveStateCtx: %v", err)
	}
	loaded, err := loadStateCtx(context.Background(), dir, "101")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.Phase != PhasePrepared || loaded.DeployID != "abc" {
		t.Fatalf("loaded = %+v", loaded)
	}
	missing, err := loadStateCtx(context.Background(), dir, "999")
	if err != nil {
		t.Fatalf("LoadState missing: %v", err)
	}
	if missing.Phase != PhaseEmpty {
		t.Fatalf("missing state phase = %q, want empty", missing.Phase)
	}
}

// ---------------------------------------------------------------------------
// prepare
// ---------------------------------------------------------------------------

func TestPrepareTakesSnapshotAndWritesState(t *testing.T) {
	t.Parallel()
	deps, n, s, _, _ := newDeps(t)
	opts := newOpts(t, "101")

	st, err := Prepare(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if st.Phase != PhasePrepared {
		t.Fatalf("phase = %q, want %q", st.Phase, PhasePrepared)
	}
	if len(s.snapshots) != 1 {
		t.Fatalf("snapshot calls = %d, want 1", len(s.snapshots))
	}
	if !strings.HasPrefix(s.snapshots[0].Snap, SnapshotPrefix) {
		t.Fatalf("snapshot name = %q", s.snapshots[0].Snap)
	}
	if !ContainsKind(n.kinds(), KindPrepare) {
		t.Fatalf("expected prepare kind, got %v", n.kinds())
	}
	if st.DeployID == "" {
		t.Fatalf("DeployID empty")
	}
	if _, statErr := readJSON(filepath.Join(opts.StateDir, "101", st.DeployID, "metadata.json")); statErr != nil {
		t.Fatalf("metadata.json missing: %v", statErr)
	}
}

func TestPrepareSnapshotFailureDoesNotWriteState(t *testing.T) {
	t.Parallel()
	deps, n, s, _, _ := newDeps(t)
	opts := newOpts(t, "101")
	s.snapErr = errors.New("snapshot exploded")

	st, err := Prepare(context.Background(), deps, opts)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if st.Phase == PhasePrepared {
		t.Fatalf("phase should not be prepared after snapshot error")
	}
	loaded, _ := loadStateCtx(context.Background(), opts.StateDir, "101")
	if loaded.Phase == PhasePrepared {
		t.Fatalf("state file must not be left at prepared after snapshot error")
	}
	if !ContainsKind(n.kinds(), KindPrepare) {
		t.Fatalf("expected prepare error notify, got %v", n.kinds())
	}
}

// ---------------------------------------------------------------------------
// execute
// ---------------------------------------------------------------------------

func TestExecuteHappyPathReachesExecuted(t *testing.T) {
	t.Parallel()
	deps, _, _, _, _ := newDeps(t)
	opts := newOpts(t, "101")
	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st, err := Execute(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st.Phase != PhaseExecuted {
		t.Fatalf("phase = %q, want executed", st.Phase)
	}
}

func TestExecuteNonZeroExitTransitionsToExecuteFailed(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.packageUpdateExit = 1
	opts := newOpts(t, "101")
	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st, err := Execute(context.Background(), deps, opts)
	if err == nil {
		t.Fatalf("expected error")
	}
	if st.Phase != PhaseExecuteFailed {
		t.Fatalf("phase = %q, want execute_failed", st.Phase)
	}
	for _, argv := range x.argvs() {
		if argv == guestUpdater+" -bk" || argv == guestShutdown+" -r +0" {
			t.Fatalf("ran %q after the package update failed", argv)
		}
	}
}

func TestExecuteRebootWaitCancelledTransitionsToExecuteFailed(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	x.firmware.coreAvailable = "26.7.4"
	x.firmware.updaterAvailable = "26.7.4"
	opts := newOpts(t, "101")

	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The guest ignores the reboot, so every boot ID probe returns the
	// old boot and the wait falls through to its select. Run Execute under
	// a context that expires quickly so the select fires ctx.Done() rather
	// than the 2-second pause.
	x.firmware.rebootIgnored = true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	st, err := Execute(ctx, deps, opts)
	if err == nil {
		t.Fatalf("expected error from the cancelled reboot wait")
	}
	if st.Phase != PhaseExecuteFailed {
		t.Fatalf("phase = %q, want execute_failed", st.Phase)
	}
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func TestValidatePassRecordsValidatedPass(t *testing.T) {
	t.Parallel()
	deps, _, _, _, _ := newDeps(t)
	opts := newOpts(t, "101")
	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	st, res, err := Validate(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !res.AllPass {
		t.Fatalf("AllPass = false")
	}
	if st.Phase != PhaseValidatedPass {
		t.Fatalf("phase = %q", st.Phase)
	}
}

func TestValidateFailRecordsValidatedFail(t *testing.T) {
	t.Parallel()
	deps, _, _, _, v := newDeps(t)
	v.result = AggregateChecks([]CheckResult{{Name: "qga_responsive", Pass: false}})
	opts := newOpts(t, "101")
	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	st, _, err := Validate(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if st.Phase != PhaseValidatedFail {
		t.Fatalf("phase = %q", st.Phase)
	}
	if len(st.FailingCheck) == 0 {
		t.Fatalf("expected failing check names recorded")
	}
}

func TestValidatePartialAcceptedTransitionsToPartial(t *testing.T) {
	t.Parallel()
	deps, _, _, _, v := newDeps(t)
	v.result = AggregateChecks([]CheckResult{
		{Name: "qga_responsive", Pass: true},
		{Name: "frr_state", Pass: false},
	})
	opts := newOpts(t, "101")
	opts.AcceptPartial = true
	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	st, _, err := Validate(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if st.Phase != PhaseValidatedPartial {
		t.Fatalf("phase = %q, want validated_partial", st.Phase)
	}
}

// ---------------------------------------------------------------------------
// rollback
// ---------------------------------------------------------------------------

func TestRollbackOnValidateFailRestoresSnapshot(t *testing.T) {
	t.Parallel()
	deps, _, s, _, v := newDeps(t)
	v.result = AggregateChecks([]CheckResult{{Name: "qga_responsive", Pass: false}})
	s.running = true
	opts := newOpts(t, "101")

	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, _, err := Validate(context.Background(), deps, opts); err != nil {
		t.Fatalf("validate: %v", err)
	}

	st, err := Rollback(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if st.Phase != PhaseRolledBack {
		t.Fatalf("phase = %q", st.Phase)
	}
	if len(s.rollbacks) != 1 {
		t.Fatalf("rollback calls = %d", len(s.rollbacks))
	}
}

func TestRollbackFailureMarksRollbackFailed(t *testing.T) {
	t.Parallel()
	deps, n, s, _, v := newDeps(t)
	v.result = AggregateChecks([]CheckResult{{Name: "qga_responsive", Pass: false}})
	s.rollbackErr = errors.New("rollback exploded")
	opts := newOpts(t, "101")

	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, _, err := Validate(context.Background(), deps, opts); err != nil {
		t.Fatalf("validate: %v", err)
	}

	st, err := Rollback(context.Background(), deps, opts)
	if err == nil {
		t.Fatalf("expected error from failed rollback")
	}
	if st.Phase != PhaseRollbackFailed {
		t.Fatalf("phase = %q, want rollback_failed", st.Phase)
	}
	if !ContainsKind(n.kinds(), KindRollbackFailed) {
		t.Fatalf("expected rollback-failed notify, got %v", n.kinds())
	}
}

// ---------------------------------------------------------------------------
// commit
// ---------------------------------------------------------------------------

func TestCommitDeletesSnapshotAfterValidatedPass(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	opts := newOpts(t, "101")

	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, _, err := Validate(context.Background(), deps, opts); err != nil {
		t.Fatalf("validate: %v", err)
	}

	st, err := Commit(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if st.Phase != PhaseCommitted {
		t.Fatalf("phase = %q", st.Phase)
	}
	if len(s.deletes) == 0 {
		t.Fatalf("expected snapshot delete on commit")
	}
}

func TestCommitFromBadPhaseRefuses(t *testing.T) {
	t.Parallel()
	deps, _, _, _, _ := newDeps(t)
	opts := newOpts(t, "101")
	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	_, err := Commit(context.Background(), deps, opts)
	var typed TransitionNotAllowedError
	if !errors.As(err, &typed) {
		t.Fatalf("expected transition not allowed, got %v", err)
	}
}

func TestCommitIdempotent(t *testing.T) {
	t.Parallel()
	deps, _, _, _, _ := newDeps(t)
	opts := newOpts(t, "101")

	if _, err := Prepare(context.Background(), deps, opts); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, _, err := Validate(context.Background(), deps, opts); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := Commit(context.Background(), deps, opts); err != nil {
		t.Fatalf("commit: %v", err)
	}
	st, err := Commit(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("commit second: %v", err)
	}
	if st.Phase != PhaseCommitted {
		t.Fatalf("phase = %q", st.Phase)
	}
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func TestRunHappyPathReachesValidatedPass(t *testing.T) {
	t.Parallel()
	deps, _, _, x, _ := newDeps(t)
	opts := newOpts(t, "101")
	out, err := Run(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Reached != PhaseValidatedPass {
		t.Fatalf("reached = %q, want validated_pass", out.Reached)
	}
	if out.AutoRollback {
		t.Fatalf("auto rollback fired on happy path")
	}
	if x.firmware.coreVersion != "26.7.3_11" {
		t.Fatalf("core version = %q, want the hotfix 26.7.3_11 applied", x.firmware.coreVersion)
	}
	if x.firmware.reboots != 0 {
		t.Fatalf("reboots = %d, want 0 for a package-only hotfix", x.firmware.reboots)
	}
}

func TestRunValidateFailAutoRolls(t *testing.T) {
	t.Parallel()
	deps, _, s, _, v := newDeps(t)
	v.result = AggregateChecks([]CheckResult{{Name: "frr_state", Pass: false}})
	s.running = true
	opts := newOpts(t, "101")

	out, err := Run(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.AutoRollback {
		t.Fatalf("auto rollback did not fire on validate fail")
	}
	if out.Reached != PhaseRolledBack {
		t.Fatalf("reached = %q, want rolled_back", out.Reached)
	}
}

// ---------------------------------------------------------------------------
// gc
// ---------------------------------------------------------------------------

func TestGCDeletesOldSnapshotsAndKeepsYoung(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	now := time.Unix(1_800_000_000, 0)
	deps.Clock = fixedClock{t: now}
	old := SnapshotName(now.Add(-30 * 24 * time.Hour))
	young := SnapshotName(now.Add(-1 * time.Hour))
	listing := []byte(" `-> " + old + " 2026-04-01 desc\n `-> " + young + " 2026-04-08 desc\n")
	s.listing = listing
	opts := newOpts(t, "101")

	res, err := GC(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != old {
		t.Fatalf("deleted = %v, want [%s]", res.Deleted, old)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != young {
		t.Fatalf("skipped = %v, want [%s]", res.Skipped, young)
	}
}

func TestGCDryRunReportsWithoutDeleting(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	now := time.Unix(1_800_000_000, 0)
	deps.Clock = fixedClock{t: now}
	old := SnapshotName(now.Add(-30 * 24 * time.Hour))
	listing := []byte(" `-> " + old + " 2026-04-01 desc\n")
	s.listing = listing
	opts := newOpts(t, "101")
	opts.DryRunGC = true

	res, err := GC(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("GC dry-run: %v", err)
	}
	// Dry-run reports the snapshot as "would be deleted" but must not call VMDelSnapshot.
	if len(res.Deleted) != 1 || res.Deleted[0] != old {
		t.Fatalf("dry-run deleted = %v, want [%s]", res.Deleted, old)
	}
	if len(s.deletes) != 0 {
		t.Fatalf("dry-run must not call VMDelSnapshot, got %d calls", len(s.deletes))
	}
}

func TestGCSkipsActiveDeploySnapshot(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	now := time.Unix(1_800_000_000, 0)
	deps.Clock = fixedClock{t: now}

	// Prepare lands a state file with an active snapshot.
	opts := newOpts(t, "101")
	st, err := Prepare(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	activeSnap := st.Snapshot

	// Put the active snapshot in the listing, old enough that gc would
	// otherwise delete it.
	old := activeSnap
	listing := []byte(" `-> " + old + " 2026-01-01 desc\n")
	s.listing = listing

	// Force the timestamp embedded in the snapshot name to appear old
	// by advancing the clock far past DefaultGCThreshold.
	deps.Clock = fixedClock{t: time.Unix(1_800_000_000+int64(30*24*time.Hour/time.Second), 0)}

	res, err := GC(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	// The active snapshot must be in Skipped, not Deleted.
	if len(res.Deleted) != 0 {
		t.Fatalf("GC deleted active snapshot: deleted = %v", res.Deleted)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != old {
		t.Fatalf("GC skipped = %v, want [%s]", res.Skipped, old)
	}
	if len(s.deletes) != 0 {
		t.Fatalf("GC must not call VMDelSnapshot for active deploy, got %d calls", len(s.deletes))
	}
}

func TestGCCommittedDeploySnapshotIsEligible(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	now := time.Unix(1_800_000_000, 0)
	deps.Clock = fixedClock{t: now}

	// Run through the full pipeline to land at PhaseCommitted.
	opts := newOpts(t, "101")
	st, err := Prepare(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	committedSnap := st.Snapshot
	if _, err := Execute(context.Background(), deps, opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, _, err := Validate(context.Background(), deps, opts); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := Commit(context.Background(), deps, opts); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Commit deletes the snapshot; re-add it to the listing so gc sees it.
	s.deletes = nil
	oldEnough := committedSnap
	listing := []byte(" `-> " + oldEnough + " 2026-01-01 desc\n")
	s.listing = listing
	deps.Clock = fixedClock{t: time.Unix(1_800_000_000+int64(30*24*time.Hour/time.Second), 0)}

	res, err := GC(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	// Phase is committed so the snapshot is not protected; gc must delete it.
	if len(res.Deleted) != 1 || res.Deleted[0] != oldEnough {
		t.Fatalf("deleted = %v, want [%s]", res.Deleted, oldEnough)
	}
}

func TestGCVMSnapshotsErrorReturnsError(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	s.listing = nil
	// Override VMSnapshots to return an error by using a custom fakeSnap.
	errSnap := &errSnapshotter{err: errors.New("qm: connection refused")}
	deps.Snap = errSnap
	opts := newOpts(t, "101")

	_, err := GC(context.Background(), deps, opts)
	if err == nil {
		t.Fatalf("expected error from VMSnapshots failure")
	}
	if !strings.Contains(err.Error(), "VMSnapshots") {
		t.Fatalf("error = %v, want VMSnapshots in message", err)
	}
}

func TestGCDelSnapshotErrorSkipsInsteadOfAborting(t *testing.T) {
	t.Parallel()
	deps, _, s, _, _ := newDeps(t)
	now := time.Unix(1_800_000_000, 0)
	deps.Clock = fixedClock{t: now}
	old1 := SnapshotName(now.Add(-30 * 24 * time.Hour))
	old2 := SnapshotName(now.Add(-20 * 24 * time.Hour))
	listing := []byte(" `-> " + old1 + " 2026-01-01 desc\n `-> " + old2 + " 2026-01-11 desc\n")
	s.listing = listing
	// Make VMDelSnapshot always fail.
	s.delErr = errors.New("pve: snapshot locked")
	opts := newOpts(t, "101")

	res, err := GC(context.Background(), deps, opts)
	if err != nil {
		t.Fatalf("GC must not return error when VMDelSnapshot fails: %v", err)
	}
	// Both old snapshots land in Skipped; none in Deleted.
	if len(res.Deleted) != 0 {
		t.Fatalf("deleted = %v, want empty", res.Deleted)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("skipped = %v, want 2 entries", res.Skipped)
	}
}

// errSnapshotter is a Snapshotter stub whose VMSnapshots always returns an error.
// It is used only by gc error-path tests in this file.
type errSnapshotter struct {
	err error
}

func (e *errSnapshotter) VMSnapshot(_ context.Context, _, _ string) error { return nil }
func (e *errSnapshotter) VMRollback(_ context.Context, _, _ string) error { return nil }
func (e *errSnapshotter) VMSnapshots(_ context.Context, _ string) ([]byte, error) {
	return nil, e.err
}
func (e *errSnapshotter) VMDelSnapshot(_ context.Context, _, _ string) error { return nil }
func (e *errSnapshotter) VMStart(_ context.Context, _ string) error          { return nil }
func (e *errSnapshotter) VMStatus(_ context.Context, _ string) (bool, error) { return false, nil }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readJSON(path string) ([]byte, error) {
	// Path is constructed from t.TempDir output plus the operator-set
	// VMID and DeployID, so gosec G304 does not apply here.
	return os.ReadFile(path) //nolint:gosec
}

// ContainsKind reports whether any of the given notify kinds match
// the wanted kind, case-insensitively. Lives in the test file because
// only the unit tests use it; production code does not introspect the
// emitted kind list.
func ContainsKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if strings.EqualFold(k, want) {
			return true
		}
	}
	return false
}
