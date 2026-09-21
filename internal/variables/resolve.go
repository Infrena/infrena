package variables

import (
	"sort"
	"strconv"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/environments"
	"github.com/infrena/infrena/pkg/value"
)

// Scope is the resolved variable scope: every variable name mapped to the
// value that won the precedence chain.
//
// It is the only variable scope in the engine. A second implementation of one
// is how a fix comes to be applied to one copy and not the other.
type Scope struct {
	vars map[string]value.Value
}

// Variable resolves a variable by name. It satisfies half of
// expressions.Scope, which is how expression evaluation reaches these values.
func (s Scope) Variable(name string) (value.Value, bool) {
	v, ok := s.vars[name]
	return v, ok
}

// Names lists every resolved variable, sorted, for diagnostics that suggest
// what the user might have meant. Go's map iteration is randomised and such a
// list must not reorder itself between runs of the same configuration.
func (s Scope) Names() []string {
	out := make([]string, 0, len(s.vars))
	for name := range s.vars {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ProcessVariables names the variables that come from the process invocation
// rather than from any configuration file, sorted.
//
// Listed here once because two places need the same list: the compiler seeds
// them, and internal/modules carries them across a module boundary, where a
// module sees these and its own inputs and nothing else.
//
// "region" and "account" are deliberately not here. They are ordinary variable
// names, so a project declaring `region` passes it to a module as an input
// like any other variable.
var ProcessVariables = []string{"environment", "project"}

// Override records a value that comes from the process invocation rather than
// from any configuration file.
//
// It is not a second precedence ladder. It exists for the fixed names in
// ProcessVariables, whose values are not written in any file and must be
// authoritative: a ${var.environment} that disagreed with the environment being
// planned would make every diagnostic and every resource name that interpolates
// it lie about which environment it belongs to.
func (s *Scope) Override(name string, v value.Value) {
	if s.vars == nil {
		s.vars = map[string]value.Value{}
	}
	s.vars[name] = v
}

// With returns a copy of s carrying one extra name, leaving s untouched.
//
// Override mutates the map every narrowing of this scope shares, which is right
// for the three process variables — they are the same for the whole run — and
// wrong for anything per-instance. `for_each` binds a different `each` for
// every instance, so mutating would leave every instance seeing the last one's
// key, and the resources would all be built from the final entry.
//
// The copy is shallow: values are immutable once resolved, so only the map
// itself needs to be fresh.
func (s Scope) With(name string, v value.Value) Scope {
	next := Scope{vars: make(map[string]value.Value, len(s.vars)+1)}
	for k, existing := range s.vars {
		next.vars[k] = existing
	}
	next.vars[name] = v
	return next
}

// Resolve builds the ordered scope stack and resolves each variable to the
// entry that wins, recording both what kind of thing the value is (Source) and
// which rung supplied it (Scope).
//
// The rungs, lowest first — the last writer wins, and the loop never reverses:
//
//	ScopeBaseConfig          a schema's `default:`            SourceDefault
//	ScopeBaseConfig          variables.yml and vars/**        SourceVariable
//	ScopeScopedVars          resources/<dir>/vars/**          SourceVariable
//	ScopeModuleDefault       reserved
//	ScopeEnvironmentInherit  inherited environment layers     SourceEnvironment
//	ScopeEnvironmentVar      the selected environment         SourceEnvironment
//	ScopeCLIOverride         --var-file, then --var           SourceVariable
//
// scoped is one resources directory's own vars/, never a map of every
// directory's. Which resources may see them is the caller's business; this
// function only says what they beat.
//
// files holds two precedence levels in one map, told apart by each entry's own
// Scope: variables.yml decodes at ScopeUnset, because decoding declares rather
// than resolves, and a --var-file decodes straight to ScopeCLIOverride, because
// the CLI loads it once resolution order is known. Anything not at
// ScopeCLIOverride is treated as the variables.yml rung — a two-way split, so a
// caller's ScopeUnset entry lands where an explicit ScopeBaseConfig entry
// would. The --var-file pass runs after the environment chain and before --var,
// which is what lets it outrank an environment while still losing to a --var
// naming the same variable.
//
// The environment rungs are not judged here: environments.Chain already stamps
// each layer with the scope it represents, and this copies it. The scope a
// value reports is the rung that supplied it, never a parallel opinion that
// could drift.
//
// Every winning value is stamped with its Source and Scope, either in the loop
// below or, for a declared --var, inside Schema.ParseText. Even where decoding
// already tagged a value the same way it is re-stamped rather than trusted,
// because a Source that merely survived is a Source nobody is responsible for.
//
// ScopeProviderDefault is absent deliberately: provider defaults apply to
// resource attributes and never to variables.
//
// Every problem is reported in one pass; a variable that fails validation keeps
// its winning value so later stages see a value of the right shape rather than
// a hole, and the diagnostics are what make the compile fail.
func Resolve(decls []config.VariableDecl, chain environments.Chain,
	files map[string]value.Value, scoped map[string]value.Value, cliVars map[string]string,
) (Scope, diag.Diagnostics, bool) {
	var ds diag.Diagnostics

	schemas, schemaDiags := Schemas(decls, "variable")
	ds.Extend(schemaDiags)

	out := Scope{vars: make(map[string]value.Value, len(schemas)+len(files)+len(scoped)+len(cliVars))}

	// Rung 1: declared defaults.
	for _, name := range sortedSchemaNames(schemas) {
		if s := schemas[name]; s.HasDefault {
			out.vars[name] = s.Default.
				WithSource(value.SourceDefault).
				WithScope(value.ScopeBaseConfig)
		}
	}

	// Rung 2: variables.yml — every files entry not at ScopeCLIOverride.
	// Explicit configuration beats the implicit default rung 1 just wrote,
	// which is why this is a separate pass at the same scope rather than
	// merged with it. The --var-file half of files is applied at rung 5,
	// after the environment chain.
	fileNames := sortedValueNames(files)
	for _, name := range fileNames {
		if files[name].Scope == value.ScopeCLIOverride {
			continue
		}
		out.vars[name] = files[name].
			WithSource(value.SourceVariable).
			WithScope(value.ScopeBaseConfig)
	}

	// Rung 2.5: resources/<dir>/vars/** — the variables one resources directory
	// declares for itself. They sit above base configuration, because a
	// directory saying something about its own resources is more specific than
	// the project saying it about all of them, and below an environment,
	// because otherwise production could no longer tune a value a directory had
	// set.
	//
	// A separate parameter rather than another Scope-tagged bucket inside
	// files: files is keyed by name, so it holds at most one rung per name, and
	// a directory's value has to beat variables.yml and lose to --var-file.
	//
	// Nothing here knows which directory it is resolving — the caller runs this
	// once per directory — which keeps the scoping out of a ladder that only
	// ever answers what beats what.
	for _, name := range sortedValueNames(scoped) {
		out.vars[name] = scoped[name].
			WithSource(value.SourceVariable).
			WithScope(value.ScopeScopedVars)
	}

	// Rung 3 (ScopeModuleDefault) is reserved. The constant exists so a pass
	// can be inserted here rather than renumbering the whole ladder.

	// Rungs 4 and 5: the environment chain, ancestors first. Overrides are
	// walked in the slice order they were decoded in — nothing is sorted here,
	// because nothing here is a map.
	for _, layer := range chain.Layers {
		for _, o := range layer.Overrides {
			out.vars[o.Name] = o.Value.
				WithSource(value.SourceEnvironment).
				WithScope(layer.Scope)
		}
	}

	// Rung 5 (continued): --var-file — the files entries at ScopeCLIOverride,
	// applied after the environment chain so a --var-file outranks it, and
	// before rung 6's --var so a --var on the same command line still wins.
	// Both sit at the same Scope, so that tie is resolved by this pass running
	// strictly before the next one, never by comparing Scope values.
	for _, name := range fileNames {
		if files[name].Scope != value.ScopeCLIOverride {
			continue
		}
		// A --var-file entry naming a process-reserved name is refused rather
		// than applied; see reservedNameDiag.
		if processReservedNames[name] {
			ds.Add(reservedNameDiag(name, "--var-file", files[name].Origin))
			continue
		}
		out.vars[name] = files[name].
			WithSource(value.SourceVariable).
			WithScope(value.ScopeCLIOverride)
	}

	// Rung 6: --var.
	for _, name := range sortedTextNames(cliVars) {
		origin := value.Origin{File: "--var"}
		// A --var naming a process-reserved name is refused rather than
		// applied; see reservedNameDiag.
		if processReservedNames[name] {
			ds.Add(reservedNameDiag(name, "--var", origin))
			continue
		}
		if s, declared := schemas[name]; declared {
			v, parseDiags := s.ParseText(cliVars[name], origin)
			ds.Extend(parseDiags)
			out.vars[name] = v
			continue
		}
		// An undeclared variable is untyped, and --var carries text, so text
		// is what it is. See Schema.ParseText for why no type is guessed, and
		// for why SuppliedBy needs its own stamp rather than trusting Origin.
		out.vars[name] = value.String(cliVars[name], value.SourceVariable).
			WithScope(value.ScopeCLIOverride).WithOrigin(origin).WithSuppliedBy(origin.File)
	}

	checkDiags, unset := checkAgainstSchemas(schemas, &out, chain)
	ds.Extend(checkDiags)

	// The scope is usable when every error reported is one of the unset
	// variables checkAgainstSchemas just bound to an unknown. Anything else —
	// a malformed declaration, an unparseable --var, a value that failed its
	// own type — leaves a hole the later stages would compile around, so the
	// caller must stop at the stage boundary instead.
	//
	// Counted rather than flagged per diagnostic, because "recoverable" is a
	// property of the whole run: one malformed declaration alongside three
	// unset variables is not recoverable.
	return out, ds, unset == errorCount(ds)
}

// errorCount is the number of diagnostics at error severity. Warnings do not
// count: they never stop a compile, so they cannot make a scope unusable.
func errorCount(ds diag.Diagnostics) int {
	n := 0
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			n++
		}
	}
	return n
}

