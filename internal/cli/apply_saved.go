package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/report"
)

// applySavedPlan applies a plan artifact written earlier by `infrena plan --output`.
//
// THIS PATH DOES NOT COMPILE, and that is its whole purpose. The artifact is the
// executor's complete instruction set, so applying it runs exactly what was reviewed
// rather than whatever configuration happens to say now. Recompiling would make the
// review worthless: the thing approved and the thing executed could differ.
//
// It therefore also does not REFRESH. A saved plan's before-values came from the refresh
// that ran when the plan was made, and re-reading provider reality would produce
// different before-values from the ones the plan was reviewed against. What stands in for
// both the recompile and the refresh is the staleness check below.
//
// The normal apply path re-plans INSIDE the lock, because "apply must never execute
// against state or provider reality gathered before the lock was held". A saved plan
// cannot be re-planned without defeating itself, so it gets the equivalent guarantee in
// the other direction: the state is read inside the lock and the plan is REFUSED if that
// state is not the state it was made against. Verifying the premise replaces recomputing
// the conclusion.
func applySavedPlan(
	cmd *cobra.Command, opts *GlobalOptions, environment, planPath string, ro *runOutput,
) error {
	rw := ro.Report()
	// A saved plan is already resolved, so a variable cannot reach it. Unlike refresh
	// and destroy — which DO take --var, because they read `providers:` and an instance's
	// configuration interpolates — there is nothing here for one to affect: the provider
	// instances come from the artifact's own `provider` field. Accepting a flag that
	// cannot change the outcome is the advertised-and-ignored shape checkUnsupportedFlags
	// exists to refuse.
	if len(opts.Vars) > 0 || len(opts.VarFiles) > 0 {
		return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errors.New(
			"--plan does not combine with --var or --var-file: a saved plan is already "+
				"resolved, so a variable cannot change what it does.\n"+
				"Re-run `infrena plan` with those variables and save the plan that produces"))
	}

	// readSavedPlan, not os.ReadFile plus DecodePlan: `infrena plan --output`
	// now writes the artifact on a `plan` line inside the report stream, and
	// plans saved by an earlier release are still bare documents on disk.
	// Both shapes are applied; anything else is refused by name. See
	// planstream.go.
	p, err := readSavedPlan(planPath)
	if err != nil {
		return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
			fmt.Errorf("reading %s: %w", planPath, err))
	}

	// The state-only registry, for the same reason `destroy` uses it: nothing here
	// compiles, so stage 4.5 never runs and the instances the plan dispatches to have to
	// be built from `providers:` directly. It resolves variables for this environment,
	// so an instance configured per environment works here too.
	reg, _, regDiags, closePlugins := stateOnlyRegistry(opts, environment)
	defer closePlugins()
	if regDiags.HasErrors() {
		regDiags.Render(cmd.ErrOrStderr())
		return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errProviderInstances)
	}

	// The project name from CONFIGURATION, decoded and not compiled. It is what the
	// plan's own `project` is checked against, so that applying dev's plan inside
	// another project's directory is refused — the mistake a shared artifact makes easy.
	// Unreadable configuration is not fatal: the plan carries everything needed to
	// execute, and refusing to apply a reviewed plan because a file was edited would be
	// the recompile this path exists to avoid, arriving by the back door.
	project := projectNameFor(opts.Dir)
	if project == "" {
		project = p.Project
	}

	// REFUSED BEFORE THE PLAN IS RENDERED. Identity cannot change under us, so there is
	// no reason to make a reader study a plan that was never going to run here — which
	// is what happened before this check moved up: a wrong-environment plan printed in
	// full and was then refused.
	if err := p.CheckIdentity(project, environment); err != nil {
		return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
	}

	backend := backendFor(opts.Dir)

	// Shown before the confirmation, and rendered from the artifact rather than
	// recomputed: what the user is asked to approve has to be what will run.
	fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose, Definition: reg.Definition}))

	if !p.HasChanges() {
		fmt.Fprintln(ro.Out(), "This plan proposes no changes.")
		return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
	}

	if !opts.AutoApprove {
		// No approvalUnobtainable check here, and none is needed: --plan is
		// itself one of the escapes that refusal names. A plan reviewed and
		// saved earlier is the reviewed-then-applied workflow, so the only
		// question left is whether the operator standing here approves it —
		// and for the same reason no answer is treated no differently from a
		// wrong one, there being no further escape left to name.
		if confirm(cmd.InOrStdin(), ro.Out(), applyPrompt, "yes") != approvalGranted {
			return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
				errors.New("apply cancelled: you must type \"yes\" to approve"))
		}
	}

	return withLockedEnvironment(environment, "apply", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
		st, err := backend.Get(ctx, environment)
		if err != nil {
			return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
		}

		// FINGERPRINT BEFORE MUTATING, and the order is load-bearing rather than
		// tidy. `infrena plan` hashes the state exactly as it came off disk, so the
		// hash recorded in the artifact is of unmodified state. Stamping the project
		// first — which the line below does, and which computePlan does on the normal
		// path — changes the bytes and therefore the hash, and the comparison would
		// then fail on a state nothing had touched. Measured, not reasoned: the first
		// end-to-end run of --plan refused itself with "the state has changed (serial
		// 0, now 0)" for exactly this reason.
		//
		// INSIDE THE LOCK. Checking before taking it would leave a window in which
		// another apply lands between the check and the execution — which is the
		// specific thing this check exists to prevent.
		hash, err := planner.HashState(st)
		if err != nil {
			return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
				fmt.Errorf("fingerprinting state: %w", err))
		}
		// NO CONFIGURATION HASH is passed, deliberately. Computing one requires the
		// compile this path exists to skip, and a plan applied after configuration
		// changed is not a mistake: reviewing a plan and then applying it is the
		// workflow --plan is for. What must not have moved is the STATE the plan's
		// before-values describe.
		if err := p.CheckApplicable(project, environment, "", planner.StateFingerprint(st.Serial, hash)); err != nil {
			return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
		}

		// STAMP THE PROJECT, exactly as computePlan does on the normal path, and only
		// now that the fingerprint has been taken. Nothing else here sets it, so
		// without this line applying a saved plan writes a state file whose project is
		// "" — the bug computePlan's comment records as fixed, reappearing on this path
		// alone. The plan is the authority: it was made from configuration, so it
		// carries the name configuration gave.
		st.Project = p.Project

		g, err := planner.BuildExecution(p, dependentsOf(p))
		if err != nil {
			return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
				fmt.Errorf("building execution graph: %w", err))
		}

		execOpts := executorOptions(opts, reg, backend, environment)
		// NO OBSERVATIONS, deliberately: this path applies a plan saved
		// earlier and does not refresh, which is the whole reason it is fast
		// and does not need the compile. executor.Options.Observed is left nil,
		// and executor.currentFor then falls back to the last persisted state
		// for every address — exactly what the executor used before
		// observations existed. Refreshing here to fill it in would contradict
		// what --plan is: the before-values the operator reviewed and approved
		// are the plan's, and CheckApplicable above has already refused the run
		// if the state they describe has moved.
		execOpts.OnEvent = eventHook(ro)

		res, execDiags := executor.Apply(ctx, p, g, st, execOpts)
		renderDiagnostics(cmd.ErrOrStderr(), rw, execDiags)
		fmt.Fprint(ro.Out(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

		result := applyResultFrom(res)
		if execDiags.HasErrors() || len(res.Failed) > 0 {
			return finishApply(cmd.ErrOrStderr(), rw, result, errors.New("apply completed with failures"))
		}
		return finishApply(cmd.ErrOrStderr(), rw, result, errChanges)
	})
}

// projectNameFor reads a project's name without compiling it.
//
// Decode only: the name is a literal in infra.yml, so nothing needs resolving, and a
// path that exists to skip compilation must not compile in order to learn one string.
// An empty return means "could not tell", which every caller must treat as a reason to
// carry on rather than a reason to stop — a project whose configuration cannot be read is
// exactly the case `destroy` is allowed to serve.
func projectNameFor(dir string) string {
	files, err := config.Load(dir)
	if err != nil {
		return ""
	}
	decl, ds := config.Decode(files)
	if ds.HasErrors() || decl == nil {
		return ""
	}
	return decl.Project
}
