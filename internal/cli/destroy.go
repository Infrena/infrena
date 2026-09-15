package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/report"
)

// newDestroyCommand builds `infra destroy <environment>`: plan the removal
// of everything infra manages in the environment, confirm, execute. Spec
// §16, §9.2, §11, §13.
//
// destroy does not write a second "delete everything" code path. It hands
// computePlan an empty compiler.ResolvedConfig{Project, Environment} — no
// resources at all. Every resource recorded in state then falls into
// planner.Compute's own "in state, not in config" row (spec §11), which is
// already exactly Destroy, or Forget under retain, or a plan-time refusal
// under prevent_destroy. computePlan (Task 13) does not care whether its
// cfg argument came from compiling YAML or was built by hand, so it is
// reused unchanged, and destroy never calls config.Load or compiler.Compile
// at all: the project name comes from state.State.Project, not infra.yml.
// destroy DOES take --var and --var-file, and used to refuse them. The refusal
// rested on "there is no configuration for a variable to interpolate into",
// which was true of resources and never true of `providers:`: an instance's
// own configuration interpolates variables, and destroy has to construct that
// instance to dispatch a delete to the right account. So the flag did affect
// the outcome, and refusing it left `defaults: {region: ${var.aws_region}}`
// destroyable by nothing. Reversed 2026-09-13 with §12.1.
//
// The older reasoning it replaced is still right about its own case and worth
// keeping in mind: a flag that cannot affect the outcome must be refused
// rather than accepted and ignored, which is what checkUnsupportedFlags is
// for. The change here is that this flag can.
func newDestroyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "destroy <environment>",
		Short:         "Destroy every resource infra manages in an environment",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			ro, closeRun, err := openRun(cmd, opts, "destroy", environment)
			if err != nil {
				return err
			}
			defer closeRun()
			rw := ro.Report()

			reg, _, regDiags, closePlugins := stateOnlyRegistry(opts, environment)
			defer closePlugins()
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}
			backend := backendFor(opts.Dir)

			st0, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}
			// STATE names the plugins here, not configuration: a destroy's premise is
			// that nothing is configured.
			if stateDiags := ensureStateProviders(reg, st0); stateDiags.HasErrors() {
				stateDiags.Render(cmd.ErrOrStderr())
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errProviderInstances)
			}
			// An empty desired configuration: every resource recorded in
			// state falls into planner.Compute's "in state, not in
			// config" row, which is already Destroy/Forget/prevent_destroy
			// (spec §11) — see this command's doc comment above.
			emptyCfg := compiler.ResolvedConfig{Project: st0.Project, Environment: environment}

			// Unlocked preview — identical in spirit to `infra plan`.
			p, _, err := computePlan(cmd.Context(), cmd, backend, reg, emptyCfg, environment, opts, ro)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose, Definition: reg.Definition}))

			if !p.HasChanges() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
			}

			if !opts.AutoApprove {
				// The identical rule apply applies, and for the identical
				// reason: a higher confirmation bar does not make the answer
				// obtainable. This runs before withLockedEnvironment, so a
				// destroy nobody can approve leaves the environment untouched
				// and unlocked. One reader for stdin — see approvalUnobtainable.
				in := bufio.NewReader(cmd.InOrStdin())
				if approvalUnobtainable(opts, in) {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errNoApproval)
				}

				prompt := fmt.Sprintf("\nDestroying environment %q will delete every resource infra "+
					"manages there. This cannot be undone.\nType the environment name to confirm: ", environment)
				if !confirm(in, ro.Out(), prompt, environment) {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
						fmt.Errorf("destroy cancelled: you must type %q to confirm", environment))
				}
			}

			// Same seam as apply (Task 13): Task 11's runInterruptible owns
			// SIGINT, and Task 12's executor.Render owns the result summary.
			return withLockedEnvironment(environment, "destroy", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
				// Re-plan inside the lock — apply's doc comment explains why:
				// destroy must never execute against state or provider
				// reality gathered before the lock was held.
				p2, st, err := computePlan(ctx, cmd, backend, reg, emptyCfg, environment, opts, ro)
				if err != nil {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
				}
				if !p2.HasChanges() {
					fmt.Fprintln(ro.Out(), "\nNothing remained to destroy once the environment lock was acquired.")
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
						fmt.Errorf("building execution graph: %w", err))
				}

				execOpts := executorOptions(opts, reg, backend, environment)
				// See apply.go's identical wiring: always set, because the
				// same hook is what renders progress to stdout.
				execOpts.OnEvent = eventHook(ro)

				res, execDiags := executor.Apply(ctx, p2, g, st, execOpts)
				renderDiagnostics(cmd.ErrOrStderr(), rw, execDiags)

				// executor.Render (Task 12) is THE result renderer — see
				// apply.go's note on why this command does not write its own.
				fmt.Fprint(ro.Out(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				result := applyResultFrom(res)
				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return finishApply(cmd.ErrOrStderr(), rw, result, errors.New("destroy completed with failures"))
				}
				return finishApply(cmd.ErrOrStderr(), rw, result, errChanges)
			})
		},
	}
}
