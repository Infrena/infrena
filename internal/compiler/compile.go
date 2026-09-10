package compiler

import (
	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/registry"
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
// The partial config is returned deliberately rather than a zero value,
// because an empty ResolvedConfig would be the more dangerous shape: zero
// desired resources diffed against a populated state file is exactly what
// invariant 1 reads as "removed from configuration", so a syntax error
// would produce a plan proposing to destroy every managed resource. A
// partial config degrades one resource at a time; an empty one degrades the
// whole environment at once.
func Compile(files []config.File, reg *registry.Registry, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	project, decodeDiags := config.Decode(files)
	ds.Extend(decodeDiags)
	if decodeDiags.HasErrors() {
		return ResolvedConfig{}, ds
	}

	cfg, bindDiags := bindReferences(project, opts)
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
