package cli

import (
	"maps"
	"os"
	"path/filepath"
	"strconv"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// loadVarFiles reads every --var-file in flag order and merges them into one
// layer.
//
// A later file overrides an earlier one for the same name, so
// `--var-file base.yml --var-file prod.yml` reads as a stack of overlays. Every
// entry is stamped ScopeCLIOverride, which outranks the project's variables.yml
// and its environment configuration. `--var` is applied after this layer, so it
// still wins.
//
// Errors collect rather than stopping at the first bad file, so one run reports
// every problem.
func loadVarFiles(dir string, paths []string) (map[string]value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := map[string]value.Value{}

	for _, p := range paths {
		full := p
		if !filepath.IsAbs(p) {
			// Relative to --chdir, not to the process working directory:
			// --chdir means "run as if infrena had been started in this
			// directory".
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
		// The path as written on the command line, not the joined one, so
		// diagnostics quote what the user typed. ParseVariableFile stores it
		// verbatim.
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
		maps.Copy(out, vals)
	}
	return out, ds
}
