package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/refresh"
)

// errChanges signals that a plan completed successfully but found changes to
// propose. It carries exit code 2 (spec §16) and must never be reported to
// the user as a failure — Execute checks for it with errors.Is before the
// generic error path runs.
var errChanges = errors.New("plan has changes")

// newPlanCommand builds `infra plan <environment>`. It never writes state and
// never takes the environment lock (spec §10), so it is safe to run
// repeatedly, in CI, and against an environment another command holds
// locked.
func newPlanCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "plan <environment>",
		Short: "Show what infra would change without applying it",
		Args:  cobra.ExactArgs(1),
		// SilenceUsage/SilenceErrors are also set on root, which is enough
		// in production: cobra's ExecuteC always resolves to the root
		// command's flags when Execute is called through the root. But
		// tests build this command standalone and call cmd.Execute()
		// directly on it with no parent — cobra then treats this command as
		// its own root for those checks, so without setting them here too,
		// cobra prints "Usage: ..." to stdout on every RunE error,
		// including the errChanges success-with-changes path. That would
		// leak boilerplate onto the stream the plan itself is written to.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			environment := args[0]

			// --output on plan is still the plan artifact written at the
			// end of this function rather than a report stream, so this run
			// has no report writer to open — but spec 2.1's rule, that
			// --output leaves stdout untouched, is the same one every other
			// command follows. See silentRun.
			ro := silentRun()
			if opts.Output == "" {
				var (
					closeRun func()
					err      error
				)
				if ro, closeRun, err = openRun(cmd, opts, "plan", environment); err != nil {
					return err
				}
				defer closeRun()
			}

			copts, cds := compilerOptions(opts, environment)
			if cds.HasErrors() {
				cds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}

			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}

			reg, closePlugins := buildRegistry(opts)
			defer closePlugins()

			// State is read BEFORE compiling, because §6.1's rule needs it: an
			// environment is reachable if it is declared OR it has state.
			st, err := backendFor(opts.Dir).Get(cmd.Context(), environment)
			if err != nil {
				return err
			}

			// STATE names plugins too, and for the case invariant 1 is about: a
			// resource removed from configuration is still in state, and the plugin
			// that manages it has to be loaded for the destroy to be planned at all.
			ds := cds
			ds.Extend(ensureStateProviders(reg, st))

			var cfg compiler.ResolvedConfig
			switch disp, declared := dispositionOf(files, environment, st); disp {
			case unknownEnvironment:
				return unknownEnvironmentError(environment, declared)
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
				cfg = teardownConfig(st, environment)
				fmt.Fprint(ro.Out(), teardownNotice(environment, declared))
			default:
				var compileDiags diag.Diagnostics
				cfg, compileDiags = compiler.Compile(files, reg, copts)
				ds.Extend(compileDiags)
			}
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				return errors.New("configuration is not valid")
			}

			// The refresh hook, which used to be nil here: reading every
			// resource's provider state is the phase that dominates the wait
			// on a real account, and a plan that says nothing while it runs
			// is indistinguishable from one that has hung.
			obs, refreshDiags := refresh.Refresh(cmd.Context(), st, reg, opts.Parallelism, perProviderParallelism,
				observationHook(ro, st))
			ds.Extend(refreshDiags)
			if ds.HasErrors() {
				ds.Render(cmd.ErrOrStderr())
				return errors.New("refreshing provider state failed")
			}

			p, planDiags := planner.Compute(cfg, st, obs, planner.Options{
				Environment: environment,
				Now:         time.Now,
				// Registry gives the diff the schemas it needs to tell a
				// ForceNew attribute from an updatable one and a computed
				// attribute from desired state (spec §11) — reuse the same
				// registry Compile already built, rather than constructing
				// a second one.
				Registry: reg,
			})
			// planDiags already carries every plan-time diagnostic,
			// including operation-level refusals such as prevent_destroy
			// (spec §11): Compute accumulates each resource's diagnostics
			// into the same value it returns here before copying that value,
			// unchanged, into p.Diagnostics for the saved plan artifact.
			// p.Diagnostics and planDiags are therefore always identical in
			// content — confirmed by reading planner.Compute, where every
			// return path sets p.Diagnostics from a copy of the same ds it
			// is about to return, and by internal/planner's own
			// TestPreventDestroyIsAPlanTimeError, which asserts against
			// Compute's returned diagnostics directly, never against
			// p.Diagnostics. Extending ds with p.Diagnostics a second time
			// would not catch anything planDiags missed; it would print
			// every plan-time diagnostic twice. p.Diagnostics still matters
			// — it is what --output writes into the saved plan artifact —
			// it just is not a second channel this command needs to gate on.
			ds.Extend(planDiags)
			ds.Render(cmd.ErrOrStderr())
			if ds.HasErrors() {
				return errors.New("planning failed")
			}

			fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{
				Verbose:    opts.Verbose,
				Definition: reg.Definition,
			}))

			if opts.Output != "" {
				// The full artifact, CreatedAt included — Canonical() exists
				// to define determinism over the plan's inputs (spec §12.1)
				// and deliberately excludes it; a saved plan is a record of
				// what this run produced. It contains sensitive values, so
				// 0600 (spec §12.2).
				data, err := json.MarshalIndent(p, "", "  ")
				if err != nil {
					return fmt.Errorf("serializing plan: %w", err)
				}
				if err := os.WriteFile(opts.Output, data, 0o600); err != nil {
					return fmt.Errorf("writing plan: %w", err)
				}
			}

			if p.HasChanges() {
				return errChanges
			}
			return nil
		},
	}
}

// parseVars turns --var name=value flags into the map the compiler's
// variable scope consumes. The full variable system — typed schemas,
// --var-file, precedence — is M4; M2 only makes the raw strings available.
func parseVars(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, v := range raw {
		name, val, ok := strings.Cut(v, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--var %q must be in the form name=value", v)
		}
		out[name] = val
	}
	return out, nil
}
