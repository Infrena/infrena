package cli

import (
	"encoding/json"
	"errors"
	"fmt"
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
	var show string

	cmd := &cobra.Command{
		Use:   "plan <environment>",
		Short: "Show what infra would change without applying it",
		// MaximumNArgs rather than ExactArgs, because --show takes no
		// environment: a saved plan already names the one it was made for, and
		// asking for it again invites the two to disagree.
		Args: cobra.MaximumNArgs(1),
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
			if show != "" {
				if len(args) > 0 {
					return fmt.Errorf("plan --show reads a saved plan and takes no environment: "+
						"the plan already names the one it was made for (%q was given)", args[0])
				}
				return showSavedPlan(cmd, opts, show)
			}
			if len(args) != 1 {
				return errors.New("plan needs an environment: `infrena plan <environment>`, " +
					"or `infrena plan --show <file>` to read a saved one")
			}
			environment := args[0]

			ro, closeRun, err := openRun(cmd, opts, "plan", environment)
			if err != nil {
				return err
			}
			defer closeRun()

			copts, cds := compilerOptions(opts, environment)
			if cds.HasErrors() {
				cds.Render(cmd.ErrOrStderr())
				return errNotValid
			}

			files, err := config.Load(opts.Dir)
			if err != nil {
				return err
			}

			reg, loader := buildRegistryWithLoader(opts)
			defer loader.Close()

			backend, closeBackend, err := backendFor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer closeBackend()

			// State is read BEFORE compiling, because §6.1's rule needs it: an
			// environment is reachable if it is declared OR it has state.
			st, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return err
			}

			// STATE names plugins too, and for the case invariant 1 is about: a
			// resource removed from configuration is still in state, and the plugin
			// that manages it has to be loaded for the destroy to be planned at all.
			ds := cds
			ds.Extend(ensureStateProviders(reg, st, files))

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
				_, stateInstanceDiags := registerStateInstances(reg, opts, "", false)
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
				return configurationIsNotValid(cmd, opts, ro.Out(), loader)
			}

			// The refresh hook, which used to be nil here: reading every
			// resource's provider state is the phase that dominates the wait
			// on a real account, and a plan that says nothing while it runs
			// is indistinguishable from one that has hung.
			obs, refreshDiags := refresh.Refresh(cmd.Context(), st, reg, opts.Parallelism, perProviderParallelism, readRetryPolicy(),
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

			if rw := ro.Report(); rw != nil {
				// The full artifact, CreatedAt included — Canonical() exists
				// to define determinism over the plan's inputs (spec §12.1)
				// and deliberately excludes it; a saved plan is a record of
				// what this run produced.
				//
				// It travels on a `plan` line inside the report stream rather
				// than as a bare document, so a frontend tails ONE format for
				// every command (spec 2.4). The file still contains cleartext
				// values and is still 0600 (spec §12.2) — openReport opens it
				// that way, which is why nothing here sets a mode.
				data, err := json.Marshal(p)
				if err != nil {
					return fmt.Errorf("serializing plan: %w", err)
				}
				// Reported, not swallowed: unlike the best-effort hooks that
				// run on worker goroutines, this is the product of the run,
				// and a plan file with no plan line in it is worse than a
				// plan command that failed.
				if err := rw.WritePlan(data); err != nil {
					return fmt.Errorf("writing plan: %w", err)
				}
			}

			if p.HasChanges() {
				return errChanges
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&show, "show", "",
		"read a saved plan and render it, without touching state or any provider")
	return cmd
}

// showSavedPlan renders a plan artifact and stops.
//
// IT EXISTS FOR THE REVIEWER. §38's `require_approval` makes a saved plan the
// way CI applies to a protected environment, which only works if somebody reads
// the plan first — and until this, the only ways to read one were to run
// `apply --plan` and answer "no", or to read NDJSON by hand. Asking a reviewer
// to invoke the apply command in order to decide whether to apply is a bad
// thing to ask of the one person the protection depends on.
//
// IT LOADS NOTHING. No providers, no plugins, no state, no lock — the artifact
// is complete by construction (that is why `apply --plan` does not recompile),
// and planner.RenderOptions.Definition is optional. So a reviewer with nothing
// installed, on a laptop that has never seen this project, can read the plan
// they are being asked to approve. Requiring the plugins would have put the
// review behind the same setup the pipeline has, which defeats the point of
// reviewing somewhere else.
//
// The exit code matches `plan`'s: 2 when the plan has changes, 0 when it does
// not. A reviewer's eye and a pipeline's `$?` get the same answer, and nothing
// has to learn a third convention.
func showSavedPlan(cmd *cobra.Command, opts *GlobalOptions, path string) error {
	ro, closeRun, err := openRun(cmd, opts, "plan", "")
	if err != nil {
		return err
	}
	defer closeRun()

	p, err := readSavedPlan(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	fmt.Fprint(ro.Out(), planner.Render(p, planner.RenderOptions{Verbose: opts.Verbose}))
	if p.HasChanges() {
		return errChanges
	}
	return nil
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
