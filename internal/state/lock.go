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
	return filepath.Join(l.root, "state", environment+".lock")
}

// Lock acquires an exclusive environment lock by creating a file with O_EXCL.
// Locks never expire: a timeout that guesses wrong is exactly how two applies
// end up running at once. Spec §9.2.
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

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		held, _, inspectErr := l.Inspect(environment)
		if inspectErr != nil {
			return Lock{}, fmt.Errorf("environment %q is locked, and the lock file could not be read: %w", environment, ErrLocked)
		}
		return Lock{}, fmt.Errorf("environment %q is locked: held by %s on %s (pid %d) since %s, running %q; release it with `infra state unlock %s`: %w",
			environment, held.User, held.Host, held.PID, held.At.Format(time.RFC3339), held.Operation, environment, ErrLocked)
	}
	if err != nil {
		return Lock{}, err
	}
	defer f.Close()

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return Lock{}, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

// Unlock releases a lock this process holds. It refuses to release a lock held
// by anyone else — otherwise "force" would mean nothing and a stray Unlock
// could free an environment another apply is actively mutating.
func (l *Local) Unlock(ctx context.Context, environment string) error {
	held, ok, err := l.Inspect(environment)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("environment %q is not locked", environment)
	}
	if held.PID != os.Getpid() || held.Host != hostname() {
		return fmt.Errorf("environment %q is locked by %s on %s (pid %d), not by this process; use `infra state unlock %s` to override",
			environment, held.User, held.Host, held.PID, environment)
	}
	return l.removeLock(environment)
}

// ForceUnlock removes a lock regardless of holder. `infra state unlock` uses it
// after telling the user who holds the lock.
func (l *Local) ForceUnlock(environment string) error {
	return l.removeLock(environment)
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
func (l *Local) Inspect(environment string) (Lock, bool, error) {
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

const operationKey contextKey = "infra.operation"

// WithOperation labels a context with the operation being performed, so a lock
// conflict can say what the holder is doing.
func WithOperation(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, operationKey, op)
}

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
