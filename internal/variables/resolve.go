package variables

import (
	"sort"
	"strconv"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/environments"
	"github.com/infrata/infrata/pkg/value"
)

// Scope is the resolved variable scope: every variable name mapped to the
// value that won the precedence chain (PLAN.md §7, spec §7.1).
//
// It is the ONLY variable scope in the engine. internal/compiler used to build
// a second, equivalent one of its own (deleted by task 7); two implementations
// of one concept is the defect that leaked a plaintext secret in M2, because a
// fix applied to one copy left the other one wrong.
type Scope struct {
	vars map[string]value.Value
}

// Variable resolves a variable by name. It satisfies half of
// expressions.Scope, which is how compiler stage 6 reaches these values.
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

// ProcessVariables names the three variables that come from the process
// invocation rather than from any configuration file, sorted.
//
// Listed here, once, because two places need the same list and a second copy
// would drift: compiler.seedProcessVariables sets them, and internal/modules
// carries them across a module boundary (a module sees these and its own
// inputs, and nothing else — PLAN.md §11.3). Override's doc comment below has
// always named exactly these three; this is that sentence made readable by a
// caller.
var ProcessVariables = []string{"account", "environment", "project", "region"}

// Override records a value that comes from the process invocation rather than
// from any configuration file.
//
// This is NOT a second precedence ladder. It exists for exactly three fixed
// names — `environment`, `region`, `account` — whose values are not written in
// any file and which must be authoritative: a ${environment} that disagreed
// with the environment being planned would make every diagnostic and every
// resource name that interpolates it lie about which environment it belongs
// to. See task 7, which is the only caller.
func (s *Scope) Override(name string, v value.Value) {
	if s.vars == nil {
		s.vars = map[string]value.Value{}
	}
	s.vars[name] = v
}

