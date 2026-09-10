package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/config"
	"infra/internal/executor"
	"infra/internal/planner"
	"infra/internal/refresh"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
)

// applyPrompt is what a human sees before infra mutates anything. "yes",
// typed in full — not a bare y — is the bar for an ordinary apply. destroy
// (task 14) sets a higher bar, typing the environment name, because destroy
// is unconditionally destructive by definition; most applies are pure
// creates with nothing to destroy at all, and attaching the heavier bar to
// every apply would desensitize users to it well before it mattered. Spec
// §13's stronger "typed confirmation... when the environment is
// type: production" is environment-level and explicitly M6 (no environment
// types exist yet in M3) — this is the ordinary-apply default that holds
// until that lands.
const applyPrompt = "\nDo you want to perform these actions?\n" +
	"  infra will perform the actions described above.\n" +
	"  Only 'yes' will be accepted to approve.\n\n" +
	"  Enter a value: "

// newApplyCommand builds `infra apply <environment>`: compile, refresh,
// plan, show it, take approval, execute. Spec §16, §9.2, §10.
func newApplyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "apply <environment>",
		Short:         "Reconcile real infrastructure with the configured desired state",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			vars, err := parseVars(opts.Vars)
			if err != nil {
				return err
			}

			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}

			reg := buildRegistry(opts.Dir)
			cfg, cds := compiler.Compile(files, reg, compiler.Options{
				Environment: environment,
				Vars:        vars,
			})
			if cds.HasErrors() {
				cds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}

			backend := backendFor(opts.Dir)

			// Unlocked preview — identical in spirit to `infra plan`: safe
			// to run against a locked environment, in CI, or repeatedly.
			p, _, err := computePlan(cmd.Context(), cmd, backend, reg, cfg, environment, opts)
			if err != nil {
				return err
			}

			fmt.Fprint(cmd.OutOrStdout(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose}))

			if !p.HasChanges() {
				return nil
			}

			if !opts.AutoApprove {
				if !confirm(cmd, applyPrompt, "yes") {
					return errors.New("apply cancelled: you must type \"yes\" to approve")
				}
			}

			// Everything from here runs under runInterruptible (Task 11),
			// which owns SIGINT: the first one cancels this context so no
			// NEW work is scheduled, while the operation already in flight
			// finishes against a context Task 8 derives with
			// context.WithoutCancel; the second exits immediately and
			// deliberately leaves the lock, which the next run reports as
			// stale. Signal handling lives in internal/cli and never in the
			// executor, so a library import cannot install a handler behind
			// a caller's back.
			return runInterruptible(environment, func(ctx context.Context) error {
				ctx = state.WithOperation(ctx, "apply")
				if _, err := backend.Lock(ctx, environment); err != nil {
					// Lock's own error already names the holder (spec §9.2) —
					// nothing to add.
					return err
				}
				defer releaseLock(backend, environment, cmd.ErrOrStderr())

				// Re-plan inside the lock — see this task's doc comment above
				// for why: apply must never execute against state or provider
				// reality gathered before the lock was held.
				p2, st, err := computePlan(ctx, cmd, backend, reg, cfg, environment, opts)
				if err != nil {
					return err
				}
				if !p2.HasChanges() {
					fmt.Fprintln(cmd.OutOrStdout(), "\nNo changes remained once the environment lock was acquired; nothing to apply.")
					return nil
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return fmt.Errorf("building execution graph: %w", err)
				}

				res, execDiags := executor.Apply(ctx, p2, g, st, executor.Options{
					Parallelism: opts.Parallelism,
					PerProvider: opts.Parallelism, // no dedicated flag yet; see the note below
					Registry:    reg,
					Backend:     backend,
					Environment: environment,
					Retry:       defaultRetryPolicy(),
					Now:         time.Now,
				})
				execDiags.Render(cmd.ErrOrStderr())

				// executor.Render (Task 12) is THE result renderer. An earlier
				// draft of this task wrote a private renderResult here; two
				// renderers for one Result is the duplicate-implementation
				// defect that put a secret in M2's output, so this calls the
				// shared one.
				fmt.Fprint(cmd.OutOrStdout(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return errors.New("apply completed with failures")
				}
				return errChanges
			})
		},
	}
}

