package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/activation"
	"github.com/coreos/go-systemd/v22/daemon"
)

const (
	// drainBufSize is the per-read buffer for both relay directions.
	drainBufSize = 64 * 1024
	// drainReconnectBackoff paces chardev re-dials while the VM is down.
	drainReconnectBackoff = 2 * time.Second
	// drainQueueDepth is the number of chunks the reader can stage before
	// the writer catches up. At drainBufSize each, 256 slots is ~16 MiB.
	// A client that keeps reading never comes close to filling it.
	drainQueueDepth = 256
)

// drainChunk carries a byte slice and the hub epoch at which it was enqueued.
// The writer discards any chunk whose epoch does not match the current hub epoch,
// so a chunk that was dequeued by the writer before a client swap is never
// delivered to the new client.
type drainChunk struct {
	data  []byte
	epoch uint64
}

// drainHub holds the current client and chardev connections. Both sides have
// independent lifecycles: the bridge (client) reconnects on its own schedule,
// and the chardev is re-dialed only when the VM restarts. The pumps read the
// current peer from the hub on each iteration, so a swap on either side is
// picked up without tearing the other side down.
//
// toClient is the bounded queue from the chardev reader goroutine to the
// writer goroutine. The reader never blocks on the client; on overflow it
// drops the client instead of bytes. toClient is set by drainChardev and
// flushed by setClient on a client swap.
//
// epoch is incremented by setClient on every swap so the writer can detect
// a chunk that was dequeued from a prior session and drop it.
type drainHub struct {
	mu       sync.Mutex
	client   net.Conn
	chardev  net.Conn
	epoch    uint64
	toClient chan drainChunk
}

// setClient swaps the client and flushes any queued bytes from the prior
// session, so a reconnecting bridge never receives the dead session's tail.
// It also increments the epoch so the writer can drop any chunk it has already
// dequeued but not yet written (the in-flight race that the queue flush cannot catch).
func (h *drainHub) setClient(c net.Conn) {
	h.mu.Lock()
	old := h.client
	h.client = c
	h.epoch++
	for h.toClient != nil {
		select {
		case <-h.toClient:
			continue
		default:
		}
		break
	}
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (h *drainHub) getClient() net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.client
}

func (h *drainHub) clearClient(c net.Conn) {
	h.mu.Lock()
	if h.client == c {
		h.client = nil
	}
	h.mu.Unlock()
}

func (h *drainHub) setChardev(c net.Conn) {
	h.mu.Lock()
	h.chardev = c
	h.mu.Unlock()
}

func (h *drainHub) getChardev() net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.chardev
}

// runOPNsenseHostDrain runs the host-side chardev drainer. It holds the qemu
// virtio-serial chardev open and always reads it, so a bridge restart never
// disconnects the host side and strands a guest write in the kernel (see
// docs/ops/opnsense/wedge.md). The bridge dials the relay socket this serves in
// place of the chardev. The chardev connection survives a drainer restart via
// the systemd file descriptor store, so a deploy opens no wedge window.
func runOPNsenseHostDrain(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Fprintln(os.Stdout, "usage: mwan opnsense host drain")
			fmt.Fprintln(os.Stdout, "")
			fmt.Fprintln(os.Stdout, "Reads chardev/listen from [opnsense.drain] in TOML. Holds the qemu")
			fmt.Fprintln(os.Stdout, "chardev open and relays it to the bridge over the listen socket.")
			return 0
		}
	}
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "mwan opnsense host drain: unexpected arguments: %v\n", args)
		return 2
	}

	cfg, err := loadOpnsenseConfig()
	if err != nil {
		return printAndExit("host drain", err)
	}
	chardevTarget, err := requireDrainChardev(cfg)
	if err != nil {
		return printAndExit("host drain", err)
	}
	listenPath, err := requireDrainListen(cfg)
	if err != nil {
		return printAndExit("host drain", err)
	}
	chardevPath, ok := unixPath(chardevTarget)
	if !ok {
		return printAndExit("host drain", fmt.Errorf("[opnsense.drain].chardev must be unix:///abs/path"))
	}

	log := slog.Default()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Fds passed by systemd: the socket-activated relay listener (name
	// "relay") and, after a restart, the live chardev connection from the fd
	// store (name "chardev"). The map is empty when not run under systemd.
	byName := activation.FilesWithNames()

	ln, err := acquireListener(ctx, log, byName, listenPath)
	if err != nil {
		return printAndExit("host drain", fmt.Errorf("acquire listener: %w", err))
	}
	// Closing ln on return (after runDrainRelay sees ctx cancelled) unblocks
	// acceptLoop's Accept, so no separate ctx-watcher goroutine is needed.
	defer func() { _ = ln.Close() }()

	// Signal readiness once the relay socket is up, independent of the chardev.
	// The drainer can accept the bridge while it re-dials a chardev whose VM is
	// down, so a Type=notify start must not hang waiting for the VM to boot.
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	openChardev := makeChardevOpener(log, byName, chardevPath, acquireChardev)

	log.InfoContext(ctx, "opnsense drain: serving", "chardev", chardevPath, "listen", listenPath)
	runDrainRelay(ctx, log, ln, openChardev)
	return 0
}

