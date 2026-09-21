package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

func (l *Local) lockPath(environment string) string {
	return filepath.Join(l.stateDir(), environment+".lock")
}

// Lock acquires an exclusive environment lock by writing the lock content to
// a temp file and then linking it into place with os.Link, which — like
// O_CREATE|O_EXCL — fails with EEXIST if the target already exists.
//
// Locks never expire: a timeout that guesses wrong is exactly how two applies
// end up running at once.
func (l *Local) Lock(ctx context.Context, environment string) (Lock, error) {
	path := l.lockPath(environment)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Lock{}, err
	}

	lock := Lock{
		Environment: environment,
		PID:         os.Getpid(),
		Host:        hostname(),
		User:        username(),
		Operation:   operationFromContext(ctx),
		At:          time.Now().UTC(),
	}

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return Lock{}, err
	}

	// Acquisition must be atomic and the content must never be observable
	// half-written: a concurrent Inspect — including the one below, on the
	// fs.ErrExist path — can land between creation and the write.
	// O_CREATE|O_EXCL alone makes only creation atomic, leaving a window
	// where the file exists but is empty.
	//
	// So write-then-link, not write-then-rename: os.Rename overwrites its
	// target, which would let a second Lock silently replace the first
	// holder's lock file. os.Link fails with EEXIST instead, giving the
	// same create-or-fail semantics while guaranteeing the file is fully
	// written the instant it becomes visible under its final name. Put's
	// use of Rename is correct for its own job, where overwriting is the
	// point.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lock-*.tmp")
	if err != nil {
		return Lock{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the link has succeeded and this temp is orphaned

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return Lock{}, err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return Lock{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Lock{}, err
	}
	if err := tmp.Close(); err != nil {
		return Lock{}, err
	}

	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			held, _, inspectErr := l.Inspect(ctx, environment)
			if inspectErr != nil {
				return Lock{}, fmt.Errorf("environment %q is locked, and the lock file could not be read: %w", environment, ErrLocked)
			}
			return Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d) since %s, running %q; release it with `infrena state unlock %s`: %w",
				environment, held.User, held.Host, held.PID, held.At.Format(time.RFC3339), held.Operation, environment, ErrLocked)
		}
		return Lock{}, err
	}
	return lock, nil
}

// Unlock releases a lock this process holds. It refuses to release a lock held
// by anyone else — otherwise "force" would mean nothing and a stray Unlock
// could free an environment another apply is actively mutating.
func (l *Local) Unlock(ctx context.Context, environment string) error {
	held, ok, err := l.Inspect(ctx, environment)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	if held.PID != os.Getpid() || held.Host != hostname() {
		return fmt.Errorf("environment %q is locked by %s on %s (pid %d), not by this process; use `infrena state unlock %s` to override",
			environment, held.User, held.Host, held.PID, environment)
	}
	return l.removeLock(environment)
}

// ForceUnlock removes a lock regardless of holder. `infrena state unlock` uses it
// after telling the user who holds the lock.
//
// ctx is unused: removing a local lock is one os.Remove and cannot block, so
// there is nothing to cancel. It is in the signature because Backend demands
// it, and Backend demands it because a backend reached over a wire can block
// here for as long as the network takes.
func (l *Local) ForceUnlock(ctx context.Context, environment string) error {
	return l.removeLock(environment)
}

// requireOwnLock refuses unless environment is currently locked by this
// process. Put calls it before writing anything, re-deriving ownership from
// the lock file rather than trusting a token handed back by Lock.
//
// Ownership is PID plus host, the same check Unlock makes, and deliberately no
// stronger: an OS-reused PID on the same host could pass it without ever
// having called Lock. These locks guard against ordinary concurrent applies,
// not an adversary. To close that, put a random token in the lock file
// alongside PID and host and compare the token here.
func (l *Local) requireOwnLock(ctx context.Context, environment string) error {
	held, ok, err := l.Inspect(ctx, environment)
	if err != nil {
		return fmt.Errorf("checking lock for %q before writing state: %w", environment, err)
	}
	if !ok {
		return fmt.Errorf("refusing to write state for %q: no lock is held; call Lock first: %w", environment, ErrNotLocked)
	}
	if held.PID != os.Getpid() || held.Host != hostname() {
		return fmt.Errorf("refusing to write state for %q: locked by %s on %s (pid %d), not by this process; if that lock is stale, release it with `infrena state unlock %s`: %w",
			environment, held.User, held.Host, held.PID, environment, ErrNotLocked)
	}
	return nil
}

// removeLock deletes the lock file, reporting a missing lock as an error rather
// than a silent success — a typo in an environment name must not look like it
// worked.
func (l *Local) removeLock(environment string) error {
	err := os.Remove(l.lockPath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	return err
}

// Inspect reports the current lock holder, if any.
//
// ctx is unused: reading a local lock file is a synchronous os.ReadFile with
// nothing to cancel. It is in the signature because Backend demands it, and
// Backend demands it because a backend reached over a wire can block here.
func (l *Local) Inspect(ctx context.Context, environment string) (Lock, bool, error) {
	data, err := os.ReadFile(l.lockPath(environment))
	if errors.Is(err, fs.ErrNotExist) {
		return Lock{}, false, nil
	}
	if err != nil {
		return Lock{}, false, err
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil {
		return Lock{}, true, fmt.Errorf("lock file for %q is malformed: %w", environment, err)
	}
	return lock, true, nil
}

type contextKey string

const operationKey contextKey = "infrena.operation"

// WithOperation labels a context with the operation being performed, so a lock
// conflict can say what the holder is doing.
func WithOperation(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, operationKey, op)
}

// OperationFrom reports the operation a context was labelled with, or
// "unknown".
//
// Exported for internal/backendhost, which has to put the operation into the
// lock it sends a backend plugin: the plugin cannot read this process's
// context, and a lock recording no operation cannot say what its holder is
// doing.
func OperationFrom(ctx context.Context) string { return operationFromContext(ctx) }

func operationFromContext(ctx context.Context) string {
	if op, ok := ctx.Value(operationKey).(string); ok && op != "" {
		return op
	}
	return "unknown"
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
