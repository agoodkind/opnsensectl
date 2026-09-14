package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// socketPair returns two connected stream sockets as net.Conn, with kernel
// buffering and deadline support (unlike net.Pipe, which is synchronous).
func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	mk := func(fd int) net.Conn {
		f := os.NewFile(uintptr(fd), "sockpair")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("FileConn: %v", err)
		}
		return c
	}
	return mk(fds[0]), mk(fds[1])
}

func writeAll(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

// TestDrainAlwaysWithoutClient proves the drainer keeps consuming the chardev
// even with no client attached, so a guest write never stalls. With no drain,
// a 4 MiB write would block once the socket buffer fills.
func TestDrainAlwaysWithoutClient(t *testing.T) {
	chLocal, chRemote := socketPair(t)
	defer func() { _ = chLocal.Close() }()
	defer func() { _ = chRemote.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &drainHub{}
	go func() { _ = drainChardev(ctx, testLog(), hub, chLocal) }()

	payload := make([]byte, 4<<20)
	done := make(chan error, 1)
	go func() {
		_ = chRemote.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := chRemote.Write(payload)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("guest write stalled with no client attached: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("guest write did not complete: drain is not consuming the chardev")
	}
}

// TestClientChurn proves chardev data reaches the attached client, and that a
// new client transparently takes over from a dropped one.
func TestClientChurn(t *testing.T) {
	chLocal, chRemote := socketPair(t)
	defer func() { _ = chLocal.Close() }()
	defer func() { _ = chRemote.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &drainHub{}
	go func() { _ = drainChardev(ctx, testLog(), hub, chLocal) }()

	c1Local, c1Remote := socketPair(t)
	hub.setClient(c1Local)
	writeAll(t, chRemote, []byte("hello"))
	if got := readN(t, c1Remote, 5); got != "hello" {
		t.Fatalf("client 1 got %q, want hello", got)
	}

	// Churn: setClient closes c1Local; the next chardev data must reach c2.
	c2Local, c2Remote := socketPair(t)
	hub.setClient(c2Local)
	writeAll(t, chRemote, []byte("world"))
	if got := readN(t, c2Remote, 5); got != "world" {
		t.Fatalf("client 2 got %q, want world", got)
	}
	_ = c1Remote.Close()
	_ = c2Remote.Close()
}

// TestClientToChardev proves the reverse direction: client writes reach the
// chardev.
func TestClientToChardev(t *testing.T) {
	chLocal, chRemote := socketPair(t)
	defer func() { _ = chLocal.Close() }()
	defer func() { _ = chRemote.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &drainHub{}
	hub.setChardev(chLocal)
	go func() { _ = drainChardev(ctx, testLog(), hub, chLocal) }()

	cLocal, cRemote := socketPair(t)
	hub.setClient(cLocal)
	go clientPump(hub, cLocal)

	writeAll(t, cRemote, []byte("ping"))
	if got := readN(t, chRemote, 4); got != "ping" {
		t.Fatalf("chardev got %q, want ping", got)
	}
	_ = cRemote.Close()
}

// TestNotifyWithFD proves the hand-built SCM_RIGHTS notify delivers both the
// state payload and a working duplicate of the sent fd.
func TestNotifyWithFD(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "notify.sock")
	rconn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen notify socket: %v", err)
	}
	defer func() { _ = rconn.Close() }()
	t.Setenv("NOTIFY_SOCKET", sockPath)

	// Send one end of a socket pair; verify on the other end.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	sendFD := fds[0]
	verify := os.NewFile(uintptr(fds[1]), "verify")
	defer func() { _ = verify.Close() }()

	const state = "FDSTORE=1\nFDNAME=chardev"
	if err := notifyWithFD(context.Background(), testLog(), state, sendFD); err != nil {
		t.Fatalf("notifyWithFD: %v", err)
	}
	_ = syscall.Close(sendFD)

	buf := make([]byte, 256)
	oob := make([]byte, 256)
	_ = rconn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, oobn, _, _, err := rconn.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatalf("ReadMsgUnix: %v", err)
	}
	if string(buf[:n]) != state {
		t.Fatalf("payload %q, want %q", buf[:n], state)
	}
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		t.Fatalf("ParseSocketControlMessage: %v", err)
	}
	recvFDs, err := syscall.ParseUnixRights(&scms[0])
	if err != nil || len(recvFDs) != 1 {
		t.Fatalf("ParseUnixRights: fds=%v err=%v", recvFDs, err)
	}
	recv := os.NewFile(uintptr(recvFDs[0]), "recv")
	defer func() { _ = recv.Close() }()

	// The received fd is a dup of sendFD (= fds[0]); writing it reaches fds[1].
	if _, err := recv.Write([]byte("ok")); err != nil {
		t.Fatalf("write through received fd: %v", err)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(verify, got); err != nil {
		t.Fatalf("read from verify end: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("got %q through received fd, want ok", got)
	}
}

// TestNotifyWithFDNoSocket is a no-op when NOTIFY_SOCKET is unset.
func TestNotifyWithFDNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifyWithFD(context.Background(), testLog(), "FDSTORE=1", 0); err != nil {
		t.Fatalf("expected nil without NOTIFY_SOCKET, got %v", err)
	}
}

// TestDrainRelayEndToEnd exercises the whole relay through real unix sockets: a
// fake qemu chardev, the production runDrainRelay orchestration, a bridge client
// on the relay socket, bidirectional data, the wedge-prevention property (the
// chardev keeps draining after the bridge drops), and bridge reconnect.
func TestDrainRelayEndToEnd(t *testing.T) {
	dir := t.TempDir()
	chardevPath := filepath.Join(dir, "chardev.sock")
	relayPath := filepath.Join(dir, "relay.sock")

	// Fake qemu chardev (server=on): accepts the drainer's one connection and
	// hands the test the guest side of it.
	chardevLn, err := net.Listen("unix", chardevPath)
	if err != nil {
		t.Fatalf("listen chardev: %v", err)
	}
	defer func() { _ = chardevLn.Close() }()
	guestCh := make(chan net.Conn, 1)
	go func() {
		c, err := chardevLn.Accept()
		if err == nil {
			guestCh <- c
		}
	}()

	// The relay socket the drainer serves and the bridge dials.
	relayLn, err := net.Listen("unix", relayPath)
	if err != nil {
		t.Fatalf("listen relay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = relayLn.Close()
	}()

	openChardev := func(c context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(c, "unix", chardevPath)
	}
	go runDrainRelay(ctx, testLog(), relayLn, openChardev)

	var guest net.Conn
	select {
	case guest = <-guestCh:
	case <-time.After(3 * time.Second):
		t.Fatal("drainer never connected to the chardev")
	}
	defer func() { _ = guest.Close() }()

	bridge, err := net.Dial("unix", relayPath)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	defer func() { _ = bridge.Close() }()

	// Sync the bridge end to end (bridge -> chardev) so the client is wired,
	// then verify guest -> bridge.
	writeAll(t, bridge, []byte("syn1"))
	if got := readN(t, guest, 4); got != "syn1" {
		t.Fatalf("guest got %q from bridge, want syn1", got)
	}
	writeAll(t, guest, []byte("from-guest"))
	if got := readN(t, bridge, 10); got != "from-guest" {
		t.Fatalf("bridge got %q from guest, want from-guest", got)
	}

	// Wedge prevention: drop the bridge, keep the guest writing 4 MiB; the drain
	// must keep consuming the chardev so the write completes.
	_ = bridge.Close()
	big := make([]byte, 4<<20)
	done := make(chan error, 1)
	go func() {
		_ = guest.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, e := guest.Write(big)
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("guest write stalled after bridge drop: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("guest write did not complete after bridge drop: wedge not prevented")
	}
	_ = guest.SetWriteDeadline(time.Time{})

	// Reconnect: a fresh bridge attaches and traffic flows again. Sync first so
	// the new client is the current one before asserting guest -> bridge.
	bridge2, err := net.Dial("unix", relayPath)
	if err != nil {
		t.Fatalf("bridge2 dial: %v", err)
	}
	defer func() { _ = bridge2.Close() }()
	writeAll(t, bridge2, []byte("syn2"))
	if got := readN(t, guest, 4); got != "syn2" {
		t.Fatalf("guest got %q from bridge2, want syn2", got)
	}
	// Drain any leftover queued bytes toward bridge2, then assert a fresh write.
	_ = bridge2.SetReadDeadline(time.Now().Add(time.Second))
	drainLeft := make([]byte, 1<<20)
	for {
		if _, err := bridge2.Read(drainLeft); err != nil {
			break
		}
	}
	writeAll(t, guest, []byte("again!"))
	if got := readN(t, bridge2, 6); got != "again!" {
		t.Fatalf("bridge2 got %q from guest, want again!", got)
	}
}

// TestDrainReaderNeverBlocksOnHungClient proves the chardev reader goroutine
// never blocks on the client write: a guest write of 4 MiB completes even when
// the attached bridge socket is full and never reads.
func TestDrainReaderNeverBlocksOnHungClient(t *testing.T) {
	chLocal, chRemote := socketPair(t)
	defer func() { _ = chLocal.Close() }()
	defer func() { _ = chRemote.Close() }()
	hubClient, hungBridge := socketPair(t)
	defer func() { _ = hubClient.Close() }()
	defer func() { _ = hungBridge.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &drainHub{}
	hub.setClient(hubClient)
	go func() { _ = drainChardev(ctx, testLog(), hub, chLocal) }()
	payload := make([]byte, 4<<20)
	done := make(chan error, 1)
	go func() {
		_ = chRemote.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, e := chRemote.Write(payload)
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("guest write stalled, reader blocked on hung client: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("guest write did not complete: reader blocked on the hung client (wedge risk)")
	}
}

// TestDrainNoByteLossToReadingClient proves that a client actively reading
// receives every byte the guest writes, with no loss.
func TestDrainNoByteLossToReadingClient(t *testing.T) {
	chLocal, chRemote := socketPair(t)
	defer func() { _ = chLocal.Close() }()
	defer func() { _ = chRemote.Close() }()
	hubClient, bridge := socketPair(t)
	defer func() { _ = hubClient.Close() }()
	defer func() { _ = bridge.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &drainHub{}
	hub.setClient(hubClient)
	go func() { _ = drainChardev(ctx, testLog(), hub, chLocal) }()
	msg := bytes.Repeat([]byte("MWN1"), 4096) // 16 KiB
	go func() { _, _ = chRemote.Write(msg) }()
	got := readN(t, bridge, len(msg))
	if got != string(msg) {
		t.Fatalf("byte loss to a reading client")
	}
}

// TestFreshDialAfterStoreIsFlagged proves that a second consecutive fresh dial
// (adopted=false) after an fd has been stored is logged as an ERROR, since it is
// the only window in which qemu can see a chardev disconnect while the VM is up.
func TestFreshDialAfterStoreIsFlagged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := context.Background()

	// Two socketpairs simulate two consecutive fresh chardev dials. Only the
	// "a" side of each pair is returned by fakeAcquire; "b" sides are closed on
	// test exit.
	c1a, c1b := socketPair(t)
	c2a, c2b := socketPair(t)
	defer func() { _ = c1b.Close() }()
	defer func() { _ = c2b.Close() }()

	dialCount := 0
	conns := []net.Conn{c1a, c2a}
	fakeAcquire := func(_ context.Context, _ *slog.Logger, _ map[string][]*os.File, _ string) (net.Conn, bool, error) {
		conn := conns[dialCount]
		dialCount++
		return conn, false, nil // always fresh dial, never adopt
	}

	opener := makeChardevOpener(logger, nil, "/fake/chardev", fakeAcquire)

	// First call: reclaimExpected=false, so no error is logged.
	conn1, err := opener(ctx)
	if err != nil {
		t.Fatalf("opener call 1: %v", err)
	}
	defer func() { _ = conn1.Close() }()

	const wantMsg = "fresh chardev dial while a stored fd was expected"
	if strings.Contains(buf.String(), wantMsg) {
		t.Fatalf("first fresh dial should not log the error, got: %q", buf.String())
	}

	// Second call: reclaimExpected=true (set after the first store), so the
	// disconnect-window ERROR must appear in the log.
	conn2, err := opener(ctx)
	if err != nil {
		t.Fatalf("opener call 2: %v", err)
	}
	defer func() { _ = conn2.Close() }()

	if !strings.Contains(buf.String(), wantMsg) {
		t.Fatalf("second fresh dial did not log the expected error; log: %q", buf.String())
	}
}

// TestStaleChunkDroppedOnClientSwap proves that a chunk enqueued for client A is
// not delivered to client B after a setClient swap. It exercises the epoch-check
// path in chardevWriteLoop directly, simulating the race where the writer has
// already dequeued a chunk stamped for the prior session before the new client
// swaps in (the window the queue flush in setClient cannot catch).
func TestStaleChunkDroppedOnClientSwap(t *testing.T) {
	hub := &drainHub{}

	// Attach client A. Epoch increments to 1.
	c1Local, c1Remote := socketPair(t)
	defer func() { _ = c1Local.Close() }()
	defer func() { _ = c1Remote.Close() }()
	hub.setClient(c1Local)

	// Attach client B. Epoch increments to 2. setClient closes c1Local.
	c2Local, c2Remote := socketPair(t)
	defer func() { _ = c2Local.Close() }()
	defer func() { _ = c2Remote.Close() }()
	hub.setClient(c2Local)

	q := make(chan drainChunk, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run the writer with the current hub state (epoch=2, client=c2Local).
	go chardevWriteLoop(ctx, hub, q)

	// Enqueue a stale chunk stamped with epoch 1 (client A's session).
	// The writer must drop it rather than delivering it to client B.
	q <- drainChunk{data: []byte("stale"), epoch: 1}

	// Enqueue a fresh chunk stamped with epoch 2 (current session).
	// The writer must deliver it to client B.
	q <- drainChunk{data: []byte("fresh"), epoch: 2}

	// Client B should receive exactly the fresh chunk.
	if got := readN(t, c2Remote, 5); got != "fresh" {
		t.Fatalf("client B got %q, want fresh (stale chunk was not dropped)", got)
	}
}

// TestAcquireListenerNoUnlink proves a socket-activated listener does not unlink
// its path on Close, since systemd owns it.
func TestAcquireListenerNoUnlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sock")

	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := syscall.Listen(fd, 5); err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := os.NewFile(uintptr(fd), path)

	ln, err := acquireListener(context.Background(), testLog(), map[string][]*os.File{"relay": {f}}, "")
	if err != nil {
		t.Fatalf("acquireListener: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket path was unlinked on Close: %v", err)
	}
}