// makeChardevOpener returns the stateful openChardev func passed to runDrainRelay.
// On the first call it offers byName to acquire for fd-store adoption; on all
// later calls it passes nil so a VM-restart re-dial is always fresh (systemd
// auto-prunes the stored fd on POLLHUP when the VM goes down, so there is
// nothing to adopt). After the first successful store or adopt, reclaimExpected
// is true. A subsequent fresh dial while it is true means the previous drainer
// process died without having stored the fd; that is the only window in which
// qemu can see a chardev disconnect while the VM is up.
//
// No path in the returned function closes the chardev fd. The fd outlives the
// drainer process in the systemd fd-store and is reclaimed by the next instance.
// The only intentional fd-close windows are:
//   - runDrainRelay closes the local fd after drainChardev returns (the VM went
//     down; systemd already pruned the store entry on POLLHUP before this point).
//   - ctx cancellation tears down the process; the fd-store carries the fd forward.
func makeChardevOpener(
	log *slog.Logger,
	byName map[string][]*os.File,
	chardevPath string,
	acquire func(context.Context, *slog.Logger, map[string][]*os.File, string) (net.Conn, bool, error),
) func(context.Context) (net.Conn, error) {
	first := true
	reclaimExpected := false
	return func(ctx context.Context) (net.Conn, error) {
		var reclaimable map[string][]*os.File
		if first {
			reclaimable = byName
		}
		conn, adopted, err := acquire(ctx, log, reclaimable, chardevPath)
		first = false
		if err != nil {
			return nil, err
		}
		if !adopted {
			if reclaimExpected {
				log.ErrorContext(ctx, "opnsense drain: fresh chardev dial while a stored fd was expected; this is the only wedge window",
					"err", errors.New("no stored fd reclaimed; drainer restarted without fd-store handoff"))
			}
			// A failed fdstore upload is non-fatal: the drainer still serves,
			// it just loses the zero-window hand-off on its next restart.
			// storeChardevFD logs the failure.
			_ = storeChardevFD(ctx, log, conn)
		}
		reclaimExpected = true
		return conn, nil
	}
}

// runDrainRelay owns the relay. It accepts bridge clients on ln and keeps a
// chardev connection (from openChardev) continuously drained, re-opening the
// chardev when a session ends. It returns when ctx is cancelled. The chardev
// transport detail lives in openChardev, so this loop is the same under systemd
// and in tests.
func runDrainRelay(ctx context.Context, log *slog.Logger, ln net.Listener, openChardev func(context.Context) (net.Conn, error)) {
	hub := &drainHub{}
	spawn(ctx, log, "drain accept", func() { acceptLoop(ctx, log, hub, ln) })
	for {
		if ctx.Err() != nil {
			return
		}
		chardev, err := openChardev(ctx)
		if err != nil {
			log.WarnContext(ctx, "opnsense drain: chardev unavailable; retrying", "err", err)
			if !sleepCtxOK(ctx, drainReconnectBackoff) {
				return
			}
			continue
		}
		hub.setChardev(chardev)
		serveErr := drainChardev(ctx, log, hub, chardev)
		hub.setChardev(nil)
		_ = chardev.Close()
		if ctx.Err() != nil {
			return
		}
		log.WarnContext(ctx, "opnsense drain: chardev session ended; re-opening", "err", serveErr)
		if !sleepCtxOK(ctx, drainReconnectBackoff) {
			return
		}
	}
}

