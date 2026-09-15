package svc

import (
	"fmt"
	"io"
)

// serialMaxWrite caps each underlying write to the FreeBSD tty input
// queue size at 115200 baud (c_ispeed/5 = 23040 bytes). Writes larger
// than the queue silently lose their tail, so yamux frames must be
// chunked beneath the ceiling before reaching the kernel. Eight kilo-
// bytes leaves substantial headroom for back-to-back writes when the
// reader has not yet drained the previous chunk.
const serialMaxWrite = 8 * 1024

// serialStream adapts the virtio-serial fd to the [io.ReadWriteCloser]
// shape yamux.Server expects. The wrapper enforces the FreeBSD tty
// per-write size cap so yamux frames do not get truncated.
type serialStream struct {
	rwc io.ReadWriteCloser
}

func (s serialStream) Read(p []byte) (int, error) {
	n, err := s.rwc.Read(p)
	if err != nil {
		return n, fmt.Errorf("serial: read: %w", err)
	}
	return n, nil
}

func (s serialStream) Write(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		end := min(total+serialMaxWrite, len(p))
		n, err := s.rwc.Write(p[total:end])
		total += n
		if err != nil {
			return total, fmt.Errorf("serial: write: %w", err)
		}
	}
	return total, nil
}

// Close is intentionally a no-op. The underlying virtio-serial fd is
// owned by Serve and must outlive every yamux session built on top of
// it, since yamux.Session.Close cascades through this method when a
// session ends. Closing the fd here would prevent the daemon from
// rebuilding a fresh session over the same chardev. The fd is closed
// by Serve's outer defer when the daemon exits.
func (s serialStream) Close() error {
	return nil
}
