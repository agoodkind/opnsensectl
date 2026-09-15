// Package svc hosts the gRPC services that the mwan-opnsense
// daemon exposes over its virtio-serial transport.
package svc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
)

// AtomicWriteFile writes content to target through a same-directory
// temp file with a cryptographically random suffix, fsyncs the temp
// file, atomically renames it into place, and fsyncs the parent
// directory so the rename is durable across crash. The mode is applied
// to the temp file before rename.
func AtomicWriteFile(ctx context.Context, target string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(target)
	pending, err := renameio.NewPendingFile(target, renameio.WithStaticPermissions(mode))
	if err != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite pending", "atomicwrite: pending "+target, err,
			slog.String("path", target))
	}
	defer func() { _ = pending.Cleanup() }()
	if _, writeErr := pending.Write(content); writeErr != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite write", "atomicwrite: write "+target, writeErr,
			slog.String("path", target))
	}
	if closeErr := pending.CloseAtomicallyReplace(); closeErr != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite rename", "atomicwrite: rename "+target, closeErr,
			slog.String("path", target))
	}
	slog.Default().DebugContext(ctx, "opnsensesvc: AtomicWriteFile",
		slog.String("path", target), slog.Int("bytes", len(content)))
	return fsyncDir(ctx, dir)
}

// AtomicRenameFile renames an existing source file into target,
// then fsyncs the parent directory of target. The caller is
// responsible for fsyncing the source's contents before calling.
func AtomicRenameFile(ctx context.Context, source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite rename", "atomicwrite: rename "+source+" -> "+target, err,
			slog.String("source", source), slog.String("target", target))
	}
	slog.Default().DebugContext(ctx, "opnsensesvc: AtomicRenameFile",
		slog.String("source", source), slog.String("target", target))
	return fsyncDir(ctx, filepath.Dir(target))
}

// fsyncDir opens the directory and calls Sync on its file descriptor.
// On Linux and FreeBSD this flushes directory metadata so a rename is
// durable across a crash. renameio/v2 does not do this on its own.
func fsyncDir(ctx context.Context, dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite open dir", "atomicwrite: open dir "+dir, err,
			slog.String("dir", dir))
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite sync dir", "atomicwrite: sync dir "+dir, syncErr,
			slog.String("dir", dir))
	}
	if closeErr != nil {
		return logWrappedErrorContext(ctx, slog.Default(),
			"opnsensesvc: atomicwrite close dir", "atomicwrite: close dir "+dir, closeErr,
			slog.String("dir", dir))
	}
	return nil
}
