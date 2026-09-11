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
// coerced the bound to the declared type, Validate has already compared the
// value's Kind to the schema's, and checkBounds — the only caller — checks
// each bound against s.Kind before ever calling this. Returns -1, 0 or 1; a
// pair it cannot read compares as 0, which callers must NOT treat as "no
// violation" on its own — checkBounds reports a pair it cannot read via
// malformedBound instead of calling this, precisely so 0 here never has to
// mean "passed".
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

// Every diagnostic below that names "the value supplied by" a scope goes
// through value.ScopeLabel, never v.Scope.String() directly (M4 final review,
// MAJOR 1). v.Scope.String() at ScopeCLIOverride is always "--var" — the
// generic scope label — even when the value actually arrived through
// --var-file, so building the sentence from it told every --var-file user
// their own diagnostic was about a flag they never typed. ScopeLabel is the
// same rule the plan renderer already applies (Amendment 6, contract.md): it
// prefers v.SuppliedBy, which names the actual input, and falls back to the
// scope label only when SuppliedBy is unset. One rule, read from one place,
// rather than a second copy that can drift from the renderer's.

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

// Coerce normalises v to s's declared kind before it is judged.
//
// It reports ONLY a lossy numeric conversion. A kind mismatch that is not a
// numeric pair — a string where a float is declared — is passed through
// untouched for Validate to report, because Validate's message names the
// declared type and the supplying scope and this one could not.
//
// An untyped schema coerces nothing: it has declared no kind to normalise to.
func (s Schema) Coerce(v value.Value) (value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	if s.Kind == value.KindInvalid || v.Kind == s.Kind {
		return v, ds
	}
	if !isNumericKind(s.Kind) || !isNumericKind(v.Kind) {
		return v, ds
	}

	out, ok := value.Coerce(v, s.Kind)
	if !ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(s.Name) + " cannot be stored as " + article(s.Kind) + " " + s.Kind.String(),
			Detail: "The value supplied by " + value.ScopeLabel(v) + " is " + show(v) +
				", which cannot be converted to " + s.Kind.String() + " without changing it. " +
				strconv.Quote(s.Name) + " is declared at " + s.Origin.String() + ".",
			Action: "Write a value that is exactly representable as " + article(s.Kind) + " " + s.Kind.String() + ", or change the declared type.",
			Origin: originOr(v.Origin, s.Origin),
		})
		return v, ds
	}
	return out, ds
}

