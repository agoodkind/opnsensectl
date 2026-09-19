package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// entryPointEnv makes a child copy of the test binary run the command's real
// main with its own argv instead of the tests, so a test drives the binary
// through the same entry point, streams, and exit code an operator sees.
const entryPointEnv = "OPNSENSECTL_TEST_ENTRY_POINT"

// entryPointWait bounds how long a test waits for a started service to reach
// the state it checks for.
const entryPointWait = 10 * time.Second

func TestMain(m *testing.M) {
	if os.Getenv(entryPointEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// entryPointResult is what one run of the binary produced.
type entryPointResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// entryPointCommand builds a run of the real binary with argv0 as its argv[0],
// args after it, and exactly env as its environment.
func entryPointCommand(t *testing.T, argv0 string, env []string, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	command := exec.CommandContext(t.Context(), testBinary)
	command.Args = append([]string{argv0}, args...)
	command.Env = append([]string{entryPointEnv + "=1"}, env...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	return command, &stdout, &stderr
}

// runEntryPoint runs the binary to completion.
func runEntryPoint(t *testing.T, argv0 string, env []string, args ...string) entryPointResult {
	t.Helper()
	command, stdout, stderr := entryPointCommand(t, argv0, env, args...)
	err := command.Run()
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %q: %v", command.Args, err)
	}
	return entryPointResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitCode}
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// daemonConfig returns a complete daemon-side TOML whose serial device does not
// exist, so the daemon stops at the serial open.
func daemonConfig(dir string) string {
	return strings.Join([]string{
		"[daemon]",
		`serial_path = "` + filepath.Join(dir, "ttyV0.1") + `"`,
		"baud = 115200",
		`config_xml_path = "` + filepath.Join(dir, "config.xml") + `"`,
		`backup_dir = "` + filepath.Join(dir, "backup") + `"`,
		`logfile = "` + filepath.Join(dir, "mwan-opnsense.log") + `"`,
		`state_dir = "` + filepath.Join(dir, "transfers") + `"`,
	}, "\n") + "\n"
}

const testPath = "PATH=/usr/bin:/bin"

// TestNoArgumentsPrintsHelpAndDoesNothingElse covers the binary under both of
// its installed names. The router's mwan-opnsense name used to start the
// daemon; it now prints help like any other name.
func TestNoArgumentsPrintsHelpAndDoesNothingElse(t *testing.T) {
	t.Parallel()
	for _, argv0 := range []string{
		"/usr/local/bin/opnsensectl",
		"/usr/local/sbin/mwan-opnsense",
		"/usr/local/sbin/mwan-opnsense.current",
	} {
		t.Run(filepath.Base(argv0), func(t *testing.T) {
			t.Parallel()
			result := runEntryPoint(t, argv0, []string{testPath})
			if result.exitCode != 0 {
				t.Errorf("exit code = %d, want 0", result.exitCode)
			}
			if !strings.HasPrefix(result.stdout, "usage: opnsensectl ") {
				t.Errorf("stdout = %q, want the help", result.stdout)
			}
			if result.stderr != "" {
				t.Errorf("stderr = %q, want nothing", result.stderr)
			}
		})
	}
}

func TestUnknownCommandPrintsHelpAndFails(t *testing.T) {
	t.Parallel()
	result := runEntryPoint(t, "/usr/local/bin/opnsensectl", []string{testPath}, "bogus")
	if result.exitCode != 2 {
		t.Errorf("exit code = %d, want 2", result.exitCode)
	}
	if !strings.Contains(result.stderr, `unknown verb "bogus"`) || !strings.Contains(result.stderr, "usage: opnsensectl ") {
		t.Errorf("stderr = %q, want the unknown verb and the help", result.stderr)
	}
}

// serviceEnvironments is an environment that satisfies every service, so a
// test can isolate the --config requirement.
func serviceEnvironments(t *testing.T) []string {
	t.Helper()
	return []string{testPath, notifySocketEnv + "=" + filepath.Join(t.TempDir(), "notify.sock")}
}

func TestServiceWithoutConfigFails(t *testing.T) {
	t.Parallel()
	for _, command := range [][]string{
		{"daemon", "serve"},
		{"host", "serve"},
		{"host", "drain"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			t.Parallel()
			result := runEntryPoint(t, "/usr/local/bin/opnsensectl", serviceEnvironments(t), command...)
			if result.exitCode != 2 {
				t.Errorf("exit code = %d, want 2", result.exitCode)
			}
			want := "opnsensectl " + strings.Join(command, " ") + ": --config PATH is required"
			if !strings.Contains(result.stderr, want) {
				t.Errorf("stderr = %q, want %q", result.stderr, want)
			}
		})
	}
}

func TestServiceWithMissingConfigFileFailsNamingIt(t *testing.T) {
	t.Parallel()
	for _, command := range [][]string{
		{"daemon", "serve"},
		{"host", "serve"},
		{"host", "drain"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			t.Parallel()
			missing := filepath.Join(t.TempDir(), "absent.toml")
			args := append(append([]string(nil), command...), "--config", missing)
			result := runEntryPoint(t, "/usr/local/bin/opnsensectl", serviceEnvironments(t), args...)
			if result.exitCode != 1 {
				t.Errorf("exit code = %d, want 1", result.exitCode)
			}
			if !strings.Contains(result.stderr, "read "+missing) {
				t.Errorf("stderr = %q, want a read error naming %s", result.stderr, missing)
			}
		})
	}
}

