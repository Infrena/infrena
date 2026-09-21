package backendtest

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/infrena/infrena/pkg/backend"
)

// A correct backend passes.
func TestConformancePassesACorrectBackend(t *testing.T) {
	Conformance(t, func(*testing.T) func() backend.Backend {
		store := newMemoryStore()
		return func() backend.Backend { return newMemoryBackend(store) }
	})
}

// And every check catches the one thing it is for. Each broken backend
// violates exactly one rule, so a check that fires on the wrong one is as
// visible as a check that does not fire at all: the suite is only worth
// running if a failure names the rule that was actually broken.
func TestEveryConformanceCheckCatchesItsOwnViolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken func(*memoryStore) backend.Backend
		check  string
		expect string
	}{
		{
			name:   "a lock that does not exclude",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{lockAlwaysSucceeds: true}) },
			check:  checkSecondLock,
			expect: "second lock",
		},
		{
			name:   "a write with no lock held",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{putWithoutLock: true}) },
			check:  checkWriteNeedsLock,
			expect: "not locked",
		},
		{
			name:   "ownership of the lock remembered in a field",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{cachedLockOwnership: true}) },
			check:  checkWriteAfterLockLoss,
			expect: "force-unlocked",
		},
		{
			name: "a write allowed whenever anybody holds the lock",
			broken: func(s *memoryStore) backend.Backend {
				return newBroken(s, brokenBackend{writeIfAnyoneHoldsTheLock: true})
			},
			check:  checkWriteAfterTakeover,
			expect: "a different run holds that lock now",
		},
		{
			name:   "a missing environment erroring",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{missingIsError: true}) },
			check:  checkMissingIsEmpty,
			expect: "never written",
		},
		{
			name:   "listing that includes lock objects",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{listIncludesLocks: true}) },
			check:  checkListExcludesLocks,
			expect: "lock",
		},
		{
			name:   "state altered in transit",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{mangleState: true}) },
			check:  checkRoundTrip,
			expect: "byte",
		},
		{
			name:   "a conflict that does not name its holder",
			broken: func(s *memoryStore) backend.Backend { return newBroken(s, brokenBackend{anonymousConflict: true}) },
			check:  checkConflictNamesHolder,
			expect: "holder",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &recordingT{}
			// One store per check, and every backend the check opens
			// over it has the same flaw: a broken backend is a broken
			// backend on every machine it runs on.
			Conformance(fake, func(*recordingT) func() backend.Backend {
				store := newMemoryStore()
				return func() backend.Backend { return tc.broken(store) }
			})

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

// memoryStore is the storage the broken backends sit in front of, lifted out
// of the backend object for the same reason the reference one is: two backends
// over one store are two runs on two machines, which is the only way to pose
// the rule about a lock somebody else has taken.
type memoryStore struct {
	mu     sync.Mutex
	states map[string][]byte
	locks  map[string]storedLock
}

// storedLock is a lock as the store holds it: the holder a conflict reports,
// and a token naming the acquisition, so a correct backend can tell a lock it
// took from one that merely exists.
type storedLock struct {
	holder backend.Lock
	token  string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{states: map[string][]byte{}, locks: map[string]storedLock{}}
}

// newMemoryBackend is a correct backend: brokenBackend with every switch off.
func newMemoryBackend(store *memoryStore) *brokenBackend { return newBroken(store, brokenBackend{}) }

// newBroken opens a backend over store with the given switches flipped. It
// takes the switches as a value rather than a list of options so that each
// case in the table above reads as the one flaw it is.
func newBroken(store *memoryStore, flaws brokenBackend) *brokenBackend {
	flaws.store = store
	flaws.acquired = map[string]string{}
	flaws.owned = map[string]bool{}
	return &flaws
}

// brokenBackend is a correct in-memory backend with one switch each. They are
// independent on purpose — a backend that violates two rules cannot prove
// which check fired.
type brokenBackend struct {
	store *memoryStore

	lockAlwaysSucceeds        bool // a lock that never excludes
	putWithoutLock            bool // a write accepted with no lock held
	cachedLockOwnership       bool // ownership remembered from Lock instead of read back at the write
	writeIfAnyoneHoldsTheLock bool // a write allowed because A lock exists, not because this run holds it
	missingIsError            bool // a never-written environment reported as an error
	listIncludesLocks         bool // lock objects listed as if they were environments
	mangleState               bool // state altered between Put and Get
	anonymousConflict         bool // a conflict that refuses without naming the holder

	// acquired is what a correct backend remembers: the token of each lock
	// this run took, checked against the store at the write.
	acquired map[string]string

	// owned is cachedLockOwnership's memory: the field a real backend of
	// that shape sets when Lock returns. It stands in for a whole process,
	// which is why ForceUnlock leaves it alone — the force-unlock that
	// strands a holder is run by somebody else, and nothing reaches into
	// the stranded run to correct what it believes.
	owned map[string]bool
}

func (b *brokenBackend) Get(ctx context.Context, environment string) ([]byte, error) {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	stored, ok := b.store.states[environment]
	if !ok && b.missingIsError {
		return nil, fmt.Errorf("no state for %q", environment)
	}
	return stored, nil
}

func (b *brokenBackend) Put(ctx context.Context, environment string, state []byte) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	if !b.holdsLock(environment) && !b.putWithoutLock {
		return fmt.Errorf("refusing to write state for %q: no lock is held: %w", environment, backend.ErrNotLocked)
	}
	stored := slices.Clone(state)
	if b.mangleState && len(stored) > 0 {
		stored[len(stored)-1] ^= 0x01
	}
	b.store.states[environment] = stored
	return nil
}

