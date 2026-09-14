package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Firmware artefacts record the installed OPNsense firmware before and
// after execute, plus the change execute found pending. Execute compares
// them to decide whether the update landed, and the operator reads the
// plan to see what a dry run would apply.
const (
	// ArtefactFirmwarePre holds the installed firmware state prepare captured.
	ArtefactFirmwarePre = "firmware.pre.json"
	// ArtefactFirmwarePlan holds the pending change execute found.
	ArtefactFirmwarePlan = "firmware.plan.json"
	// ArtefactFirmwarePost holds what execute applied and the state it left.
	ArtefactFirmwarePost = "firmware.post.json"
	// artefactUpgradeLog records every guest command execute ran.
	artefactUpgradeLog = "upgrade.log"
)

// Guest commands the firmware flow runs. The pkg, pluginctl, and web GUI
// paths are the ones the OPNsense firmware scripts use
// (opnsense/core src/opnsense/scripts/firmware/config.sh:36 and
// update.sh:47,57).
const (
	guestPkg       = "/usr/local/sbin/pkg"
	guestPluginctl = "/usr/local/sbin/pluginctl"
	guestWebGUI    = "/usr/local/etc/rc.restart_webgui"
	guestUpdater   = "opnsense-update"
	guestVersion   = "opnsense-version"
	updaterPackage = "opnsense-update"
	rebootSetting  = "system.firmware.reboot"
	pkgOlder       = "<"
	settingEnabled = "1"
)

// firmwareMode names the OPNsense update path execute takes.
type firmwareMode string

const (
	// firmwareModeUpdate applies the pending package, base, and kernel
	// updates inside the installed release series, as the web
	// interface's update action does.
	firmwareModeUpdate firmwareMode = "update"
	// firmwareModeMajor upgrades to the release series Target names.
	firmwareModeMajor firmwareMode = "major"
)

// firmwareState is the installed firmware identity: the core package
// name and version, and the base and kernel set versions.
type firmwareState struct {
	CorePackage   string `json:"core_package"`
	CoreVersion   string `json:"core_version"`
	BaseVersion   string `json:"base_version"`
	KernelVersion string `json:"kernel_version"`
}

// firmwarePlan is the change execute found pending before it applied
// anything. A dry run writes it and stops.
type firmwarePlan struct {
	Mode           firmwareMode  `json:"mode"`
	Target         string        `json:"target"`
	DryRun         bool          `json:"dry_run"`
	Installed      firmwareState `json:"installed"`
	CoreAvailable  string        `json:"core_available"`
	CorePending    bool          `json:"core_pending"`
	SetsAvailable  string        `json:"sets_available"`
	BasePending    bool          `json:"base_pending"`
	KernelPending  bool          `json:"kernel_pending"`
	AlwaysReboot   bool          `json:"always_reboot"`
	RebootRequired bool          `json:"reboot_required"`
}

// firmwareResult is what execute applied and the firmware state the
// guest reported afterwards.
type firmwareResult struct {
	SetsApplied     bool          `json:"sets_applied"`
	SetsRelease     string        `json:"sets_release"`
	PackagesChanged bool          `json:"packages_changed"`
	RebootRequired  bool          `json:"reboot_required"`
	Post            firmwareState `json:"post"`
}

// guestRunner runs firmware commands through the Executor. When logPath
// is set, every invocation is appended to that file as it completes, so
// an interrupted update still leaves a readable trail.
type guestRunner struct {
	exec    Executor
	vmid    string
	logPath string
	log     []byte
}