// Resolve is compiler stage 4. It builds the ordered scope stack in exactly
// PLAN.md §7's order and resolves each variable to the entry that wins,
// recording both what KIND of thing the value is (Source) and WHICH RUNG
// supplied it (Scope).
//
// The rungs, lowest first — the last writer wins, and the loop never reverses:
//
//	ScopeBaseConfig          a schema's `default:`            SourceDefault
//	ScopeBaseConfig          variables.yml and vars/**        SourceVariable
//	ScopeScopedVars          resources/<dir>/vars/**          SourceVariable
//	ScopeModuleDefault       — M5 fills this in
//	ScopeEnvironmentInherit  inherited environment layers     SourceEnvironment
//	ScopeEnvironmentVar      the selected environment         SourceEnvironment
//	ScopeCLIOverride         --var-file, then --var            SourceVariable
//
// scoped is ONE resources directory's own vars/ (PLAN.md §4.1) — never a map of
// every directory's. Which resources may see them is the caller's business;
// this function only says what they beat. See the rung 2.5 pass for why they
// need their own parameter rather than a third bucket inside files.
//
// files holds two precedence levels in one map, told apart by each entry's OWN
// Scope rather than by a second parameter: variables.yml decodes at
// ScopeUnset (config.Decode is stage 2; it declares, it does not resolve — see
// pkg/value/scope.go) and a --var-file entry decodes straight to
// ScopeCLIOverride (internal/cli's loadVarFiles, which runs after resolution
// order is already known). Anything NOT at ScopeCLIOverride is treated as the
// variables.yml rung; this is a two-way split, not an equality check against
// ScopeBaseConfig specifically, so a caller's ScopeUnset entry lands in the
// same bucket a caller's explicit ScopeBaseConfig entry would. A --var-file
// entry is applied in its own pass AFTER the environment chain and BEFORE
// --var, which is what lets it outrank an environment's own configuration
// while still losing to a --var naming the same variable on the same command
// line (PLAN.md §8: "CLI values override variable files"). Task 8 is what
// makes this split matter: before it, every files entry was stamped
// ScopeBaseConfig unconditionally and a --var-file could never outrank an
// environment no matter which flag order the user chose.
//
// The environment rungs are not judged here: environments.Chain already stamps
// each layer with the scope it represents, and this walks the layers in order
// and copies it. That is what spec §7.1 means by one implementation — the
// scope a value reports IS the rung that supplied it, never a parallel opinion
// about it that could drift.
//
// Every winning value is stamped with its Source and Scope — either directly
// in the loop below, or, for a declared rung-6 entry, inside Schema.ParseText
// (which this function calls and whose result it stores unchanged; look
// there, not here, for that one rung's stamp). Either way stage 4 is what
// decides the provenance: even where stage 2 already tagged a value the same
// way, it is re-stamped rather than trusted to have survived, because a
// Source that was set elsewhere and merely survived is a Source nobody is
// responsible for.
//
// ScopeProviderDefault is absent deliberately: provider defaults apply to
// resource attributes in stage 7 and never to variables.
//
// Every problem is reported in one pass (spec §7.4); a variable that fails
// validation keeps its winning value so later stages see a value of the right
// shape rather than a hole, and the diagnostics are what make the compile fail.
func Resolve(decls []config.VariableDecl, chain environments.Chain,
	files map[string]value.Value, scoped map[string]value.Value, cliVars map[string]string,
) (Scope, diag.Diagnostics) {
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

	// Rung 2: variables.yml — every files entry NOT at ScopeCLIOverride.
	// Explicit configuration beats the implicit default rung 1 just wrote
	// (PLAN.md §7's closing line), which is why this is a separate pass at the
	// same scope rather than merged with it. The --var-file half of files is
	// applied later, at rung 5 below, after the environment chain — see the
	// doc comment above for why a single map holds two rungs.
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
	// declares for itself (PLAN.md §4.1). They sit ABOVE base configuration,
	// because a directory saying something about its own resources is more
	// specific than the project saying it about all of them, and BELOW an
	// environment, because a directory is how the project is ORGANISED while an
	// environment is where it is DEPLOYED: if a directory outranked an
	// environment, production could no longer tune a value a directory had set.
	//
	// A SEPARATE PARAMETER rather than another Scope-tagged bucket inside files,
	// unlike the --var-file split below. files is keyed by name, so it holds at
	// most one rung per name, and last-writer-wins between its two rungs is
	// correct only because --var-file genuinely outranks variables.yml (see
	// compiler.fileVars, which merges them in that order). A directory's value
	// has to beat variables.yml AND lose to --var-file, and no single
	// name-keyed map can say that about one name.
	//
	// Nothing here knows WHICH directory it is resolving: the caller runs stage
	// 4 once per directory, each run seeing one directory's values. That is what
	// keeps the scoping — which resources can see these — out of the ladder,
	// which only ever answers what beats what.
	for _, name := range sortedValueNames(scoped) {
		out.vars[name] = scoped[name].
			WithSource(value.SourceVariable).
			WithScope(value.ScopeScopedVars)
	}

	// Rung 3 (ScopeModuleDefault) is M5's. The constant exists so M5 inserts a
	// pass here rather than renumbering the whole ladder.

	// Rungs 4 and 5: the environment chain, ancestors first. Overrides are
	// walked in the slice order stage 2 built and stage 3 preserved — nothing
	// is sorted here, because nothing here is a map.
	for _, layer := range chain.Layers {
		for _, o := range layer.Overrides {
			out.vars[o.Name] = o.Value.
				WithSource(value.SourceEnvironment).
				WithScope(layer.Scope)
		}
	}

	// Rung 5 (continued): --var-file — the files entries AT ScopeCLIOverride,
	// applied after the environment chain so a --var-file outranks it, and
	// before rung 6's --var so a --var on the same command line still wins a
	// tie at this same Scope (PLAN.md §8's "CLI values override variable
	// files"). With one entry per name inside files there is no tie WITHIN
	// this pass; the real tie is against rung 6, and it is resolved by this
	// pass running strictly before that one, never by comparing Scope values
	// that are equal.
	for _, name := range fileNames {
		if files[name].Scope != value.ScopeCLIOverride {
			continue
		}
		// A --var-file entry naming one of the three process-reserved names
		// is refused rather than applied — see reservedNameDiag.
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
		// A --var naming one of the three process-reserved names is refused
		// rather than applied — see reservedNameDiag.
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
		// is what it is. See Schema.ParseText for why no type is guessed.
		//
		// WithSuppliedBy(origin.File) reuses the same "--var" literal origin
		// already carries, rather than a second spelling (Amendment 6,
		// contract.md) — see Schema.ParseText's mirror of this for why
		// SuppliedBy needs its own stamp instead of trusting Origin.
		out.vars[name] = value.String(cliVars[name], value.SourceVariable).
			WithScope(value.ScopeCLIOverride).WithOrigin(origin).WithSuppliedBy(origin.File)
	}

	ds.Extend(checkAgainstSchemas(schemas, &out, chain))
	return out, ds
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
// schema's declared kind (see Schema.Coerce) and the coerced value is what
// gets validated and stored. The order is load-bearing, not a style choice —
// see Schema.Coerce's doc comment and TestResolveCoercesBeforeCheckingBounds.
//
// A winner whose Source is SourceDefault is judged NOWHERE here: Schemas
// already validated it once, at declaration time, specifically so a bad
// default is reported once rather than once per call where it happens to
// win. Re-running Coerce and Validate on it would report the identical
// diagnostic a second time, not a new one — see
// TestResolveDoesNotDoubleReportADefaultsOwnBoundViolation.
//
// The meaning of "unset" depends on whether an environment was selected, and
// this is the only place in the engine that distinguishes the two:
//
//   - An environment IS selected (`infra plan production`): every rung that
//     could set the variable has been consulted, so "nothing set it" is a
//     definite answer and an error.
//   - No environment is selected (`infra validate`, which takes no environment
//     argument): the variable may well be set by an environment this run never
//     looked at. It resolves to an unknown of the declared kind, which is the
//     engine's existing machinery for "typed but not yet determined" (spec
//     §5.1). Downstream kind checks still work, and nothing invents a value.
//     Erroring instead would make `infra validate` reject configuration that
//     `infra plan production` plans perfectly well.
//
// This is PLAN.md §9's "validation must happen during `infra validate` AND
// before planning" being two checks at two times: validate checks shape,
// plan additionally checks presence.
//
// ASSUMPTION, and what it rests on: an entry reaching the unset branch always
// has a real Kind to build an unknown from. An untyped declaration is legal,
// but one that reaches stage 4 has a default, so rung 1 set it and it never
// arrives here.
//
// That holds for every ProjectDecl that reaches stage 4 — not for declarations
// in general. Stage 2 suppresses its empty-declaration error when another
// diagnostic on the same line already names the root cause, so empty
// declarations without that error do exist; they are kept out because Compile
// halts at the stage boundary when HasErrors() is true. The assumption is
// therefore on the HALTING, not on decode. Make decode errors non-fatal, or
// run stage 4 before that boundary check, and this builds
// value.Unknown(KindInvalid, ...) and hands an indeterminate value to the rest
// of the pipeline.
func checkAgainstSchemas(schemas map[string]Schema, out *Scope, chain environments.Chain) diag.Diagnostics {
	var ds diag.Diagnostics

	for _, name := range sortedSchemaNames(schemas) {
		s := schemas[name]
		v, set := out.vars[name]
		if set {
			// The declared default is validated exactly once, at declaration
			// time (Schemas, beside where s.Default is stored — see that
			// doc comment: "checked against its own constraints here, once,
			// rather than every time the default wins"). SourceDefault is
			// stamped ONLY by rung 1, nowhere else in this ladder, so it is
			// a reliable signal that the winning value IS that same
			// untouched default rather than something a later rung wrote
			// over it. Falling through to Coerce+Validate below would judge
			// it a second time and — since it is the same Value — produce
			// the identical diagnostic twice for one mistake. See
			// TestResolveDoesNotDoubleReportADefaultsOwnBoundViolation.
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
			// A project may legally declare one of the process's own
			// reserved names as its own typed variable — "environment",
			// "region" and "account" are ordinary identifiers, and a
			// project might reasonably want them type-checked. But
			// whether one of these three is ultimately supplied is
			// decided strictly AFTER Resolve returns, by
			// compiler.seedProcessVariables (via Scope.Override) — for
			// "environment" unconditionally, and for "region"/"account"
			// whenever the corresponding Option is non-empty. Reporting
			// "is not set" here would report a fact that becomes false
			// moments later in the only caller, and its suggested fix
			// ("pass --var environment=...") would itself be silently
			// overridden by that same Override — recommending an action
			// that cannot work (spec §44 requires the message and the
			// action to both be true).
			//
			// Left absent rather than filled with a placeholder, exactly
			// like the !chain.Selected branch above: if a reserved name
			// genuinely never ends up supplied (region/account with no
			// Option set), stage 6 reports "undefined variable" at the
			// use site — the same honest answer an undeclared reserved
			// name already gets today.
			continue
		}

		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(name) + " is not set",
			Detail: strconv.Quote(name) + " is declared at " + s.Origin.String() +
				" with no `default`, and nothing set it while resolving environment " +
				strconv.Quote(chain.Name) + ".",
			Action: "Give it a `default`, set it in variables.yml or environments/" +
				chain.Name + ".yml, or pass --var " + name + "=<value>.",
			Origin: s.Origin,
		})
		// Left absent rather than filled with a poison value: stage 6 will
		// report `undefined variable` at each USE SITE, which tells the user
		// where the missing value is needed. Both diagnostics are useful and
		// spec §7.4 collects rather than choosing between them.
	}
	return ds
}

