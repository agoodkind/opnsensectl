package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// hostVerb is the typed enum of `mwan opnsense host <verb>` sub-verbs.
// Today the only verb is serve; the namespace is reserved so future
// host-side maintenance commands can land next to it.
type hostVerb string

const (
	hostVerbServe hostVerb = "serve"
	hostVerbDrain hostVerb = "drain"
)

func hostUsage(out *os.File) {
	fmt.Fprintln(out, "usage: opnsensectl host <verb>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Verbs:")
	fmt.Fprintln(out, "  serve --config PATH   run the Proxmox-host-side yamux bridge")
	fmt.Fprintln(out, "  drain --config PATH   run the chardev drainer that holds the qemu chardev open")
}

func runOPNsenseHost(args []string) int {
	if len(args) < 1 {
		hostUsage(os.Stderr)
		return 2
	}
	verb := hostVerb(args[0])
	rest := args[1:]
	switch verb {
	case hostVerbServe:
		return runOPNsenseHostServe(rest)
	case hostVerbDrain:
		return runOPNsenseHostDrain(rest)
	default:
		fmt.Fprintf(os.Stderr, "mwan opnsense host: unknown verb %q\n", string(verb))
		hostUsage(os.Stderr)
		return 2
	}
}

// hostServeCommand is the host bridge service. It needs nothing from the
// environment.
var hostServeCommand = serviceCommand{
	name: "host serve",
	description: []string{
		"Run the Proxmox-host yamux bridge. It dials the drainer's relay socket and",
		"serves the probe socket the operator verbs dial.",
	},
	configDoc:   "TOML file whose [opnsense.host] sets upstream, listen, reconnect, heartbeat_interval, and heartbeat_timeout",
	environment: nil,
}

// runOPNsenseHostServe runs the host-side yamux bridge. All inputs come from
// [opnsense.host] in the file --config names.
func runOPNsenseHostServe(args []string) int {
	configPath, exitCode, ok := parseServiceArgs(hostServeCommand, args)
	if !ok {
		return exitCode
	}

	cfg, err := loadServiceConfig(configPath)
	if err != nil {
		return printAndExit("host serve", err)
	}
	upstream, err := requireHostUpstream(cfg)
	if err != nil {
		return printAndExit("host serve", err)
	}
	listen, err := requireHostListen(cfg)
	if err != nil {
		return printAndExit("host serve", err)
	}
	reconnect, hbInterval, hbTimeout, err := parseHostDurations(cfg)
	if err != nil {
		return printAndExit("host serve", err)
	}

	upstreamPath, ok := unixPath(upstream)
	if !ok {
		return printAndExit("host serve", fmt.Errorf("[opnsense.host].upstream must be unix:///abs/path"))
	}
	if !strings.HasPrefix(listen, "/") {
		return printAndExit("host serve", fmt.Errorf("[opnsense.host].listen must be an absolute path"))
	}

	log := slog.Default()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := removeStaleSocket(listen); err != nil {
		return printAndExit("host serve", err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", listen)
	if err != nil {
		return printAndExit("host serve", fmt.Errorf("listen %s: %w", listen, err))
	}
	if err := restrictSocket(listen); err != nil {
		_ = listener.Close()
		return printAndExit("host serve", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ctx, "opnsense host: stop watcher panic",
					"panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		<-ctx.Done()
		_ = listener.Close()
	}()

	log.InfoContext(ctx, "opnsense host: serving",
		"upstream", upstreamPath,
		"listen", listen,
		"heartbeat_interval", hbInterval.String(),
		"heartbeat_timeout", hbTimeout.String())
	if err := bridgeLoop(ctx, log, listener, upstreamPath, reconnect, hbInterval, hbTimeout); err != nil {
		log.ErrorContext(ctx, "opnsense host: bridge terminated", "err", err)
		return 1
	}
	return 0
}

// bridgeLoop maintains one yamux session at a time and forwards every
// accepted local connection to a fresh substream over the current
// session. When the session dies the next accepted local connection
// triggers a reconnect.
func bridgeLoop(ctx context.Context, log *slog.Logger, listener net.Listener, upstreamPath string, backoff, heartbeatInterval, heartbeatTimeout time.Duration) error {
	state := &sessionState{
		mu:                sync.Mutex{},
		session:           nil,
		upstreamPath:      upstreamPath,
		log:               log,
		backoff:           backoff,
		heartbeatInterval: heartbeatInterval,
		heartbeatTimeout:  heartbeatTimeout,
	}
	defer state.closeSession(ctx)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			wrapped := fmt.Errorf("accept: %w", err)
			log.ErrorContext(ctx, "opnsense host: accept failed", "err", wrapped)
			return wrapped
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.ErrorContext(ctx, "opnsense host: client handler panic",
						"panic", r, "err", fmt.Errorf("panic: %v", r))
				}
			}()
			state.handleClient(ctx, conn)
		}()
	}
}

