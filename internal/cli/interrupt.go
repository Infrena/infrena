package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"infra/internal/state"
)

// errInterrupted signals that a run stopped because of a SIGINT rather than
// because it failed or completed on its own. Execute's existing generic
// error path (root.go) already prints any non-nil error and returns
// ExitError for anything that is not errChanges, so errInterrupted needs no
// special case there — only errors.Is-based tests, and a human reading the
// message, need to tell an interruption apart from an ordinary failure.
//
// Its text says only what runInterruptible itself knows: that a SIGINT
// arrived and fn was allowed to finish before returning. It deliberately
// does NOT say "state was saved" or "the lock was released" — an earlier
// version did, and that is a claim runInterruptible has no way to verify:
// fn is the thing that persists state and holds the lock, and fn's own
// error (wrapped in below, when non-nil) is the one place that can say
// whether persistence actually succeeded. A wrapper asserting an outcome it
// cannot see is exactly how an interrupted run whose persist failed would
// print "state was saved" — the opposite of Task 9's own diagnostic saying
// so, and a direct violation of spec §44 ("state what is wrong"). Reusing
// fn's own error as the single source of truth about what happened is the
// fix, not adding a second one here.
var errInterrupted = errors.New("interrupted by SIGINT: the in-flight operation was allowed to finish before returning")

// runInterruptible runs fn under a context canceled by the first SIGINT,
// and does not return until fn itself returns. A second SIGINT — received
// while fn is still winding down from the first — exits the process
// immediately instead of waiting any longer.
//
// It lives here, not in internal/executor, because os/signal registration
// is process-global and belongs to the one binary entrypoint, not to a
// package other tools may import as a library (see this task's own
// write-up for the fuller argument). apply, destroy and refresh (spec
// §9.2: all three take the lock for their whole run) each call this rather
// than reimplementing it.
func runInterruptible(environment string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Buffered 2, not 1: os/signal's delivery to sig is a non-blocking send,
	// so a second SIGINT arriving before the first is dequeued would
	// otherwise be silently dropped. A human at a terminal cannot press
	// ctrl-C twice inside that window, but Task 16's integration test drives
	// both signals programmatically against a real subprocess and needs the
	// second one to land reliably rather than racing the first select's
	// dequeue.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	// Correct leak hygiene, deliberately kept even though no in-process test
	// can pin it. Without this defer, every runInterruptible call leaves its
	// sig channel registered with os/signal for the rest of the process's
	// life — an unbounded registration leak, one per call, never freed.
	//
	// It cannot be red-tested here: Go delivers a signal to every registered
	// channel independently, so a leaked registration from one cleanly-
	// completed call cannot steal or delay delivery to a later call's own
	// channel (confirmed by mutation — removing this defer left every test
	// in this file green, including one written specifically to try to
	// catch it). The only way a leak becomes observable is a leaked
	// goroutine still actively blocked in a select on its sig channel, which
	// requires a second, independent bug (that is what an earlier, broken
	// runInterruptible without a working cancel() produced, and is a
	// different failure entirely from this defer being present or absent).
	// So: if this defer is ever removed, the symptom is a silent,
	// ever-growing registration leak — not a failing test.
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
				// can say whether persistence actually succeeded (see
				// errInterrupted's doc comment), and wrapping it too lets an
				// errors.Is check reach it — e.g. a test asserting an
				// interrupted-with-failed-persist error still identifies as
				// that specific persist failure, not just as "interrupted".
				return fmt.Errorf("%w (%w)", errInterrupted, err)
			}
			return errInterrupted

		case <-sig:
			// Second SIGINT, received before fn finished winding down from
			// the first. The user has now asked twice not to wait, so this
			// stops waiting: fn's goroutine, and the lock it still holds
			// (spec §9.2), are abandoned here deliberately — Local.Lock
			// already reports exactly this the next time anyone touches
			// this environment; this message is for the person still at
			// this terminal, who has no "next command" yet to tell them.
			fmt.Fprintf(os.Stderr,
				"infra: second interrupt received, exiting without waiting; "+
					"the lock on environment %q was not released and must be cleared with `infra state unlock %s`\n",
				environment, environment)
			os.Exit(ExitError)
			return nil // unreachable; os.Exit does not return
		}
	}
}

// withLockedEnvironment runs fn holding environment's lock, under a context
// SIGINT cancels, with the lock released on every exit path.
//
// It exists because apply, destroy and refresh — spec §9.2's three
// whole-run lock holders — had five identical lines each:
//
//	runInterruptible(env, func(ctx context.Context) error {
//	    ctx = state.WithOperation(ctx, "<op>")
//	    if _, err := backend.Lock(ctx, env); err != nil { return err }
//	    defer releaseLock(backend, env, errOut)
//	    ...
//	})
//
// Duplicated five lines are not the problem; what they guard is. Removing
// runInterruptible from destroy's copy failed NOTHING in the whole suite —
// measured — while apply's and refresh's copies each happened to be pinned.
// So the command where stranding a lock is worst was the one where dropping
// the wrapper was invisible, and the reason is simply that it was three
// separate opportunities to forget rather than one.
//
// Folding them into one unexported helper makes the omission
// unrepresentable rather than merely detectable: a command cannot take the
// lock without also getting SIGINT handling and the release, because there
// is no longer a place to put the Lock call that skips them. That is the
// same reasoning that settled operationContext (internal/state) — the
// guarantee is structural, so no future command has to remember it and no
// test has to catch them not remembering.
//
// operation is the label recorded in the lock file (spec §9.2: the next
// run's error names who holds it and what they are doing), so it is
// required rather than derived — a command whose lock says "apply" while it
// destroys is worse than one that says nothing.
func withLockedEnvironment(environment, operation string, backend *state.Local, errOut io.Writer, fn func(ctx context.Context) error) error {
	return runInterruptible(environment, func(ctx context.Context) error {
		ctx = state.WithOperation(ctx, operation)
		if _, err := backend.Lock(ctx, environment); err != nil {
			// Lock's own error already names the holder (spec §9.2) —
			// nothing to add.
			return err
		}
		defer releaseLock(backend, environment, errOut)
		return fn(ctx)
	})
}
