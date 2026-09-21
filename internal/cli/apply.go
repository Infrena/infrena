package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/retry"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/report"
)

// applyPrompt is what a human sees before infrena mutates anything. "yes", typed
// in full and not a bare y, is the bar for an ordinary apply. `destroy` sets a
// higher bar — typing the environment name — because it is unconditionally
// destructive; most applies are pure creates with nothing to destroy at all, and
// attaching the heavier bar to every one of them would desensitize users to it
// well before it mattered.
const applyPrompt = "\nDo you want to perform these actions?\n" +
	"  infrena will perform the actions described above.\n" +
	"  Only 'yes' will be accepted to approve.\n\n" +
	"  Enter a value: "

// errNoApproval signals that the plan has changes, approval is required, and this
// run has no way to obtain it. It carries exit code ExitNoApproval, and its text
// names an escape the caller can actually take rather than telling a pipeline with
// nobody at the other end to type "yes".
var errNoApproval = errors.New(
	"these changes need approval and this run cannot ask for it: with --output set nothing " +
		"is reading stdout, and with stdin at end of input nothing is there to type.\n" +
		"Pass --auto-approve to approve without being asked, or review a saved plan " +
		"(`infrena plan --output FILE`) and apply it with --plan")

// requireApprovalRefusal reports why an environment declaring `require_approval`
// will not accept this run's approval, or nil when it will.
//
// `--auto-approve` is refused, and that is the entire point: a protection a flag can
// switch off protects nothing, because the flag is one line in the pipeline that was
// going to run anyway. On a protected environment there are exactly two ways through,
// and both involve a person — somebody confirms at a terminal, or somebody reviewed a
// plan and it is applied with `--plan`.
//
// A saved plan is the stronger of the two, not the weaker: it is the artifact that was
// reviewed, applied without recompiling, and refused outright if state moved since, so
// what runs is what was read. That is why `apply --plan` does not come through here at
// all.
//
// `--approved-by` is deliberately not a third way. It cannot be verified, and a
// protection resting on an unverifiable string is a protection in name. It is recorded
// in the report; it does not permit the run.
func requireApprovalRefusal(opts *GlobalOptions, p compiler.Protections, environment string, savedPlanAccepted bool) error {
	if !p.RequireApproval || !opts.AutoApprove {
		return nil
	}
	return &noApprovalError{msg: requireApprovalMessage(p, environment, savedPlanAccepted)}
}

// noApprovalError is an errNoApproval that carries its own wording.
//
// Unwrap rather than %w: root.go decides the exit code with
// errors.Is(err, errNoApproval), so this has to BE that error for 77's
// purposes, while saying something errNoApproval cannot say — which of the
// approvals this environment refuses, and why. %w would have printed both
// messages, and errNoApproval's names `--auto-approve` as the way out.
type noApprovalError struct{ msg string }

func (e *noApprovalError) Error() string { return e.msg }
func (e *noApprovalError) Unwrap() error { return errNoApproval }

// requireApprovalMessage is the wording for an environment that will not take this
// run's approval. Shared by the --auto-approve refusal and by the "nobody could
// answer" path, because on a protected environment those two are the same problem:
// errNoApproval's default advice is to pass --auto-approve, which is the one thing
// that cannot work here.
//
// savedPlanAccepted says whether the command being refused can take a saved plan.
// `apply` can; `destroy` cannot, and offering it `--plan` would name an escape that
// does not exist.
func requireApprovalMessage(p compiler.Protections, environment string, savedPlanAccepted bool) string {
	where := "environment " + strconv.Quote(environment)
	if p.RequireApprovalFrom != "" && p.RequireApprovalFrom != environment {
		// Inherited. Naming the environment being applied would send the reader
		// to a file that does not contain the setting.
		where += " (inherited from " + strconv.Quote(p.RequireApprovalFrom) + ")"
	}
	if !savedPlanAccepted {
		return fmt.Sprintf(
			"%s sets `require_approval`, so --auto-approve is refused: a protection a flag can "+
				"switch off is not one.\n"+
				"Run this without --auto-approve and confirm at the terminal. There is no saved-plan "+
				"route here, because destroy takes no plan — which is the point: an environment that "+
				"asks to be approved before it changes is not one a pipeline should be able to empty "+
				"unattended.\n"+
				"To tear it down from CI, remove its resources from configuration and apply that: the "+
				"change is then reviewable, and `infrena apply %s --plan plan.json --auto-approve` "+
				"carries the approval.",
			where, environment)
	}
	return fmt.Sprintf(
		"%s sets `require_approval`, so --auto-approve is refused: a protection a flag can "+
			"switch off is not one.\n"+
			"Approve it one of these two ways, both of which involve a person:\n"+
			"  - run this without --auto-approve and confirm at the terminal\n"+
			"  - review a plan and apply that:\n"+
			"      infrena plan %s --output plan.json\n"+
			"      infrena apply %s --plan plan.json --auto-approve\n"+
			"The second is what CI wants, and --auto-approve is accepted there because the plan "+
			"is the approval: it is applied without recompiling and is refused outright if state "+
			"moved since it was made, so what runs is what was reviewed. Add --approved-by to "+
			"record who reviewed it.",
		where, environment, environment)
}