// run executes one guest command. A transport error comes back wrapped
// with the command text; a non-zero exit is not an error here.
func (r *guestRunner) run(ctx context.Context, args ...string) (GuestExecResult, error) {
	res, err := r.exec.GuestExec(ctx, r.vmid, args...)
	if r.logPath != "" {
		r.log = fmt.Appendf(r.log, "argv=%v\nexit=%d\nerr=%v\nstdout:\n%s\nstderr:\n%s\n",
			args, res.ExitCode, err, res.Stdout, res.Stderr)
		if writeErr := WriteFileBytes(ctx, r.logPath, r.log); writeErr != nil {
			slog.WarnContext(ctx, "upgrade: write upgrade.log failed", "err", writeErr)
		}
	}
	if err != nil {
		return res, fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return res, nil
}

// output runs a command that must exit 0 and returns its trimmed stdout.
func (r *guestRunner) output(ctx context.Context, args ...string) (string, error) {
	command := strings.Join(args, " ")
	res, err := r.run(ctx, args...)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: guest command failed",
			"err", err, "vmid", r.vmid, "command", command)
		return "", err
	}
	if res.ExitCode != 0 {
		exitErr := fmt.Errorf("%s: exit=%d stderr=%s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
		slog.ErrorContext(ctx, "upgrade: guest command non-zero exit",
			"err", exitErr, "vmid", r.vmid, "exit", res.ExitCode)
		return "", exitErr
	}
	return strings.TrimSpace(res.Stdout), nil
}

// value runs a command that must exit 0 and print a non-empty value.
func (r *guestRunner) value(ctx context.Context, args ...string) (string, error) {
	out, err := r.output(ctx, args...)
	if err != nil {
		return "", err
	}
	if out == "" {
		emptyErr := fmt.Errorf("%s: empty output", strings.Join(args, " "))
		slog.ErrorContext(ctx, "upgrade: guest command printed nothing",
			"err", emptyErr, "vmid", r.vmid)
		return "", emptyErr
	}
	return out, nil
}

// captureFirmwareState reads the installed core package, base, and
// kernel versions. opnsense-version -n names the core package
// (opnsense/core src/sbin/opnsense-version:66-68), and opnsense-update
// -vb and -vk print the installed base and kernel versions
// (opnsense/update src/update/opnsense-update.sh.in:466-471).
func captureFirmwareState(ctx context.Context, r *guestRunner) (firmwareState, error) {
	corePackage, err := r.value(ctx, guestVersion, "-n")
	if err != nil {
		return emptyFirmwareState(), err
	}
	coreVersion, err := r.value(ctx, guestPkg, "query", "%v", corePackage)
	if err != nil {
		return emptyFirmwareState(), err
	}
	baseVersion, err := r.value(ctx, guestUpdater, "-vb")
	if err != nil {
		return emptyFirmwareState(), err
	}
	kernelVersion, err := r.value(ctx, guestUpdater, "-vk")
	if err != nil {
		return emptyFirmwareState(), err
	}
	return firmwareState{
		CorePackage:   corePackage,
		CoreVersion:   coreVersion,
		BaseVersion:   baseVersion,
		KernelVersion: kernelVersion,
	}, nil
}

// captureFirmware writes firmware.pre.json during prepare. Execute
// cannot tell whether an update landed without it, so a failed capture
// aborts prepare before the snapshot.
func captureFirmware(ctx context.Context, deps Deps, opts Options, deployDir string) error {
	if deps.Exec == nil {
		slog.ErrorContext(ctx, "upgrade.Prepare: capture firmware: deps.Exec missing",
			"err", errCaptureExecMissing)
		return errCaptureExecMissing
	}
	runner := &guestRunner{exec: deps.Exec, vmid: opts.VMID, logPath: "", log: nil}
	state, err := captureFirmwareState(ctx, runner)
	if err != nil {
		return fmt.Errorf("upgrade.Prepare: capture firmware: %w", err)
	}
	if err := writeFirmwareState(ctx, filepath.Join(deployDir, ArtefactFirmwarePre), state); err != nil {
		return fmt.Errorf("upgrade.Prepare: write firmware state: %w", err)
	}
	slog.InfoContext(ctx, "upgrade.Prepare: captured firmware state",
		"vmid", opts.VMID, "core_package", state.CorePackage, "core_version", state.CoreVersion,
		"base_version", state.BaseVersion, "kernel_version", state.KernelVersion)
	return nil
}