func sortedSchemaNames(m map[string]Schema) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedValueNames(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedTextNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkAgainstSchemas normalises and validates every declared variable's
// winning value, and decides what an unset variable means.
//
// Normalisation happens before judgement: the winning value is coerced to its
// schema's declared kind and the coerced value is what gets validated and
// stored. The order is load-bearing; see Schema.Coerce.
//
// A winner whose Source is SourceDefault is not judged here. Schemas already
// validated it once, at declaration time, so re-running Coerce and Validate
// would report the identical diagnostic a second time rather than a new one.
//
// The meaning of "unset" depends on whether an environment was selected, and
// this is the only place in the engine that distinguishes the two:
//
//   - An environment IS selected (`infrena plan production`): every rung that
//     could set the variable has been consulted, so "nothing set it" is a
//     definite answer and an error.
//   - No environment is selected (`infrena validate`, which takes no environment
//     argument): the variable may well be set by an environment this run never
//     looked at. It resolves to an unknown of the declared kind, so downstream
//     kind checks still work and nothing invents a value. Erroring instead
//     would make `infrena validate` reject configuration that `infrena plan
//     production` plans perfectly well.
//
// So validate checks shape and plan additionally checks presence.
//
// It assumes an entry reaching the unset branch has a real Kind to build an
// unknown from. An untyped declaration is legal, but one that gets this far has
// a default, so rung 1 set it and it never arrives here — and that rests on the
// compiler halting at the stage boundary when decoding reported errors, not on
// decoding itself. Run this before that boundary check and it builds
// value.Unknown(KindInvalid, ...) and hands an indeterminate value to the rest
// of the pipeline.
//
// It also reports how many of its errors are the recoverable kind — a variable
// properly declared but with no value — because those are the only ones after
// which the scope it leaves behind is still usable. See Resolve's third
// return.
func checkAgainstSchemas(schemas map[string]Schema, out *Scope, chain environments.Chain) (diag.Diagnostics, int) {
	var ds diag.Diagnostics
	unset := 0

	for _, name := range sortedSchemaNames(schemas) {
		s := schemas[name]
		v, set := out.vars[name]
		if set {
			// The declared default is validated exactly once, in Schemas.
			// SourceDefault is stamped only by rung 1, so it is a reliable
			// signal that the winner is that same untouched default rather
			// than something a later rung wrote over it. Falling through
			// would judge the identical Value a second time and report one
			// mistake twice.
			if v.Source == value.SourceDefault {
				continue
			}
			// Coerce, then judge, and store the normalised value. The order is
			// load-bearing: compareBounds reads both sides in the declared
			// kind, so an uncoerced value is not merely mistyped — it is
			// silently unbounded, because a bound it cannot read compares as
			// no complaint.
			coerced, coerceDiags := s.Coerce(v)
			if coerceDiags.HasErrors() {
				// Reported once, by the function that knows why it was lossy.
				// Validate would add a kind mismatch on top, describing the
				// same mistake less well.
				ds.Extend(coerceDiags)
				continue
			}
			out.vars[name] = coerced
			ds.Extend(s.Validate(coerced))
			continue
		}

		if !chain.Selected {
			out.vars[name] = value.Unknown(s.Kind, value.SourceVariable).WithOrigin(s.Origin)
			continue
		}

		if processReservedNames[name] {
			// A project may legally declare one of these as its own typed
			// variable, but whether it is supplied is decided after Resolve
			// returns, by the compiler seeding it through Scope.Override,
			// which supplies them all unconditionally. Reporting "is not
			// set" here would state a fact that becomes false moments later,
			// and suggest a fix that Override would itself discard.
			continue
		}

		unset++
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " is not set",
			Detail: strconv.Quote(name) + " is declared at " + s.Origin.String() +
				" with no `default`, and nothing set it while resolving environment " +
				strconv.Quote(chain.Name) + ".",
			// Both layouts, because both work and a reader has only one of
			// them: variables.yml / environments/<env>.yml is the single-file
			// form `init` scaffolds, vars/default.yml / vars/<env>.yml the
			// directory form. Naming only the first tells a project using
			// directories to edit files it does not have.
			Action: "Give it a `default`, set it for every environment in variables.yml or " +
				"vars/default.yml, set it for this one in environments/" + chain.Name +
				".yml or vars/" + chain.Name + ".yml, or pass --var " + name + "=<value>.",
			Origin: s.Origin,
		})
		// Bound to an unknown rather than left absent, so that the rest of
		// the compile still runs and `validate` can report every other
		// problem in the same pass. Leaving it absent stops the compile at
		// this stage boundary, and a project with an unset variable and an
		// unrelated mistake in a resource would only ever hear about the
		// variable.
		//
		// An unknown is the same shape the unselected-chain branch above
		// produces, and it is exactly true: the variable is declared and in
		// scope, just without a value. The diagnostic just recorded is still
		// an error, so nothing proceeds to a plan on the strength of it.
		out.vars[name] = value.Unknown(s.Kind, value.SourceVariable).WithOrigin(s.Origin)
	}
	return ds, unset
}