// processReservedNames are the four variable names the process invocation
// itself supplies rather than any configuration file — see
// compiler.seedProcessVariables, the only caller of Scope.Override, and its
// doc comment for exactly which of the three are unconditional. Declaring one
// of these names under `variables:` must not make checkAgainstSchemas treat
// an as-yet-unsupplied value as an error; see the call site above.
var processReservedNames = map[string]bool{
	"environment": true,
	"region":      true,
	"account":     true,
	// `project` joins them for the same reason `environment` is here: it is
	// declared in configuration and recorded in state, so a --var that changed
	// it would make a resource claim one project while its state recorded
	// another.
	"project": true,
}

// reservedNameDiag refuses a --var or --var-file entry naming one of the
// four process-reserved names (M4 final review, MAJOR 3).
//
// Before this, Resolve applied the entry like any other variable and
// compiler.seedProcessVariables silently overwrote it moments later with no
// diagnostic of any kind — `infra plan dev --var environment=production`
// planned `dev` and, worse, rendered `[environment, from --var]`, which reads
// as confirmation the flag WAS honoured. That is the accept-and-ignore
// failure this project refuses everywhere else: destroy and refresh already
// REFUSE --var/--var-file outright for the parallel reason that neither
// command interpolates a variable into anything (varopts.go's
// rejectVariableFlags). This is that same refusal, narrowed to the four
// names it actually applies to — --var otherwise works normally for
// validate/plan/apply.
//
// Declaring one of these names under `variables:` is unaffected and stays
// legal (see checkAgainstSchemas' processReservedNames branch): this only
// refuses the FLAG overriding it, never the declaration. A value set through
// variables.yml or an environment layer still resolves normally; only the
// ScopeCLIOverride rungs are refused, because the environment argument
// decides which state file this run writes, and letting a flag override it
// would let a plan name one environment while writing another's.
func reservedNameDiag(name, flag string, origin value.Origin) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  flag + " cannot set " + strconv.Quote(name),
		// Says which ONE of the four this is, rather than describing the set.
		// The previous wording enumerated three sources and was left behind by
		// M9 adding `project` — a message that lists the wrong set is worse than
		// one that lists none, because a reader checks their case against it.
		// "set by the engine, not by a variable" is the one framing true of all
		// four. An earlier draft of this said "comes from the invocation rather
		// than from configuration", which is right for three of them and
		// self-contradictory for `project` — it comes from `project:`, which IS
		// configuration.
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

// reservedNameAction names what to do instead, one sentence, per PLAN.md §44.
func reservedNameAction(name, flag string) string {
	if name == "environment" {
		return "Pass the environment as infra's own argument instead, e.g. `infra plan <environment>`, and remove it from " + flag + "."
	}
	return "Remove " + strconv.Quote(name) + " from " + flag + "; it cannot be set this way."
}
