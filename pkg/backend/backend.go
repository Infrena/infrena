// Package backend is the public contract a state backend implements.
//
// It holds the types a third-party backend author must be able to import, and
// nothing else. `state.State` is deliberately ABSENT: a backend stores bytes
// and never parses them (spec §6), so a backend needs no state type at all.
// One that parsed state would be a second reader, free to disagree with the
// first about what a state file means.
//
// Lock's fields are public contract rather than an implementation detail,
// because they are what the stale-lock diagnostic already prints: a user who
// sees "held by alice on host-3, pid 4211" is reading these.
package backend

import (
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
