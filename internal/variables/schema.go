// Package variables implements compiler stage 4: it resolves every variable to
// the value that wins the precedence chain, and validates it against its
// declared schema (PLAN.md §8, §9; spec §7, §7.1).
package variables

import (
	"strconv"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/pkg/value"
)

// Schema is one variable's declared type and constraints (PLAN.md §9).
//
// Min and Max keep the value.Value shape stage 2 decoded them into rather than
// collapsing to float64. An int64 above 2^53 does not survive a round trip
// through float64, so a large integer bound would silently validate the wrong
// thing; and a Value carries Origin, which is what lets a bound diagnostic say
// WHERE the bound was declared as PLAN.md §44 requires.
//
// Kind is KindInvalid for an untyped declaration, which PLAN.md §9 permits.
// Schemas guarantees that every entry in its table either has a real Kind or
// has a default — see the empty-declaration warning below — so a consumer that
// needs a kind to build an unknown from always has one when it needs one.
type Schema struct {
	Name       string
	Kind       value.Kind
	Default    value.Value
	HasDefault bool
	Min        value.Value
	HasMin     bool
	Max        value.Value
	HasMax     bool
	Origin     value.Origin
}

// Schemas builds the schema table from stage 2's declarations.
//
// The `type:` spelling has already been mapped to a Kind by stage 2, which is
// where an unknown spelling is reported — with the line and column it was
// written at. Nothing here re-derives that mapping: two implementations of one
// concept is the defect that leaked a plaintext secret in M2.
//
// Every problem in every declaration is reported in one pass (spec §7.4): a
// declaration with a bad bound keeps its type and loses the bound rather than
// stopping the walk, so a second bad declaration is still reported.
func Schemas(decls []config.VariableDecl) (map[string]Schema, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := make(map[string]Schema, len(decls))

	for _, d := range decls {
		// Every declaration reaching here is kept. A declaration with neither a
		// `type` nor a `default` was already rejected by stage 2, at the line
		// the user wrote it — do not add a second check that DROPS one here.
		// Discarding a declaration discards input the user supplied, and a
		// warning in front of a discard is still a discard.
		s := Schema{Name: d.Name, Kind: d.Type, Origin: d.Origin}
		// Bounds are copied, not re-validated. Stage 2 coerced each one to
		// d.Type and rejected every malformed declaration, with the line and
		// column this stage does not have. Adding a second check here would
		// report one mistake twice, and the second report would be the worse
		// of the two.
		s.Min, s.HasMin = d.Min, d.HasMin
		s.Max, s.HasMax = d.Max, d.HasMax
		if d.HasDefault {
			s.Default, s.HasDefault = d.Default, true
			// Checked against its own constraints here, once, rather than
			// every time the default wins. The stamped copy is for the
			// diagnostic's wording only; the stored Default stays unstamped
			// because stage 4 is the one place that decides what provenance a
			// winning value carries.
			//
			// This is the ONE declaration-level check stage 2 does not make,
			// and it belongs here rather than there: a default is a VALUE, not
			// part of the declaration's shape, so it is checked by the same
			// Validate that checks every supplied value — a `default:` and a
			// `--var` can then never be judged differently.
			//
			// The diagnostic still names the line the default was written on,
			// with nothing plumbed down from stage 2: d.Default is a
			// value.Value and carries its own Origin, which Validate reads.
			// Flattening declarations into bare numbers is what would have
			// lost that.
			ds.Extend(s.Validate(d.Default.
				WithSource(value.SourceDefault).
				WithScope(value.ScopeBaseConfig)))
		}
		out[d.Name] = s
	}
	return out, ds
}

