package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/opnsensectl/internal/daemoncfg"
)

const rcSubrStub = `load_rc_config() { :; }
`

// renderRCDScript writes a sourceable copy of the embedded rc.d script to dir
// with the rc.subr include redirected to a stub and the run_rc_command tail
// neutralized, so a test can source the script under /bin/sh and call its
// functions directly. It returns the temp script path and the rc.subr
// stub path.
func renderRCDScript(t *testing.T, dir string) (scriptPath, rcSubrPath string) {
	t.Helper()

	rcSubrPath = filepath.Join(dir, "rc.subr")
	if err := os.WriteFile(rcSubrPath, []byte(rcSubrStub), 0o600); err != nil {
		t.Fatalf("write rc.subr stub: %v", err)
	}

	rendered := string(rcdScript)
	rewritten := strings.Replace(rendered, ". /etc/rc.subr", `. "${RC_SUBR_STUB}"`, 1)
	if rewritten == rendered {
		t.Fatal("rc.d script does not contain the expected rc.subr include")
	}
	neutralized := strings.Replace(rewritten, `run_rc_command "$1"`, `:`, 1)
	if neutralized == rewritten {
		t.Fatal("rc.d script does not contain the expected run_rc_command tail")
	}

	scriptPath = filepath.Join(dir, "mwan_opnsense")
	if err := os.WriteFile(scriptPath, []byte(neutralized), 0o700); err != nil {
		t.Fatalf("write temp rc.d script: %v", err)
	}
	return scriptPath, rcSubrPath
}

// rcdBootPath is the PATH a boot start gives the daemon. rcdServicePath is the
// PATH service(8) runs an rc.d script with, which lacks the /usr/local
// directories.
const (
	rcdBootPath    = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin:/usr/local/sbin"
	rcdServicePath = "/sbin:/bin:/usr/sbin:/usr/bin"
)

