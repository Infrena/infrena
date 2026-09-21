package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/executor"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/report"
)

// newDestroyCommand builds `infrena destroy <environment>`: plan the removal of
// everything infrena manages in the environment, confirm, execute.
//
// destroy writes no second "delete everything" code path. It hands computePlan an
// empty compiler.ResolvedConfig — no resources at all — and every resource recorded
// in state then falls into planner.Compute's own "in state, not in config" row, which
// is already Destroy, or Forget under retain, or a plan-time refusal under
// prevent_destroy. So destroy never calls config.Load or compiler.Compile: its
// project name comes from state, not from infrena.yml.
//
// It does take --var and --var-file, unlike the resources it is deleting: an
// instance's own configuration interpolates variables, and destroy has to construct
// that instance to dispatch a delete to the right account. Without them a project
// whose provider defaults depend on a variable would be destroyable by nothing.
func newDestroyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "destroy <environment>",
		Short:         "Destroy every resource infrena manages in an environment",
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
			backend, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}
			defer closeBackend()

			st0, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}
			// State names the plugins here, not configuration: a destroy's premise
			// is that nothing is configured.
			if stateDiags := ensureStateProviders(reg, st0, nil); stateDiags.HasErrors() {
				stateDiags.Render(cmd.ErrOrStderr())
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errProviderInstances)
			}
			// An empty desired configuration — see this command's doc comment.
			// The protections are carried onto it, which is what makes
			// `prevent_destroy` mean anything here: a protected environment
			// refuses each resource at plan time, before the confirmation
			// prompt rather than after it.
			protections := environmentProtections(opts, environment)
			emptyCfg := compiler.ResolvedConfig{
				Project: st0.Project, Environment: environment, Protections: protections,
			}

			// Unlocked preview, as `infrena plan` is.
			p, _, _, err := computePlan(cmd.Context(), cmd, backend, reg, emptyCfg, environment, opts, ro)
			if err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose, Definition: reg.Definition}))
			reportPlan(ro, p, report.StageProposed)

			if !p.HasChanges() {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, nil)
			}

			// `require_approval` gates destroy exactly as it gates apply, and
			// with more reason: destroy is the more dangerous of the two. There
			// is no `--plan` escape here — destroy takes no saved plan — so on a
			// protected environment a person must type the environment name.
			if err := requireApprovalRefusal(opts, protections, environment, false); err != nil {
				return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
			}

			if !opts.AutoApprove {
				// The identical rule apply applies, and for the identical
				// reason: a higher confirmation bar does not make the answer
				// obtainable. This runs before withLockedEnvironment, so a
				// destroy nobody can approve leaves the environment untouched
				// and unlocked — and so does the end-of-input case confirm
				// reports below, which is refused on the same terms.
				if approvalUnobtainable(opts) {
					err := errNoApproval
					if protections.RequireApproval {
						err = &noApprovalError{msg: requireApprovalMessage(protections, environment, false)}
					}
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, err)
				}

				prompt := fmt.Sprintf("\nDestroying environment %q will delete every resource infrena "+
					"manages there. This cannot be undone.\nType the environment name to confirm: ", environment)
				switch confirm(cmd.InOrStdin(), ro.Out(), prompt, environment) {
				case approvalNoInput:
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{}, errNoApproval)
				case approvalDeclined:
					return finishApply(cmd.ErrOrStderr(), rw, report.ApplyResult{},
						fmt.Errorf("destroy cancelled: you must type %q to confirm", environment))
				}
			}

			// The same seam as apply: runInterruptible owns SIGINT, and
			// executor.Render owns the result summary.
			return withLockedEnvironment(environment, "destroy", backend, cmd.ErrOrStderr(), func(ctx context.Context) error {
				// Re-plan inside the lock: destroy must never execute against
				// state or provider reality gathered before the lock was held.
				p2, st, obs, err := computePlan(ctx, cmd, backend, reg, emptyCfg, environment, opts, ro)
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
				// See apply.go's identical wiring, and executor.currentFor for what
				// a destroy does with them: Delete receives `current` too, so it
				// wants the same observed-now view an update does.
				execOpts.Observed = obs.States()
				// See apply.go's identical wiring: always set, because the
				// same hook is what renders progress to stdout.
				execOpts.OnEvent = eventHook(ro)

				reportPlan(ro, p2, report.StageExecuting)
				res, execDiags := executor.Apply(ctx, p2, g, st, execOpts)
				renderDiagnostics(cmd.ErrOrStderr(), rw, execDiags)

				// executor.Render is the one result renderer — see apply.go's
				// note on why this command does not write its own.
				fmt.Fprint(ro.Out(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				result := applyResultFrom(res)
				// After Apply, so it is the serial the run produced rather than
				// the one it started from. The executor writes state under
				// context.WithoutCancel, so this is set even for a run that was
				// interrupted after its last Put — the run an audit most wants
				// to be able to place.
				result.StateSerial = st.Serial
				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return finishApply(cmd.ErrOrStderr(), rw, result, errors.New("destroy completed with failures"))
				}
				return finishApply(cmd.ErrOrStderr(), rw, result, errChanges)
			})
		},
	}
}
