package cli

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/backendhost"
	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/pluginhost"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/report"
)

// newValidateCommand builds `infra validate [environment]`.
//
// The environment argument is optional. Spec §16 lists validate without one and
// today it is NoArgs, so requiring one would break every existing invocation.
// But PLAN.md §9 requires typed variable validation to happen during validate,
// and a variable whose value exists only in environments/production.yml cannot
// be checked without resolving that environment — while validating with NO
// environment would report "required variable not set" for a variable every
// environment does set, which is validate failing on valid configuration.
//
// So: no argument validates every declared environment, which is spec §7.4's
// "report every problem in one pass" applied to environments. Exit 0 valid,
// exit 1 invalid; there is no exit 2 here, because §16's exit 2 means "success
// with changes present" and validate never computes changes.
func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate [environment]",
		Short: "Check configuration for errors without contacting providers",
		Args:  cobra.MaximumNArgs(1),
		// See newPlanCommand for why these are set per-command as well as on
		// the root: cobra consults this command's own fields when it has no
		// parent, which is how every test constructs it.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var environment string
			if len(args) == 1 {
				environment = args[0]
			}

			// validate has no per-resource progress to report (it never
			// contacts a provider), so its NDJSON stream is exactly meta,
			// then diagnostics, then this command's own "result" line — the
			// same three-part shape apply and refresh open with, minus the
			// event/observation lines neither has anything to report.
			ro, closeRun, err := openRun(cmd, opts, "validate", environment)
			if err != nil {
				return err
			}
			defer closeRun()
			rw := ro.Report()

			envs, ds := environmentsToValidate(opts.Dir, args)
			ds.Extend(validateBackends(opts))
			// Declared out here so the failure path below can ask it what was
			// missing. It stays nil when the environments themselves did not
			// resolve, and configurationIsNotValid handles that.
			var loader *pluginhost.Loader
			if !ds.HasErrors() {
				reg, l := buildRegistryWithLoader(opts)
				loader = l
				defer loader.Close()
				perEnv := make([]diag.Diagnostics, len(envs))
				for i, env := range envs {
					copts, cds := compilerOptions(opts, env)
					if !cds.HasErrors() {
						cds.Extend(validateProject(opts.Dir, reg, copts))
					}
					perEnv[i] = cds
				}
				ds.Extend(foldByEnvironment(envs, perEnv))
			}
			renderDiagnostics(cmd.ErrOrStderr(), rw, ds)

			valid := !ds.HasErrors()
			if rw != nil {
				if werr := rw.WriteValidateResult(report.ValidateResult{Valid: valid}); werr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to write --output result: %v\n", werr)
				}
			}

			if !valid {
				return configurationIsNotValid(cmd, opts, ro.Out(), loader)
			}
			fmt.Fprintln(ro.Out(), "✓ Configuration valid")
			return nil
		},
	}
}

// validateBackends checks that a `backend:` (and a `migrate_from:`) naming a
// plugin can actually be resolved to a binary on disk.
//
// ONCE PER RUN, NOT ONCE PER ENVIRONMENT. Where state lives is a property of
// the project, so running it inside the per-environment loop would say the same
// thing three times for a project declaring three environments. foldByEnvironment
// would collapse the duplicates, but relying on that to hide a check that should
// not have run three times is the wrong way round.
//
// It resolves and stops there — no process is started and nothing is contacted,
// which is what keeps it inside validate's promise. See backendhost.Verify for
// what that leaves unchecked: the block's own CONTENTS are the plugin's to
// validate, and the plugin only gets to do that when it runs.
//
// Why this exists at all: validate already refuses a missing PROVIDER plugin,
// and it used to report "✓ Configuration valid" for a project whose backend was
// not installed. `plan` then failed on the same project. validate is the cheap
// gate a CI pipeline runs, so that combination is a pipeline approving a project
// that cannot run.
func validateBackends(opts *GlobalOptions) diag.Diagnostics {
	var ds diag.Diagnostics

	files, err := config.Load(opts.Dir)
	if err != nil {
		// Reported by the per-environment pass, which loads the same files.
		// Saying it twice from two places is the same problem told twice.
		return ds
	}
	project, dds := config.Decode(files)
	if dds.HasErrors() {
		// A block that did not decode has already been reported as such, and
		// a plugin name read out of a broken block is not one to go looking
		// for on disk.
		return ds
	}

	dirs := backendhost.Search(opts.Dir, opts.PluginDirs)
	for _, b := range []struct {
		key  string
		decl config.BackendDecl
	}{
		{"backend", project.Backend},
		{"migrate_from", project.MigrateFrom},
	} {
		if b.decl.Plugin == "" || b.decl.Plugin == localBackendName {
			continue
		}
		if err := backendhost.Verify(b.decl.Plugin, opts.Dir, dirs); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + b.key + "` names state backend " + strconv.Quote(b.decl.Plugin) + ", which is not installed",
				Detail:   err.Error(),
				Origin:   b.decl.Origin,
			})
		}
	}
	return ds
}