// planFirmwareChange decides the update path and finds what is pending.
// A Target in another release series than the installed core package is
// a major upgrade; any other Target, including none, applies the updates
// pending inside the installed series.
func planFirmwareChange(ctx context.Context, r *guestRunner, pre firmwareState, opts Options) (firmwarePlan, error) {
	if isMajorUpgrade(opts.Target, pre.CoreVersion) {
		return majorPlan(pre, opts), nil
	}
	return probeUpdatePlan(ctx, r, pre, opts)
}

// majorPlan describes a release upgrade. It replaces the package, base,
// and kernel sets and always needs a reboot (opnsense/core
// src/opnsense/scripts/firmware/upgrade.sh:32-38).
func majorPlan(pre firmwareState, opts Options) firmwarePlan {
	return firmwarePlan{
		Mode:           firmwareModeMajor,
		Target:         opts.Target,
		DryRun:         opts.DryRunExecute,
		Installed:      pre,
		CoreAvailable:  "",
		CorePending:    true,
		SetsAvailable:  opts.Target,
		BasePending:    true,
		KernelPending:  true,
		AlwaysReboot:   false,
		RebootRequired: true,
	}
}

// probeUpdatePlan finds the pending update without installing anything.
// It refreshes the package catalogue and compares the installed and
// available core package with pkg version -t, as the firmware reboot
// check does (opnsense/core src/opnsense/scripts/firmware/reboot.sh:31,49-58).
// The base and kernel sets follow the opnsense-update package version
// without its revision, so a newer one in the repository means new sets
// (check.sh:309-318). A reboot is needed when base or kernel change, or
// when the always-reboot setting is on and packages change
// (update.sh:47,72-82).
func probeUpdatePlan(ctx context.Context, r *guestRunner, pre firmwareState, opts Options) (firmwarePlan, error) {
	if _, err := r.output(ctx, guestPkg, "update"); err != nil {
		return emptyFirmwarePlan(), err
	}
	coreAvailable, err := r.value(ctx, guestPkg, "rquery", "%v", pre.CorePackage)
	if err != nil {
		return emptyFirmwarePlan(), err
	}
	order, err := r.value(ctx, guestPkg, "version", "-t", pre.CoreVersion, coreAvailable)
	if err != nil {
		return emptyFirmwarePlan(), err
	}
	updaterAvailable, err := r.value(ctx, guestPkg, "rquery", "%v", updaterPackage)
	if err != nil {
		return emptyFirmwarePlan(), err
	}
	alwaysReboot, err := readAlwaysReboot(ctx, r)
	if err != nil {
		return emptyFirmwarePlan(), err
	}
	setsAvailable := stripPackageRevision(updaterAvailable)
	plan := firmwarePlan{
		Mode:           firmwareModeUpdate,
		Target:         opts.Target,
		DryRun:         opts.DryRunExecute,
		Installed:      pre,
		CoreAvailable:  coreAvailable,
		CorePending:    order == pkgOlder,
		SetsAvailable:  setsAvailable,
		BasePending:    setsAvailable != pre.BaseVersion,
		KernelPending:  setsAvailable != pre.KernelVersion,
		AlwaysReboot:   alwaysReboot,
		RebootRequired: false,
	}
	plan.RebootRequired = plan.BasePending || plan.KernelPending || (plan.AlwaysReboot && plan.CorePending)
	return plan, nil
}

// readAlwaysReboot reads the "always reboot after update" setting. The
// web interface reads it without checking pluginctl's exit status
// (update.sh:47), so only a transport error fails here.
func readAlwaysReboot(ctx context.Context, r *guestRunner) (bool, error) {
	res, err := r.run(ctx, guestPluginctl, "-g", rebootSetting)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: read reboot setting failed", "err", err, "vmid", r.vmid)
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == settingEnabled, nil
}

