package svc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunExec_Echo(t *testing.T) {
	res, err := runExec(context.Background(), ExecArgs{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if res.TimedOut {
		t.Fatal("did not expect TimedOut")
	}
	if res.StdoutTruncated {
		t.Fatal("did not expect StdoutTruncated")
	}
}

func TestRunExec_NonzeroExit(t *testing.T) {
	res, err := runExec(context.Background(), ExecArgs{
		Command: "/bin/sh",
		Args:    []string{"-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("expected exit=7, got %d", res.ExitCode)
	}
}

func TestRunExec_Stderr(t *testing.T) {
	res, err := runExec(context.Background(), ExecArgs{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo oops 1>&2; exit 0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stderr), "oops") {
		t.Fatalf("stderr=%q", res.Stderr)
	}
}

func TestRunExec_Timeout(t *testing.T) {
	start := time.Now()
	res, err := runExec(context.Background(), ExecArgs{
		Command:        "/bin/sh",
		Args:           []string{"-c", "sleep 5"},
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dur := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true; result=%+v", res)
	}
	if dur > 3*time.Second {
		t.Fatalf("timeout enforcement slow: %v", dur)
	}
}

func TestRunExec_Stdin(t *testing.T) {
	res, err := runExec(context.Background(), ExecArgs{
		Command:    "/bin/cat",
		StdinBytes: []byte("piped"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Stdout, []byte("piped")) {
		t.Fatalf("stdout=%q", res.Stdout)
	}
}

func TestRunExec_OutputCap(t *testing.T) {
	// emit ~12 MB; cap is 10 MB
	res, err := runExec(context.Background(), ExecArgs{
		Command: "/bin/sh",
		Args:    []string{"-c", "yes hello | head -c 12000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StdoutTruncated {
		t.Fatal("expected StdoutTruncated=true")
	}
	if len(res.Stdout) > maxOutputBytes {
		t.Fatalf("stdout exceeded cap: %d > %d", len(res.Stdout), maxOutputBytes)
	}
}

func TestValidateExecArgs(t *testing.T) {
	cases := []struct {
		name string
		args ExecArgs
		ok   bool
	}{
		{"empty cmd", ExecArgs{Command: ""}, false},
		{"null in cmd", ExecArgs{Command: "/bin/sh\x00x"}, false},
		{"null in arg", ExecArgs{Command: "/bin/sh", Args: []string{"\x00"}}, false},
		{"valid", ExecArgs{Command: "/bin/sh", Args: []string{"-c", "true"}}, true},
		{"long cmd", ExecArgs{Command: strings.Repeat("a", 5000)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateExecArgs(c.args)
			if (err == nil) != c.ok {
				t.Fatalf("validateExecArgs ok=%v err=%v", c.ok, err)
			}
		})
	}
}

func TestRunExec_BinaryNotFound(t *testing.T) {
	_, err := runExec(context.Background(), ExecArgs{Command: "/this/does/not/exist"})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

// TestRunExec_StdinClosesForCatChild verifies that a 64 KB stdin
// payload pushed into `/bin/sh -c "cat > file"` is fully consumed by
// the child before the handler returns. The test pins the stdin EOF
// contract: cmd.Stdin = bytes.NewReader closes the child's stdin pipe
// on Reader EOF, which lets cat exit cleanly. A regression that breaks
// this contract (e.g. by switching to a Pipe that never closes) would
// hang the handler past the 5 s deadline.
func TestRunExec_StdinClosesForCatChild(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "stdin-eof.bin")
	payload := bytes.Repeat([]byte{'A'}, 64*1024)

	start := time.Now()
	res, err := runExec(context.Background(), ExecArgs{
		Command:        "/bin/sh",
		Args:           []string{"-c", "cat > " + out},
		StdinBytes:     payload,
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dur := time.Since(start); dur > 5*time.Second {
		t.Fatalf("handler took %v; child likely never saw stdin EOF", dur)
	}
	if res.TimedOut {
		t.Fatalf("TimedOut=true; child did not see stdin EOF: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("output file size=%d; want %d", len(got), len(payload))
	}
}

// TestRunExec_TimeoutKillsChildThatIgnoresStdin verifies the timeout
// path kills a child that never reads its stdin. The stdin pipe stays
// full, the child runs the busy loop forever, and the handler must
// return within the timeout window (plus a small grace) with
// TimedOut=true and a negative exit code. A regression that leaves the
// stdin writer goroutine blocked would hang the handler past the
// timeout.
func TestRunExec_TimeoutKillsChildThatIgnoresStdin(t *testing.T) {
	start := time.Now()
	res, err := runExec(context.Background(), ExecArgs{
		Command:        "/bin/sh",
		Args:           []string{"-c", "while true; do :; done"},
		StdinBytes:     []byte("ignored payload"),
		TimeoutSeconds: 2,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	dur := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true; got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode=-1 on timeout; got %d", res.ExitCode)
	}
	if dur > 5*time.Second {
		t.Fatalf("timeout enforcement slow: %v (limit 5s)", dur)
	}
}

// TestRunExec_CappedBufferDoesNotWedge stresses the cappedBuffer drop
// path with a payload five times the 10 MB cap. The child writes ~50
// MB of stdout; cappedBuffer accepts the first 10 MB and silently
// drops the rest. The contract is that cappedBuffer.Write returns
// success even after the cap is hit so the subprocess never sees a
// short-write or SIGPIPE, which would otherwise stall the child and
// hang the handler.
func TestRunExec_CappedBufferDoesNotWedge(t *testing.T) {
	start := time.Now()
	res, err := runExec(context.Background(), ExecArgs{
		Command:        "/bin/sh",
		Args:           []string{"-c", "yes hello | head -c 50000000"},
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dur := time.Since(start); dur > 30*time.Second {
		t.Fatalf("handler took %v; cappedBuffer likely stalled the child", dur)
	}
	if res.TimedOut {
		t.Fatalf("TimedOut=true; child did not finish: %+v", res)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !res.StdoutTruncated {
		t.Fatal("expected StdoutTruncated=true after 50 MB write into 10 MB cap")
	}
	if len(res.Stdout) > maxOutputBytes {
		t.Fatalf("stdout exceeded cap: %d > %d", len(res.Stdout), maxOutputBytes)
	}
}
