package state

import (
	"context"
	"errors"
	"time"
)

// ErrLocked is wrapped by every lock conflict so callers can test for it.
var ErrLocked = errors.New("environment is locked")

// ErrNotLocked is wrapped by Put when the caller has not acquired the
// environment's lock — or no longer holds it — at the moment of the write.
// The storage layer enforces this itself, at the write, rather than relying
// on callers to have locked earlier: invariant 5 is a property of what the
// backend allows, not of caller discipline (spec §9.2, §15).
var ErrNotLocked = errors.New("state write refused: environment is not locked by this process")

// Lock describes who holds an environment lock. It exists so a conflict can
// report the holder rather than merely refusing. Spec §9.2.
type Lock struct {
	Environment string    `json:"environment"`
	PID         int       `json:"pid"`
	Host        string    `json:"host"`
	User        string    `json:"user"`
	Operation   string    `json:"operation"`
	At          time.Time `json:"at"`
}

// Backend is the storage abstraction from PLAN.md §21. Get and Put move whole
// states, which is what lets the same shape serve a local file today and an S3
// object in Phase 4.
type Backend interface {
	// Get loads the state for an environment. A missing environment is an
	// empty state, not an error.
	Get(ctx context.Context, environment string) (*State, error)
	// Put atomically writes the state for an environment.
	Put(ctx context.Context, environment string, s *State) error
	// Lock acquires an exclusive lock on an environment, failing if already held.
	Lock(ctx context.Context, environment string) (Lock, error)
	// Unlock releases a lock held on an environment.
	Unlock(ctx context.Context, environment string) error
}