// TestServiceHelpNamesItsEnvironmentAndAMissingOneFails checks each service
// that needs the environment: its help names the variable, and a start without
// it fails naming it before any config is read.
func TestServiceHelpNamesItsEnvironmentAndAMissingOneFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command []string
		envName string
		env     []string
	}{
		{command: []string{"daemon", "serve"}, envName: "PATH", env: nil},
		{command: []string{"host", "drain"}, envName: notifySocketEnv, env: []string{testPath}},
	}
	for _, testCase := range cases {
		t.Run(strings.Join(testCase.command, " "), func(t *testing.T) {
			t.Parallel()
			helpArgs := append(append([]string(nil), testCase.command...), "--help")
			help := runEntryPoint(t, "/usr/local/bin/opnsensectl", testCase.env, helpArgs...)
			if help.exitCode != 0 || !strings.Contains(help.stdout, "Required environment:\n  "+testCase.envName+" ") {
				t.Errorf("help exit code = %d stdout = %q, want 0 and %s named", help.exitCode, help.stdout, testCase.envName)
			}

			configPath := writeTestFile(t, t.TempDir(), "config.toml", "")
			startArgs := append(append([]string(nil), testCase.command...), "--config", configPath)
			result := runEntryPoint(t, "/usr/local/bin/opnsensectl", testCase.env, startArgs...)
			if result.exitCode != 2 {
				t.Errorf("exit code = %d, want 2", result.exitCode)
			}
			want := "environment variable " + testCase.envName + " is required"
			if !strings.Contains(result.stderr, want) {
				t.Errorf("stderr = %q, want %q", result.stderr, want)
			}
		})
	}
}

// TestDaemonServeWithValidConfigReachesTheSerialOpen starts the router daemon
// under its installed name with a complete config whose serial device does not
// exist. It loads the config, starts serving, and stops at the serial open.
func TestDaemonServeWithValidConfigReachesTheSerialOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := writeTestFile(t, dir, "opnsensectl.conf", daemonConfig(dir))

	result := runEntryPoint(t, "/usr/local/sbin/mwan-opnsense", []string{testPath},
		"daemon", "serve", "--config", configPath)
	if result.exitCode != 1 {
		t.Errorf("exit code = %d, want 1 from the serial open", result.exitCode)
	}
	serialPath := filepath.Join(dir, "ttyV0.1")
	for _, want := range []string{"mwan-opnsense: serving", "open serial", serialPath} {
		if !strings.Contains(result.stderr, want) {
			t.Errorf("stderr = %q, want %q", result.stderr, want)
		}
	}
}

// waitFor polls ready until it reports true or entryPointWait passes.
func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(entryPointWait)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stopService sends SIGTERM and returns the exit code.
func stopService(t *testing.T, command *exec.Cmd) int {
	t.Helper()
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	err := command.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	return 0
}

// TestHostServeWithValidConfigServesTheProbeSocket starts the bridge from a
// complete [opnsense.host], with a stale file left at the probe socket path,
// and waits for the bridge to replace it with a root-only socket.
func TestHostServeWithValidConfigServesTheProbeSocket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	listen := writeTestFile(t, dir, "bridge.sock", "stale")
	configPath := writeTestFile(t, dir, "config.toml", strings.Join([]string{
		"[opnsense.host]",
		`upstream = "unix://` + filepath.Join(dir, "drain.sock") + `"`,
		`listen = "` + listen + `"`,
		`reconnect = "1s"`,
		`heartbeat_interval = "30s"`,
		`heartbeat_timeout = "10s"`,
	}, "\n")+"\n")

	command, _, stderr := entryPointCommand(t, "/usr/local/bin/opnsensectl", []string{testPath},
		"host", "serve", "--config", configPath)
	if err := command.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the probe socket "+listen, func() bool {
		info, err := os.Stat(listen)
		return err == nil && info.Mode()&os.ModeSocket != 0 && info.Mode().Perm() == socketFileMode
	})
	if exitCode := stopService(t, command); exitCode != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0; stderr:\n%s", exitCode, stderr.String())
	}
}

