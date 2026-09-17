// Command backendplugin is the remote state backend this package's migration
// tests run as a real subprocess.
//
// IT STORES STATE IN A DIRECTORY, which is the one thing internal/backendhost's
// in-memory test backend cannot do for these tests: every `runCommand` opens a
// fresh backend process, so a map dies with the process that held it and a
// migration could never be observed by the command that ran next. A directory
// outlives the process the way a bucket outlives a run, which is the property
// the three-case decision is actually about.
//
// It behaves according to the name it was launched under, so one build serves
// every case. infrena-backend-store keeps state under the directory its
// `backend:` block names; infrena-backend-brokenstore reads exactly the same
// way and refuses every write, which is how a test stages a destination that
// cannot accept state without also staging one that cannot be read.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/backendsdk"
)

func main() {
	name := strings.TrimPrefix(filepath.Base(os.Args[0]), "infrena-backend-")
	backendsdk.Main(&store{name: name, refuseWrites: name == "brokenstore"})
}

// store is a backend whose whole storage is a directory: one file per
// environment for state, one more for the lock. Locking is O_EXCL on the lock
// file, so two processes really do exclude each other.
type store struct {
	name         string
	refuseWrites bool
	dir          string
}

func (s *store) Name() string { return s.name }

// Configure takes the directory to store state in. A backend that took no
// configuration would have nowhere to put it, so the key is required and a
// missing one is refused rather than defaulted.
func (s *store) Configure(ctx context.Context, config map[string]any) error {
	dir, ok := config["dir"].(string)
	if !ok || dir == "" {
		return fmt.Errorf("backend %q needs a `dir:` in its `backend:` block saying where to store state", s.name)
	}
	s.dir = dir
	return os.MkdirAll(dir, 0o755)
}

func (s *store) statePath(environment string) string {
	return filepath.Join(s.dir, environment+".json")
}

func (s *store) lockPath(environment string) string {
	return filepath.Join(s.dir, environment+".lock")
}

func (s *store) Get(ctx context.Context, environment string) ([]byte, error) {
	data, err := os.ReadFile(s.statePath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing stored is an empty answer, not an error.
		return nil, nil
	}
	return data, err
}

func (s *store) Put(ctx context.Context, environment string, state []byte) error {
	if s.refuseWrites {
		return fmt.Errorf("backend %q cannot write state for %q: this store is unavailable", s.name, environment)
	}
	if _, err := os.Stat(s.lockPath(environment)); err != nil {
		return fmt.Errorf("refusing to write state for %q: no lock is held: %w", environment, backend.ErrNotLocked)
	}
	return os.WriteFile(s.statePath(environment), state, 0o600)
}

func (s *store) List(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if env, ok := strings.CutSuffix(e.Name(), ".json"); ok && env != "" {
			out = append(out, env)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *store) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error) {
	data, err := os.ReadFile(s.lockPath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return backend.Lock{}, false, nil
	}
	if err != nil {
		return backend.Lock{}, false, err
	}
	var held backend.Lock
	if err := json.Unmarshal(data, &held); err != nil {
		return backend.Lock{}, false, err
	}
	return held, true, nil
}

func (s *store) ForceUnlock(ctx context.Context, environment string) error {
	if err := os.Remove(s.lockPath(environment)); err != nil {
		return fmt.Errorf("environment %q is not locked: %w", environment, err)
	}
	return nil
}

func (s *store) Lock(ctx context.Context, environment string) (backend.Lock, error) {
	held := backend.HolderFrom(ctx)
	held.Environment = environment
	data, err := json.Marshal(held)
	if err != nil {
		return backend.Lock{}, err
	}
	// O_EXCL, so the refusal is the filesystem's and not a check with a gap
	// in it.
	f, err := os.OpenFile(s.lockPath(environment), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		current, _, _ := s.Inspect(ctx, environment)
		return backend.Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d), running %q: %w",
			environment, current.User, current.Host, current.PID, current.Operation, backend.ErrLocked)
	}
	if err != nil {
		return backend.Lock{}, err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return backend.Lock{}, err
	}
	return held, nil
}

func (s *store) Unlock(ctx context.Context, environment string) error {
	if err := os.Remove(s.lockPath(environment)); err != nil {
		return fmt.Errorf("environment %q is not locked by this run: %w", environment, err)
	}
	return nil
}
