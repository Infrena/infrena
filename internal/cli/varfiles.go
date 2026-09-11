package cli

import (
	"os"
	"path/filepath"
	"strconv"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/pkg/value"
)

// loadVarFiles reads every --var-file in flag order and merges them into one
// layer.
//
// A later file overrides an earlier one for the same name, so
// `--var-file base.yml --var-file prod.yml` reads as a stack of overlays. Every
// entry is stamped ScopeCLIOverride: a file named on the command line is a
// value the invoker chose for this run, which outranks the project's implicit
// variables.yml and its environment configuration (PLAN.md §7, §8). `--var` is
// applied after this layer, so it still wins.
//
// Errors collect rather than stopping at the first bad file, per spec §7.4:
// `infra validate` reports every problem in one pass.
func loadVarFiles(dir string, paths []string) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := map[string]value.Value{}

	for _, p := range paths {
		full := p
		if !filepath.IsAbs(p) {
			// Relative to --chdir, not to the process working directory:
			// --chdir means "run as if infra had been started in this
			// directory", and a flag that ignored it would resolve paths from
			// somewhere the user is not looking.
			full = filepath.Join(dir, p)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot read variable file " + strconv.Quote(p),
				Detail:   err.Error(),
				Action:   "Check the path. Relative paths resolve against " + strconv.Quote(dir) + ".",
				Origin:   value.Origin{File: p},
			})
			continue
		}
		// Path as WRITTEN on the command line, not the joined path: it is what
		// the user typed, and it keeps diagnostics free of the temporary
		// directory an integration test happens to run in. ParseVariableFile
		// stores it verbatim, unresolved, for exactly that reason.
		f, err := config.ParseVariableFile(p, data)
		if err != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot parse variable file " + strconv.Quote(p),
				Detail:   err.Error(),
				Action:   "Fix the YAML syntax.",
				Origin:   value.Origin{File: p},
			})
			continue
		}
		vals, fds := config.DecodeVariableFile(f, value.ScopeCLIOverride)
		ds.Extend(fds)
		for name, v := range vals {
			out[name] = v
		}
	}
	return out, ds
}
