package state

import (
	"context"

	"github.com/infrena/infrena/pkg/backend"
)

// Lock is an alias, not a copy. Two structs would be two things to keep in
// step, and the first drift would be a lock a backend can build and the
// engine cannot read.
type Lock = backend.Lock

// ErrLocked and ErrNotLocked are re-exported rather than redeclared, for the
// same reason Lock is an alias: errors.Is must give the same answer whether
// the error came from the built-in local backend or from a plugin that only
// ever saw the public package.
var (
	ErrLocked    = backend.ErrLocked
	ErrNotLocked = backend.ErrNotLocked
)

// Backend stores whole states, one per environment. Get and Put move entire
// documents, which is what lets the same shape serve a local file and a
// remote object store.
type Backend interface {
	// Get loads the state for an environment. A missing environment is an
	// empty state, not an error.
	Get(ctx context.Context, environment string) (*State, error)
	// Put atomically writes the state for an environment. Implementations
	// must refuse the write unless the caller currently holds that
	// environment's lock, so that two applies cannot mutate one environment
	// concurrently. This is a contract on every Backend, not a detail of
	// Local, and any other implementation must enforce it by whatever
	// locking mechanism it uses.
	Put(ctx context.Context, environment string, s *State) error
	// List names every environment this backend holds state for, in a
	// stable order. An environment never stored is simply absent, and a
	// backend with no state at all returns an empty slice and no error.
	//
	// It takes a context because for a remote backend this is a network
	// call, and a signature that cannot be cancelled is a bad one to
	// discover after callers are built around it.
	List(ctx context.Context) ([]string, error)
	// Inspect reports the current lock holder for an environment. The bool
	// distinguishes "no lock is held" (false, nil error) from "the lock
	// could not be read" (false, error): a caller that conflates them would
	// report an unreadable lock as a free environment and let a second apply
	// start.
	Inspect(ctx context.Context, environment string) (Lock, bool, error)
	// ForceUnlock removes a lock regardless of who holds it, for
	// `infrena state unlock` after it has told the user whose lock it is
	// about to drop. Implementations must report a lock that was not there
	// as an error rather than a silent success — a typo in an environment
	// name must not look like it worked.
	ForceUnlock(ctx context.Context, environment string) error
	// Lock acquires an exclusive lock on an environment, failing if already held.
	Lock(ctx context.Context, environment string) (Lock, error)
	// Unlock releases a lock held on an environment.
	Unlock(ctx context.Context, environment string) error
}
