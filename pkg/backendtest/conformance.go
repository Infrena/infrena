// Package backendtest checks that a state backend obeys the contract, and is
// the thing a backend author runs against their own implementation.
//
// IT TESTS SEMANTICS, NOT TRANSPORT. Whether a plugin speaks the protocol
// correctly is pkg/backendsdk's business, and testing it once per backend
// would test infrena's code rather than the author's. What differs between
// backends, and what silently corrupts state when it is wrong, is whether a
// second lock is actually refused and whether a write without one is.
//
// "Every backend must lock" (spec §5) is enforced at the type level today:
// a backend without the methods does not compile. Nothing checks the methods
// EXCLUDE. This does, which is the same reason internal/pluginhost's adapter
// enforces what the engine will not trust a provider to honour — a
// third-party binary cannot be held to a doc comment.
//
// Every check here is proven, in this package's own tests, against a backend
// that breaks exactly that rule and nothing else: a conformance suite that
// passes everything is worthless, and one that fires on the wrong rule is as
// bad as one that never fires at all.
package backendtest

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/infrena/infrena/pkg/backend"
)

// TB is the part of *testing.T the suite uses.
//
// It is an interface rather than *testing.T so that the suite can be run
// against a double and its own failures observed — testing.T cannot be
// constructed, and testing.TB is sealed by an unexported method, so a suite
// taking either is a suite nothing can prove. Self is the implementing type
// because Run's callback takes the concrete type: *testing.T's Run takes a
// func(*testing.T), so TB[*testing.T] is what *testing.T satisfies, and
// inference fills Self in for a caller that simply passes its own t.
type TB[Self any] interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Helper()
	Run(name string, f func(Self)) bool
}

// The names of the checks, which are also the subtest names a failure is
// reported under. They are constants so that this package's own tests can
// assert WHICH check caught a broken backend, rather than only that something
// did.
const (
	checkSecondLock          = "a second lock on a held environment is refused"
	checkWriteNeedsLock      = "a write without a lock is refused"
	checkWriteAfterLockLoss  = "a write after the lock was taken away is refused"
	checkWriteAfterTakeover  = "a write after another run took the lock is refused"
	checkMissingIsEmpty      = "an environment that was never written is empty rather than an error"
	checkListExcludesLocks   = "list names environments with state and not lock objects"
	checkRoundTrip           = "state survives a round trip byte for byte"
	checkConflictNamesHolder = "a lock conflict names its holder"
)

// Conformance runs every check against a backend, failing t for each rule the
// backend breaks.
//
// newBackend is called once per check and returns an OPEN function: a fresh
// store with no state and no locks, and a way to open as many backend values
// over that one store as the check needs. The store being fresh per check is
// what keeps a rule broken by one check from being blamed on another; the
// store being shared between the values one check opens is what makes two
// infrena runs on two machines expressible at all, and remote state exists
// for no other reason.
//
// Most checks open one backend and are handed it. The one that cannot —
// a second run taking over a lock the first still believes it holds — opens
// two, and that is the only reason the factory has this shape rather than
// simply returning a backend.
//
// A backend author's whole test is:
//
//	func TestConformance(t *testing.T) {
//		backendtest.Conformance(t, func(t *testing.T) func() backend.Backend {
//			bucket := somethingDisposable(t)
//			return func() backend.Backend { return newBackendPointedAt(bucket) }
//		})
//	}
//
// The outer function makes the disposable storage, once per check; the inner
// one opens a backend over it, which for most backends is the constructor the
// author already has.
func Conformance[Self TB[Self]](t Self, newBackend func(Self) func() backend.Backend) {
	t.Helper()
	// Each check is its own function, named for what it proves, so a
	// failure names the rule rather than a line number. A check states
	// which it needs: one instance, or the factory it can open several
	// from. Taking the instance where one will do keeps the checks honest
	// about which rules actually need a second run to show.
	checks := []struct {
		name   string
		run    func(Self, backend.Backend)
		shared func(Self, func() backend.Backend)
	}{
		{name: checkSecondLock, run: aSecondLockIsRefused[Self]},
		{name: checkWriteNeedsLock, run: aWriteWithoutALockIsRefused[Self]},
		{name: checkWriteAfterLockLoss, run: aWriteAfterTheLockIsTakenAwayIsRefused[Self]},
		{name: checkWriteAfterTakeover, shared: aWriteAfterAnotherRunTookTheLockIsRefused[Self]},
		{name: checkMissingIsEmpty, run: aMissingEnvironmentIsEmpty[Self]},
		{name: checkListExcludesLocks, run: listExcludesLockObjects[Self]},
		{name: checkRoundTrip, run: stateSurvivesARoundTrip[Self]},
		{name: checkConflictNamesHolder, run: aConflictNamesItsHolder[Self]},
	}
	for _, check := range checks {
		t.Run(check.name, func(sub Self) {
			sub.Helper()
			open := newBackend(sub)
			if check.shared != nil {
				check.shared(sub, open)
				return
			}
			check.run(sub, open())
		})
	}
}

