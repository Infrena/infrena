// Command backendplugin is the backend this package's tests run as a real
// subprocess.
//
// A REAL BINARY, not an in-process double, because everything this package does
// that could be got wrong lives outside the Go call: the cookie, the handshake,
// the framing, and above all a process that exits while the host is waiting for
// it. An in-process fake would exercise none of that, and the failure mode that
// matters most here — a backend dying mid-run — cannot be staged without one.
//
// It behaves according to the name it was launched under, so one program built
// once serves every case the tests need. infrena-backend-memory keeps state in
// a map; infrena-backend-crash-on-put dies the moment a write arrives.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/backendsdk"
)

func main() {
	name := strings.TrimPrefix(filepath.Base(os.Args[0]), "infrena-backend-")
	m := &memory{
		name:       name,
		crashOnPut: name == "crash-on-put",
		states:     map[string][]byte{},
		locks:      map[string]backend.Lock{},
	}
	switch name {
	case "picky":
		backendsdk.Main(pickyConfig{m})
	case "configonly":
		backendsdk.Main(configOnly{m})
	default:
		backendsdk.Main(m)
	}
}

// memory is the smallest thing that is honestly a backend: a map, a mutex, and
// locking that actually refuses.
type memory struct {
	name       string
	crashOnPut bool

	mu     sync.Mutex
	states map[string][]byte
	locks  map[string]backend.Lock
}

func (m *memory) Name() string { return m.name }

func (m *memory) Get(ctx context.Context, environment string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Nothing stored is an empty answer and not an error, which is what the
	// host turns into a new empty state.
	return m.states[environment], nil
}

func (m *memory) Put(ctx context.Context, environment string, state []byte) error {
	// BEFORE THE LOCK CHECK, deliberately. The crash case is a backend that
	// dies while the host is waiting on the write, so it must not be able to
	// answer with a tidy error first.
	if m.crashOnPut {
		fmt.Fprintln(os.Stderr, "the crash-on-put backend is exiting on purpose")
		os.Exit(1)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.locks[environment]; !held {
		return fmt.Errorf("refusing to write state for %q: no lock is held: %w", environment, backend.ErrNotLocked)
	}
	m.states[environment] = slices.Clone(state)
	return nil
}

func (m *memory) List(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.states))
	for env := range m.states {
		out = append(out, env)
	}
	slices.Sort(out)
	return out, nil
}

func (m *memory) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, ok := m.locks[environment]
	return held, ok, nil
}

func (m *memory) ForceUnlock(ctx context.Context, environment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.locks[environment]; !held {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(m.locks, environment)
	return nil
}

func (m *memory) Lock(ctx context.Context, environment string) (backend.Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.locks[environment]; ok {
		return backend.Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d), running %q: %w",
			environment, held.User, held.Host, held.PID, held.Operation, backend.ErrLocked)
	}
	// HolderFrom is who the HOST said is asking. A lock stamped with this
	// process's own identity would name a child that stops existing when the
	// run ends.
	held := backend.HolderFrom(ctx)
	held.Environment = environment
	m.locks[environment] = held
	return held, nil
}

func (m *memory) Unlock(ctx context.Context, environment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.locks[environment]; !held {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(m.locks, environment)
	return nil
}

// pickyConfig makes the memory backend take configuration, so the two optional
// methods can be tested apart from each other.
//
// Three behaviours under three names, because what matters here is not what a
// backend reads but WHICH of the optional methods it implements:
//
//   - memory     — neither Configurable nor Validator
//   - configonly — Configurable, no Validator: `validate` must answer
//     UNSUPPORTED and the host must carry on as though the method did not exist
//   - picky      — both: `validate` refuses a `secret` key offline, which is the
//     shape of the case the method was added for
type pickyConfig struct{ *memory }

func (p pickyConfig) Configure(ctx context.Context, config map[string]any) error {
	if _, ok := config["explode"]; ok {
		return fmt.Errorf("configure refused it")
	}
	return nil
}

// ValidateConfig is the offline half, and refuses what can be refused from the
// bytes alone.
func (p pickyConfig) ValidateConfig(config map[string]any) error {
	if _, ok := config["secret"]; ok {
		return fmt.Errorf("`backend.secret` is a credential and this backend will not read one")
	}
	return nil
}

type configOnly struct{ *memory }

func (c configOnly) Configure(ctx context.Context, config map[string]any) error { return nil }