// applyFirmwareChange applies the plan. It never reboots; the caller
// reboots when the result says a reboot is required.
func applyFirmwareChange(ctx context.Context, r *guestRunner, plan firmwarePlan) (firmwareResult, error) {
	if plan.Mode == firmwareModeMajor {
		return applyMajorUpgrade(ctx, r, plan)
	}
	return applyPendingUpdate(ctx, r, plan)
}

// applyMajorUpgrade stages the Target release with opnsense-update -u -r.
// -u fetches the package, base, and kernel sets and stages them for the
// next boot, then asks for a reboot (opnsense-update.sh.in:385-387,
// 403-409, 1125-1179, 1240-1242).
func applyMajorUpgrade(ctx context.Context, r *guestRunner, plan firmwarePlan) (firmwareResult, error) {
	result := emptyFirmwareResult()
	if _, err := r.output(ctx, guestUpdater, "-u", "-r", plan.Target); err != nil {
		return result, err
	}
	result.SetsApplied = true
	result.SetsRelease = plan.Target
	result.RebootRequired = true
	return result, nil
}

// applyPendingUpdate runs the web interface's update sequence
// (opnsense/core src/opnsense/scripts/firmware/update.sh:48-82). It
// updates packages with opnsense-update -p -t <core>, restarts the web
// GUI, and installs base and kernel only when opnsense-update -bk -c
// reports them pending. That check exits 0 when a set release differs
// from the installed one and 1 otherwise (opnsense-update.sh.in:686-728),
// and installing base or kernel outside a major upgrade requires a
// reboot (update.sh:72-76).
func applyPendingUpdate(ctx context.Context, r *guestRunner, plan firmwarePlan) (firmwareResult, error) {
	result := emptyFirmwareResult()
	packagesBefore := ""
	if plan.AlwaysReboot {
		listing, err := r.output(ctx, guestPkg, "query", "%n-%v")
		if err != nil {
			return result, err
		}
		packagesBefore = listing
	}
	_, updateErr := r.output(ctx, guestUpdater, "-p", "-t", plan.Installed.CorePackage)
	restartWebGUI(ctx, r)
	if updateErr != nil {
		return result, updateErr
	}
	check, err := r.run(ctx, guestUpdater, "-bk", "-c")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: base and kernel check failed", "err", err, "vmid", r.vmid)
		return result, err
	}
	if check.ExitCode == 0 {
		release, err := r.value(ctx, guestUpdater, "-v")
		if err != nil {
			return result, err
		}
		if _, err := r.output(ctx, guestUpdater, "-bk"); err != nil {
			return result, err
		}
		result.SetsApplied = true
		result.SetsRelease = release
		result.RebootRequired = true
	}
	if plan.AlwaysReboot {
		packagesAfter, err := r.output(ctx, guestPkg, "query", "%n-%v")
		if err != nil {
			return result, err
		}
		result.PackagesChanged = packagesAfter != packagesBefore
		result.RebootRequired = result.RebootRequired || result.PackagesChanged
	}
	return result, nil
}

// restartWebGUI restarts the web server after the package update, which
// the web interface does whether or not the update succeeded
// (update.sh:56-57). Its outcome does not decide the update's outcome.
func restartWebGUI(ctx context.Context, r *guestRunner) {
	res, err := r.run(ctx, guestWebGUI)
	if err != nil || res.ExitCode != 0 {
		slog.WarnContext(ctx, "upgrade: web GUI restart did not exit cleanly",
			"err", err, "vmid", r.vmid, "exit", res.ExitCode)
	}
}

