package cli

import (
	"context"
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

// newValidateCommand builds `infrena validate [environment]`.
//
// The environment argument is optional, and omitting it validates every declared
// environment. Typed variable validation happens during validate, and a variable
// whose value exists only in one environment's file cannot be checked without
// resolving that environment — while validating with no environment at all would
// report "required variable not set" for a variable every environment does set,
// which is validate failing on valid configuration.
//
// Exit 0 valid, exit 1 invalid. There is no exit 2 here: that code means "success
// with changes present", and validate never computes changes.
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

			// validate never contacts a provider, so it has no per-resource
			// progress: its stream is meta, then diagnostics, then the result
			// line — the shape apply and refresh open with, minus the event
			// and observation lines it has nothing to put in.
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
// Once per run, not once per environment: where state lives is a property of the
// project, so running it inside the per-environment loop would say the same thing
// once per declared environment. foldByEnvironment would collapse the duplicates, but
// relying on that to hide a check that should not have run repeatedly is the wrong
// way round.
//
// It resolves and stops there — no process is started and nothing is contacted, which
// is what keeps it inside validate's promise.
//
// It exists because validate is the cheap gate a CI pipeline runs: without it a
// project whose backend is not installed reported "Configuration valid" here and
// failed at `plan`, which is a pipeline approving a project that cannot run.
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
			// The CONTENTS check below needs the binary Verify just failed to
			// find, so asking would only restate this.
			continue
		}

		// The block's contents, which Verify cannot judge because only the
		// backend understands its own settings. Protocol 2's `validate` asks it,
		// under a contract that it contacts nothing, so this stays inside
		// validate's promise — and catches at the CI gate what would otherwise
		// be refused only at `plan`.
		//
		// Silent for a backend that cannot answer — protocol 1, or protocol 2
		// without the optional method — so nothing that worked yesterday starts
		// failing today.
		if err := backendhost.ValidateConfig(
			context.Background(), b.decl.Plugin, opts.Dir, dirs, b.decl.Config,
		); err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "`" + b.key + "` is not a block the " + b.decl.Plugin + " backend can read",
				Detail:   err.Error(),
				Origin:   b.decl.Origin,
			})
		}
	}
	return ds
}

// environmentsToValidate decides which environments one `infrena validate` run
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
		// A project declaring no environments validates under the empty one.
		return []string{""}, ds
	}
	return names, ds
}

// environmentNames lists declared environments in sorted order.
//
// config.Decode already returns ProjectDecl.Environments sorted by name, so the sort
// is defensive rather than load-bearing today. It is kept because it costs one call
// and the alternative, if that guarantee ever changes, is a diagnostic list that
// reorders itself between runs of the same command.
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
// A diagnostic produced identically in every validated environment is
// environment-independent — an unknown resource type does not become known in
// staging — so it is reported once, untagged. Anything else is tagged with the
// environment it came from. Nothing is deduplicated across a subset of environments:
// an error that occurs in production and not in dev is precisely the error a user
// needs attributed.
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
// It takes the whole compiler.Options rather than just the --var map, so that a
// command cannot forget to pass a variable input it does not know about — the shape
// that made validate report an undefined variable for configuration `plan` accepted
// with the same flags.
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
