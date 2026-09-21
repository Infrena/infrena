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
// propose. It carries exit code ExitChanges and must never be reported to the user
// as a failure — Execute checks for it with errors.Is before the generic error path
// runs.
var errChanges = errors.New("plan has changes")

// newPlanCommand builds `infrena plan <environment>`. It never writes state and
// never takes the environment lock, so it is safe to run repeatedly, in CI, and
// against an environment another command holds locked.
func newPlanCommand(opts *GlobalOptions) *cobra.Command {
	var show string

	cmd := &cobra.Command{
		Use:   "plan <environment>",
		Short: "Show what infrena would change without applying it",
		// MaximumNArgs rather than ExactArgs, because --show takes no
		// environment: a saved plan already names the one it was made for, and
		// asking for it again invites the two to disagree.
		Args: cobra.MaximumNArgs(1),
		// Set here as well as on root, because cobra consults a command's own
		// fields when it is executed with no parent, which is how the tests
		// build it. Without these it prints "Usage: ..." to stdout on every
		// RunE error, including the errChanges success-with-changes path,
		// leaking boilerplate onto the stream the plan is written to.
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

			// State is read before compiling, because the reachability rule
			// needs it: an environment is reachable if it is declared or it
			// has state.
			st, err := backend.Get(cmd.Context(), environment)
			if err != nil {
				return err
			}

			// State names plugins too: a resource removed from configuration is
			// still in state, and the plugin that manages it has to be loaded
			// for its destroy to be planned at all.
			ds := cds
			ds.Extend(ensureStateProviders(reg, st, files))

			var cfg compiler.ResolvedConfig
			switch disp, declared := dispositionOf(files, environment, st); disp {
			case unknownEnvironment:
				return unknownEnvironmentError(environment, declared)
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

			// Reading every resource's provider state dominates the wait on a
			// real account, and a plan that says nothing while it runs is
			// indistinguishable from one that has hung.
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
				// ForceNew attribute from an updatable one, and a computed
				// attribute from desired state. The same registry Compile
				// already built, never a second one.
				Registry: reg,
			})
			// planDiags already carries every plan-time diagnostic, including
			// operation-level refusals such as prevent_destroy: Compute returns
			// the same value it copies into p.Diagnostics for the artifact, so
			// extending ds with p.Diagnostics as well would print each plan-time
			// diagnostic twice rather than catch anything more. p.Diagnostics is
			// what --output writes; it is not a second channel to gate on.
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
				// The full artifact, CreatedAt included: Canonical() defines
				// determinism over the plan's inputs and deliberately excludes
				// it, but a saved plan is a record of what this run produced.
				//
				// It travels on a `plan` line inside the report stream rather
				// than as a bare document, so a frontend tails one format for
				// every command. The file holds cleartext values and is 0600 —
				// openReport opens it that way, which is why nothing here sets
				// a mode.
				data, err := json.Marshal(p)
				if err != nil {
					return fmt.Errorf("serializing plan: %w", err)
				}
				// Reported, not swallowed: unlike the best-effort hooks that
				// run on worker goroutines, this is the product of the run,
				// and a plan file with no plan line in it is worse than a
				// plan command that failed outright.
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
// It exists for the reviewer. `require_approval` makes a saved plan the way CI
// applies to a protected environment, which only works if somebody reads the plan
// first — and asking that person to invoke the apply command and answer "no" in
// order to decide whether to apply is a bad thing to ask of the one person the
// protection depends on.
//
// It loads nothing: no providers, no plugins, no state, no lock. The artifact is
// complete by construction, and planner.RenderOptions.Definition is optional, so a
// reviewer with nothing installed can read the plan they are being asked to approve.
// Requiring the plugins would put the review behind the same setup the pipeline has.
//
// The exit code matches `plan`'s: changes present, or not. A reviewer's eye and a
// pipeline's `$?` get the same answer.
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

// parseVars turns --var name=value flags into the map the compiler's variable scope
// consumes.
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
