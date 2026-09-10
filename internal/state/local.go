package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Local stores one JSON document per environment beneath a project root,
// conventionally ".infra".
type Local struct {
	root string
}

// NewLocal creates a Local backend rooted at the given directory.
func NewLocal(root string) *Local { return &Local{root: root} }

var _ Backend = (*Local)(nil)

func (l *Local) statePath(environment string) string {
	return filepath.Join(l.root, "state", environment+".json")
}

// Get loads the state for an environment. A missing environment is an empty
// state, not an error: a project that has never applied anything has no state.
func (l *Local) Get(ctx context.Context, environment string) (*State, error) {
	data, err := os.ReadFile(l.statePath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return New("", environment), nil
	}
	if err != nil {
		return nil, err
	}
	s, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", l.statePath(environment), err)
	}
	return s, nil
}

// Put writes state atomically: a temporary file in the same directory, then
// a rename. A crash mid-write therefore cannot corrupt state.
//
// Put refuses to write unless the caller currently holds environment's
// lock, checked by re-reading the lock file's contents and comparing PID
// and host against this process — the same check Unlock already makes. This
// keeps the Backend.Put(ctx, environment, s) signature exactly as the M3
// authoring contract already pins it, consumed by internal/refresh,
// internal/cli and every later M3 task as written: a token-carrying
// signature such as Put(ctx, environment, s, lock) would ripple through
// every one of those call sites and the Backend interface itself, for a
// guarantee the weaker check already gives at the point that actually
// matters — the write. What that costs: the check is enforced at runtime on
// every call rather than at compile time, and each Put now costs one extra
// file read to re-Inspect the lock. Both are cheap next to a signature
// change touching every caller in the tree — refresh and apply each Put
// many times across one run under a single held lock (spec §15), so the
// added read is one stat-and-read alongside a write already going to disk —
// and plan never calls Put at all, so it stays lock-free with no
// special-casing needed here.
func (l *Local) Put(ctx context.Context, environment string, s *State) error {
	if err := l.requireOwnLock(environment); err != nil {
		return err
	}

	path := l.statePath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// Stamp the write, but restore on any failure path: a Serial that has
	// advanced past what is on disk would make a retry skip a value, and any
	// caller inspecting s.Serial after a failed Put would believe a write
	// happened. The serial is what detects stale plans, so it must not drift.
	prevSerial, prevEnv, prevUpdated := s.Serial, s.Environment, s.UpdatedAt
	committed := false
	defer func() {
		if !committed {
			s.Serial, s.Environment, s.UpdatedAt = prevSerial, prevEnv, prevUpdated
		}
	}()

	s.Serial++
	s.Environment = environment
	s.UpdatedAt = time.Now().UTC()

	data, err := s.Encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}