// approvalUnobtainable reports whether asking for approval would be pointless
// because nobody could see the question. --output silences stdout for the whole run,
// so a prompt written there would reach nothing.
//
// One condition only, because a flag is knowable up front. The other half of "nobody
// can answer" — stdin already at end of input — cannot be detected without reading,
// and reading first would hold the prompt back until after the keystrokes it was
// meant to ask for. That half is decided where the answer is read, by confirm, which
// still runs before withLockedEnvironment and so refuses having mutated nothing.
func approvalUnobtainable(opts *GlobalOptions) bool {
	return opts.Output != ""
}

// approval is confirm's answer, which has three cases rather than two: an answer that
// is not the required one is a human declining, while no answer at all means nothing
// was there to ask. They earn different exit codes, so confirm reports which it was
// instead of a bare bool.
type approval int

const (
	approvalGranted approval = iota
	approvalDeclined
	approvalNoInput
)

// newApplyCommand builds `infrena apply <environment>`: compile, refresh, plan, show
// it, take approval, execute.
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

			// One backend for the whole run, opened before the first read and
			// closed when the command ends. Opening a second one would start a
			// second process, and for a backend that holds a connection open,
			// leave one of them behind holding it.
			backend, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}
			defer closeBackend()

			// An environment is reachable if it is declared or it has state, so
			// state is read here rather than inside computePlan: the rule decides
			// whether to compile at all.
			st0, stErr := backend.Get(cmd.Context(), environment)
			if stErr != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, stErr)
			}

			// State names plugins too: a resource removed from configuration is
			// still in state, and the plugin that manages it has to be loaded for
			// its destroy to be planned at all.
			ds := cds
			ds.Extend(ensureStateProviders(reg, st0, files))

			var cfg compiler.ResolvedConfig
			teardown := false
			switch disp, declared := dispositionOf(files, environment, st0); disp {
			case unknownEnvironment:
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
					unknownEnvironmentError(environment, declared))
			case orphanedEnvironment:
				// Deliberately not compiled — see orphanedEnvironment's doc
				// comment. Nothing compiles, so the provider instances this
				// teardown dispatches to have to be built the state-only way.
				//
				// The environment passed is empty, deliberately: this one is no
				// longer declared, so resolving its chain would report "unknown
				// environment", and its per-environment variable values went away
				// with the declaration. What a variable can still supply without
				// an environment is resolved; anything that needed one is refused
				// by name rather than guessed at.
				_, stateInstanceDiags := registerStateInstances(reg, opts, "", false)
				ds.Extend(stateInstanceDiags)
				cfg = teardownConfig(st0, environment)
				teardown = true
				fmt.Fprint(ro.Out(), teardownNotice(environment, declared))
			default:
				var compileDiags diag.Diagnostics
				cfg, compileDiags = compiler.Compile(files, reg, copts)
				ds.Extend(compileDiags)
			}

			// Rendered unconditionally, then checked: unlike plan.go, apply has
			// no later diagnostics pass to carry a warning through to the user,
			// so gating this render on HasErrors would drop one silently on an
			// otherwise-successful apply.
			renderDiagnostics(cmd.ErrOrStderr(), rw, ds)
			if ds.HasErrors() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
					configurationIsNotValid(cmd, opts, ro.Out(), loader))
			}

			// Unlocked preview, as `infrena plan` is: safe to run against a
			// locked environment, in CI, or repeatedly.
			p, _, _, err := computePlan(cmd.Context(), cmd, backend, reg, cfg, environment, opts, ro)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose, Definition: reg.Definition}))
			reportPlan(ro, p, report.StageProposed)

			if !p.HasChanges() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
			}

			// Before the --auto-approve branch, because it is that flag this
			// refuses, and before withLockedEnvironment on the same terms as
			// everything else here: a run that cannot be approved leaves the
			// environment exactly as it found it.
			if err := requireApprovalRefusal(opts, cfg.Protections, environment, true); err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			if !opts.AutoApprove {
				// Before withLockedEnvironment, and before anything is asked of a
				// provider: a run that cannot be approved must leave the
				// environment unlocked and unchanged rather than discover the
				// problem at the prompt.
				if approvalUnobtainable(opts) {
					// On a protected environment the default wording is wrong
					// rather than merely unhelpful: it offers --auto-approve,
					// which requireApprovalRefusal above would then refuse.
					err := errNoApproval
					if cfg.Protections.RequireApproval {
						err = &noApprovalError{msg: requireApprovalMessage(cfg.Protections, environment, true)}
					}
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
				}

				// A teardown apply deletes everything in the environment, so it
				// gets `destroy`'s stronger confirmation. The weaker prompt is
				// calibrated for a plan the user chose the contents of; here the
				// contents are "all of it", and the reason is a line they may
				// have deleted by accident.
				prompt, want := applyPrompt, "yes"
				if teardown {
					prompt = fmt.Sprintf("\nEnvironment %q is no longer declared, so this will delete "+
						"every resource infrena manages there. This cannot be undone.\n"+
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

			// Everything from here runs under runInterruptible, which owns
			// SIGINT: the first one cancels this context so no new work is
			// scheduled, while the operation already in flight finishes
			// against a context derived with context.WithoutCancel; the
			// second exits immediately and deliberately leaves the lock,
			// which the next run reports as stale. Signal handling lives in
			// internal/cli and never in the executor, so a library import
			// cannot install a handler behind a caller's back.
			return withLockedEnvironment(environment, "apply", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
				// Re-plan inside the lock: apply must never execute against
				// state or provider reality gathered before the lock was held.
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
				// What the provider reports now, which is what a provider's
				// `current` is built from — see executor.currentFor. These are the
				// observations the in-lock re-plan just took, never the pre-lock
				// ones: executing against reality gathered before the lock was
				// held is what re-planning inside the lock exists to prevent.
				execOpts.Observed = obs.States()
				// Always set, not only when there is a report to write: the same
				// hook renders progress to stdout, which is the half a human
				// watching an apply actually reads. Both halves are nil-safe.
				execOpts.OnEvent = eventHook(ro)

				reportPlan(ro, p2, report.StageExecuting)
				res, execDiags := executor.Apply(ctx, p2, g, st, execOpts)
				renderDiagnostics(cmd.ErrOrStderr(), rw, execDiags)

				// executor.Render is the one result renderer, shared rather than
				// reimplemented here: two renderers for one Result is how a
				// secret reaches output past the redaction the other one does.
				fmt.Fprint(ro.Out(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				result := applyResultFrom(res)
				// After Apply, so it is the serial the run produced rather than
				// the one it started from. The executor writes state under
				// context.WithoutCancel, so this is set even for a run that was
				// interrupted after its last Put — the run an audit most wants
				// to be able to place.
				result.StateSerial = st.Serial
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

// computePlan runs the same unlocked, side-effect-free sequence `infrena plan` uses
// — backend.Get, refresh.Refresh, planner.Compute — and returns the resulting plan,
// the state it was computed against, and the observations it was computed against.
//
// The three are returned separately and must stay separate. st is the last state
// persisted and obs is what the provider reports now; the planner's entire diff is
// the one against the other. Folding the observations into st here — the tidy-looking
// "one current truth" refactor — collapses that diff to nothing, so the planner would
// propose no change and drift would silently stop being corrected. The executor needs
// the observations too, which is why they come back from here, but the merge happens
// inside the executor, strictly between planning and execution.
//
// apply calls it twice, before and after taking the lock. destroy reuses it unchanged
// with an empty desired configuration, which turns "everything currently in state"
// into a full teardown plan through the planner's own decision table rather than a
// second, bespoke "destroy everything" code path.
func computePlan(ctx context.Context, cmd *cobra.Command, backend state.Backend, reg *registry.Registry, cfg compiler.ResolvedConfig, environment string, opts *GlobalOptions, ro *runOutput) (*planner.Plan, *state.State, refresh.Observations, error) {
	rw := ro.Report()
	st, err := backend.Get(ctx, environment)
	if err != nil {
		return nil, nil, nil, err
	}
	// Stamp the project name from the compiled configuration into the state this run
	// will persist. Nothing else sets it, and a state file carrying "" is what makes
	// destroy — the one command that reads its project name from state rather than
	// from configuration — print a blank name on its confirmation screen. cfg.Project
	// is authoritative: configuration wins over a stored value, and on destroy's path
	// cfg.Project is st.Project already, so this is a no-op there.
	st.Project = cfg.Project

	// On a real account the refresh phase dominates the wait, and an apply that says
	// nothing while it runs is indistinguishable from one that has hung. st is passed
	// as loaded, before refresh applies anything, which is what observationHook needs.
	obs, refreshDiags := refresh.Refresh(ctx, st, reg, opts.Parallelism, perProviderParallelism, readRetryPolicy(),
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

// confirm prints prompt to out and reads exactly one line from in, reporting whether
// it equals want exactly (ScanLines strips the line terminator; nothing else is
// trimmed). A near miss — wrong case, "y" for "yes", trailing spaces — is refused: a
// command about to mutate or destroy infrastructure fails closed on ambiguous input.
//
// Print, then read, and the order matters: on a terminal any read blocks until the
// user types, so reading first would hold the most important prompt in the tool back
// until after the keystrokes it was meant to ask for.
//
// Scan returning false is end of input — a closed pipe, /dev/null, a CI job with no
// terminal — and is reported as approvalNoInput rather than as a decline, because
// nobody was there to decline.
//
// The prompt goes to out, the run's own writer, rather than to stdout directly: under
// --output stdout carries nothing at all, and a prompt is not the exception.
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

// dependentsOf builds the `deps` function planner.BuildExecution requires: for an
// address, the addresses that depend on it.
//
// It is a lookup into what the planner already decided, never a second resolution of
// dependency edges. apply and destroy share it for that reason — two answers to "what
// depends on what" could disagree.
func dependentsOf(p *planner.Plan) func(address.Address) []address.Address {
	byAddr := make(map[string]planner.Operation, len(p.Operations))
	for _, op := range p.Operations {
		byAddr[op.Address.String()] = op
	}
	return func(addr address.Address) []address.Address {
		return byAddr[addr.String()].Dependents
	}
}

// readRetryPolicy is the backoff for a read — refresh's provider reads and
// discover's sweeps — and it is deliberately more patient than defaultRetryPolicy.
//
// defaultRetryPolicy is tuned for mutations, where few attempts is itself the safety
// property: another Create may make a second resource. A read cannot do that. Its
// only cost is time, weighed against failing an entire plan over a throttle that
// would have cleared, which is what a refresh of a large environment actually hits.
//
// Five attempts from half a second is 8s of waiting at worst, under a 30s ceiling the
// schedule never reaches; the ceiling is there for a policy that later grows more
// attempts. Full jitter spreads the herd, and a refresh retries many resources at
// once by construction.
func readRetryPolicy() retry.Policy {
	return retry.Policy{
		MaxAttempts: 5,
		Base:        500 * time.Millisecond,
		Max:         30 * time.Second,
	}
}

// defaultRetryPolicy is the backoff apply and destroy hand the executor: three
// attempts, half a second doubling toward a ten second ceiling, with full jitter so
// operations retrying together do not all wake on the same tick and hammer the
// provider at once. Sleep is a real timer that still respects context cancellation,
// so a SIGINT during a retry wait does not have to run out the clock before the
// executor notices.
//
// Which operation kinds are retried at all is the executor's own decision — a Create
// is never retried on an ambiguous failure, a Delete only when it is safe to. This
// supplies the timing and nothing else.
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

// releaseLock unlocks best-effort on the way out of apply, destroy and refresh.
//
// It uses a fresh background context, not the run's own: the run's may already be
// cancelled by a SIGINT, and releasing the lock is exactly the cleanup that must
// still happen when it is. A failure is reported, not fatal — the run's result was
// already decided, and demoting a working apply to "error" because a lock file could
// not be removed would hide a fine outcome behind a worse one.
func releaseLock(backend state.Backend, environment string, stderr io.Writer) {
	if err := backend.Unlock(context.Background(), environment); err != nil {
		fmt.Fprintf(stderr, "warning: failed to release the lock on %q: %v\n", environment, err)
	}
}

// perProviderParallelism is the second concurrency bound: how many operations may be
// in flight against one provider at a time, independent of --parallelism.
//
// It must be a constant rather than opts.Parallelism. The global semaphore already
// admits at most Parallelism operations, so a per-provider limit set to the same
// number can never bind, and the whole mechanism sits inert while appearing to
// protect one provider from an unrelated wide graph.
//
// 8, below --parallelism's default of 10, so it binds at default settings rather than
// only for users who raise the global bound. It is a deliberately conservative
// ceiling of the order cloud APIs throttle at, not a figure tuned to any real one; a
// provider that knows its own limits declares them instead — see
// executor.Options.ProviderLimits.
const perProviderParallelism = 8

// executorOptions builds the executor.Options apply and destroy both run with. One
// constructor, not two identical literals, so destroy cannot silently drift into
// different concurrency or a different retry policy from apply.
func executorOptions(opts *GlobalOptions, reg *registry.Registry, backend state.Backend, environment string) executor.Options {
	return executor.Options{
		Parallelism: opts.Parallelism,
		PerProvider: perProviderParallelism,
		// What individual plugins declared, which overrides the constant above
		// for those that did. See executor.Options.ProviderLimits.
		ProviderLimits: reg.DeclaredConcurrency(),
		Registry:       reg,
		Backend:        backend,
		Environment:    environment,
		Retry:          defaultRetryPolicy(),
		Now:            time.Now,
	}
}