// holder is the run the suite claims to be, so that a conflict has a name to
// report and the check can look for it. The PID is deliberately not this
// process's: a backend that names the wrong process still names something.
func holder(operation string) backend.Lock {
	return backend.Lock{
		PID:       424242,
		Host:      "conformance-host",
		User:      "conformance-user",
		Operation: operation,
	}
}

// aSecondLockIsRefused is invariant 5 itself. Everything else in the contract
// is a detail beside it: a lock that does not exclude lets two applies mutate
// one environment, and the loser's resources become state nothing records.
func aSecondLockIsRefused[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-exclusion"

	if _, err := b.Lock(backend.WithHolder(ctx, holder("apply")), env); err != nil {
		t.Fatalf("the first Lock of %q failed: %v", env, err)
	}

	_, err := b.Lock(backend.WithHolder(ctx, holder("destroy")), env)
	if err == nil {
		t.Fatalf("a second lock on %q succeeded while the first was held; a lock that does not exclude is not a lock", env)
	}
	if !errors.Is(err, backend.ErrLocked) {
		t.Errorf("a second lock on %q was refused with %v, which does not wrap backend.ErrLocked; the host classifies conflicts by that error, not by the message", env, err)
	}
}

// aWriteWithoutALockIsRefused is the same invariant enforced where it cannot
// be skipped. Refusing in the caller is caller discipline; refusing in the
// backend is a property of what the storage allows (spec §9.2, §15).
func aWriteWithoutALockIsRefused[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-unlocked-write"

	err := b.Put(ctx, env, []byte(`{"version":1}`))
	if err == nil {
		t.Fatalf("Put wrote state for %q while nothing held its lock; a write to an environment that is not locked must be refused", env)
	}
	if !errors.Is(err, backend.ErrNotLocked) {
		t.Errorf("Put refused a write to %q, which is not locked, with %v; the refusal must wrap backend.ErrNotLocked so the host can tell it from a storage failure", env, err)
	}
}

// aWriteAfterTheLockIsTakenAwayIsRefused is that same refusal asked of a
// backend that HAS locked: ownership has to be re-derived from STORAGE at the
// moment of the write, not from a field that remembers Lock having succeeded.
//
// The sequence is the one `infra state unlock` creates. A holds the lock; an
// operator judges A stale and forces the lock off; B takes it and starts
// applying. A is still running, and its next Put lands on an environment B
// now owns — two applies mutating one environment, which is invariant 5
// (spec §47.5). A backend that asks its own memory whether it holds the lock
// answers yes and writes, silently, over B.
//
// state.Local is exactly the shape this asks for: requireOwnLock re-reads the
// lock file before every Put rather than trusting what Lock handed back, and
// a store-shaped backend gets there by comparing the stored lock object with
// the one it wrote.
//
// It asks for no more than that: the lock is gone and nobody has taken it, so
// one backend value is enough to pose the question. What happens once SOMEBODY
// ELSE holds it is the other half of the same rule, and needs a second run to
// ask at all — aWriteAfterAnotherRunTookTheLockIsRefused.
//
// A backend that writes with no lock at all fails aWriteWithoutALockIsRefused,
// and this returns silently rather than reporting it a second time — the same
// reason aConflictNamesItsHolder stands down when a second lock succeeds.
func aWriteAfterTheLockIsTakenAwayIsRefused[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-lock-taken-away"
	state := []byte(`{"version":1}`)

	if err := b.Put(ctx, env, state); err == nil {
		return
	}

	if _, err := b.Lock(backend.WithHolder(ctx, holder("apply")), env); err != nil {
		t.Fatalf("Lock of %q failed: %v", env, err)
	}
	if err := b.ForceUnlock(ctx, env); err != nil {
		t.Fatalf("ForceUnlock of the locked %q failed: %v", env, err)
	}

	err := b.Put(ctx, env, state)
	if err == nil {
		t.Fatalf("Put wrote state for %q after that lock had been force-unlocked; a backend must decide a write from the lock its storage holds at that moment, or a run whose lock was taken away writes over whoever holds it now", env)
	}
	if !errors.Is(err, backend.ErrNotLocked) {
		t.Errorf("Put refused a write to %q, whose lock had been force-unlocked, with %v; the refusal must wrap backend.ErrNotLocked so the host can tell it from a storage failure", env, err)
	}
}

