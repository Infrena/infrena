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

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/report"
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

// errNoApproval signals that the plan has changes, approval is required, and
// this run has no way to obtain it. It carries exit code 77 (ExitNoApproval),
// and its text names an escape the caller can actually take — which is the
// whole reason it exists, the outcome it replaced being a suggestion to type
// "yes" into a pipeline with nobody at the other end.
var errNoApproval = errors.New(
	"these changes need approval and this run cannot ask for it: with --output set nothing " +
		"is reading stdout, and with stdin at end of input nothing is there to type.\n" +
		"Pass --auto-approve to approve without being asked, or review a saved plan " +
		"(`infrena plan --output FILE`) and apply it with --plan")

// approvalUnobtainable reports whether asking for approval would be pointless
// because nobody could see the question. --output silences stdout for the
// whole run (spec 2.1), so a prompt written there would reach nothing.
//
// ONE CONDITION, decided up front because a flag is knowable up front. The
// other half of "nobody can answer" — stdin already at end of input — is not
// knowable without reading, and an earlier version of this function detected
// it by peeking a byte off stdin. On a terminal that peek blocks until the
// user types, so the prompt appeared only after their keystrokes and a user
// faced a blank screen with no idea they were being asked to approve a
// mutation. End of input is therefore decided where the answer is read, by
// confirm, which prints first and reads second — and which still runs before
// withLockedEnvironment, so refusing there mutates exactly as little.
func approvalUnobtainable(opts *GlobalOptions) bool {
	return opts.Output != ""
}

// approval is confirm's answer, which has three cases rather than two: an
// answer that is not the required one is a human declining, while no answer
// at all means nothing was there to ask. They deserve different exit codes,
// so confirm reports which it was instead of a bare bool.
type approval int

const (
	approvalGranted approval = iota
	approvalDeclined
	approvalNoInput
)

