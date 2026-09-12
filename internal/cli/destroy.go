package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/executor"
	"github.com/infrata/infrata/internal/planner"
	"github.com/infrata/infrata/pkg/report"
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
// destroy also refuses --var and --var-file outright (rejectVariableFlags),
// rather than accepting and silently ignoring them. There is no
// configuration for either flag to interpolate into — see above — but
// accepting them anyway would be exactly the advertised-and-ignored shape
// checkUnsupportedFlags exists to prevent for an unwired flag, and that
// reasoning does not change just because the flag works on other commands.
// An earlier version of this comment argued the opposite ("simply
// inapplicable ... not a broken promise"); it was wrong. A flag that cannot
// affect the outcome and is accepted anyway is ignored, by definition, and
// the failure a user hits is the same one either way: they believe --var did
// something here, and it did not.
func newDestroyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "destroy <environment>",
		Short:         "Destroy every resource infra manages in an environment",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rejectVariableFlags(opts, "destroy"); err != nil {
				return err
			}

			environment := args[0]

			rw, closeReport, err := openReport(opts, "destroy", environment, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer closeReport()

			reg, regDiags := stateOnlyRegistry(opts.Dir)
			if regDiags.HasErrors() {
				regDiags.Render(cmd.ErrOrStderr())
				return errProviderInstances
			}
			backend := backendFor(opts.Dir)

			st0, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}
			// An empty desired configuration: every resource recorded in
			// state falls into planner.Compute's "in state, not in
			// config" row, which is already Destroy/Forget/prevent_destroy
			// (spec §11) — see this command's doc comment above.
			emptyCfg := compiler.ResolvedConfig{Project: st0.Project, Environment: environment}

			// Unlocked preview — identical in spirit to `infra plan`.
			p, _, err := computePlan(cmd.Context(), cmd, backend, reg, emptyCfg, environment, opts, rw)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			fmt.Fprint(cmd.OutOrStdout(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose}))

			if !p.HasChanges() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
			}

			if !opts.AutoApprove {
				prompt := fmt.Sprintf("\nDestroying environment %q will delete every resource infra "+
					"manages there. This cannot be undone.\nType the environment name to confirm: ", environment)
				if !confirm(cmd, prompt, environment) {
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
				p2, st, err := computePlan(ctx, cmd, backend, reg, emptyCfg, environment, opts, rw)
				if err != nil {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
				}
				if !p2.HasChanges() {
					fmt.Fprintln(cmd.OutOrStdout(), "\nNothing remained to destroy once the environment lock was acquired.")
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
						fmt.Errorf("building execution graph: %w", err))
				}

				execOpts := executorOptions(opts, reg, backend, environment)
				if rw != nil {
					execOpts.OnEvent = func(e executor.Event) {
						// Best-effort and silent on failure — see apply.go's
						// identical wiring for why.
						_ = rw.WriteEvent(toReportEvent(e))
					}
				}

				res, execDiags := executor.Apply(ctx, p2, g, st, execOpts)
				renderDiagnostics(cmd.ErrOrStderr(), rw, execDiags)

				// executor.Render (Task 12) is THE result renderer — see
				// apply.go's note on why this command does not write its own.
				fmt.Fprint(cmd.OutOrStdout(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				result := applyResultFrom(res)
				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return finishApply(cmd.ErrOrStderr(), rw, result, errors.New("destroy completed with failures"))
				}
				return finishApply(cmd.ErrOrStderr(), rw, result, errChanges)
			})
		},
	}
}
