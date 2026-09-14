//go:build !freebsd

package svc

import (
	"errors"
	"io"
	"log/slog"
)

// OpenVirtioSerial is the non-FreeBSD stub. The mwan-opnsense daemon
// only runs on FreeBSD; this exists so the package compiles on other
// platforms (for `go test ./...` and the cross-compile gate).
func OpenVirtioSerial(_ string, _ uint32, _ *slog.Logger) (io.ReadWriteCloser, error) {
	return nil, errors.New("OpenVirtioSerial: only supported on FreeBSD")
}