// newApplyCommand builds `infra apply <environment>`: compile, refresh,
// plan, show it, take approval, execute. Spec §16, §9.2, §10.
func newApplyCommand(opts *GlobalOptions) *cobra.Command {
	var planPath string

	cmd := &cobra.Command{
		Use:           "apply <environment>",
		Short:         "Reconcile real infrastructure with the configured desired state",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			ro, closeRun, err := openRun(cmd, opts, "apply", environment)
			if err != nil {
				return err
			}
			defer closeRun()
			rw := ro.Report()

			// A saved plan takes an entirely separate path rather than branching
			// through the one below: it compiles nothing, refreshes nothing, and
			// re-plans nothing, so almost every step here would have to be skipped.
			// See applySavedPlan.
			if planPath != "" {
				return applySavedPlan(cmd, opts, environment, planPath, ro)
			}

			copts, cds := compilerOptions(opts, environment)
			if cds.HasErrors() {
				renderDiagnostics(cmd.ErrOrStderr(), rw, cds)
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errNotValid)
			}

			files, err := config.Load(opts.Dir)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			reg, loader := buildRegistryWithLoader(opts)
			defer loader.Close()
			// apply is permitted to change the project directory, so it is
			// where a pin first gets recorded. validate and plan only compare.
			copts.RecordLocks = true

			// §6.1: reachable if declared OR stateful. State is read here rather
			// than inside computePlan because the rule decides whether to
			// compile at all.
			st0, stErr := backendFor(opts.Dir).Get(cmd.Context(), environment)
			if stErr != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, stErr)
			}

			// STATE names plugins too, and for the case invariant 1 is about: a
			// resource removed from configuration is still in state, and the plugin
			// that manages it has to be loaded for the destroy to be planned at all.
			ds := cds
			ds.Extend(ensureStateProviders(reg, st0))

			var cfg compiler.ResolvedConfig
			teardown := false
			switch disp, declared := dispositionOf(files, environment, st0); disp {
			case unknownEnvironment:
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
					unknownEnvironmentError(environment, declared))
			case orphanedEnvironment:
				// Deliberately NOT compiled — see orphanedEnvironment's doc
				// comment for what compiling would produce instead. Which means
				// stage 4.5 never runs, so the provider instances this teardown
				// dispatches to have to be built the state-only way: the names come
				// from state, and an instance whose configuration depends on an
				// environment that no longer exists has nothing to resolve against.
				//
				// EMPTY ENVIRONMENT, deliberately: this environment is no longer
				// declared, so resolving its chain would report "unknown
				// environment" — and its per-environment variable values went away
				// with the declaration. What a variable can still supply without an
				// environment is resolved; anything that needed one is refused by
				// name rather than guessed at.
				_, stateInstanceDiags := registerStateInstances(reg, opts, "")
				ds.Extend(stateInstanceDiags)
				cfg = teardownConfig(st0, environment)
				teardown = true
				fmt.Fprint(ro.Out(), teardownNotice(environment, declared))
			default:
				var compileDiags diag.Diagnostics
				cfg, compileDiags = compiler.Compile(files, reg, copts)
				ds.Extend(compileDiags)
			}

			// Rendered unconditionally, THEN checked: unlike plan.go, apply
			// has no later diagnostics pass that would otherwise carry a
			// --var-file warning (the reserved block-name check in
			// DecodeVariableFile) through to the user. Gating this render on
			// HasErrors would silently drop it on an otherwise-successful
			// apply.
			renderDiagnostics(cmd.ErrOrStderr(), rw, ds)
			if ds.HasErrors() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
					configurationIsNotValid(cmd, opts, ro.Out(), loader))
			}

			backend := backendFor(opts.Dir)

			// Unlocked preview — identical in spirit to `infra plan`: safe
			// to run against a locked environment, in CI, or repeatedly.
			p, _, _, err := computePlan(cmd.Context(), cmd, backend, reg, cfg, environment, opts, ro)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose, Definition: reg.Definition}))

			if !p.HasChanges() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
			}

			if !opts.AutoApprove {
				// BEFORE withLockedEnvironment, and before anything is asked
				// of a provider: a run that cannot be approved must leave the
				// environment exactly as it found it, unlocked and unchanged,
				// rather than discovering the problem at the prompt.
				if approvalUnobtainable(opts) {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errNoApproval)
				}

				// A teardown apply deletes everything in the environment, so it
				// gets `destroy`'s stronger confirmation rather than "yes". The
				// two commands are doing the same thing at this point, and the
				// weaker prompt is calibrated for a plan the user chose the
				// contents of — here the contents are "all of it", and the
				// reason is a line they may have deleted by accident.
				prompt, want := applyPrompt, "yes"
				if teardown {
					prompt = fmt.Sprintf("\nEnvironment %q is no longer declared, so this will delete "+
						"every resource infra manages there. This cannot be undone.\n"+
						"Type the environment name to confirm: ", environment)
					want = environment
				}
				// Still before the lock, so end of input discovered here refuses
				// on exactly the same terms as the flag check above: nothing has
				// been locked and nothing has been mutated.
				switch confirm(cmd.InOrStdin(), ro.Out(), prompt, want) {
				case approvalNoInput:
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errNoApproval)
				case approvalDeclined:
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
						errors.New("apply cancelled: you must type \"yes\" to approve"))
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
			return withLockedEnvironment(environment, "apply", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
				// Re-plan inside the lock — see this task's doc comment above
				// for why: apply must never execute against state or provider
				// reality gathered before the lock was held.
				p2, st, obs, err := computePlan(ctx, cmd, backend, reg, cfg, environment, opts, ro)
				if err != nil {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
				}
				if !p2.HasChanges() {
					fmt.Fprintln(ro.Out(), "\nNo changes remained once the environment lock was acquired; nothing to apply.")
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
						fmt.Errorf("building execution graph: %w", err))
				}

				execOpts := executorOptions(opts, reg, backend, environment)
				// What the provider reports NOW, which is what a provider's
				// `current` is built from — see executor.currentFor. These are the
				// observations the in-lock re-plan just took, never the pre-lock
				// ones: executing against reality gathered before the lock was held
				// is the exact thing re-planning inside the lock exists to prevent.
				execOpts.Observed = obs.States()
				// Always set now, not only when there is a report to write:
				// the same hook renders progress to stdout, which is the
				// half a human watching an apply actually reads. Both halves
				// are nil-safe — see eventHook.
				execOpts.OnEvent = eventHook(ro)

				res, execDiags := executor.Apply(ctx, p2, g, st, execOpts)
				renderDiagnostics(cmd.ErrOrStderr(), rw, execDiags)

				// executor.Render (Task 12) is THE result renderer. An earlier
				// draft of this task wrote a private renderResult here; two
				// renderers for one Result is the duplicate-implementation
				// defect that put a secret in M2's output, so this calls the
				// shared one.
				fmt.Fprint(ro.Out(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				result := applyResultFrom(res)
				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return finishApply(cmd.ErrOrStderr(), rw, result, errors.New("apply completed with failures"))
				}
				return finishApply(cmd.ErrOrStderr(), rw, result, errChanges)
			})
		},
	}

	// The backticked word is cobra's argument placeholder, so there is exactly one of
	// them and it is the one a user types. A second pair would print literal backticks.
	cmd.Flags().StringVar(&planPath, "plan", "",
		"apply the plan artifact in `file`, written earlier by infrena plan --output, "+
			"instead of compiling configuration")
	return cmd
}