// acquireListener prefers the socket-activated relay listener passed by systemd
// (fd name "relay") and falls back to binding the path directly when not run
// under systemd (manual runs and tests). A reclaimed unix listener must not
// unlink its path on Close, since systemd owns it.
func acquireListener(ctx context.Context, log *slog.Logger, byName map[string][]*os.File, listenPath string) (net.Listener, error) {
	if fs := byName["relay"]; len(fs) > 0 {
		ln, err := net.FileListener(fs[0])
		_ = fs[0].Close()
		if err != nil {
			log.ErrorContext(ctx, "opnsense drain: socket-activated listener unusable", "err", err)
			return nil, fmt.Errorf("file listener: %w", err)
		}
		if ul, ok := ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		log.InfoContext(ctx, "opnsense drain: adopted socket-activated relay listener")
		return ln, nil
	}
	if err := os.Remove(listenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.ErrorContext(ctx, "opnsense drain: clear stale relay socket", "path", listenPath, "err", err)
		return nil, fmt.Errorf("clear stale relay socket %s: %w", listenPath, err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", listenPath)
	if err != nil {
		log.ErrorContext(ctx, "opnsense drain: bind relay listener", "path", listenPath, "err", err)
		return nil, fmt.Errorf("listen %s: %w", listenPath, err)
	}
	if err := os.Chmod(listenPath, 0o600); err != nil {
		_ = ln.Close()
		log.ErrorContext(ctx, "opnsense drain: chmod relay listener", "path", listenPath, "err", err)
		return nil, fmt.Errorf("chmod %s: %w", listenPath, err)
	}
	log.InfoContext(ctx, "opnsense drain: bound relay listener", "path", listenPath)
	return ln, nil
}

// acquireChardev adopts the live chardev connection reclaimed from the fd store
// (name "chardev") when present, else dials the chardev fresh. The bool reports
// whether the connection was adopted, so the caller stores a freshly dialed one
// but does not re-store an adopted one.
func acquireChardev(ctx context.Context, log *slog.Logger, byName map[string][]*os.File, path string) (net.Conn, bool, error) {
	if fs := byName["chardev"]; len(fs) > 0 {
		c, err := net.FileConn(fs[0])
		_ = fs[0].Close()
		if err == nil {
			log.InfoContext(ctx, "opnsense drain: adopted reclaimed chardev fd")
			return c, true, nil
		}
		log.WarnContext(ctx, "opnsense drain: reclaimed chardev fd unusable; dialing fresh", "err", err)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, false, fmt.Errorf("dial chardev %s: %w", path, err)
	}
	log.InfoContext(ctx, "opnsense drain: dialed chardev")
	return c, false, nil
}

// storeChardevFD uploads the chardev connection fd to the systemd file
// descriptor store so it survives a drainer restart. It reads the fd without
// net.Conn.File, which would force the connection into blocking mode. FDPOLL is
// left at the default so systemd auto-prunes the entry when the chardev closes
// on POLLHUP (the VM went down); a live fd is never pruned and is passed back
// to the next instance.
func storeChardevFD(ctx context.Context, log *slog.Logger, c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		err := fmt.Errorf("chardev conn is %T, not *net.UnixConn", c)
		log.ErrorContext(ctx, "opnsense drain: fdstore upload", "err", err)
		return err
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		log.ErrorContext(ctx, "opnsense drain: fdstore upload", "err", err)
		return fmt.Errorf("chardev syscall conn: %w", err)
	}
	var sendErr error
	ctlErr := rc.Control(func(fd uintptr) {
		sendErr = notifyWithFD(ctx, log, "FDSTORE=1\nFDNAME=chardev", int(fd))
	})
	if ctlErr != nil {
		log.ErrorContext(ctx, "opnsense drain: fdstore upload", "err", ctlErr)
		return fmt.Errorf("chardev rawconn control: %w", ctlErr)
	}
	if sendErr == nil {
		log.InfoContext(ctx, "opnsense drain: stored chardev fd in systemd fdstore")
	}
	return sendErr
}

// notifyWithFD sends a single fd to the systemd notify socket as SCM_RIGHTS
// ancillary data, alongside the state payload (for example "FDSTORE=1"). The
// go-systemd daemon helper cannot pass fds, so this hand-builds the message. It
// is a no-op when not run under systemd (NOTIFY_SOCKET unset).
func notifyWithFD(ctx context.Context, log *slog.Logger, state string, fd int) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	name := sock
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:] // abstract namespace socket
	}
	// Send from an unbound datagram socket with sendmsg addressed to the notify
	// socket, carrying the fd as SCM_RIGHTS ancillary data. Go's net package
	// cannot autobind a unixgram socket, so this uses raw syscalls. This is the
	// canonical sd_notify-with-fds path.
	s, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		log.ErrorContext(ctx, "opnsense drain: notify socket", "err", err)
		return fmt.Errorf("notify socket: %w", err)
	}
	defer func() { _ = syscall.Close(s) }()
	syscall.CloseOnExec(s)
	dst := &syscall.SockaddrUnix{Name: name}
	if err := syscall.Sendmsg(s, []byte(state), syscall.UnixRights(fd), dst, 0); err != nil {
		log.ErrorContext(ctx, "opnsense drain: notify sendmsg", "err", err)
		return fmt.Errorf("notify sendmsg: %w", err)
	}
	return nil
}

