package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
)

// errInterrupted signals that a run stopped because of a SIGINT rather than
// because it failed or completed on its own. Execute's existing generic
// error path (root.go) already prints any non-nil error and returns
// ExitError for anything that is not errChanges, so errInterrupted needs no
// special case there — only errors.Is-based tests, and a human reading the
// message, need to tell an interruption apart from an ordinary failure.
var errInterrupted = errors.New("interrupted by SIGINT: the in-flight operation finished, state was saved, and the lock was released")

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

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
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
				return fmt.Errorf("%w (%v)", errInterrupted, err)
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
