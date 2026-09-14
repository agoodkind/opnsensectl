// Package version reports the OPNsense daemon's build identity. The commit
// and dirty flag come from goodkind.io/gklog/version, which the build stamps
// at link time, and this package adds a hash of the binary on disk so the
// Version RPC can name the exact file the daemon is running.
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	gklogversion "goodkind.io/gklog/version"
)

const unknown = "unknown"

// dirtyStamp is the value gklog's Dirty field carries. The pipeline writes a
// boolean word; the older "clean" and "dirty" spellings are accepted so a
// binary stamped by hand still reports correctly.
type dirtyStamp string

const (
	dirtyStampTrue  dirtyStamp = "true"
	dirtyStampFalse dirtyStamp = "false"
	dirtyStampDirty dirtyStamp = "dirty"
	dirtyStampClean dirtyStamp = "clean"
)

// BinaryHash returns the first 12 hex characters of SHA-256 of the running
// binary. Returns "unknown" on any error.
func BinaryHash() string {
	return binaryHashFrom("")
}

// binaryHashFrom hashes the file at path. If path is empty it falls back to
// [os.Executable]. Not exported (used internally and by tests).
func binaryHashFrom(path string) string {
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return unknown
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return unknown
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return unknown
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// BuildVersionString returns a full one-line summary for startup logs.
func BuildVersionString() string {
	return fmt.Sprintf("commit=%s dirty=%s binhash=%s", GitCommit(), GitDirty(), BinaryHash())
}

// GitCommit returns the stamped git commit, or "unknown".
func GitCommit() string {
	return stampedOrUnknown(gklogversion.Commit)
}

// GitDirty returns "clean", "dirty", or "unknown".
func GitDirty() string {
	switch dirtyStamp(gklogversion.Dirty) {
	case dirtyStampTrue, dirtyStampDirty:
		return "dirty"
	case dirtyStampFalse, dirtyStampClean:
		return "clean"
	default:
		return unknown
	}
}

// stampedOrUnknown normalizes an unstamped value: gklog's own default and an
// empty string both mean the field was never written.
func stampedOrUnknown(value string) string {
	if value == "" {
		return unknown
	}
	return value
}