// environmentsToValidate decides which environments one `infra validate` run
// covers: the named one, or every declared one, or the empty environment for a
// project that declares none.
func environmentsToValidate(dir string, args []string) ([]string, diag.Diagnostics) {
	var ds diag.Diagnostics
	if len(args) == 1 {
		return []string{args[0]}, ds
	}

	files, err := config.Load(dir)
	if err != nil {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return nil, ds
	}
	project, dds := config.Decode(files)
	ds.Extend(dds)
	if dds.HasErrors() {
		// The per-environment pass would report the same syntax errors once
		// per environment; returning here reports them once.
		return nil, ds
	}

	names := environmentNames(project)
	if len(names) == 0 {
		// A project with no environments validates exactly as it did in M3.
		return []string{""}, ds
	}
	return names, ds
}

// environmentNames lists declared environments in sorted order.
//
// config.Decode already returns ProjectDecl.Environments sorted by Name (see
// internal/config/decode.go), so the sort.Strings below is defensive rather
// than load-bearing for the current []EnvironmentDecl shape — it only earns
// its keep if that upstream guarantee ever changes or the shape becomes a
// map. It is kept anyway: an unsorted diagnostic list would be a list that
// reorders itself between runs of the same command, which is the invariant
// 6 failure mode, and the cost of asserting it twice is one function call.
func environmentNames(p *config.ProjectDecl) []string {
	out := make([]string, 0, len(p.Environments))
	for _, e := range p.Environments {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// foldByEnvironment merges per-environment diagnostics into one list.
//
// A diagnostic produced identically in EVERY validated environment is
// environment-independent — an unknown resource type does not become known in
// staging — so it is reported once, untagged. Anything else is tagged with the
// environment it came from, per spec §44: an error says which environment it is
// about. Nothing is deduplicated across a subset of environments; an error that
// occurs in production and not in dev is precisely the error a user needs
// attributed.
//
// Order comes from the slices, never from the maps: the maps are consulted by
// key and never ranged over, so output is byte-stable across runs.
func foldByEnvironment(envs []string, perEnv []diag.Diagnostics) diag.Diagnostics {
	type key struct {
		severity diag.Severity
		summary  string
		detail   string
		action   string
		origin   string
	}
	keyOf := func(d diag.Diagnostic) key {
		return key{d.Severity, d.Summary, d.Detail, d.Action, d.Origin.String()}
	}

	counts := map[key]int{}
	for _, ds := range perEnv {
		seen := map[key]bool{}
		for _, d := range ds {
			k := keyOf(d)
			if !seen[k] {
				counts[k]++
				seen[k] = true
			}
		}
	}

	var out diag.Diagnostics
	emitted := map[key]bool{}
	for i, ds := range perEnv {
		for _, d := range ds {
			k := keyOf(d)
			if counts[k] == len(perEnv) {
				if emitted[k] {
					continue
				}
				emitted[k] = true
				out.Add(d)
				continue
			}
			if envs[i] != "" {
				d.Summary = "environment " + strconv.Quote(envs[i]) + ": " + d.Summary
			}
			out.Add(d)
		}
	}
	return out
}

// validateProject runs the full compiler pipeline — stages 1 through 8 — for
// one environment and returns every diagnostic it produces.
//
// It takes the whole compiler.Options rather than just the --var map so that a
// command cannot forget to pass a variable input it does not know about. It
// used to take only vars, and the version before that took none at all, which
// is how `infra validate --var cidr=10.0.0.0/16` came to report
//
//	Error: undefined variable "cidr"
//
// for configuration `infra plan dev --var cidr=10.0.0.0/16` planned without
// complaint.
func validateProject(dir string, reg *registry.Registry, copts compiler.Options) diag.Diagnostics {
	files, err := config.Load(dir)
	if err != nil {
		var ds diag.Diagnostics
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: err.Error()})
		return ds
	}
	_, ds := compiler.Compile(files, reg, copts)
	return ds
}