// compareBounds orders two values IN THE DECLARED KIND: integers as int64,
// floats as float64. Nothing is funnelled through one numeric type — that
// funnelling loses an int64 above 2^53, which is the whole reason bounds are
// Values and not float64s.
//
// Both operands are known to share s.Kind by the time this is called: stage 2
// coerced the bound to the declared type, and Validate has already compared
// the value's Kind to the schema's. Returns -1, 0 or 1; a pair it cannot read
// compares as 0, so a malformed value produces no bound complaint on top of
// the malformed-value diagnostic its caller already emits.
func compareBounds(k value.Kind, a, b value.Value) int {
	switch k {
	case value.KindInt:
		x, okA := a.AsInt()
		y, okB := b.AsInt()
		if !okA || !okB {
			return 0
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	case value.KindFloat:
		x, okA := a.AsFloat()
		y, okB := b.AsFloat()
		if !okA || !okB {
			return 0
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// numericDatumMatchesKind reports whether v's datum really is of kind k.
//
// Kind is a CLAIM about Raw and this never takes it on trust: value.Equal and
// value.Format each shipped a version that did, producing a false equality in
// one case and a printed secret in the other. Used by Validate on incoming
// values, where a mismatch is reachable — a Value can arrive from state, from
// a provider, or from a test.
func numericDatumMatchesKind(k value.Kind, v value.Value) bool {
	switch k {
	case value.KindInt:
		_, ok := v.AsInt()
		return ok
	case value.KindFloat:
		_, ok := v.AsFloat()
		return ok
	}
	return false
}

// show renders a value for a diagnostic. It goes through value.Format, which
// is the engine's only rendering path — a second one is what leaked a
// plaintext secret in M2, and a bound is as capable of being sensitive as
// anything else.
func show(v value.Value) string {
	return value.Format(v, value.FormatOptions{Unknown: "(unknown)"})
}

// originOr prefers the more specific of two origins. A bound decoded from YAML
// carries its own line; a synthesised one does not, and the declaration's
// origin is then the closest true answer.
func originOr(specific, fallback value.Origin) value.Origin {
	if specific.File != "" {
		return specific
	}
	return fallback
}

// article picks "a" or "an" so diagnostics read as English.
func article(k value.Kind) string {
	if k == value.KindInt {
		return "an"
	}
	return "a"
}

// Validate reports every way v violates s.
//
// An untyped declaration (Kind KindInvalid) constrains nothing: PLAN.md §9
// makes schemas optional, and a declaration that gave no type has said nothing
// about what values are acceptable. Bounds cannot reach an untyped schema —
// stage 2 rejects them — so there is nothing left to check.
//
// An unknown value is checked for kind and nothing else. Spec §5.1 keeps Kind
// on an unknown precisely so type errors surface at plan time rather than
// apply time, but there is no datum to compare against a bound, and inventing
// one would be a confident wrong answer.
//
// The kind switch is an allowlist with no `default` arm that trusts Raw.
// KindInvalid is the zero value of Kind, and value.Equal and value.Format each
// shipped a permissive default that turned a malformed value into a wrong
// answer — a false equality in one case and a printed secret in the other.
// A Value whose Raw does not match its Kind is reported, not interpreted.
func (s Schema) Validate(v value.Value) diag.Diagnostics {
	var ds diag.Diagnostics

	if s.Kind == value.KindInvalid {
		return ds
	}

	if v.Kind != s.Kind {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(s.Name) + " must be " + article(s.Kind) + " " + s.Kind.String(),
			Detail: "Declared as " + s.Kind.String() + " at " + s.Origin.String() +
				". The value supplied by " + v.Scope.String() + " is " + article(v.Kind) + " " + v.Kind.String() + ".",
			Action: "Supply " + article(s.Kind) + " " + s.Kind.String() + " value, or change the declared type.",
			Origin: originOr(v.Origin, s.Origin),
		})
		return ds
	}
	if !v.Known {
		return ds
	}

	switch s.Kind {
	case value.KindInt, value.KindFloat:
		if !numericDatumMatchesKind(s.Kind, v) {
			ds.Add(malformed(s, v))
			return ds
		}
		checkBounds(s, v, &ds)
	case value.KindString, value.KindBool, value.KindList, value.KindMap:
		// Kind is the whole constraint. PLAN.md §9 declares no element type
		// for list or map, and adding one is a language extension, not a
		// validation detail.
	default:
		ds.Add(malformed(s, v))
	}
	return ds
}

// checkBounds reports a value outside its schema's inclusive range. Both
// bounds are inclusive: `min: 1` permits 1. Comparison happens in the declared
// kind (see compareBounds).
func checkBounds(s Schema, v value.Value, ds *diag.Diagnostics) {
	if s.HasMin && compareBounds(s.Kind, v, s.Min) < 0 {
		ds.Add(boundDiag(s, "at least", s.Min, v))
	}
	if s.HasMax && compareBounds(s.Kind, v, s.Max) > 0 {
		ds.Add(boundDiag(s, "at most", s.Max, v))
	}
}

// boundDiag names the constraint, the offending value, the scope that supplied
// it, and WHERE the bound was declared. PLAN.md §44 requires the last of those
// and it is the one a reader cannot reconstruct for themselves.
func boundDiag(s Schema, relation string, bound value.Value, v value.Value) diag.Diagnostic {
	limit := show(bound)
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "variable " + strconv.Quote(s.Name) + " must be " + relation + " " + limit,
		Detail: "The value supplied by " + v.Scope.String() + " is " + show(v) + ". The bound is declared at " +
			originOr(bound.Origin, s.Origin).String() + ".",
		Action: "Choose a value " + relation + " " + limit + ".",
		Origin: originOr(v.Origin, s.Origin),
	}
}

// malformed reports a Value whose Raw does not match the Kind it claims. This
// is an engine bug rather than a user error, but it is reported rather than
// ignored: the alternative is a silently unchecked value.
func malformed(s Schema, v value.Value) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "variable " + strconv.Quote(s.Name) + " holds a malformed value",
		Detail:   "It claims kind " + v.Kind.String() + " but its datum does not match. This is an internal error.",
		Action:   "Report this, with the configuration that produced it.",
		Origin:   originOr(v.Origin, s.Origin),
	}
}
