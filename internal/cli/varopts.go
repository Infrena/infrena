package cli

import (
	"os"

	"github.com/infrena/infrena/internal/compiler"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/version"
	"github.com/infrena/infrena/pkg/value"
)

// compilerOptions builds the compiler options for a command that compiles
// configuration.
//
// It exists so validate, plan and apply cannot resolve variables three slightly
// different ways — a divergence that once had validate reject configuration
// plan accepted.
//
// Diagnostics are returned rather than errors because a bad variable file is a
// configuration problem, and one run should report every one of them rather
// than aborting on the first.
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

	secrets, secretsErr := secretSource(opts, environment)

	// Dir travels with the options because a module's relative `source:` is
	// resolved against the project directory. Passing anything but opts.Dir
	// here makes --chdir wrong for modules and right for everything else.
	return compiler.Options{
		Dir:        opts.Dir,
		Secrets:    secrets,
		SecretsErr: secretsErr,
		Templates:  templateSource(opts),
		// The running build, for a project's `infrena:` floor. The compiler
		// takes it as an input, so this is the one place it is supplied.
		Version:     version.Version(),
		Environment: environment,
		Vars:        vars,
		FileVars:    fileVars,
	}, ds
}

// secretsFromEnvironment resolves ${secret.NAME} from the process environment,
// which is the only secret backend the CLI reads: it needs no configuration,
// and infrena never has to store or transport the value.
//
// An empty value is treated as unset. `PASSWORD=` is the ordinary shape of a
// secret that failed to inject, and an empty credential fails at the provider
// rather than at the plan — after somebody has approved the run.
func secretsFromEnvironment(name string) (value.Value, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return value.Value{}, false
	}
	// SourceVariable: it came from outside the configuration. Sensitivity is
	// stamped by the evaluator, so there is one place that decides it.
	return value.String(v, value.SourceVariable), true
}