// isNumericKind reports whether k is one of the two numeric kinds.
//
// Not to be confused with its neighbour numericDatumMatchesKind, which asks a
// different question — whether a Value's DATUM really is of the kind it claims
// — and is what Validate uses to catch a malformed value. This one looks only
// at a Kind and never at a datum, which is why it is safe to call on an
// unknown.
func isNumericKind(k value.Kind) bool {
	return k == value.KindInt || k == value.KindFloat
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
				". The value supplied by " + value.ScopeLabel(v) + " is " + article(v.Kind) + " " + v.Kind.String() + ".",
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
//
// Each bound is checked against s.Kind BEFORE it is compared against v. A
// bound whose datum does not match s.Kind cannot be read by compareBounds,
// and compareBounds answers "no violation" when it cannot read a pair — the
// right behaviour for a value it already trusts, but the wrong one to reach
// on an untrustworthy bound: it would silently accept a value that is
// actually out of range. Configuration can never produce this — stage 2
// coerces a declared bound to its variable's Type before this package ever
// sees one — so reaching it means a Schema was built with a bound that does
// not match its own Kind, and that is reported rather than silently passed.
func checkBounds(s Schema, v value.Value, ds *diag.Diagnostics) {
	if s.HasMin {
		if !numericDatumMatchesKind(s.Kind, s.Min) {
			ds.Add(malformedBound(s, "min", s.Min))
		} else if compareBounds(s.Kind, v, s.Min) < 0 {
			ds.Add(boundDiag(s, "at least", s.Min, v))
		}
	}
	if s.HasMax {
		if !numericDatumMatchesKind(s.Kind, s.Max) {
			ds.Add(malformedBound(s, "max", s.Max))
		} else if compareBounds(s.Kind, v, s.Max) > 0 {
			ds.Add(boundDiag(s, "at most", s.Max, v))
		}
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
		Detail: "The value supplied by " + value.ScopeLabel(v) + " is " + show(v) + ". The bound is declared at " +
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

// malformedBound is malformed's sibling for the neighbouring inconsistency: a
// schema whose declared bound does not match its own declared Kind, rather
// than a value whose Raw does not match its Kind. Worded as the internal
// error it is rather than as user error, because a user cannot produce this
// through configuration — stage 2 coerces every declared bound to its
// variable's Type before this package ever sees one. Reaching this means a
// Schema was constructed by hand with an inconsistent bound, so the message
// points at that rather than telling anyone to edit YAML.
func malformedBound(s Schema, which string, bound value.Value) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "variable " + strconv.Quote(s.Name) + "'s " + which + " bound does not match its declared kind",
		Detail: "Declared as " + s.Kind.String() + " at " + s.Origin.String() +
			", but its " + which + " bound is " + article(bound.Kind) + " " + bound.Kind.String() + ". This is an internal error.",
		Action: "Report this, with the configuration that produced it.",
		Origin: originOr(bound.Origin, s.Origin),
	}
}

// ParseText converts one command-line string to s's kind.
//
// `--var name=value` can only ever produce text, so a typed variable needs the
// text converted or `--var replicas=20` would fail its own integer schema. The
// conversion is the whole of the mini-language this system has, and it stops
// at scalars deliberately: `--var tags=a,b,c` would require a separator
// convention, an escape for the separator, and then a nesting syntax, which is
// the general-purpose language PLAN.md §9 forbids.
//
// An untyped declaration yields text unchanged. Guessing a type from the
// spelling — "20" becomes an integer, "true" a boolean — would make a
// variable's type depend on the value someone happened to pass, and
// `--var version=1.10` would silently become the float 1.1.
//
// Booleans go through strconv.ParseBool rather than a bespoke table, so the
// accepted spellings are Go's and documented rather than invented here.
func (s Schema) ParseText(text string, origin value.Origin) (value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	// SuppliedBy (Amendment 6, contract.md) reuses origin.File rather than a
	// second literal "--var": ParseText's one caller (resolve.go's rung 6)
	// always passes origin built from that exact string, and Origin does not
	// itself survive to the renderer (internal/expressions/eval.go re-origins
	// every ${var} reference), which is why SuppliedBy needs its own stamp
	// here rather than trusting Origin to carry it through.
	stamp := func(v value.Value) value.Value {
		return v.WithScope(value.ScopeCLIOverride).WithOrigin(origin).WithSuppliedBy(origin.File)
	}
	bad := func(expected string) (value.Value, diag.Diagnostics) {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "--var " + s.Name + "=" + text + " is not " + article(s.Kind) + " " + s.Kind.String(),
			Detail:   strconv.Quote(s.Name) + " is declared as " + s.Kind.String() + " at " + s.Origin.String() + ". " + expected,
			Action:   "Correct the value passed to --var.",
			Origin:   origin,
		})
		return stamp(value.Unknown(s.Kind, value.SourceVariable)), ds
	}

	switch s.Kind {
	case value.KindInvalid, value.KindString:
		return stamp(value.String(text, value.SourceVariable)), ds
	case value.KindInt:
		n, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			return stamp(value.Int(n, value.SourceVariable)), ds
		}
		// A numeric literal coerces to the declared kind wherever it
		// appears, exactly or not at all (Amendment 4, contract.md) — and
		// the amendment says so explicitly of a SUPPLIED value including
		// --var, not only a declared default or a --var-file entry. Before
		// this fix, "--var size=42.0" against `type: integer` was rejected
		// while a --var-file entry of `size: 42.0` was silently coerced
		// (M4 final review, MINOR 1): the identical literal, judged two
		// different ways depending only on which input carried the text.
		// value.Coerce is the one implementation of "exact or not at all" —
		// reused here rather than re-deriving the float64 round-trip check
		// a second time.
		if f, ferr := strconv.ParseFloat(text, 64); ferr == nil {
			if coerced, ok := value.Coerce(value.Float(f, value.SourceVariable), value.KindInt); ok {
				return stamp(coerced), ds
			}
		}
		return bad("Expected a whole number.")
	case value.KindFloat:
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return bad("Expected a number.")
		}
		return stamp(value.Float(f, value.SourceVariable)), ds
	case value.KindBool:
		b, err := strconv.ParseBool(text)
		if err != nil {
			return bad("Expected true or false.")
		}
		return stamp(value.Bool(b, value.SourceVariable)), ds
	default:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "variable " + strconv.Quote(s.Name) + " cannot be set with --var",
			Detail:   "It is declared as " + s.Kind.String() + " at " + s.Origin.String() + ", and --var carries a single line of text.",
			Action:   "Set " + strconv.Quote(s.Name) + " in variables.yml or in environments/<environment>.yml, where YAML can express " + article(s.Kind) + " " + s.Kind.String() + ".",
			Origin:   origin,
		})
		return stamp(value.Unknown(s.Kind, value.SourceVariable)), ds
	}
}
