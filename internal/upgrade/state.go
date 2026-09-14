package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is the on-disk snapshot of where one upgrade-deploy lives in
// the lifecycle. It is written to <state_dir>/<vmid>/state.json after
// every phase transition. The shape is designed so a re-invocation of
// the subcommand can read this file and refuse the wrong transition.
type State struct {
	VMID         string    `json:"vmid"`
	DeployID     string    `json:"deploy_id"`
	Target       string    `json:"target"`
	Snapshot     string    `json:"snapshot"`
	Phase        Phase     `json:"phase"`
	UpdatedAt    time.Time `json:"updated_at"`
	FailingCheck []string  `json:"failing_check,omitempty"`
}

// statePathFor builds the path to the per-vmid state file. The schema
// per the design (section 4.7) is one state.json per vmid, sibling to
// the per-deploy directory tree.
func statePathFor(stateDir, vmid string) string {
	return filepath.Join(stateDir, vmid, "state.json")
}

// deployPathFor builds the path to the per-deploy directory. Artifacts
// from prepare (config.xml.pre, version.txt, interfaces.json,
// metadata.json) live under this directory.
func deployPathFor(stateDir, vmid, deployID string) string {
	return filepath.Join(stateDir, vmid, deployID)
}

// readStateFile reads the raw bytes for the state file at the given
// path. The path is composed from the operator-supplied state-dir and
// vmid; gosec G304 is mitigated because [filepath.Join] normalizes
// the result and [filepath.Clean] is applied below.
func readStateFile(path string) ([]byte, error) {
	clean := filepath.Clean(path)
	data, err := os.ReadFile(clean)
	if err != nil {
		slog.Error("upgrade: ReadFile state", "err", err, "path", clean)
		return nil, fmt.Errorf("upgrade: read %q: %w", clean, err)
	}
	return data, nil
}

// loadStateCtx reads the state file for the given vmid. A missing
// file returns a zero-value State with PhaseEmpty and a nil error so
// the caller can treat "no upgrade in flight" as a normal condition.
func loadStateCtx(ctx context.Context, stateDir, vmid string) (State, error) {
	path := statePathFor(stateDir, vmid)
	data, err := readStateFile(path)
	zero := State{
		VMID: "", DeployID: "", Target: "", Snapshot: "",
		Phase: PhaseEmpty, UpdatedAt: time.Time{}, FailingCheck: nil,
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return zero, nil
		}
		slog.ErrorContext(ctx, "upgrade: read state", "err", err, "path", path)
		return zero, fmt.Errorf("upgrade: read state %q: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		slog.ErrorContext(ctx, "upgrade: parse state", "err", err, "path", path)
		return zero, fmt.Errorf("upgrade: parse state %q: %w", path, err)
	}
	return st, nil
}

