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
		Dir:     opts.Dir,
		Secrets: secretsFromEnvironment,
		// The running build, for a project's `infrena:` floor. The compiler takes it
		// as an input rather than reading it, so this is the one place it is supplied.
		Version:     version.Version(),
		Environment: environment,
		Vars:        vars,
		FileVars:    fileVars,
	}, ds
}

// secretsFromEnvironment resolves ${secret.NAME} from the process environment
// (PLAN.md §36).
//
// THE ENVIRONMENT AND NOTHING ELSE, in the free CLI. It is the one place every
// CI system already puts secrets, it needs no configuration, and — the part
// that decides it — infrena never has to store or transport the value. Backends
// like Vault, AWS Secrets Manager and 1Password are named in §60 as the
// platform's, and that line is easy to hold precisely because the core reads
// only what the process was already given.
//
// AN EMPTY VALUE IS TREATED AS UNSET. `PASSWORD=` in a CI configuration is the
// ordinary shape of a secret that failed to inject — a missing repository
// secret expands to empty rather than disappearing — and an empty credential
// does not fail at the plan, it fails at the provider, after somebody approved
// the run. Refusing costs a clear diagnostic; accepting costs a debugging
// session at the wrong end of the system.
func secretsFromEnvironment(name string) (value.Value, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return value.Value{}, false
	}
	// SourceVariable: it came from outside the configuration, which is what
	// that source means. Sensitivity is stamped by the evaluator rather than
	// here, so there is one place that decides it.
	return value.String(v, value.SourceVariable), true
}
