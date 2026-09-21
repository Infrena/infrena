// Package backend is the public contract a state backend implements.
//
// It holds the types a third-party backend author must be able to import, and
// nothing else. There is deliberately no state type here: a backend stores
// bytes and never parses them, and one that parsed state would be a second
// reader, free to disagree with the first about what a state file means.
//
// Lock's fields are public contract rather than an implementation detail,
// because they are what the stale-lock diagnostic prints: a user who sees
// "held by alice on host-3, pid 4211" is reading these.
package backend

import (
	"context"
	"errors"
	"os"
	"os/user"
	"time"
)

// ErrLocked is wrapped by every lock conflict so callers can test for it.
var ErrLocked = errors.New("environment is locked")

// ErrNotLocked is wrapped by Put when the caller has not acquired the
// environment's lock — or no longer holds it — at the moment of the write.
//
// The storage layer enforces this itself, at the write, rather than relying on
// callers to have locked earlier: exclusion has to be a property of what the
// backend allows, not of caller discipline.
var ErrNotLocked = errors.New("state write refused: environment is not locked by this process")

// Lock describes who holds an environment lock. It exists so a conflict can
// report the holder rather than merely refusing.
//
// PID, Host and User name the run that took the lock, so a user reading a
// conflict can check whether that run is still alive before forcing the lock
// off. Operation is what that run is doing ("apply", "destroy") and At is when
// it took the lock, in UTC.
type Lock struct {
	Environment string    `json:"environment"`
	PID         int       `json:"pid"`
	Host        string    `json:"host"`
	User        string    `json:"user"`
	Operation   string    `json:"operation"`
	At          time.Time `json:"at"`
}

// Backend is what a state backend plugin implements: the seven methods
// infrena's own storage interface has, and not one more.
//
// It is the same seven so that a plugin can stand in for the built-in local
// backend without anything upstream noticing. The one difference is that state
// arrives and leaves as raw bytes rather than as a parsed state: a backend
// stores bytes and does not interpret them, and handing it a parsed type would
// invite it to become a second reader of state. Keeping the engine out of this
// package also means a third-party author imports one small thing.
//
// Locking is part of the interface, not an option. A backend that cannot lock
// must not compile, because two applies mutating one environment concurrently
// is the failure this whole contract exists to prevent, and a backend
// discovered to be unlockable at apply time is discovered a whole apply too
// late.
type Backend interface {
	// Get loads the stored state for an environment. Nothing stored is an
	// empty result and NOT an error: a project that has never applied
	// anything is the ordinary case for the commands that ask.
	Get(ctx context.Context, environment string) ([]byte, error)
	// Put writes the whole state for an environment, atomically. It must
	// refuse the write unless the caller currently holds that
	// environment's lock, wrapping ErrNotLocked when it does: the refusal
	// belongs to the storage layer rather than to caller discipline.
	//
	// It is called once per operation during an apply and carries the whole
	// state each time, which is why state crosses the wire as bytes.
	Put(ctx context.Context, environment string, state []byte) error
	// List names every environment this backend holds state for, in a
	// stable order. No state at all is an empty slice and no error.
	List(ctx context.Context) ([]string, error)
	// Inspect reports the current lock holder. The bool distinguishes "no
	// lock is held" (false, nil error) from "the lock could not be read"
	// (false, error): a caller that conflated them would report an
	// unreadable lock as a free environment and let a second apply start.
	Inspect(ctx context.Context, environment string) (Lock, bool, error)
	// ForceUnlock removes a lock regardless of who holds it, for
	// `infrena state unlock` after it has named the holder. A lock that was
	// not there is an error rather than a silent success — a typo in an
	// environment name must not look like it worked.
	ForceUnlock(ctx context.Context, environment string) error
	// Lock acquires an exclusive lock, failing if one is already held. A
	// conflict must wrap ErrLocked, and its message should name the holder
	// the way Inspect reports one, because that message is what the user
	// reads. HolderFrom(ctx) gives the run that is asking.
	Lock(ctx context.Context, environment string) (Lock, error)
	// Unlock releases a lock this run holds. It must refuse to release one
	// held by anybody else, or ForceUnlock means nothing and a stray
	// Unlock frees an environment another apply is actively mutating.
	Unlock(ctx context.Context, environment string) error
}

