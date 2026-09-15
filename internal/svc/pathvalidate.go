package svc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// PathDirection indicates whether a path is being opened for read or
// for write. The two directions carry separate allowlists.
type PathDirection int

const (
	// PathRead opens an existing file for reading.
	PathRead PathDirection = iota
	// PathWrite creates or replaces a file via atomic rename.
	PathWrite
)

// DefaultWriteAllowlist is the on-disk set of directory roots in which
// the daemon will accept WRITE-direction transfers. Each entry must
// already exist when the daemon starts.
var DefaultWriteAllowlist = []string{
	"/conf",
	"/usr/local/sbin",
	"/var/log/mwan",
	"/var/lib/mwan",
	"/tmp",
}

// DefaultReadAllowlist is the on-disk set of directory roots from
// which the daemon will accept READ-direction transfers. Write paths
// are also implicitly readable.
var DefaultReadAllowlist = []string{
	"/etc",
	"/var/log",
	"/var/run",
}

// PathValidator holds the open [os.Root] handles for each allowlisted
// directory and resolves caller-supplied absolute paths into
// per-root relative paths.
type PathValidator struct {
	mu    sync.Mutex
	roots map[string]*os.Root
	log   *slog.Logger
	read  []string
	write []string
}

// NewPathValidator opens an [os.Root] for each entry in the combined
// read and write allowlists. Missing or non-directory entries are
// skipped with a warning so a development daemon can still run when
// some FreeBSD-only paths are absent.
func NewPathValidator(log *slog.Logger, readDirs, writeDirs []string) *PathValidator {
	if log == nil {
		log = slog.Default()
	}
	pv := &PathValidator{
		mu:    sync.Mutex{},
		roots: make(map[string]*os.Root),
		log:   log,
		read:  append([]string(nil), readDirs...),
		write: append([]string(nil), writeDirs...),
	}
	all := make(map[string]struct{})
	for _, dir := range readDirs {
		all[dir] = struct{}{}
	}
	for _, dir := range writeDirs {
		all[dir] = struct{}{}
	}
	for dir := range all {
		root, err := os.OpenRoot(dir)
		if err != nil {
			log.Warn("pathvalidate: open root failed", "dir", dir, "err", err)
			continue
		}
		pv.roots[dir] = root
	}
	return pv
}

// Close releases every open root handle.
func (p *PathValidator) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	for dir, root := range p.roots {
		if err := root.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("pathvalidate: close %s: %w", dir, err)
		}
	}
	p.roots = nil
	return firstErr
}

// OpenForRead returns a read-only handle to the file at path. The
// path must sit beneath one of the read or write allowlist roots,
// must not traverse a symlink, and must not contain `..` segments.
func (p *PathValidator) OpenForRead(path string) (*os.File, error) {
	return p.openWithin(path, PathRead, os.O_RDONLY, 0)
}

// CreateForWrite returns a writable handle to a fresh path beneath
// one of the write allowlist roots. The caller is expected to use
// AtomicWriteFile or to drive the file through a renameio
// PendingFile; this helper is only used when a transfer needs to
// open a per-transfer staging file directly.
func (p *PathValidator) CreateForWrite(path string, mode os.FileMode) (*os.File, error) {
	return p.openWithin(path, PathWrite, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
}

// ResolveWrite returns the absolute path and the parent directory
// for a WRITE-direction transfer. The path must sit beneath a write
// allowlist root. The parent directory must already exist.
func (p *PathValidator) ResolveWrite(path string) (string, string, error) {
	clean, err := p.validate(path, PathWrite)
	if err != nil {
		return "", "", err
	}
	parent := filepath.Dir(clean)
	info, statErr := os.Lstat(parent)
	if statErr != nil {
		return "", "", logWrappedErrorContext(context.Background(), p.log,
			"opnsensesvc: pathvalidate stat parent",
			"pathvalidate: stat parent "+parent, statErr,
			slog.String("parent", parent))
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("pathvalidate: parent %s not a directory", parent)
	}
	return clean, parent, nil
}

// ResolveRead returns the absolute path for a READ-direction
// transfer. The path must sit beneath a read or write allowlist root.
func (p *PathValidator) ResolveRead(path string) (string, error) {
	return p.validate(path, PathRead)
}

func (p *PathValidator) validate(path string, dir PathDirection) (string, error) {
	if path == "" {
		return "", errors.New("pathvalidate: empty path")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("pathvalidate: %s not absolute", path)
	}
	clean := filepath.Clean(path)
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("pathvalidate: %s contains dot-dot segment", path)
	}
	allow := p.write
	if dir == PathRead {
		allow = append(append([]string(nil), p.read...), p.write...)
	}
	for _, root := range allow {
		if pathBeneath(clean, root) {
			return clean, nil
		}
	}
	return "", fmt.Errorf("pathvalidate: %s outside allowlist", clean)
}

func (p *PathValidator) openWithin(path string, dir PathDirection, flag int, mode os.FileMode) (*os.File, error) {
	clean, err := p.validate(path, dir)
	if err != nil {
		return nil, err
	}
	root, rel, ok := p.matchRoot(clean)
	if !ok {
		return nil, fmt.Errorf("pathvalidate: no open root for %s", clean)
	}
	flag |= syscall.O_NOFOLLOW
	file, err := root.OpenFile(rel, flag, mode)
	if err != nil {
		ctx := context.Background()
		if errors.Is(err, fs.ErrInvalid) || isSymlinkLoop(err) {
			return nil, logWrappedErrorContext(ctx, p.log,
				"opnsensesvc: pathvalidate symlink refused",
				"pathvalidate: "+clean+" refused (symlink or invalid)", err,
				slog.String("path", clean))
		}
		return nil, logWrappedErrorContext(ctx, p.log,
			"opnsensesvc: pathvalidate open",
			"pathvalidate: open "+clean, err,
			slog.String("path", clean))
	}
	return file, nil
}

func (p *PathValidator) matchRoot(clean string) (*os.Root, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var bestDir string
	for dir := range p.roots {
		if pathBeneath(clean, dir) && len(dir) > len(bestDir) {
			bestDir = dir
		}
	}
	if bestDir == "" {
		return nil, "", false
	}
	rel, err := filepath.Rel(bestDir, clean)
	if err != nil {
		return nil, "", false
	}
	return p.roots[bestDir], rel, true
}

func pathBeneath(path, root string) bool {
	cleanRoot := filepath.Clean(root)
	if cleanRoot == path {
		return true
	}
	prefix := cleanRoot
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

func isSymlinkLoop(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