// computePlan runs the same unlocked, side-effect-free sequence `infra
// plan` uses — backend.Get, refresh.Refresh, planner.Compute — and returns
// the resulting plan together with the state it was computed against.
// apply calls it twice (before and after taking the lock); destroy (task
// 14) reuses it unchanged with an empty desired configuration, which is
// what turns "everything currently in state" into a full teardown plan
// through the planner's own decision table rather than a second, bespoke
// "destroy everything" code path.
func computePlan(ctx context.Context, cmd *cobra.Command, backend *state.Local, reg *registry.Registry, cfg compiler.ResolvedConfig, environment string, opts *GlobalOptions) (*planner.Plan, *state.State, error) {
	st, err := backend.Get(ctx, environment)
	if err != nil {
		return nil, nil, err
	}
	// Stamp the project name from the compiled configuration into the state
	// this run will persist if it reaches executor.Apply. Before this fix,
	// nothing in production ever set state.State.Project — every state.New
	// call outside a _test.go file grepped to nothing — so every real state
	// file on disk carried "" regardless of the project's actual name.
	// destroy (task 14) is the only command that reads its project name
	// FROM state rather than from compiled configuration (see destroy.go's
	// doc comment on why), which is what surfaced this: its one destructive
	// confirmation screen printed a blank project name instead of the real
	// one. cfg.Project is authoritative here — apply's cfg comes from
	// compiler.Compile (config wins over a stored value, this project's
	// usual rule), and destroy's own cfg.Project is already st.Project
	// itself (see newDestroyCommand's emptyCfg), so this line is a harmless
	// no-op re-assignment on that path.
	st.Project = cfg.Project

	obs, refreshDiags := refresh.Refresh(ctx, st, reg, opts.Parallelism)
	refreshDiags.Render(cmd.ErrOrStderr())
	if refreshDiags.HasErrors() {
		return nil, nil, errors.New("refreshing provider state failed")
	}

	p, planDiags := planner.Compute(cfg, st, obs, planner.Options{
		Environment: environment,
		Now:         time.Now,
		Registry:    reg,
	})
	planDiags.Render(cmd.ErrOrStderr())
	if planDiags.HasErrors() {
		return nil, nil, errors.New("planning failed")
	}
	return p, st, nil
}

// confirm prints prompt to stdout and reads exactly one line from stdin,
// reporting whether it equals want exactly (bufio.Scanner's default
// ScanLines split already strips the line terminator; nothing else is
// trimmed). A near miss — wrong case, "y" for "yes", trailing spaces — is
// refused, not generously accepted: a command about to mutate or destroy
// infrastructure should fail closed on ambiguous input.
//
// Scan returning false — EOF or any other read error — is treated as
// declined. See this task's doc comment for why that must never block.
func confirm(cmd *cobra.Command, prompt, want string) bool {
	fmt.Fprint(cmd.OutOrStdout(), prompt)
	scanner := bufio.NewScanner(cmd.InOrStdin())
	if !scanner.Scan() {
		return false
	}
	return scanner.Text() == want
}

// dependentsOf builds the `deps` function planner.BuildExecution requires.
// Reading internal/planner/execution.go: deps(addr) must report the
// addresses that depend ON addr, and BuildExecution's own doc comment says
// "Operation.Dependents already holds the resolved answer" — so this is a
// lookup into what the planner already decided, never a second resolution
// of dependency edges computed independently of it. apply and destroy
// share this helper for exactly that reason: two answers to "what depends
// on what" could disagree with each other.
func dependentsOf(p *planner.Plan) func(address.Address) []address.Address {
	byAddr := make(map[string]planner.Operation, len(p.Operations))
	for _, op := range p.Operations {
		byAddr[op.Address.String()] = op
	}
	return func(addr address.Address) []address.Address {
		return byAddr[addr.String()].Dependents
	}
}

// defaultRetryPolicy is the backoff apply and destroy hand the executor.
// Three attempts total, half a second base doubling toward a ten second
// ceiling, with full jitter (a uniform random duration between zero and the
// computed delay) so many operations retrying together do not all wake on
// the same tick and hammer the provider at once — the thundering-herd
// failure mode plain exponential backoff invites. Sleep is a real timer
// that still respects context cancellation, so a SIGINT during a retry
// wait does not have to run out the clock before the executor notices.
// Which operation kinds are ever retried at all, and under which
// classification, is the executor's own decision (spec §15, §35: Create
// never retried on an ambiguous failure, Delete only on SafeToRetry) — this
// policy only supplies the timing, identical for apply and destroy.
func defaultRetryPolicy() executor.RetryPolicy {
	return executor.RetryPolicy{
		MaxAttempts: 3,
		Base:        500 * time.Millisecond,
		Max:         10 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Jitter: func(d time.Duration) time.Duration {
			if d <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(d)))
		},
	}
}

// NOTE: an earlier draft of this task defined installDoubleInterruptHandler
// here, because no seam for signal handling was contract-named at the time.
// Task 11 owns that seam and exposes runInterruptible; two signal handlers in
// one binary is not a style question, it is a correctness one, so this
// definition was removed and the RunE above calls Task 11's instead.

// releaseLock unlocks best-effort on the way out of apply/destroy/refresh.
// It uses a fresh background context, not the run's own: the run's context
// may already be cancelled (SIGINT) or its deadline passed, and releasing
// the lock is exactly the cleanup that must still happen when that is
// true. A failure here is reported, not fatal: the run's actual result was
// already decided, and demoting a working apply to "error" because the
// lock file could not be removed would hide a fine outcome behind a worse
// one.
func releaseLock(backend *state.Local, environment string, stderr io.Writer) {
	if err := backend.Unlock(context.Background(), environment); err != nil {
		fmt.Fprintf(stderr, "warning: failed to release the lock on %q: %v\n", environment, err)
	}
}

// NOTE: an earlier draft of this task defined renderResult here, because no
// shared summary renderer was contract-named at the time. Task 12 owns that
// and exposes executor.Render(Result, RenderOptions). Two renderers for one
// Result is the duplicate-implementation defect that leaked a secret in M2,
// so this definition was removed and the RunE above calls Task 12's instead.