// Configurable is the optional eighth method: a backend that takes settings
// from the project's `backend:` block implements it, and one that takes none
// does not.
//
// Optional rather than part of Backend so that the interface stays the seven
// methods the engine calls, and so the smallest possible backend really is
// seven methods and one call.
type Configurable interface {
	// Configure hands the backend the project's `backend:` block, minus
	// `plugin:`, exactly as the user wrote it.
	//
	// config holds plain Go values decoded from YAML — string, int, bool,
	// []any, map[string]any. There are no unknowns to carry: `backend:`
	// cannot interpolate, because state is read before anything is compiled
	// and compiling is what resolves variables.
	//
	// The SDK refuses configuration a backend cannot read rather than
	// dropping it, because a key that silently does nothing is a user who
	// believes their state is in one place while it is written to another.
	Configure(ctx context.Context, config map[string]any) error
}

// Validator is the optional ninth method: a backend that can tell offline
// whether a `backend:` block is readable implements it, and `infrena validate`
// calls it.
//
// It exists because Configure cannot answer that question cheaply. Configuring
// means building a client, and a backend may legitimately prove something about
// the store while doing so — the s3 backend checks that conditional writes
// work, because a store that cannot do one cannot lock. Those are round trips,
// and `infrena validate` promises not to make any. Without a separate method a
// bad credential committed to infrena.yml passes validate and is refused only at
// plan, which is the wrong way round: validate is the cheap CI gate, and CI is
// where committed credentials arrive.
//
// Optional, and a backend that does not implement it loses nothing: validate
// then checks that the plugin exists and stops there, as it does for a backend
// built against protocol 1, which is never asked.
type Validator interface {
	// ValidateConfig reports whether config is a `backend:` block this
	// backend could read.
	//
	// The implementation must not contact anything: no DNS, no HTTP, no
	// credential file, no clock-dependent answer. Check the block's shape
	// and refuse what can be refused from the bytes alone; leave everything
	// else to Configure. A backend that dials here makes every project's
	// validate slow and unreliable, not only its own.
	ValidateConfig(config map[string]any) error
}

type holderKey struct{}

// WithHolder labels a context with the run asking for a lock. The SDK calls it
// before Lock, from what the host sent.
//
// The holder travels rather than being invented inside the backend because a
// backend plugin is a child of the infrena process: a lock stamped with the
// plugin's own PID would name a process that stops existing when the run ends,
// and `infrena state unlock` prints that PID for a user to go and check. The
// operation ("apply", "destroy") is not knowable inside the plugin at all.
func WithHolder(ctx context.Context, holder Lock) context.Context {
	return context.WithValue(ctx, holderKey{}, holder)
}

// HolderFrom returns the run to record as a lock's holder, filling anything
// the host did not send from the serving process.
//
// A backend's Lock implementation calls this and stores what it gets. The
// fallbacks exist so that a backend driven over a pipe by hand still produces
// a lock that names somebody, rather than one whose diagnostic reads "held by
// on (pid 0)". Environment is the caller's to set, since it is the argument
// Lock was given.
func HolderFrom(ctx context.Context) Lock {
	holder, _ := ctx.Value(holderKey{}).(Lock)
	if holder.PID == 0 {
		holder.PID = os.Getpid()
	}
	if holder.Host == "" {
		holder.Host = hostname()
	}
	if holder.User == "" {
		holder.User = username()
	}
	if holder.Operation == "" {
		holder.Operation = "unknown"
	}
	if holder.At.IsZero() {
		holder.At = time.Now().UTC()
	}
	return holder
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func username() string {
	u, err := user.Current()
	if err != nil {
		return "unknown"
	}
	return u.Username
}