// computePlan runs the same unlocked, side-effect-free sequence `infra
// plan` uses — backend.Get, refresh.Refresh, planner.Compute — and returns
// the resulting plan, the state it was computed against, and the
// observations it was computed against.
//
// THE THREE ARE RETURNED SEPARATELY AND MUST STAY SEPARATE. st is the last
// state persisted and obs is what the provider reports NOW; the planner's
// entire diff is the one against the other. Folding the observations into
// st here — the tidy-looking "one current truth" refactor — collapses that
// diff to nothing: the planner would propose no change, and every drift
// would silently stop being corrected. The executor needs the observations
// too (they are what a provider's `current` is built from, see
// executor.currentFor) and that is why they come back from here, but the
// merge happens strictly between planning and execution, inside the
// executor, and never before planner.Compute.
// apply calls it twice (before and after taking the lock); destroy (task
// 14) reuses it unchanged with an empty desired configuration, which is
// what turns "everything currently in state" into a full teardown plan
// through the planner's own decision table rather than a second, bespoke
// "destroy everything" code path.
func computePlan(ctx context.Context, cmd *cobra.Command, backend *state.Local, reg *registry.Registry, cfg compiler.ResolvedConfig, environment string, opts *GlobalOptions, ro *runOutput) (*planner.Plan, *state.State, refresh.Observations, error) {
	rw := ro.Report()
	st, err := backend.Get(ctx, environment)
	if err != nil {
		return nil, nil, nil, err
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

	// The refresh hook, which used to be nil here: on a real account this
	// phase dominates the wait, and an apply that says nothing while it runs
	// is indistinguishable from one that has hung. st is passed as loaded,
	// before refresh applies anything, which is what observationHook needs.
	obs, refreshDiags := refresh.Refresh(ctx, st, reg, opts.Parallelism, perProviderParallelism,
		observationHook(ro, st))
	renderDiagnostics(cmd.ErrOrStderr(), rw, refreshDiags)
	if refreshDiags.HasErrors() {
		return nil, nil, nil, errors.New("refreshing provider state failed")
	}

	p, planDiags := planner.Compute(cfg, st, obs, planner.Options{
		Environment: environment,
		Now:         time.Now,
		Registry:    reg,
	})
	renderDiagnostics(cmd.ErrOrStderr(), rw, planDiags)
	if planDiags.HasErrors() {
		return nil, nil, nil, errors.New("planning failed")
	}
	return p, st, obs, nil
}

// confirm prints prompt to stdout and reads exactly one line from stdin,
// reporting whether it equals want exactly (bufio.Scanner's default
// ScanLines split already strips the line terminator; nothing else is
// trimmed). A near miss — wrong case, "y" for "yes", trailing spaces — is
// refused, not generously accepted: a command about to mutate or destroy
// infrastructure should fail closed on ambiguous input.
//
// PRINT, THEN READ, and the order is the whole point rather than the
// obvious way round. Nothing may touch stdin before the prompt has been
// written, because on a terminal any read blocks until the user types: a
// check that read first would hold the most important prompt in the tool
// back until after the keystrokes it was meant to ask for.
//
// Scan returning false is end of input — a closed pipe, /dev/null, a CI job
// with no terminal — and is reported as approvalNoInput rather than as a
// decline, because nobody was there to decline. Callers that can offer an
// escape turn it into errNoApproval and exit 77.
//
// The prompt goes to out rather than to cmd.OutOrStdout() directly, because
// out is the run's own writer: under --output stdout carries nothing at all
// (spec 2.1), and a prompt is not the exception to that rule.
func confirm(in io.Reader, out io.Writer, prompt, want string) approval {
	fmt.Fprint(out, prompt)
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return approvalNoInput
	}
	if scanner.Text() != want {
		return approvalDeclined
	}
	return approvalGranted
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

// perProviderParallelism is spec §15/§34's SECOND concurrency bound: how
// many operations may be in flight against one provider at a time,
// independent of --parallelism.
//
// It is a constant rather than opts.Parallelism, and that is the whole
// point. Both commands used to pass PerProvider: opts.Parallelism, which
// can never bind: the global semaphore already admits at most Parallelism
// operations, so providerInFlight[p] >= PerProvider is unreachable before
// the global check has already deferred the node. The mechanism was
// implemented, tested in internal/executor, and inert in the shipped
// product — "one provider's rate limits cannot be exhausted by an unrelated
// wide graph" was a property nothing actually provided.
//
// 8, below --parallelism's default of 10, so it binds at default settings
// rather than only for users who raise the global bound — a ceiling that is
// inert unless configured is the same defect one step removed. It is not
// tuned to any real API, because M3 has no real provider to tune against;
// it is a deliberately conservative ceiling of the order cloud APIs
// throttle at.
//
// A per-provider FLAG is deliberately not added here. Spec §37 fixes the
// global option set for this milestone, and the right long-run answer is
// probably for a provider to declare its own ceiling (it is the only party
// that knows its rate limits) rather than for the user to guess one. Both
// are Phase 3 decisions, when a provider with real limits exists. What this
// constant fixes is that the bound is real in the meantime.
const perProviderParallelism = 8

// executorOptions builds the executor.Options apply and destroy both run
// with. One constructor, not two identical literals: the copies had already
// drifted in a comment and not in behaviour, and the next drift is the one
// where destroy silently gets different concurrency or a different retry
// policy from apply for no stated reason.
func executorOptions(opts *GlobalOptions, reg *registry.Registry, backend *state.Local, environment string) executor.Options {
	return executor.Options{
		Parallelism: opts.Parallelism,
		PerProvider: perProviderParallelism,
		Registry:    reg,
		Backend:     backend,
		Environment: environment,
		Retry:       defaultRetryPolicy(),
		Now:         time.Now,
	}
}