// TestRCDStartGivesDaemonTheBootPathAndTheExplicitCommand runs the real start
// function the way service(8) runs it, with an emptied environment and the
// short PATH, and asserts that daemon(8) is launched with the boot PATH,
// auto-restart, and the run shim followed by the daemon's full command naming
// the config file install writes. A stub stands in for daemon(8): it records
// the PATH it inherited and its arguments, and writes the calling shell's pid
// as the supervisor pidfile, so the start's status poll finds a live process.
func TestRCDStartGivesDaemonTheBootPathAndTheExplicitCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	scriptPath, rcSubrPath := renderRCDScript(t, dir)

	pathLog := filepath.Join(dir, "daemon-path.log")
	argvLog := filepath.Join(dir, "daemon-argv.log")
	daemonStub := filepath.Join(dir, "daemon")
	stubText := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "${PATH}" > "` + pathLog + `"`,
		`printf '%s\n' "$@" > "` + argvLog + `"`,
		`printf '%s' "${PPID}" > "$3"`,
	}, "\n") + "\n"
	if err := os.WriteFile(daemonStub, []byte(stubText), 0o700); err != nil {
		t.Fatalf("write daemon stub: %v", err)
	}

	scriptData, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read rendered rc.d script: %v", err)
	}
	rendered := string(scriptData)
	stubbed := strings.Replace(rendered, "/usr/sbin/daemon -r ", daemonStub+" -r ", 1)
	if stubbed == rendered {
		t.Fatal("rc.d script does not launch /usr/sbin/daemon -r")
	}
	if err := os.WriteFile(scriptPath, []byte(stubbed), 0o700); err != nil {
		t.Fatalf("write stubbed rc.d script: %v", err)
	}

	commandText := strings.Join([]string{
		"set -u",
		`. "${SCRIPT_PATH}"`,
		`pidfile="${PIDFILE}"`,
		`child_pidfile="${PIDFILE}.child"`,
		`mwan_opnsense_start`,
	}, "\n")

	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", commandText)
	command.Env = []string{
		"HOME=/",
		"PATH=" + rcdServicePath,
		"RC_SUBR_STUB=" + rcSubrPath,
		"SCRIPT_PATH=" + scriptPath,
		"PIDFILE=" + filepath.Join(dir, "mwan_opnsense.pid"),
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run start: %v\n%s", err, output)
	}

	pathData, err := os.ReadFile(pathLog)
	if err != nil {
		t.Fatalf("daemon stub recorded no PATH: %v\nstart output:\n%s", err, output)
	}
	if got := strings.TrimSuffix(string(pathData), "\n"); got != rcdBootPath {
		t.Fatalf("daemon(8) PATH = %q, want the boot PATH %q", got, rcdBootPath)
	}

	argvData, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("daemon stub recorded no arguments: %v", err)
	}
	pidfile := filepath.Join(dir, "mwan_opnsense.pid")
	wantArgv := []string{
		"-r", "-P", pidfile, "-p", pidfile + ".child", "-o", "/var/log/mwan-opnsense.log",
		"/usr/local/libexec/mwan-opnsense-run",
		"/usr/local/sbin/mwan-opnsense", "daemon", "serve", "--config", daemoncfg.InstallPath,
	}
	gotArgv := strings.Split(strings.TrimSuffix(string(argvData), "\n"), "\n")
	if !slices.Equal(gotArgv, wantArgv) {
		t.Fatalf("daemon(8) argv = %q, want %q", gotArgv, wantArgv)
	}
}

// TestRCDStopKillsWedgedChild drives mwan_opnsense_stop against a stub
// kill that keeps the supervisor "alive" through SIGTERM (the wedge), and
// asserts the forced path signals the tracked child pid directly and the
// supervisor's process group. This is the stop-orphan backstop: a SIGKILL
// aimed only at the supervisor cannot reach a child parked in a blocking
// serial read.
func TestRCDStopKillsWedgedChild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	scriptPath, rcSubrPath := renderRCDScript(t, dir)

	const supervisorPID = "4242"
	const childPID = "4243"
	pidfile := filepath.Join(dir, "mwan_opnsense.pid")
	childPidfile := filepath.Join(dir, "mwan_opnsense.child.pid")
	killLog := filepath.Join(dir, "kill.log")
	if err := os.WriteFile(pidfile, []byte(supervisorPID+"\n"), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	if err := os.WriteFile(childPidfile, []byte(childPID+"\n"), 0o600); err != nil {
		t.Fatalf("write child pidfile: %v", err)
	}

	// Stub kill: log every call and always succeed, so the supervisor never
	// "dies" on SIGTERM and the stop is driven to the forced KILL path. Stub
	// sleep so the wait loop does not spend real time.
	commandText := strings.Join([]string{
		"set -u",
		`. "${SCRIPT_PATH}"`,
		`pidfile="${PIDFILE}"`,
		`child_pidfile="${CHILD_PIDFILE}"`,
		`mwan_opnsense_stop_timeout=1`,
		`sleep() { :; }`,
		`kill() { echo "$*" >> "${KILL_LOG}"; return 0; }`,
		`mwan_opnsense_stop`,
	}, "\n")

	command := exec.Command("/bin/sh", "-c", commandText)
	command.Env = append(os.Environ(),
		"RC_SUBR_STUB="+rcSubrPath,
		"SCRIPT_PATH="+scriptPath,
		"PIDFILE="+pidfile,
		"CHILD_PIDFILE="+childPidfile,
		"KILL_LOG="+killLog,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run stop: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "did not stop after TERM; sending KILL") {
		t.Fatalf("forced path not taken; output:\n%s", output)
	}

	logData, err := os.ReadFile(killLog)
	if err != nil {
		t.Fatalf("read kill log: %v", err)
	}
	log := string(logData)
	wantSignals := []string{
		"-TERM " + supervisorPID,     // supervisor TERM first
		"-KILL " + supervisorPID,     // supervisor killed (stop supervision)
		"-KILL -- -" + supervisorPID, // supervisor process group killed
		"-KILL " + childPID,          // child killed directly (the orphan fix)
	}
	for _, want := range wantSignals {
		if !strings.Contains(log, want) {
			t.Errorf("kill log missing %q\nlog:\n%s", want, log)
		}
	}

	// Order is load-bearing under daemon(8) -r: the supervisor must be
	// killed before the child, or a still-alive supervisor would respawn
	// the child and a `service stop` would leave the lever running.
	supKillIdx := strings.Index(log, "-KILL "+supervisorPID)
	childKillIdx := strings.Index(log, "-KILL "+childPID)
	if supKillIdx < 0 || childKillIdx < 0 || supKillIdx > childKillIdx {
		t.Errorf("supervisor KILL must precede child KILL (sup=%d child=%d)\nlog:\n%s",
			supKillIdx, childKillIdx, log)
	}

	// The forced stop must clear both pidfiles.
	if _, statErr := os.Stat(pidfile); !os.IsNotExist(statErr) {
		t.Errorf("supervisor pidfile should be removed, statErr=%v", statErr)
	}
	if _, statErr := os.Stat(childPidfile); !os.IsNotExist(statErr) {
		t.Errorf("child pidfile should be removed, statErr=%v", statErr)
	}
}
