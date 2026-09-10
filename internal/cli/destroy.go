package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"infra/internal/compiler"
	"infra/internal/executor"
	"infra/internal/planner"
	"infra/internal/state"
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
// This also means --var/--var-file are simply inapplicable here — there is
// no configuration for a variable to interpolate into — not a broken
// promise the way an unwired flag would be.
func newDestroyCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "destroy <environment>",
		Short:         "Destroy every resource infra manages in an environment",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			reg := buildRegistry(opts.Dir)
			backend := backendFor(opts.Dir)

			st0, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return err
			}
			// An empty desired configuration: every resource recorded in
			// state falls into planner.Compute's "in state, not in
			// config" row, which is already Destroy/Forget/prevent_destroy
			// (spec §11) — see this command's doc comment above.
			emptyCfg := compiler.ResolvedConfig{Project: st0.Project, Environment: environment}

			// Unlocked preview — identical in spirit to `infra plan`.
			p, _, err := computePlan(cmd.Context(), cmd, backend, reg, emptyCfg, environment, opts)
			if err != nil {
				return err
			}

			fmt.Fprint(cmd.OutOrStdout(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose}))

			if !p.HasChanges() {
				return nil
			}

			if !opts.AutoApprove {
				prompt := fmt.Sprintf("\nDestroying environment %q will delete every resource infra "+
					"manages there. This cannot be undone.\nType the environment name to confirm: ", environment)
				if !confirm(cmd, prompt, environment) {
					return fmt.Errorf("destroy cancelled: you must type %q to confirm", environment)
				}
			}

			// Same seam as apply (Task 13): Task 11's runInterruptible owns
			// SIGINT, and Task 12's executor.Render owns the result summary.
			return runInterruptible(environment, func(ctx context.Context) error {
				ctx = state.WithOperation(ctx, "destroy")
				if _, err := backend.Lock(ctx, environment); err != nil {
					// Lock's own error already names the holder (spec §9.2) —
					// nothing to add.
					return err
				}
				defer releaseLock(backend, environment, cmd.ErrOrStderr())

				// Re-plan inside the lock — apply's doc comment explains why:
				// destroy must never execute against state or provider
				// reality gathered before the lock was held.
				p2, st, err := computePlan(ctx, cmd, backend, reg, emptyCfg, environment, opts)
				if err != nil {
					return err
				}
				if !p2.HasChanges() {
					fmt.Fprintln(cmd.OutOrStdout(), "\nNothing remained to destroy once the environment lock was acquired.")
					return nil
				}

				g, err := planner.BuildExecution(p2, dependentsOf(p2))
				if err != nil {
					return fmt.Errorf("building execution graph: %w", err)
				}

				res, execDiags := executor.Apply(ctx, p2, g, st, executor.Options{
					Parallelism: opts.Parallelism,
					PerProvider: opts.Parallelism,
					Registry:    reg,
					Backend:     backend,
					Environment: environment,
					Retry:       defaultRetryPolicy(),
					Now:         time.Now,
				})
				execDiags.Render(cmd.ErrOrStderr())

				// executor.Render (Task 12) is THE result renderer — see
				// apply.go's note on why this command does not write its own.
				fmt.Fprint(cmd.OutOrStdout(), executor.Render(res, executor.RenderOptions{Verbose: opts.Verbose}))

				if execDiags.HasErrors() || len(res.Failed) > 0 {
					return errors.New("destroy completed with failures")
				}
				return errChanges
			})
		},
	}
}
