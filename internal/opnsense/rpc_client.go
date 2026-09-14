// Package opnsense is the gRPC client for the mwan-opnsense daemon.
package opnsense

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"

	mwanv1 "goodkind.io/opnsensectl/gen/opnsense/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client speaks gRPC to the mwan-opnsense daemon over the OOB Unix
// socket. The socket is the Proxmox-side endpoint for the virtio-
// serial channel; only root on the host has access, so the gRPC
// transport runs without TLS.
type Client struct {
	target   string
	log      *slog.Logger
	conn     *grpc.ClientConn
	opnsense mwanv1.OpnsenseServiceClient
	transfer mwanv1.TransferServiceClient
	mu       sync.Mutex
	closed   bool
}

// ErrClientClosed surfaces when a call is attempted after Close.
var ErrClientClosed = errors.New("opnsense: client is closed")

// DialContext is the ctx-aware variant of Dial. The underlying
// grpc.NewClient is lazy, so the ctx mainly governs the initial
// argument validation and slog records; the socket dial itself
// happens on the first RPC. Callers that already hold a ctx should
// prefer this entry point.
func DialContext(ctx context.Context, target string) (*Client, error) {
	c, err := dialUnix(ctx, target)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Dial opens a gRPC client against the unix-socket target.
func Dial(target string) (*Client, error) {
	c, err := dialUnix(context.Background(), target)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func dialUnix(ctx context.Context, target string) (*Client, error) {
	if target == "" {
		return nil, errors.New("opnsense: empty target")
	}
	socketPath, ok := unixTargetPath(target)
	if !ok {
		return nil, errors.New("opnsense: only unix:// targets supported")
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "unix", socketPath)
		if err != nil {
			return nil, logWrap(ctx, slog.Default(), "unix dial "+socketPath, err)
		}
		return c, nil
	}
	conn, err := grpc.NewClient(
		"passthrough:///"+socketPath,
		grpc.WithWriteBufferSize(0),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, logWrap(ctx, slog.Default(), "grpc.NewClient", err)
	}
	c := &Client{
		target:   target,
		log:      slog.Default(),
		conn:     conn,
		opnsense: mwanv1.NewOpnsenseServiceClient(conn),
		transfer: mwanv1.NewTransferServiceClient(conn),
		mu:       sync.Mutex{},
		closed:   false,
	}
	return c, nil
}

// Close terminates the underlying gRPC connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if err := c.conn.Close(); err != nil {
		return logWrap(context.Background(), c.log, "close", err)
	}
	return nil
}

// OpnsenseClient returns the generated gRPC client stub for direct
// streaming use.
func (c *Client) OpnsenseClient() mwanv1.OpnsenseServiceClient { return c.opnsense }

// TransferClient returns the generated gRPC client stub for direct
// streaming use.
func (c *Client) TransferClient() mwanv1.TransferServiceClient { return c.transfer }

// Logger returns the logger used by client-side helpers.
func (c *Client) Logger() *slog.Logger { return c.log }

func unixTargetPath(target string) (string, bool) {
	const unixScheme = "unix://"
	if !strings.HasPrefix(target, unixScheme) {
		return "", false
	}
	path := strings.TrimPrefix(target, unixScheme)
	if path == "" {
		return "", false
	}
	return path, true
}