// aWriteAfterAnotherRunTookTheLockIsRefused is the case the whole package
// exists for, and the only one a single backend value cannot pose.
//
// A locks. An operator judges A stale and forces the lock off. B — a DIFFERENT
// infrena process, on a different machine, over the same bucket — takes the
// lock and starts applying. A is still running, and its next Put lands on an
// environment B owns. Two applies mutating one environment is invariant 5
// (spec §47.5), and the run that loses leaves resources nothing records.
//
// aWriteAfterTheLockIsTakenAwayIsRefused asks the weaker half of this: it
// catches a backend that remembers ownership in a field, because after a force
// unlock there is no lock in storage at all. It cannot catch a backend that
// asks storage the WRONG QUESTION — "is this environment locked?" rather than
// "do I hold this environment's lock?" — since after B locks, the answer to
// the wrong question is yes. That backend writes straight over B's apply with
// no error anywhere, and only a second instance over the same storage shows it.
//
// A correct backend answers by comparing what storage holds with what it
// acquired: the lock object it wrote, an id it generated into it, the ETag of
// the object it put. Anything that survives only inside this process is the
// bug.
//
// It stands down twice, so that a backend already caught by a weaker rule
// fails one check rather than two: once if A can write with no lock at all
// (aWriteWithoutALockIsRefused), and once if A can write with its lock merely
// gone (aWriteAfterTheLockIsTakenAwayIsRefused).
func aWriteAfterAnotherRunTookTheLockIsRefused[Self TB[Self]](t Self, open func() backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-lock-taken-over"
	state := []byte(`{"version":1}`)

	// Two runs over one store. They are opened before anything happens so
	// that B is not a thing that only exists after A has finished with the
	// lock: B is a whole other machine, and it was there all along.
	a, b := open(), open()

	if err := a.Put(ctx, env, state); err == nil {
		return
	}

	if _, err := a.Lock(backend.WithHolder(ctx, holder("apply")), env); err != nil {
		t.Fatalf("A's Lock of %q failed: %v", env, err)
	}
	// B does the forcing, because B is who runs `infra state unlock`, and
	// because a B that cannot see the lock A took is a newBackend that
	// handed out two separate stores — in which case nothing below would
	// mean anything, so it is worth failing loudly here.
	if err := b.ForceUnlock(ctx, env); err != nil {
		t.Fatalf("a second backend's ForceUnlock of %q, which the first had locked, failed: %v; the backends newBackend opens must share one store, or no check here can tell one run from two", env, err)
	}

	if err := a.Put(ctx, env, state); err == nil {
		return
	}

	if _, err := b.Lock(backend.WithHolder(ctx, holder("apply")), env); err != nil {
		t.Fatalf("a second backend's Lock of %q, force-unlocked a moment ago, failed: %v", env, err)
	}

	err := a.Put(ctx, env, state)
	if err == nil {
		t.Fatalf("Put wrote state for %q from the run whose lock was force-unlocked, while a different run holds that lock now; a write is decided by WHO holds the stored lock and not by whether one exists, or the stranded run overwrites the apply that replaced it", env)
	}
	if !errors.Is(err, backend.ErrNotLocked) {
		t.Errorf("Put refused a write to %q, whose lock a different run now holds, with %v; the refusal must wrap backend.ErrNotLocked so the host can tell it from a storage failure", env, err)
	}
}

