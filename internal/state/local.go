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

// Put writes state atomically: a temporary file in the same directory, then a
// rename. A crash mid-write therefore cannot corrupt state.
func (l *Local) Put(ctx context.Context, environment string, s *State) error {
	path := l.statePath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

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
	return os.Rename(tmpName, path)
}
