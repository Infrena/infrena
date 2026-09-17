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
	checkMissingIsEmpty      = "an environment that was never written is empty rather than an error"
	checkListExcludesLocks   = "list names environments with state and not lock objects"
	checkRoundTrip           = "state survives a round trip byte for byte"
	checkConflictNamesHolder = "a lock conflict names its holder"
)

// Conformance runs every check against a backend, failing t for each rule the
// backend breaks.
//
// newBackend is called once per check and must return a backend with no state
// and no locks, so that a rule broken by one check cannot be blamed on
// another. A backend author's whole test is:
//
//	func TestConformance(t *testing.T) {
//		backendtest.Conformance(t, func(t *testing.T) backend.Backend {
//			return newBackendPointedAtSomethingDisposable(t)
//		})
//	}
func Conformance[Self TB[Self]](t Self, newBackend func(Self) backend.Backend) {
	t.Helper()
	// Each check is its own function, named for what it proves, so a
	// failure names the rule rather than a line number.
	checks := []struct {
		name string
		run  func(Self, backend.Backend)
	}{
		{checkSecondLock, aSecondLockIsRefused[Self]},
		{checkWriteNeedsLock, aWriteWithoutALockIsRefused[Self]},
		{checkWriteAfterLockLoss, aWriteAfterTheLockIsTakenAwayIsRefused[Self]},
		{checkMissingIsEmpty, aMissingEnvironmentIsEmpty[Self]},
		{checkListExcludesLocks, listExcludesLockObjects[Self]},
		{checkRoundTrip, stateSurvivesARoundTrip[Self]},
		{checkConflictNamesHolder, aConflictNamesItsHolder[Self]},
	}
	for _, check := range checks {
		t.Run(check.name, func(sub Self) {
			sub.Helper()
			check.run(sub, newBackend(sub))
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
// It asks for no more than that, which is why B does not re-lock before the
// write. Put carries no holder, and a backend object IS one run — that is
// what Unlock's "a lock this run holds" means, and why newBackend hands each
// check a store of its own. Once B had acquired through this same object,
// nothing in the interface could tell A's write from B's, so no correct
// backend could pass. Losing the lock is the observable half, and it is the
// half the bug gets wrong.
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
