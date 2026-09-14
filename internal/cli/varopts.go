package cli

import (
	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/version"
)

// compilerOptions builds the compiler options for a command that compiles
// configuration.
//
// It exists so validate, plan and apply cannot resolve variables three
// slightly different ways. They already did once: validate omitted --var
// entirely and rejected configuration plan accepted (see validate.go's doc
// comment). M4 adds --var-file, which is a second chance to make the same
// mistake in two places out of three.
//
// Diagnostics are returned rather than errors because a bad variable file is a
// configuration problem, and spec §7.4 wants every one of them reported in one
// pass rather than the first one aborting the run.
func compilerOptions(opts *GlobalOptions, environment string) (compiler.Options, diag.Diagnostics) {
	var ds diag.Diagnostics

	vars, err := parseVars(opts.Vars)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  err.Error(),
			Action:   "Pass variables as --var name=value.",
		})
	}
	fileVars, fds := loadVarFiles(opts.Dir, opts.VarFiles)
	ds.Extend(fds)

	// Dir travels with the options because stage 5 resolves a module's relative
	// `source:` against the project directory. Passing anything but opts.Dir
	// here makes --chdir silently wrong for modules and right for everything
	// else, which is the worst combination to debug.
	return compiler.Options{
		Dir: opts.Dir,
		// The running build, for a project's `infrena:` floor. The compiler takes it
		// as an input rather than reading it, so this is the one place it is supplied.
		Version:     version.Version(),
		Environment: environment,
		Vars:        vars,
		FileVars:    fileVars,
	}, ds
}
