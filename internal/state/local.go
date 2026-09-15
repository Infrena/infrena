package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// stateDir is the one directory Local keeps state and locks in. statePath,
// lockPath and List all derive from it so a layout change stays in one place.
func (l *Local) stateDir() string { return filepath.Join(l.root, "state") }

func (l *Local) statePath(environment string) string {
	return filepath.Join(l.stateDir(), environment+".json")
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
//
// This is check-then-act, not a held handle: requireOwnLock reads the lock
// file, and nothing holds it across the write that follows. Atomicity — the
// temp-file-plus-rename below — does not close this window: atomicity
// protects one write from being torn by a crash mid-write, which is
// orthogonal to two complete writes racing each other. If a human
// force-unlocks this environment mid-run and a different process acquires
// the lock in the gap, this Put can still land after that. That is accepted
// for M3: it requires a human to deliberately intervene on a live lock, not
// two ordinary concurrent applies, which is what invariant 5 actually
// guards against — Lock's O_EXCL create already makes two applies mutually
// exclusive from the start. Closing the gap fully would mean holding an
// open file handle (or a lease) across the write instead of re-reading a
// path, which is more than this check-then-act design costs today.
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

// List reports the environments that have state, sorted.
//
// PLAN.md section 6.1 makes an environment reachable if it is DECLARED or it
// HAS STATE, and until now the second half was only ever answered for one
// named environment at a time. Discover needs the whole set: a resource
// managed in production is managed whichever environment you are importing
// into, and adopting it twice would put one real resource under two addresses
// for invariant 1 to then schedule for destruction under whichever loses.
//
// A missing directory is an empty list, not an error: a project that has
// never applied anything is the ordinary case for the command that needs
// this.
//
// Only files named exactly as statePath writes them count. Lock files
// (.lock) and Put's in-flight temporaries (.state-*.tmp) sit in the same
// directory, and reporting either as an environment would have discover
// exclude resources against a file holding no resources at all.
func (l *Local) List() ([]string, error) {
	entries, err := os.ReadDir(l.stateDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var environments []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		env, ok := strings.CutSuffix(name, ".json")
		if !ok || env == "" {
			continue
		}
		environments = append(environments, env)
	}
	slices.Sort(environments)
	return environments, nil
}
