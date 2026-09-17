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

// Backend is the storage abstraction from PLAN.md §21. Get and Put move whole
// states, which is what lets the same shape serve a local file today and an S3
// object in Phase 4.
type Backend interface {
	// Get loads the state for an environment. A missing environment is an
	// empty state, not an error.
	Get(ctx context.Context, environment string) (*State, error)
	// Put atomically writes the state for an environment. Implementations
	// must refuse the write unless the caller currently holds that
	// environment's lock — writing without one breaks acceptance invariant
	// 5 (two applies must not mutate one environment concurrently). This is
	// a contract on every Backend, not a detail of Local: Local enforces it
	// by re-deriving lock ownership from the lock file before writing (see
	// Local.Put and requireOwnLock in local.go and lock.go), and any future
	// implementation — an S3 backend in Phase 4, for instance — must enforce
	// it too, by whatever locking mechanism it uses.
	Put(ctx context.Context, environment string, s *State) error
	// List names every environment this backend holds state for, in a
	// stable order. An environment the backend has never stored state for
	// is simply absent; a backend with no state at all returns an empty
	// slice and no error, because a project that has never applied
	// anything is the ordinary case for the commands that ask.
	//
	// It takes no context: unlike the other six, callers use it to
	// enumerate what exists rather than to move state, and adding one here
	// would change a signature the CLI already depends on for no gain a
	// caller could use.
	List() ([]string, error)
	// Inspect reports the current lock holder for an environment. The bool
	// distinguishes "no lock is held" (false, nil error) from "the lock
	// could not be read" (false, error): a caller that conflates them
	// would report an unreadable lock as a free environment and let a
	// second apply start.
	Inspect(ctx context.Context, environment string) (Lock, bool, error)
	// ForceUnlock removes a lock regardless of who holds it, for
	// `infra state unlock` after it has told the user whose lock it is
	// about to drop. Implementations must report a lock that was not there
	// as an error rather than a silent success — a typo in an environment
	// name must not look like it worked.
	ForceUnlock(ctx context.Context, environment string) error
	// Lock acquires an exclusive lock on an environment, failing if already held.
	Lock(ctx context.Context, environment string) (Lock, error)
	// Unlock releases a lock held on an environment.
	Unlock(ctx context.Context, environment string) error
}
