package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const rcSubrStub = `load_rc_config() { :; }
`

// renderRCDScript writes a sourceable copy of the rc.d script to dir with
// the rc.subr include redirected to a stub and the run_rc_command tail
// neutralized, so a test can source the script under /bin/sh and call its
// functions directly. It returns the temp script path and the rc.subr
// stub path.
func renderRCDScript(t *testing.T, dir string) (scriptPath, rcSubrPath string) {
	t.Helper()

	rcSubrPath = filepath.Join(dir, "rc.subr")
	if err := os.WriteFile(rcSubrPath, []byte(rcSubrStub), 0o600); err != nil {
		t.Fatalf("write rc.subr stub: %v", err)
	}

	srcPath := filepath.Join("opnsense-src", "etc", "rc.d", "mwan_opnsense")
	scriptData, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read rc.d script: %v", err)
	}

	rendered := string(scriptData)
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

func TestRCDWritesDaemonTOML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	scriptPath, rcSubrPath := renderRCDScript(t, dir)

	daemonDir := filepath.Join(dir, "var-lib-mwan")
	daemonPath := filepath.Join(daemonDir, "daemon.toml")

	// This keeps the contract test portable: only the rc.subr include and
	// run_rc_command tail are neutralized, and the real render function still
	// runs under /bin/sh against temp paths.
	commandText := strings.Join([]string{
		"set -eu",
		`. "${SCRIPT_PATH}"`,
		`daemon_toml_dir="${DAEMON_TOML_DIR}"`,
		`daemon_toml_path="${DAEMON_TOML_PATH}"`,
		`mwan_opnsense_listen_serial="/dev/ttyV0.1"`,
		`mwan_opnsense_baud="921600"`,
		`mwan_opnsense_config_xml="/tmp/config.xml"`,
		`mwan_opnsense_backup_dir="/tmp/backup"`,
		`mwan_opnsense_logfile="/tmp/mwan-opnsense.log"`,
		`mwan_opnsense_state_dir="/tmp/transfers"`,
		`mwan_opnsense_write_daemon_toml`,
	}, "\n")

	command := exec.Command("/bin/sh", "-c", commandText)
	command.Env = append(os.Environ(),
		"RC_SUBR_STUB="+rcSubrPath,
		"SCRIPT_PATH="+scriptPath,
		"DAEMON_TOML_DIR="+daemonDir,
		"DAEMON_TOML_PATH="+daemonPath,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render daemon.toml: %v\n%s", err, output)
	}

	info, err := os.Stat(daemonPath)
	if err != nil {
		t.Fatalf("stat daemon.toml: %v", err)
	}
	if gotPerm := info.Mode().Perm(); gotPerm != 0o600 {
		t.Fatalf("daemon.toml perm = %#o, want %#o", gotPerm, 0o600)
	}

	data, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatalf("read daemon.toml: %v", err)
	}
	got := string(data)
	wantLines := []string{
		"[daemon]",
		`serial_path = "/dev/ttyV0.1"`,
		`baud = 921600`,
		`config_xml_path = "/tmp/config.xml"`,
		`backup_dir = "/tmp/backup"`,
		`logfile = "/tmp/mwan-opnsense.log"`,
		`state_dir = "/tmp/transfers"`,
	}
	for _, wantLine := range wantLines {
		if !strings.Contains(got, wantLine) {
			t.Fatalf("daemon.toml missing %q\n%s", wantLine, got)
		}
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
