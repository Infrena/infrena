package compiler

import (
	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/environments"
	"infra/internal/registry"
	"infra/internal/variables"
	"infra/pkg/value"
)

// Compile runs the full compiler pipeline — decode, reference binding,
// schema binding, and whole-graph validation — accumulating diagnostics from
// whichever stages run.
//
// Diagnostics still collect within a stage: one bad resource does not stop
// that stage from reporting its siblings' problems too. But a stage that
// could not build a usable ResolvedConfig must not hand it to the next one —
// a stage-7 kind check against a resource whose type was never resolved
// produces noise, not signal, and the same is true of stage 8 reasoning about
// a graph that stage 6 or 7 could not finish. Compile therefore stops at the
// first stage boundary where HasErrors() is true, returning everything
// collected up to and including that stage.
//
// CONTRACT: the returned ResolvedConfig is meaningless if the returned
// diagnostics contain errors. Check HasErrors() before touching it. On an
// early return it holds whatever the last completed stage built — some
// resources fully resolved, others never given defaults or checked for
// required attributes — and nothing in it distinguishes the two. Nothing
// panics; it simply looks valid. `cfg, _ := Compile(...)` is always a bug.
//
// From stage 6 onward the partial config is returned deliberately rather
// than a zero value, because an empty ResolvedConfig is the more dangerous
// shape: zero desired resources diffed against a populated state file is
// exactly what invariant 1 reads as "removed from configuration", so it
// would describe a plan destroying every managed resource. A partial config
// degrades one resource at a time; an empty one degrades the whole
// environment at once.
//
// Decode is the exception, and it cannot be otherwise: it fails before any
// ResolvedConfig exists, so there is nothing partial to return and the zero
// value is all there is. That is precisely the syntax-error case, so the
// paragraph above buys NO defence in depth there — the caller's HasErrors()
// check is the only thing standing between a malformed file and a
// destroy-everything plan. Both callers make it. A third must too.
//
// Stages 3 (environments.Resolve) and 4 (variables.Resolve) are two more such
// exceptions, and for the same reason as Decode: neither builds a
// ResolvedConfig, so a failure there has nothing partial to hand forward
// either. Stage 3 failing means the environment chain itself could not be
// established — extending an unknown environment or a cycle in `extends` — so
// stage 4 is not run at all: resolving variables against a broken chain would
// report every environment-scoped variable as unset, burying the one
// diagnostic that explains why. Stage 4 failing means one or more declared
// variables could not be resolved to a value; stage 6 would otherwise report
// `undefined variable` again at every use site, the same problem told worse.
func Compile(files []config.File, reg *registry.Registry, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)
	if decodeDiags.HasErrors() {
		return ResolvedConfig{}, ds
	}

	chain, envDiags := environments.Resolve(project.Environments, opts.Environment)
	ds.Extend(envDiags)
	if envDiags.HasErrors() {
		// A failed chain makes stage 4 report every environment-scoped
		// variable as unset, which buries the one diagnostic that explains
		// the failure.
		return ResolvedConfig{}, ds
	}

	scope, varDiags := variables.Resolve(project.Variables, chain, fileVars(project, opts), opts.Vars)
	ds.Extend(varDiags)
	seedProcessVariables(&scope, opts)
	if varDiags.HasErrors() {
		// Stage 6 would report `undefined variable` for each of the same
		// names at each use site — the same problem told twice, with the
		// second telling less informative than the first.
		return ResolvedConfig{}, ds
	}

	cfg, bindDiags := bindReferences(project, scope, opts)
	ds.Extend(bindDiags)
	if bindDiags.HasErrors() {
		return cfg, ds
	}

	schemaDiags := bindSchemas(&cfg, reg, opts)
	ds.Extend(schemaDiags)
	if schemaDiags.HasErrors() {
		return cfg, ds
	}

	validateDiags := validateGraph(&cfg, reg)
	ds.Extend(validateDiags)

	return cfg, ds
}

// fileVars merges --var-file entries over variables.yml's.
//
// The two arrive as separate fields because they are separate things — one is
// a decoded project file, the other a command-line input — and they merge only
// here, at the one rung of the precedence chain they share. The file named on
// the command line is the more specific of the two, so it wins.
func fileVars(project *config.ProjectDecl, opts Options) map[string]value.Value {
	out := make(map[string]value.Value, len(project.VariableValues)+len(opts.FileVars))
	for name, v := range project.VariableValues {
		out[name] = v
	}
	for name, v := range opts.FileVars {
		out[name] = v
	}
	return out
}

// seedProcessVariables adds the three variables that come from the process
// invocation rather than from any file.
//
// `environment` is unconditional and authoritative: configuration names its
// own environment (PLAN.md §10's example is `name: ${project_name}-${environment}`),
// and a --var that could change it would produce resource names claiming one
// environment while the plan changed another.
//
// `region` and `account` are added only when supplied. Injecting an empty
// string instead would interpolate silently into a resource name; leaving
// them undefined makes stage 6 say so, which the user can act on.
//
// They carry SourceEnvironment because they are facts about the environment
// being planned rather than variables anyone declared, and ScopeCLIOverride
// because the command line is where they enter the process. Source and Scope
// are orthogonal: one says what kind of thing a value is, the other says
// which precedence level supplied it.
func seedProcessVariables(scope *variables.Scope, opts Options) {
	origin := value.Origin{File: "<command line>"}
	set := func(name, text string) {
		scope.Override(name, value.String(text, value.SourceEnvironment).
			WithScope(value.ScopeCLIOverride).WithOrigin(origin))
	}
	set("environment", opts.Environment)
	if opts.Region != "" {
		set("region", opts.Region)
	}
	if opts.Account != "" {
		set("account", opts.Account)
	}
}
