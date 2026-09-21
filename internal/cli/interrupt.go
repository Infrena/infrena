package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/infrena/infrena/internal/state"
)

// errInterrupted signals that a run stopped because of a SIGINT rather than because
// it failed or completed on its own. Execute's generic error path already prints it
// and returns ExitError, so it needs no special case there.
//
// Its text says only what runInterruptible itself knows: that a SIGINT arrived and fn
// was allowed to finish before returning. It deliberately does not say "state was
// saved" or "the lock was released", because fn is what persists state and holds the
// lock, and fn's own error — wrapped in below when non-nil — is the one place that
// can say whether persistence succeeded.
var errInterrupted = errors.New("interrupted by SIGINT: the in-flight operation was allowed to finish before returning")

// runInterruptible runs fn under a context canceled by the first SIGINT,
// and does not return until fn itself returns. A second SIGINT — received
// while fn is still winding down from the first — exits the process
// immediately instead of waiting any longer.
//
// It lives here, not in internal/executor, because os/signal registration is
// process-global and belongs to the binary's entrypoint rather than to a package
// other tools may import as a library. apply, destroy and refresh — the three
// commands that hold the lock for their whole run — each call it rather than
// reimplement it.
func runInterruptible(environment string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Buffered 2, not 1: os/signal's delivery is a non-blocking send, so a second
	// SIGINT arriving before the first is dequeued would otherwise be dropped. A
	// human cannot press ctrl-C twice inside that window, but a test driving both
	// signals programmatically can.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	// Without this, every call leaves its channel registered with os/signal for the
	// rest of the process's life. No test can catch it — Go delivers a signal to
	// each registered channel independently, so a leaked registration from a
	// completed call cannot steal or delay a later one's delivery — and the symptom
	// of removing it is a silent, ever-growing registration leak.
	defer signal.Stop(sig)

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		// fn finished on its own; no interruption happened.
		return err

	case <-sig:
		// First SIGINT: ask fn to wind down, then WAIT for it. Returning
		// early here would let this function's caller — and eventually the
		// process — move on while fn is still mid-write to a state file it
		// holds locked.
		cancel()
		select {
		case err := <-done:
			if err != nil {
				// Both %w, not %w/%v: fn's own error is the one place that
				// can say whether persistence succeeded, and wrapping it too
				// lets an errors.Is check reach it — an
				// interrupted-with-failed-persist error still identifies as
				// that specific failure, not only as "interrupted".
				return fmt.Errorf("%w (%w)", errInterrupted, err)
			}
			return errInterrupted

		case <-sig:
			// Second SIGINT, received before fn finished winding down from
			// the first. The user has now asked twice not to wait, so fn's
			// goroutine and the lock it still holds are abandoned
			// deliberately. The next command to touch this environment
			// reports the stale lock by itself; this message is for the
			// person at this terminal, who has no next command yet.
			fmt.Fprintf(os.Stderr,
				"infrena: second interrupt received, exiting without waiting; "+
					"the lock on environment %q was not released and must be cleared with `infrena state unlock %s`\n",
				environment, environment)
			os.Exit(ExitError)
			return nil // unreachable; os.Exit does not return
		}
	}
}

// withLockedEnvironment runs fn holding environment's lock, under a context
// SIGINT cancels, with the lock released on every exit path.
//
// The three whole-run lock holders each had the same five lines: take the lock under
// runInterruptible, defer the release, do the work. Folding them into one helper
// makes the omission unrepresentable rather than merely detectable — a command cannot
// take the lock without also getting SIGINT handling and the release, because there
// is no longer a place to put the Lock call that skips them. Nothing pinned destroy's
// copy of the wrapper, so the command where stranding a lock is worst was the one
// where dropping it was invisible.
//
// operation is the label recorded in the lock file, so the next run's error names who
// holds it and what they are doing. It is required rather than derived: a command
// whose lock says "apply" while it destroys is worse than one that says nothing.
func withLockedEnvironment(environment, operation string, backend state.Backend, errOut io.Writer, fn func(ctx context.Context) error) error {
	return runInterruptible(environment, func(ctx context.Context) error {
		ctx = state.WithOperation(ctx, operation)
		if _, err := backend.Lock(ctx, environment); err != nil {
			// Lock's own error already names the holder.
			return err
		}
		defer releaseLock(backend, environment, errOut)
		return fn(ctx)
	})
}