// verifyFirmware compares the post-run state with the plan. It fails
// only when a change that was pending did not land: the core package did
// not reach the available version, the base or kernel set did not reach
// the release that was installed, or a major upgrade did not reach the
// Target release series.
func verifyFirmware(plan firmwarePlan, result firmwareResult) error {
	var problems []string
	post := result.Post
	if plan.Mode == firmwareModeMajor {
		if releaseSeries(post.CoreVersion) != releaseSeries(plan.Target) {
			problems = append(problems, fmt.Sprintf("core %s is not on release %s", post.CoreVersion, plan.Target))
		}
		return problemsError(problems)
	}
	if plan.CorePending && post.CoreVersion != plan.CoreAvailable {
		problems = append(problems, fmt.Sprintf("core %s, want %s", post.CoreVersion, plan.CoreAvailable))
	}
	wantSets := plan.SetsAvailable
	if result.SetsApplied {
		wantSets = result.SetsRelease
	}
	if (plan.BasePending || result.SetsApplied) && post.BaseVersion != wantSets {
		problems = append(problems, fmt.Sprintf("base %s, want %s", post.BaseVersion, wantSets))
	}
	if (plan.KernelPending || result.SetsApplied) && post.KernelVersion != wantSets {
		problems = append(problems, fmt.Sprintf("kernel %s, want %s", post.KernelVersion, wantSets))
	}
	return problemsError(problems)
}

func problemsError(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return errors.New("pending change not applied: " + strings.Join(problems, "; "))
}

// isMajorUpgrade reports whether target names a release series other
// than the installed core version's.
func isMajorUpgrade(target, coreVersion string) bool {
	if target == "" {
		return false
	}
	return releaseSeries(target) != releaseSeries(coreVersion)
}

// releaseSeries returns the OPNsense release series of a version, the
// first two dotted components without a package revision: "26.7.3_8"
// and "26.7" both return "26.7".
func releaseSeries(version string) string {
	parts := strings.SplitN(stripPackageRevision(version), ".", 3)
	if len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + "." + parts[1]
}

// stripPackageRevision drops the pkg revision suffix, so "26.7.3_2"
// becomes "26.7.3". The firmware scripts do the same with ${VAR%%_*}
// (check.sh:311-313, reboot.sh:41).
func stripPackageRevision(version string) string {
	base, _, _ := strings.Cut(version, "_")
	return base
}

func writeFirmwareState(ctx context.Context, path string, state firmwareState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: marshal firmware state", "err", err, "path", path)
		return fmt.Errorf("upgrade: marshal firmware state %q: %w", path, err)
	}
	return WriteFileBytes(ctx, path, data)
}

func writeFirmwarePlan(ctx context.Context, path string, plan firmwarePlan) error {
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: marshal firmware plan", "err", err, "path", path)
		return fmt.Errorf("upgrade: marshal firmware plan %q: %w", path, err)
	}
	return WriteFileBytes(ctx, path, data)
}

func writeFirmwareResult(ctx context.Context, path string, result firmwareResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: marshal firmware result", "err", err, "path", path)
		return fmt.Errorf("upgrade: marshal firmware result %q: %w", path, err)
	}
	return WriteFileBytes(ctx, path, data)
}

func readFirmwareState(ctx context.Context, path string) (firmwareState, error) {
	clean := filepath.Clean(path)
	data, err := os.ReadFile(clean)
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: read firmware state", "err", err, "path", clean)
		return emptyFirmwareState(), fmt.Errorf("upgrade: read firmware state %q: %w", clean, err)
	}
	var state firmwareState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.ErrorContext(ctx, "upgrade: parse firmware state", "err", err, "path", clean)
		return emptyFirmwareState(), fmt.Errorf("upgrade: parse firmware state %q: %w", clean, err)
	}
	return state, nil
}

func emptyFirmwareState() firmwareState {
	return firmwareState{CorePackage: "", CoreVersion: "", BaseVersion: "", KernelVersion: ""}
}

func emptyFirmwarePlan() firmwarePlan {
	return firmwarePlan{
		Mode: "", Target: "", DryRun: false, Installed: emptyFirmwareState(),
		CoreAvailable: "", CorePending: false, SetsAvailable: "",
		BasePending: false, KernelPending: false, AlwaysReboot: false, RebootRequired: false,
	}
}

func emptyFirmwareResult() firmwareResult {
	return firmwareResult{
		SetsApplied: false, SetsRelease: "", PackagesChanged: false,
		RebootRequired: false, Post: emptyFirmwareState(),
	}
}
