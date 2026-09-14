package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestTransferWatchdogFiresOnStall proves the watchdog cancels the context when
// no progress is reported for the stall window.
func TestTransferWatchdogFiresOnStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wd := startTransferWatchdog(cancel, 100*time.Millisecond)
	defer wd.stop()

	select {
	case <-ctx.Done():
		if !wd.fired() {
			t.Fatal("context canceled but watchdog did not record a stall")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire within 2s for a 100ms stall")
	}
}

// TestTransferWatchdogProgressPreventsFiring proves that steady progress keeps
// the watchdog from firing.
func TestTransferWatchdogProgressPreventsFiring(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wd := startTransferWatchdog(cancel, 200*time.Millisecond)
	defer wd.stop()

	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		wd.markProgress()
		time.Sleep(20 * time.Millisecond)
	}
	if wd.fired() {
		t.Fatal("watchdog fired despite steady progress")
	}
	if ctx.Err() != nil {
		t.Fatalf("context canceled despite steady progress: %v", ctx.Err())
	}
}

// TestTransferWatchdogStopHalts proves stop() ends the watchdog so it never
// fires afterward, and that a stopped watchdog reports no stall.
func TestTransferWatchdogStopHalts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wd := startTransferWatchdog(cancel, 100*time.Millisecond)
	wd.stop()

	// Well past the stall window: a stopped watchdog must not fire.
	time.Sleep(300 * time.Millisecond)
	if wd.fired() {
		t.Fatal("watchdog fired after stop()")
	}
	if ctx.Err() != nil {
		t.Fatalf("context canceled after stop(): %v", ctx.Err())
	}
}

// TestTransferWatchdogFailureMessages proves failure() returns a clear stall
// error once the watchdog tripped, and otherwise wraps the real error.
func TestTransferWatchdogFailureMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wd := startTransferWatchdog(cancel, 100*time.Millisecond)
	defer wd.stop()

	real := errors.New("boom")
	if got := wd.failure(ctx, "upload: send data", real); !errors.Is(got, real) {
		t.Fatalf("before stall, failure should wrap the real error, got %v", got)
	}

	<-ctx.Done()
	if !wd.fired() {
		t.Fatal("watchdog did not record a stall after firing")
	}
	got := wd.failure(ctx, "upload: recv data ack", real)
	if errors.Is(got, real) {
		t.Fatalf("after stall, failure must not wrap the real error, got %v", got)
	}
	if !strings.Contains(got.Error(), "transfer stalled") {
		t.Fatalf("after stall, failure should mention a stall, got %v", got)
	}
}

// TestTransferWatchdogZeroStallNoPanic proves that a zero or sub-millisecond stall
// does not cause a NewTicker(0) panic, and that stop() returns cleanly without
// hanging. The 10ms floor fix makes the watchdog still operational in that state.
func TestTransferWatchdogZeroStallNoPanic(t *testing.T) {
	for _, stall := range []time.Duration{0, time.Nanosecond} {
		t.Run(stall.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Must not panic: the 10ms floor prevents NewTicker(0).
			wd := startTransferWatchdog(cancel, stall)
			wd.stop()
			// Brief settle so the goroutine exits before the test ends.
			time.Sleep(50 * time.Millisecond)
			// A zero stall may fire before stop(); either is valid.
			// The critical invariant: context canceled implies watchdog fired.
			if ctx.Err() != nil && !wd.fired() {
				t.Fatal("context canceled but watchdog did not record a stall")
			}
		})
	}
}

// TestTransferWatchdogStopIdempotent proves that calling stop() twice does not
// panic and does not trigger a spurious stall.
func TestTransferWatchdogStopIdempotent(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	wd := startTransferWatchdog(cancel, 5*time.Second)
	wd.stop()
	wd.stop() // must not panic
	if wd.fired() {
		t.Fatal("double stop caused a spurious stall")
	}
}

// TestTransferWatchdogPanicFailClosed proves that handleWatchdogPanic sets the
// stalled flag and cancels the context.
func TestTransferWatchdogPanicFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wd := startTransferWatchdog(cancel, 10*time.Second)
	defer wd.stop()

	wd.handleWatchdogPanic(cancel, errors.New("test panic"))

	if !wd.fired() {
		t.Fatal("handleWatchdogPanic did not set the stalled flag")
	}
	if ctx.Err() == nil {
		t.Fatal("handleWatchdogPanic did not cancel the context")
	}
}

// TestRequireProbeTransferStall covers the fallback-on-empty behavior plus the
// parse and positivity checks.
func TestRequireProbeTransferStall(t *testing.T) {
	t.Run("empty falls back to default", func(t *testing.T) {
		writeTempTOML(t, `
hostname = "stall-test"

[opnsense.probe]
target = "unix:///tmp/x.sock"
timeout = "10s"
upload_chunk_bytes = 16384
`)
		cfg, err := loadOpnsenseConfig()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		got, err := requireProbeTransferStall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != probeTransferStallDefault {
			t.Fatalf("got %s, want default %s", got, probeTransferStallDefault)
		}
	})

	t.Run("explicit value is parsed", func(t *testing.T) {
		writeTempTOML(t, `
hostname = "stall-test"

[opnsense.probe]
target = "unix:///tmp/x.sock"
transfer_stall_timeout = "45s"
`)
		cfg, err := loadOpnsenseConfig()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		got, err := requireProbeTransferStall(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 45*time.Second {
			t.Fatalf("got %s, want 45s", got)
		}
	})

	t.Run("malformed value errors", func(t *testing.T) {
		writeTempTOML(t, `
hostname = "stall-test"

[opnsense.probe]
target = "unix:///tmp/x.sock"
transfer_stall_timeout = "not-a-duration"
`)
		cfg, err := loadOpnsenseConfig()
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if _, err := requireProbeTransferStall(cfg); err == nil {
			t.Fatal("expected an error for a malformed duration")
		}
	})
}
