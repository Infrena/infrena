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
// conventionally ".infrena".
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
// It refuses to write unless this process currently holds the environment's
// lock, re-derived from the lock file rather than carried in a token, so that
// the guarantee lands at the write itself and no caller has to thread a lock
// through. The cost is one extra file read per Put.
//
// That check is check-then-act, not a held handle, so a human who force-unlocks
// mid-run can let this Put land after another process takes the lock. Two
// ordinary concurrent applies cannot reach that state, because Lock's O_EXCL
// create makes them mutually exclusive from the start.
func (l *Local) Put(ctx context.Context, environment string, s *State) error {
	if err := l.requireOwnLock(ctx, environment); err != nil {
		return err
	}

	path := l.statePath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	// Stamp the write, but restore on any failure path. The serial is what
	// detects stale plans, so a serial that advanced past what is on disk
	// would make a retry skip a value and tell a caller a write happened.
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
// An environment is reachable if it is declared or it has state, and discover
// needs the whole set: a resource managed in one environment is managed
// whichever environment you import into, and adopting it twice would put one
// real resource under two addresses.
//
// A missing directory is an empty list, not an error: a project that has never
// applied anything is the ordinary case for the command that needs this.
//
// Only files named exactly as statePath writes them count. Lock files and
// Put's in-flight temporaries sit in the same directory, and reporting either
// as an environment would have discover exclude resources against a file
// holding no resources at all.
func (l *Local) List(ctx context.Context) ([]string, error) {
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