// holdsLock is the question Put has to answer before writing. Correctly it is
// a question about storage AND about identity — does the lock this run took
// still sit in the store — and each switch gets one half of it wrong.
//
// cachedLockOwnership answers from what Lock left behind, so the lock can be
// force-unlocked without this backend's field hearing about it.
// writeIfAnyoneHoldsTheLock does read the store, and asks it the wrong
// question: a lock another run took after this one was force-unlocked is still
// a lock, so the write goes straight over that run's apply.
func (b *brokenBackend) holdsLock(environment string) bool {
	if b.cachedLockOwnership {
		return b.owned[environment]
	}
	stored, held := b.store.locks[environment]
	if b.writeIfAnyoneHoldsTheLock {
		return held
	}
	return held && stored.token == b.acquired[environment]
}

func (b *brokenBackend) List(ctx context.Context) ([]string, error) {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	envs := make([]string, 0, len(b.store.states))
	for env := range b.store.states {
		envs = append(envs, env)
	}
	if b.listIncludesLocks {
		for env := range b.store.locks {
			envs = append(envs, env+".lock")
		}
	}
	slices.Sort(envs)
	return envs, nil
}

func (b *brokenBackend) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error) {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	stored, ok := b.store.locks[environment]
	return stored.holder, ok, nil
}

func (b *brokenBackend) ForceUnlock(ctx context.Context, environment string) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	if _, ok := b.store.locks[environment]; !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	delete(b.store.locks, environment)
	return nil
}

func (b *brokenBackend) Lock(ctx context.Context, environment string) (backend.Lock, error) {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	if stored, ok := b.store.locks[environment]; ok && !b.lockAlwaysSucceeds {
		if b.anonymousConflict {
			return backend.Lock{}, fmt.Errorf("environment %q is locked: %w", environment, backend.ErrLocked)
		}
		held := stored.holder
		return backend.Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d), running %q: %w",
			environment, held.User, held.Host, held.PID, held.Operation, backend.ErrLocked)
	}
	lock := backend.HolderFrom(ctx)
	lock.Environment = environment
	token := strconv.FormatInt(lockCounter.Add(1), 10)
	b.store.locks[environment] = storedLock{holder: lock, token: token}
	b.acquired[environment] = token
	if b.cachedLockOwnership {
		b.owned[environment] = true
	}
	return lock, nil
}

func (b *brokenBackend) Unlock(ctx context.Context, environment string) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	stored, ok := b.store.locks[environment]
	if !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	if stored.token != b.acquired[environment] {
		return fmt.Errorf("refusing to unlock %q: this run does not hold its lock", environment)
	}
	delete(b.store.locks, environment)
	delete(b.acquired, environment)
	// Releasing its own lock is the one way this backend's memory is ever
	// corrected, so cachedLockOwnership breaks the write rule and nothing
	// else: a run that unlocks normally stops writing, and only the lock
	// taken away behind its back leaves the field lying.
	delete(b.owned, environment)
	return nil
}

// lockCounter names acquisitions apart within this test binary. The reference
// backend uses randomness because its runs are separate processes; here one
// process opens every instance, so a counter says the same thing with less
// machinery.
var lockCounter atomic.Int64