type sessionState struct {
	mu                sync.Mutex
	session           *yamux.Session
	upstreamPath      string
	log               *slog.Logger
	backoff           time.Duration
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
}

func (s *sessionState) openStream(ctx context.Context) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("openStream: %w", err)
		}
		if s.session == nil || s.session.IsClosed() {
			if err := s.dialLocked(ctx); err != nil {
				s.log.WarnContext(ctx, "opnsense host: upstream dial failed", "err", err, "attempt", attempt)
				if !sleepCtxOK(ctx, s.backoff) {
					return nil, fmt.Errorf("openStream backoff: %w", ctx.Err())
				}
				continue
			}
		}
		stream, err := s.session.OpenStream()
		if err == nil {
			return stream, nil
		}
		s.log.WarnContext(ctx, "opnsense host: open stream failed; reconnecting", "err", err)
		_ = s.session.Close()
		s.session = nil
	}
}

func (s *sessionState) dialLocked(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.upstreamPath)
	if err != nil {
		wrapped := fmt.Errorf("dial upstream %s: %w", s.upstreamPath, err)
		s.log.WarnContext(ctx, "opnsense host: dial upstream failed", "err", wrapped)
		return wrapped
	}
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.MaxStreamWindowSize = 16 * 1024 * 1024
	session, err := yamux.Client(conn, cfg)
	if err != nil {
		_ = conn.Close()
		wrapped := fmt.Errorf("yamux client: %w", err)
		s.log.WarnContext(ctx, "opnsense host: yamux client failed", "err", wrapped)
		return wrapped
	}
	s.session = session
	s.log.InfoContext(ctx, "opnsense host: upstream session established")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "opnsense host: heartbeat goroutine panic",
					"panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		s.runHeartbeat(ctx, session)
	}()
	return nil
}

func (s *sessionState) runHeartbeat(ctx context.Context, watched *yamux.Session) {
	if s.heartbeatInterval <= 0 {
		return
	}
	dialer := func(_ context.Context, _ string) (net.Conn, error) {
		return watched.OpenStream()
	}
	conn, err := grpc.NewClient(
		"passthrough:///mwan-opnsense-heartbeat",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		s.log.WarnContext(ctx, "opnsense host: heartbeat grpc.NewClient failed", "err", err)
		return
	}
	defer func() { _ = conn.Close() }()
	client := mwanv1.NewOpnsenseServiceClient(conn)

	timer := time.NewTimer(s.heartbeatInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-watched.CloseChan():
			return
		case <-timer.C:
		}
		callCtx, cancel := context.WithTimeout(ctx, s.heartbeatTimeout)
		_, err := client.Version(callCtx, &mwanv1.VersionRequest{})
		cancel()
		if err != nil {
			s.log.WarnContext(ctx, "opnsense host: heartbeat Version failed; closing session", "err", err)
			s.dropSession(ctx, watched)
			return
		}
		timer.Reset(s.heartbeatInterval)
	}
}

func (s *sessionState) dropSession(ctx context.Context, target *yamux.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == target {
		s.session = nil
	}
	if err := target.Close(); err != nil && !errors.Is(err, yamux.ErrSessionShutdown) {
		s.log.WarnContext(ctx, "opnsense host: heartbeat session close failed", "err", err)
	}
}

func (s *sessionState) closeSession(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		if err := s.session.Close(); err != nil {
			s.log.WarnContext(ctx, "opnsense host: close session failed", "err", err)
		}
		s.session = nil
	}
}

func (s *sessionState) handleClient(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()
	stream, err := s.openStream(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "opnsense host: open stream for client failed", "err", err)
		return
	}
	defer func() { _ = stream.Close() }()

	errCh := make(chan error, 2)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "opnsense host: copy uplink panic",
					"panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		_, copyErr := io.Copy(stream, client)
		errCh <- copyErr
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "opnsense host: copy downlink panic",
					"panic", r, "err", fmt.Errorf("panic: %v", r))
			}
		}()
		_, copyErr := io.Copy(client, stream)
		errCh <- copyErr
	}()
	<-errCh
}

func sleepCtxOK(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func unixPath(target string) (string, bool) {
	const scheme = "unix://"
	if !strings.HasPrefix(target, scheme) {
		return "", false
	}
	path := strings.TrimPrefix(target, scheme)
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	return path, true
}