// aMissingEnvironmentIsEmpty keeps the ordinary case ordinary: a project that
// has never applied anything is what `infra plan` asks about first, and a
// backend that calls it a failure turns a first run into an error.
func aMissingEnvironmentIsEmpty[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-untouched"

	state, err := b.Get(ctx, env)
	if err != nil {
		t.Fatalf("Get of %q, an environment that was never written, returned an error: %v; nothing stored is an empty result", env, err)
	}
	if len(state) != 0 {
		t.Errorf("Get of %q, an environment that was never written, returned %q; it must be empty", env, state)
	}
}

// listExcludesLockObjects is what a store-shaped backend gets wrong: locks and
// state often live side by side under one prefix, and a listing that hands
// back the lock objects invents environments the user never made. An
// environment that is locked but has never been written is not an environment
// this backend holds state for either.
func listExcludesLockObjects[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const written, lockedOnly = "conformance-written", "conformance-locked-only"

	if _, err := b.Lock(backend.WithHolder(ctx, holder("apply")), written); err != nil {
		t.Fatalf("Lock of %q failed: %v", written, err)
	}
	if err := b.Put(ctx, written, []byte(`{"version":1}`)); err != nil {
		t.Fatalf("Put to the locked %q failed: %v", written, err)
	}
	if err := b.Unlock(ctx, written); err != nil {
		t.Fatalf("Unlock of %q failed: %v", written, err)
	}
	if _, err := b.Lock(backend.WithHolder(ctx, holder("apply")), lockedOnly); err != nil {
		t.Fatalf("Lock of %q failed: %v", lockedOnly, err)
	}

	got, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if !slices.Equal(got, []string{written}) {
		t.Errorf("List returned %q, want exactly %q; it names the environments that have state, and a lock is not one — %q holds a lock and no state", got, []string{written}, lockedOnly)
	}
}

// stateSurvivesARoundTrip is why state crosses as bytes at all. A backend
// stores bytes and never parses them (spec §6); one that re-encodes what it
// was handed is a second reader of state, and the difference shows up as a
// plan that disagrees with the last apply.
func stateSurvivesARoundTrip[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-round-trip"
	// Not valid UTF-8, with a NUL and a trailing newline, because a
	// backend that treats state as text loses exactly these.
	want := []byte("{\"version\":1,\"raw\":\"\x00\xff\xfe\"}\n")

	if _, err := b.Lock(backend.WithHolder(ctx, holder("apply")), env); err != nil {
		t.Fatalf("Lock of %q failed: %v", env, err)
	}
	if err := b.Put(ctx, env, slices.Clone(want)); err != nil {
		t.Fatalf("Put to the locked %q failed: %v", env, err)
	}
	got, err := b.Get(ctx, env)
	if err != nil {
		t.Fatalf("Get of %q, which was just written, failed: %v", env, err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Get returned %q but Put was given %q; state must come back byte for byte, since a backend stores bytes it does not interpret", got, want)
	}
}

// aConflictNamesItsHolder is the difference between a diagnostic and a wall.
// "environment is locked" tells a user to wait for nobody in particular; the
// holder is what `infra state unlock` prints so they can go and check whether
// that run is still alive.
//
// A backend whose second lock SUCCEEDS is not this check's business — that is
// aSecondLockIsRefused, and reporting it twice would leave neither failure
// able to say which rule was broken.
func aConflictNamesItsHolder[Self TB[Self]](t Self, b backend.Backend) {
	t.Helper()
	ctx := context.Background()
	const env = "conformance-conflict"
	first := holder("apply")

	if _, err := b.Lock(backend.WithHolder(ctx, first), env); err != nil {
		t.Fatalf("the first Lock of %q failed: %v", env, err)
	}

	_, err := b.Lock(backend.WithHolder(ctx, holder("destroy")), env)
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), first.User) {
		t.Errorf("the conflict on %q reported %q, which does not name its holder %q; that message is what a user reads before deciding whether to force the unlock", env, err, first.User)
	}
}