// TestHostServeRejectsAnInvalidUpstream proves the bridge reads [opnsense.host]
// from the file --config names: the bogus upstream there is the one rejected.
func TestHostServeRejectsAnInvalidUpstream(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := writeTestFile(t, dir, "config.toml", strings.Join([]string{
		"[opnsense.host]",
		`upstream = "tcp://not-a-unix-socket"`,
		`listen = "` + filepath.Join(dir, "bridge.sock") + `"`,
		`reconnect = "1s"`,
		`heartbeat_interval = "5s"`,
		`heartbeat_timeout = "2s"`,
	}, "\n")+"\n")

	result := runEntryPoint(t, "/usr/local/bin/opnsensectl", []string{testPath}, "host", "serve", "--config", configPath)
	if result.exitCode != 1 || !strings.Contains(result.stderr, "[opnsense.host].upstream must be unix:///abs/path") {
		t.Errorf("exit code = %d stderr = %q, want 1 and the upstream rejected", result.exitCode, result.stderr)
	}
}

// TestHostDrainWithValidConfigReportsReady starts the drainer from a complete
// [opnsense.drain] with a notify socket and waits for its READY=1.
func TestHostDrainWithValidConfigReportsReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notifyPath := filepath.Join(dir, "notify.sock")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notify socket: %v", err)
	}
	t.Cleanup(func() { _ = notify.Close() })
	configPath := writeTestFile(t, dir, "config.toml", strings.Join([]string{
		"[opnsense.drain]",
		`chardev = "unix://` + filepath.Join(dir, "chardev.sock") + `"`,
		`listen = "` + filepath.Join(dir, "relay.sock") + `"`,
	}, "\n")+"\n")

	command, _, stderr := entryPointCommand(t, "/usr/local/bin/opnsensectl",
		[]string{testPath, notifySocketEnv + "=" + notifyPath}, "host", "drain", "--config", configPath)
	if err := command.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := notify.SetReadDeadline(time.Now().Add(entryPointWait)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	message := make([]byte, 256)
	n, err := notify.Read(message)
	if err != nil {
		t.Fatalf("read notify socket: %v; stderr:\n%s", err, stderr.String())
	}
	if string(message[:n]) != "READY=1" {
		t.Errorf("notify message = %q, want READY=1", message[:n])
	}
	if exitCode := stopService(t, command); exitCode != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0; stderr:\n%s", exitCode, stderr.String())
	}
}

// unitExecStart returns the ExecStart argv of an embedded systemd unit.
func unitExecStart(t *testing.T, unit []byte) []string {
	t.Helper()
	for _, line := range strings.Split(string(unit), "\n") {
		if command, found := strings.CutPrefix(line, "ExecStart="); found {
			return strings.Fields(command)
		}
	}
	t.Fatal("unit has no ExecStart")
	return nil
}

// TestHostUnitsStartTheirServiceFromTheDeployedConfig runs each installed
// unit's ExecStart through the binary. On a machine without the hypervisor's
// config file, each one fails reading exactly that file, which proves the unit
// passes a complete, explicit service command.
func TestHostUnitsStartTheirServiceFromTheDeployedConfig(t *testing.T) {
	t.Parallel()
	const deployedConfig = "/etc/opnsensectl/config.toml"
	if _, err := os.Stat(deployedConfig); err == nil {
		t.Fatalf("%s exists on this machine; run this test where the hypervisor config is absent", deployedConfig)
	}
	for name, unit := range map[string][]byte{hostUnitName: hostServiceUnit, drainUnitName: drainServiceUnit} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			argv := unitExecStart(t, unit)
			if argv[0] != "/usr/local/bin/opnsensectl" {
				t.Fatalf("ExecStart runs %q, want /usr/local/bin/opnsensectl", argv[0])
			}
			result := runEntryPoint(t, argv[0], serviceEnvironments(t), argv[1:]...)
			if result.exitCode != 1 || !strings.Contains(result.stderr, "read "+deployedConfig) {
				t.Errorf("exit code = %d stderr = %q, want 1 and a read error naming %s",
					result.exitCode, result.stderr, deployedConfig)
			}
		})
	}
}