// processReservedNames are the names the process invocation itself supplies,
// which a --var therefore cannot set: the flag would be silently overridden by
// the engine's own value, and a flag that cannot change the outcome must be
// refused rather than ignored.
//
// Declaring one of these under `variables:` stays legal; only the flag is
// refused.
//
// `project` is here for the same reason `environment` is: it is declared in
// configuration and recorded in state, so a --var that changed it would make a
// resource claim one project while its state recorded another.
var processReservedNames = map[string]bool{
	"environment": true,
	"project":     true,
}

// reservedNameDiag refuses a --var or --var-file entry naming a
// process-reserved name.
//
// Applied like any other variable, such an entry is overwritten moments later
// by the engine's own value with nothing printed: `infrena plan dev --var
// environment=production` would plan `dev` while rendering `[environment, from
// --var]`, which reads as confirmation the flag was honoured.
//
// Only the flag is refused, never the declaration: a value set through
// variables.yml or an environment layer still resolves normally. The
// environment argument decides which state file a run writes, so letting a flag
// override it would let a plan name one environment while writing another's.
func reservedNameDiag(name, flag string, origin value.Origin) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  flag + " cannot set " + strconv.Quote(name),
		// Names the one reserved name in hand rather than describing the set,
		// because a reader checks their own case against the message. "Set by
		// the engine, not by a variable" is the one framing true of all of
		// them: "comes from the invocation rather than from configuration"
		// is not, since `project` comes from `project:`.
		Detail: strconv.Quote(name) + " is set by the engine, not by a variable: " +
			reservedNameSource(name) + ". Supplying it through " + flag +
			" would be silently discarded in favour of the engine's own value.",
		Action: reservedNameAction(name, flag),
		Origin: origin,
	}
}

// reservedNameSource says where the value actually comes from, one clause per
// name. A shared sentence describing "the process invocation" would leave a
// reader guessing which part of it they had collided with.
func reservedNameSource(name string) string {
	switch name {
	case "environment":
		return "it is the environment being planned or applied, named as the command's own argument, " +
			"and it decides which state file this run writes"
	case "project":
		return "it comes from `project:` in the configuration, and every resource name and state " +
			"entry is written against it"
	case "region":
		return "it comes from the invocation's own region"
	case "account":
		return "it comes from the invocation's own account"
	default:
		return "it is supplied by the process"
	}
}

// reservedNameAction names what to do instead, in one sentence.
func reservedNameAction(name, flag string) string {
	if name == "environment" {
		return "Pass the environment as infrena's own argument instead, e.g. `infrena plan <environment>`, and remove it from " + flag + "."
	}
	return "Remove " + strconv.Quote(name) + " from " + flag + "; it cannot be set this way."
}