// acceptLoop accepts bridge connections one at a time and pumps each one toward
// the current chardev. It runs for the life of the process and exits when the
// listener closes on context cancel.
// Panics in acceptLoop are recovered by its launch wrapper in runDrainRelay.
func acceptLoop(ctx context.Context, log *slog.Logger, hub *drainHub, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			// A normal stop closes the listener on ctx cancel, so only an
			// Accept failure with the context still live is unexpected and
			// worth a log signal (the relay stops accepting after it).
			if ctx.Err() == nil {
				log.WarnContext(ctx, "opnsense drain: relay accept failed; stopping accept loop", "err", err)
			}
			return
		}
		hub.setClient(c)
		spawn(ctx, log, "drain client", func() { clientPump(hub, c) })
	}
}

// clientPump copies one client (the bridge) to the current chardev. It stops
// when the client errors or is superseded by a newer client. A failed chardev
// write drops the client, which reconnects; that never wedges, because only the
// chardev read side matters for completing guest writes.
func clientPump(hub *drainHub, c net.Conn) {
	defer func() {
		hub.clearClient(c)
		_ = c.Close()
	}()
	buf := make([]byte, drainBufSize)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if dev := hub.getChardev(); dev != nil {
				// Lossless blocking write: a slow guest applies backpressure,
				// which is safe because the chardev stays connected. No write
				// deadline, since a partial write would corrupt the byte stream.
				if _, werr := dev.Write(buf[:n]); werr != nil {
					return
				}
			}
		}
		if err != nil || hub.getClient() != c {
			return
		}
	}
}

// drainChardev reads one chardev continuously and forwards to the attached
// client. A reader goroutine always drains the chardev into a bounded queue; a
// writer goroutine forwards from the queue to the current client. The reader
// never blocks on the client write: on queue overflow the client is dropped
// (never bytes), so a hung bridge cannot stall a guest write. With no client
// attached, queued chunks are discarded by the writer so the guest write still
// completes. It returns when the chardev errors (VM down) or ctx is done.
func drainChardev(ctx context.Context, log *slog.Logger, hub *drainHub, chardev net.Conn) error {
	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()

	q := make(chan drainChunk, drainQueueDepth)
	hub.mu.Lock()
	hub.toClient = q
	hub.mu.Unlock()
	defer func() {
		hub.mu.Lock()
		hub.toClient = nil
		hub.mu.Unlock()
	}()

	readErr := make(chan error, 1)
	spawn(ctx, log, "drain reader", func() { chardevReadLoop(ctx, log, hub, chardev, q, readErr) })
	spawn(ctx, log, "drain writer", func() { chardevWriteLoop(dctx, hub, q) })
	select {
	case <-ctx.Done():
		return nil
	case err := <-readErr:
		return err
	}
}

// chardevReadLoop drains the chardev into q. It never blocks on the client:
// on queue overflow it drops the client instead of bytes, because dropping
// bytes corrupts the framed yamux stream (rejected design from commit 2f5f419).
// Each chunk is stamped with the current hub epoch so the writer can drop
// chunks that belong to a prior session.
func chardevReadLoop(ctx context.Context, log *slog.Logger, hub *drainHub, chardev net.Conn, q chan drainChunk, readErr chan error) {
	buf := make([]byte, drainBufSize)
	for {
		n, err := chardev.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			hub.mu.Lock()
			ep := hub.epoch
			hub.mu.Unlock()
			select {
			case q <- drainChunk{data: data, epoch: ep}:
			default:
				// Queue full: client is hung or too slow. Drop the client;
				// never drop bytes to a surviving session.
				if c := hub.getClient(); c != nil {
					hub.clearClient(c)
					_ = c.Close()
				}
			}
		}
		if err != nil {
			log.WarnContext(ctx, "opnsense drain: chardev read ended", "err", err)
			select {
			case readErr <- fmt.Errorf("chardev read: %w", err):
			default:
			}
			return
		}
	}
}

// chardevWriteLoop forwards chunks from q to the current client. It drops any
// chunk whose epoch does not match the current hub epoch, which covers the race
// where the writer dequeued a chunk before a setClient swap could flush it.
// With no client the chunk is discarded so the chardev still drains and guest
// writes complete. It returns when ctx is done.
func chardevWriteLoop(ctx context.Context, hub *drainHub, q chan drainChunk) {
	for {
		select {
		case <-ctx.Done():
			return
		case chunk := <-q:
			hub.mu.Lock()
			cur := hub.epoch
			c := hub.client
			hub.mu.Unlock()
			if chunk.epoch != cur {
				continue // stale chunk from a prior session: drop it
			}
			if c != nil {
				if _, werr := c.Write(chunk.data); werr != nil {
					hub.clearClient(c)
					_ = c.Close()
				}
			}
		}
	}
}

// spawn runs fn in a goroutine with a recover that logs any panic, so one
// broken relay connection never crashes the drainer.
func spawn(ctx context.Context, log *slog.Logger, where string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ctx, "opnsense drain: goroutine panic",
					"where", where, "panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		fn()
	}()
}
