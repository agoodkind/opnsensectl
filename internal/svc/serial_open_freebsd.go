//go:build freebsd

package svc

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"
)

// OpenVirtioSerial opens a virtio-serial-pci character device for
// read+write in RAW mode at the requested baud. On FreeBSD with
// virtio_console loaded, the named ports appear under
// /dev/ttyV<bus>.<port>.
//
// Why raw mode: by default, opening a tty character device (which
// /dev/ttyV0.x is, despite being a virtio-serial named port) gives
// you the cooked tty line discipline. That eats arbitrary binary
// bytes, echoes input back, translates newlines, and rings a bell
// when its small input queue fills. The block below disables every
// cooked-mode feature so bytes pass through unchanged in both
// directions.
//
// Why baud matters: the FreeBSD tty input queue is sized as
// c_ispeed/5 bytes, capped at 65536. The kernel caps the speed itself
// at 115200, so the queue holds 23040 bytes at the maximum baud. Any
// single underlying write whose length exceeds the queue size loses
// its tail silently. The MWN1 streaming protocol keeps fragments well
// under that ceiling so chunks always fit.
func OpenVirtioSerial(path string, baud uint32, log *slog.Logger) (io.ReadWriteCloser, error) {
	if log == nil {
		log = slog.Default()
	}

	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		log.Warn("opnsensesvc: open serial failed",
			slog.String("path", path),
			slog.Any("err", err))
		return nil, fmt.Errorf("OpenVirtioSerial: open %s: %w", path, err)
	}

	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	if err != nil {
		_ = f.Close()
		log.Warn("opnsensesvc: TIOCGETA failed",
			slog.String("path", path),
			slog.Any("err", err))
		return nil, fmt.Errorf("OpenVirtioSerial: TIOCGETA %s: %w", path, err)
	}

	t.Iflag &^= unix.IMAXBEL | unix.IXOFF | unix.INPCK | unix.BRKINT |
		unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL |
		unix.IXON | unix.IGNPAR
	t.Iflag |= unix.IGNBRK
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHOKE | unix.ECHONL |
		unix.ECHOPRT | unix.ECHOCTL | unix.ICANON | unix.ISIG | unix.IEXTEN |
		unix.NOFLSH | unix.TOSTOP | unix.PENDIN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	if unix.HUPCL != 0 {
		t.Cflag &^= unix.HUPCL
	}
	// VMIN=1, VTIME=0 -> blocking read returns as soon as one byte is
	// available, no inter-byte timeout.
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	// Baud directly drives c_ispeed/5 input queue sizing in the kernel,
	// which caps how many bytes a single write to the host-side socket
	// can deliver. 115200 is the kernel's hard ceiling per tty.c.
	t.Ispeed = baud
	t.Ospeed = baud

	if err := unix.IoctlSetTermios(int(f.Fd()), unix.TIOCSETA, t); err != nil {
		_ = f.Close()
		log.Warn("opnsensesvc: TIOCSETA failed",
			slog.String("path", path),
			slog.Any("err", err))
		return nil, fmt.Errorf("OpenVirtioSerial: TIOCSETA %s: %w", path, err)
	}

	// Drop any bytes left in the tty input queue from a prior session.
	// FreeBSD's virtio_console driver does not flush the queue on close
	// of the userspace endpoint, so leftover yamux/serialconn frames
	// from the previous daemon would corrupt the new session's
	// handshake. TIOCFLUSH with FREAD (0x1, from <sys/file.h>) clears
	// the read queue without disturbing the kernel-side framing state.
	const ttyReadQueue = 1
	if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, ttyReadQueue); err != nil {
		_ = f.Close()
		log.Warn("opnsensesvc: TIOCFLUSH failed",
			slog.String("path", path),
			slog.Any("err", err))
		return nil, fmt.Errorf("OpenVirtioSerial: TIOCFLUSH %s: %w", path, err)
	}

	// Read termios back so the log line reflects what the kernel
	// actually applied. The kernel clamps c_ispeed/c_ospeed at 115200
	// silently if a higher value is requested.
	applied, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	if err != nil {
		_ = f.Close()
		log.Warn("opnsensesvc: TIOCGETA readback failed",
			slog.String("path", path),
			slog.Any("err", err))
		return nil, fmt.Errorf("OpenVirtioSerial: TIOCGETA readback %s: %w", path, err)
	}

	log.Info("opnsensesvc: serial opened",
		slog.String("path", path),
		slog.Uint64("ispeed", uint64(applied.Ispeed)),
		slog.Uint64("ospeed", uint64(applied.Ospeed)),
		slog.String("icanon", flagState(applied.Lflag, unix.ICANON)),
		slog.String("imaxbel", flagState(applied.Iflag, unix.IMAXBEL)),
		slog.String("ixon", flagState(applied.Iflag, unix.IXON)),
		slog.String("echo", flagState(applied.Lflag, unix.ECHO)),
		slog.String("icrnl", flagState(applied.Iflag, unix.ICRNL)),
		slog.String("opost", flagState(applied.Oflag, unix.OPOST)),
		slog.Int("vmin", int(applied.Cc[unix.VMIN])),
		slog.Int("vtime", int(applied.Cc[unix.VTIME])))

	return f, nil
}

// flagState renders a single termios bit as "on" or "off" for log
// readability.
func flagState(value, bit uint32) string {
	if value&bit != 0 {
		return "on"
	}
	return "off"
}