// saveStateCtx writes the state file atomically. The parent directory
// is created with 0750 if it does not yet exist; this matches the
// design (section 11.6) behaviour where the subcommand bootstraps the
// directory itself if systemd has not pre-created it.
func saveStateCtx(ctx context.Context, stateDir string, st State, now time.Time) error {
	if st.VMID == "" {
		err := errors.New("upgrade: SaveState requires VMID")
		slog.ErrorContext(ctx, "upgrade: SaveState missing VMID", "err", err)
		return err
	}
	st.UpdatedAt = now
	dir := filepath.Join(stateDir, st.VMID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.ErrorContext(ctx, "upgrade: SaveState mkdir", "err", err, "path", dir)
		return fmt.Errorf("upgrade: mkdir %q: %w", dir, err)
	}
	path := statePathFor(stateDir, st.VMID)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: SaveState marshal", "err", err)
		return fmt.Errorf("upgrade: marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state.json.tmp.*")
	if err != nil {
		slog.ErrorContext(ctx, "upgrade: SaveState create temp", "err", err, "dir", dir)
		return fmt.Errorf("upgrade: create temp state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		slog.ErrorContext(ctx, "upgrade: SaveState write temp", "err", err, "path", tmpName)
		return fmt.Errorf("upgrade: write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		slog.ErrorContext(ctx, "upgrade: SaveState close temp", "err", err, "path", tmpName)
		return fmt.Errorf("upgrade: close temp state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		slog.ErrorContext(ctx, "upgrade: SaveState rename", "err", err, "path", path)
		return fmt.Errorf("upgrade: rename state %q: %w", path, err)
	}
	slog.InfoContext(ctx, "upgrade: state saved", "vmid", st.VMID, "phase", st.Phase, "path", path)
	return nil
}

// allowedTransitions encodes the state machine documented in design
// section 5. Each entry maps a source phase to the set of phases that
// are reachable from it. PhaseEmpty is the initial state and only
// transitions to PhasePrepared. PhaseCommitted and PhaseRollbackFailed
// are terminal: no further transitions, only a fresh prepare from
// PhaseEmpty after the operator clears the state file.
var allowedTransitions = map[Phase]map[Phase]struct{}{
	PhaseEmpty: {
		PhasePrepared: {},
	},
	PhasePrepared: {
		PhaseExecuting: {},
		PhasePrepared:  {},
	},
	PhaseExecuting: {
		PhaseExecuted:      {},
		PhaseExecuteFailed: {},
		PhaseExecuteHung:   {},
	},
	PhaseExecuted: {
		PhaseValidatedPass:    {},
		PhaseValidatedPartial: {},
		PhaseValidatedFail:    {},
	},
	PhaseExecuteFailed: {
		PhaseValidatedPass:    {},
		PhaseValidatedPartial: {},
		PhaseValidatedFail:    {},
		PhaseRolledBack:       {},
		PhaseRollbackFailed:   {},
	},
	PhaseExecuteHung: {
		PhaseRolledBack:     {},
		PhaseRollbackFailed: {},
	},
	PhaseValidatedPass: {
		PhaseCommitted: {},
	},
	PhaseValidatedPartial: {
		PhaseRolledBack:     {},
		PhaseRollbackFailed: {},
		PhaseCommitted:      {},
	},
	PhaseValidatedFail: {
		PhaseRolledBack:     {},
		PhaseRollbackFailed: {},
	},
	PhaseRolledBack: {
		PhaseCommitted: {},
	},
}

// CanTransition reports whether the lifecycle allows moving from
// `from` to `to`. The state machine is deliberately strict: any
// transition not enumerated in allowedTransitions is rejected, so a
// careless re-run cannot land the system in an inconsistent state.
func CanTransition(from, to Phase) bool {
	allowed, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// TransitionNotAllowedError is returned by [EnforceTransition] when
// the caller attempts a transition outside the documented state
// machine. The error carries the source and destination phases so the
// caller can render an actionable message without parsing the error
// text.
type TransitionNotAllowedError struct {
	From Phase
	To   Phase
}

// Error renders the transition error in a human-readable form for
// log lines and email bodies.
func (e TransitionNotAllowedError) Error() string {
	return fmt.Sprintf("upgrade: transition %q -> %q not allowed", e.From, e.To)
}

// EnforceTransition returns nil when from -> to is permitted and a
// [TransitionNotAllowedError] otherwise. Phase functions call this
// before writing any state, so an illegal call leaves the state file
// unchanged.
func EnforceTransition(from, to Phase) error {
	if !CanTransition(from, to) {
		return TransitionNotAllowedError{From: from, To: to}
	}
	return nil
}

// SnapshotName builds the snapshot name using the documented prefix
// plus the unix timestamp. Resolved decision 11.8 calls out the exact
// shape so the gc subcommand can match it with a regex.
func SnapshotName(now time.Time) string {
	return fmt.Sprintf("%s%d", SnapshotPrefix, now.Unix())
}

// IsUpgradeSnapshot reports whether `name` matches the upgrade prefix
// shape produced by [SnapshotName]. A snapshot renamed to KeepPrefix
// is not an upgrade snapshot for gc purposes; that is the explicit
// escape hatch called out in resolved decision 11.8.
func IsUpgradeSnapshot(name string) bool {
	if !strings.HasPrefix(name, SnapshotPrefix) {
		return false
	}
	tail := strings.TrimPrefix(name, SnapshotPrefix)
	if tail == "" {
		return false
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
