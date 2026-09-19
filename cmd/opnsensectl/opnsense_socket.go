package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// socketFileMode is the mode of the unix sockets the host services bind: root
// only, because the socket is the only authentication the channel has.
const socketFileMode fs.FileMode = 0o600

// openSocketDir opens the directory that holds the unix socket at path as an
// [os.Root] and returns it with the socket's name in it, so every file
// operation on the socket acts only on that one entry of that directory.
func openSocketDir(path string) (*os.Root, string, error) {
	dir := filepath.Dir(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		slog.Error("opnsense: open socket directory", "dir", dir, "err", err)
		return nil, "", fmt.Errorf("open socket directory %s: %w", dir, err)
	}
	return root, filepath.Base(path), nil
}

// removeStaleSocket deletes a socket file a previous run left at path. A path
// with nothing at it is not an error.
func removeStaleSocket(path string) error {
	root, name, err := openSocketDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("opnsense: clear stale socket", "path", path, "err", err)
		return fmt.Errorf("clear stale socket %s: %w", path, err)
	}
	return nil
}

// restrictSocket sets the socket at path to socketFileMode.
func restrictSocket(path string) error {
	root, name, err := openSocketDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Chmod(name, socketFileMode); err != nil {
		slog.Error("opnsense: chmod socket", "path", path, "err", err)
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
