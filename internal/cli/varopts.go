package cli

import (
	"fmt"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/diag"
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
	return compiler.Options{Dir: opts.Dir, Environment: environment, Vars: vars, FileVars: fileVars}, ds
}

// rejectVariableFlags refuses --var and --var-file for the commands that never
// compile configuration.
//
// destroy synthesises an empty ResolvedConfig from state and refresh reads
// state and calls Provider.Read; neither calls config.Load or compiler.Compile,
// so a variable has nothing to interpolate into and cannot change the outcome.
// Accepting a flag that cannot change the outcome is exactly what
// checkUnsupportedFlags refuses for an unwired flag, and the reasoning does not
// change because the flag works elsewhere.
func rejectVariableFlags(opts *GlobalOptions, command string) error {
	if len(opts.Vars) == 0 && len(opts.VarFiles) == 0 {
		return nil
	}
	return fmt.Errorf("%s does not take --var or --var-file: it works from recorded state rather "+
		"than from configuration, so a variable has nothing to interpolate into. "+
		"Use `infra plan <environment>` to see what configuration would change", command)
}
