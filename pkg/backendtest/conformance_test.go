package backendtest

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/infrena/infrena/pkg/backend"
)

// A correct backend passes.
func TestConformancePassesACorrectBackend(t *testing.T) {
	Conformance(t, func(*testing.T) backend.Backend { return newMemoryBackend() })
}

// And every check catches the one thing it is for. Each broken backend
// violates exactly one rule, so a check that fires on the wrong one is as
// visible as a check that does not fire at all: the suite is only worth
// running if a failure names the rule that was actually broken.
func TestEveryConformanceCheckCatchesItsOwnViolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken func() backend.Backend
		check  string
		expect string
	}{
		{
			name:   "a lock that does not exclude",
			broken: func() backend.Backend { return &brokenBackend{lockAlwaysSucceeds: true} },
			check:  checkSecondLock,
			expect: "second lock",
		},
		{
			name:   "a write with no lock held",
			broken: func() backend.Backend { return &brokenBackend{putWithoutLock: true} },
			check:  checkWriteNeedsLock,
			expect: "not locked",
		},
		{
			name:   "a missing environment erroring",
			broken: func() backend.Backend { return &brokenBackend{missingIsError: true} },
			check:  checkMissingIsEmpty,
			expect: "never written",
		},
		{
			name:   "listing that includes lock objects",
			broken: func() backend.Backend { return &brokenBackend{listIncludesLocks: true} },
			check:  checkListExcludesLocks,
			expect: "lock",
		},
		{
			name:   "state altered in transit",
			broken: func() backend.Backend { return &brokenBackend{mangleState: true} },
			check:  checkRoundTrip,
			expect: "byte",
		},
		{
			name:   "a conflict that does not name its holder",
			broken: func() backend.Backend { return &brokenBackend{anonymousConflict: true} },
			check:  checkConflictNamesHolder,
			expect: "holder",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &recordingT{}
			Conformance(fake, func(*recordingT) backend.Backend { return tc.broken() })

			if !fake.failed {
				t.Fatalf("conformance passed a backend that %s", tc.name)
			}
			if !strings.Contains(strings.ToLower(fake.log()), tc.expect) {
				t.Errorf("failure does not name the violation %q:\n%s", tc.expect, fake.log())
			}
			// The point of one switch per backend: the check that fired
			// has to be the check for the rule that was broken, and no
			// other. A suite that fails everything proves nothing.
			if got := fake.failures(); !slices.Equal(got, []string{tc.check}) {
				t.Errorf("checks that failed = %q, want exactly %q:\n%s", got, tc.check, fake.log())
			}
		})
	}
}

// recordingT captures failures instead of failing the outer test. The standard
// library offers nothing for this — testing.T cannot be constructed and
// testing.TB is closed by a private method — so Conformance takes the four
// methods it uses and *testing.T satisfies.
type recordingT struct {
	mu     sync.Mutex
	name   string
	parent *recordingT

	failed  bool
	lines   []string
	failing []string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Errorf(format string, args ...any) {
	r.record(fmt.Sprintf(format, args...))
}

// Fatalf ends the check that called it, the way testing.T's does, so a check
// whose setup failed does not carry on and report a second, invented failure.
func (r *recordingT) Fatalf(format string, args ...any) {
	r.record(fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func (r *recordingT) Run(name string, f func(*recordingT)) bool {
	sub := &recordingT{name: name, parent: r}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(sub)
	}()
	<-done
	return !sub.failed
}

func (r *recordingT) record(message string) {
	r.mu.Lock()
	r.failed = true
	r.mu.Unlock()
	if r.parent == nil {
		r.mu.Lock()
		r.lines = append(r.lines, message)
		r.mu.Unlock()
		return
	}
	root := r
	for root.parent != nil {
		root = root.parent
	}
	root.mu.Lock()
	root.lines = append(root.lines, message)
	if !slices.Contains(root.failing, r.name) {
		root.failing = append(root.failing, r.name)
	}
	root.failed = true
	root.mu.Unlock()
}

// log is every failure message, without the check names, so a test asserting
// on the message cannot be satisfied by the name of the check it came from.
func (r *recordingT) log() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// failures names the checks that failed, in the order they first did.
func (r *recordingT) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.failing)
}

// newMemoryBackend is a correct backend: brokenBackend with every switch off.
func newMemoryBackend() *brokenBackend { return &brokenBackend{} }

// brokenBackend is a correct in-memory backend with one switch each. They are
// independent on purpose — a backend that violates two rules cannot prove
// which check fired.
type brokenBackend struct {
	mu     sync.Mutex
	states map[string][]byte
	locks  map[string]backend.Lock

	lockAlwaysSucceeds bool // a lock that never excludes
	putWithoutLock     bool // a write accepted with no lock held
	missingIsError     bool // a never-written environment reported as an error
	listIncludesLocks  bool // lock objects listed as if they were environments
	mangleState        bool // state altered between Put and Get
	anonymousConflict  bool // a conflict that refuses without naming the holder
}

func (b *brokenBackend) Get(ctx context.Context, environment string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	stored, ok := b.states[environment]
	if !ok && b.missingIsError {
		return nil, fmt.Errorf("no state for %q", environment)
	}
	return stored, nil
}

func (b *brokenBackend) Put(ctx context.Context, environment string, state []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, held := b.locks[environment]; !held && !b.putWithoutLock {
		return fmt.Errorf("refusing to write state for %q: no lock is held: %w", environment, backend.ErrNotLocked)
	}
	if b.states == nil {
		b.states = map[string][]byte{}
	}
	stored := slices.Clone(state)
	if b.mangleState && len(stored) > 0 {
		stored[len(stored)-1] ^= 0x01
	}
	b.states[environment] = stored
	return nil
}

func (b *brokenBackend) List(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	envs := make([]string, 0, len(b.states))
	for env := range b.states {
		envs = append(envs, env)
	}
	if b.listIncludesLocks {
		for env := range b.locks {
			envs = append(envs, env+".lock")
		}
	}
	slices.Sort(envs)
	return envs, nil
}

func (b *brokenBackend) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	held, ok := b.locks[environment]
	return held, ok, nil
}

func (b *brokenBackend) ForceUnlock(ctx context.Context, environment string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.locks[environment]; !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(b.locks, environment)
	return nil
}

func (b *brokenBackend) Lock(ctx context.Context, environment string) (backend.Lock, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if held, ok := b.locks[environment]; ok && !b.lockAlwaysSucceeds {
		if b.anonymousConflict {
			return backend.Lock{}, fmt.Errorf("environment %q is locked: %w", environment, backend.ErrLocked)
		}
		return backend.Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d), running %q: %w",
			environment, held.User, held.Host, held.PID, held.Operation, backend.ErrLocked)
	}
	lock := backend.HolderFrom(ctx)
	lock.Environment = environment
	if b.locks == nil {
		b.locks = map[string]backend.Lock{}
	}
	b.locks[environment] = lock
	return lock, nil
}

func (b *brokenBackend) Unlock(ctx context.Context, environment string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.locks[environment]; !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(b.locks, environment)
	return nil
}
