// Package variables resolves every variable to the value that wins the
// precedence chain, and validates it against its declared schema.
package variables

import (
	"strconv"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// Schema is one variable's declared type and constraints.
//
// Min and Max keep the value.Value shape they were decoded into rather than
// collapsing to float64. An int64 above 2^53 does not survive a round trip
// through float64, so a large integer bound would silently validate the wrong
// thing; and a Value carries Origin, which is what lets a bound diagnostic say
// where the bound was declared.
//
// Kind is KindInvalid for an untyped declaration, which is legal. Every entry
// Schemas builds either has a real Kind or has a default, so a consumer that
// needs a kind to build an unknown from always has one.
type Schema struct {
	Name string
	// Noun is what the user called this declaration: "variable" or "input".
	// A module's `inputs:` is spelled exactly as `variables:`, which is why one
	// Schema serves both, but every diagnostic below reaches a user who wrote
	// one word and not the other.
	//
	// Read it through noun(), never directly, so that a Schema built without it
	// says "variable" rather than putting an empty string in a sentence.
	Noun       string
	Kind       value.Kind
	Default    value.Value
	HasDefault bool
	Min        value.Value
	HasMin     bool
	Max        value.Value
	HasMax     bool
	Origin     value.Origin
}

// noun returns what to call this declaration in a diagnostic, defaulting to
// "variable".
func (s Schema) noun() string {
	if s.Noun == "" {
		return "variable"
	}
	return s.Noun
}

// Schemas builds the schema table from decoded declarations.
//
// The `type:` spelling has already been mapped to a Kind during decoding, which
// is where an unknown spelling is reported, with the line and column it was
// written at. Nothing here re-derives that mapping.
//
// Every problem in every declaration is reported in one pass: a declaration
// with a bad bound keeps its type and loses the bound rather than stopping the
// walk, so a second bad declaration is still reported.
//
// noun is what the user called these declarations: "variable" for infrena.yml's
// `variables:` block, "input" for a module's `inputs:`. The two have the same
// shape, so the word is the one thing that cannot be shared.
func Schemas(decls []config.VariableDecl, noun string) (map[string]Schema, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := make(map[string]Schema, len(decls))

	for _, d := range decls {
		// Every declaration reaching here is kept. One with neither a `type`
		// nor a `default` was already rejected at decode time, at the line the
		// user wrote it; do not add a second check that drops one here.
		s := Schema{Name: d.Name, Noun: noun, Kind: d.Type, Origin: d.Origin}
		// Bounds are copied, not re-validated. Decoding coerced each one to
		// d.Type and rejected every malformed declaration, with the line and
		// column this stage does not have, so a second check here would report
		// one mistake twice and report it worse.
		s.Min, s.HasMin = d.Min, d.HasMin
		s.Max, s.HasMax = d.Max, d.HasMax
		if d.HasDefault {
			s.Default, s.HasDefault = d.Default, true
			// Checked against its own constraints here, once, rather than
			// every time the default wins. The stamped copy is for the
			// diagnostic's wording only; the stored Default stays unstamped,
			// because resolution is the one place that decides what provenance
			// a winning value carries.
			//
			// A default is a value, not part of the declaration's shape, so it
			// goes through the same Validate that checks every supplied value:
			// a `default:` and a `--var` can then never be judged differently.
			// The diagnostic still names the line it was written on, because
			// d.Default carries its own Origin.
			ds.Extend(s.Validate(d.Default.
				WithSource(value.SourceDefault).
				WithScope(value.ScopeBaseConfig)))
		}
		out[d.Name] = s
	}
	return out, ds
}

// compareBounds orders two values in the declared kind: integers as int64,
// floats as float64. Funnelling both through one numeric type would lose an
// int64 above 2^53, which is the whole reason bounds are Values.
//
// Both operands share s.Kind by the time this is called: the bound was coerced
// to the declared type at decode time, Validate has compared the value's Kind
// to the schema's, and checkBounds checks each bound before calling this.
//
// Returns -1, 0 or 1. A pair it cannot read compares as 0, which a caller must
// not treat as "no violation" on its own; checkBounds reports such a pair
// through malformedBound rather than calling this at all.
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
// Kind is a claim about Raw, and this never takes it on trust. A mismatch is
// reachable: a Value can arrive from state, from a provider, or from a test.
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

// show renders a value for a diagnostic. It goes through value.Format, the
// engine's only rendering path, because a bound is as capable of being
// sensitive as anything else. ProseFormatOptions renders it bare rather than
// quoted, for a value embedded in a sentence.
func show(v value.Value) string {
	return value.Format(v, value.ProseFormatOptions)
}

// suppliedBy renders the " supplied by X" clause of a diagnostic, or nothing at
// all when the value carries no provenance.
//
// It goes through value.ScopeLabel, never v.Scope.String(): at
// ScopeCLIOverride the scope label is always "--var", even for a value that
// arrived through --var-file, so the sentence would name a flag the reader
// never typed. ScopeLabel prefers v.SuppliedBy, which names the actual input,
// and is the same rule the plan renderer applies.
//
// A value can genuinely carry no scope: decoding stamps Source and leaves
// Scope alone, so a literal written in a configuration file arrives at
// ScopeUnset, whose label reads as a noun — "The value supplied by unset is a
// string." A module input is the first value to reach this validator that way.
// Stamping it with a scope so the sentence reads well would invent a claim
// about where the value came from; saying less is the honest fix.
func suppliedBy(v value.Value) string {
	if v.Scope == value.ScopeUnset {
		return ""
	}
	return " supplied by " + value.ScopeLabel(v)
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

// Coerce normalises v to s's declared kind before it is judged.
//
// It reports only a lossy numeric conversion. A kind mismatch that is not a
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
			Summary:  s.noun() + " " + strconv.Quote(s.Name) + " cannot be stored as " + article(s.Kind) + " " + s.Kind.String(),
			Detail: "The value" + suppliedBy(v) + " is " + show(v) +
				", which cannot be converted to " + s.Kind.String() + " without changing it. " +
				strconv.Quote(s.Name) + " is declared at " + s.Origin.String() + ".",
			Action: "Write a value that is exactly representable as " + article(s.Kind) + " " + s.Kind.String() + ", or change the declared type.",
			Origin: originOr(v.Origin, s.Origin),
		})
		return v, ds
	}
	return out, ds
}

// isNumericKind reports whether k is one of the two numeric kinds. It looks
// only at a Kind and never at a datum, so it is safe to call on an unknown —
// unlike numericDatumMatchesKind, which asks whether a datum matches the kind
// it claims.
func isNumericKind(k value.Kind) bool {
	return k == value.KindInt || k == value.KindFloat
}

// Validate reports every way v violates s.
//
// An untyped declaration constrains nothing: schemas are optional, and one that
// gave no type has said nothing about what values are acceptable. Bounds cannot
// reach an untyped schema, because decoding rejects them.
//
// An unknown value is checked for kind and nothing else. An unknown keeps its
// Kind so type errors surface at plan time rather than apply time, but there is
// no datum to compare against a bound, and inventing one would be a confident
// wrong answer.
//
// The kind switch is an allowlist with no `default` arm that trusts Raw.
// KindInvalid is the zero value of Kind, so a permissive default turns a
// malformed value into a wrong answer. A Value whose Raw does not match its
// Kind is reported, not interpreted.
func (s Schema) Validate(v value.Value) diag.Diagnostics {
	var ds diag.Diagnostics

	if s.Kind == value.KindInvalid {
		return ds
	}

	if v.Kind != s.Kind {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  s.noun() + " " + strconv.Quote(s.Name) + " must be " + article(s.Kind) + " " + s.Kind.String(),
			Detail: "Declared as " + s.Kind.String() + " at " + s.Origin.String() +
				". The value" + suppliedBy(v) + " is " + article(v.Kind) + " " + v.Kind.String() + ".",
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
		// Kind is the whole constraint: there is no element type for a list or
		// a map, and adding one would be a language extension rather than a
		// validation detail.
	default:
		ds.Add(malformed(s, v))
	}
	return ds
}

// checkBounds reports a value outside its schema's inclusive range: `min: 1`
// permits 1. Comparison happens in the declared kind; see compareBounds.
//
// Each bound is checked against s.Kind before it is compared against v,
// because compareBounds answers "no violation" for a pair it cannot read,
// which on an untrustworthy bound would silently accept an out-of-range value.
// Configuration cannot produce that — a declared bound is coerced to its
// variable's Type at decode time — so reaching it means a Schema was built by
// hand with an inconsistent bound.
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
// it, and where the bound was declared — the last being the one a reader
// cannot reconstruct for themselves.
func boundDiag(s Schema, relation string, bound value.Value, v value.Value) diag.Diagnostic {
	limit := show(bound)
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  s.noun() + " " + strconv.Quote(s.Name) + " must be " + relation + " " + limit,
		Detail: "The value" + suppliedBy(v) + " is " + show(v) + ". The bound is declared at " +
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
		Summary:  s.noun() + " " + strconv.Quote(s.Name) + " holds a malformed value",
		Detail:   "It claims kind " + v.Kind.String() + " but its datum does not match. This is an internal error.",
		Action:   "Report this, with the configuration that produced it.",
		Origin:   originOr(v.Origin, s.Origin),
	}
}

// malformedBound is malformed's sibling for a schema whose declared bound does
// not match its own declared Kind. Worded as the internal error it is, because
// configuration cannot produce it: every declared bound is coerced to its
// variable's Type at decode time, so reaching this means a Schema was
// constructed by hand.
func malformedBound(s Schema, which string, bound value.Value) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  s.noun() + " " + strconv.Quote(s.Name) + "'s " + which + " bound does not match its declared kind",
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
// conversion is the whole of the mini-language this system has, and it stops at
// scalars deliberately: `--var tags=a,b,c` would need a separator convention,
// an escape for it, and then a nesting syntax.
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
	// SuppliedBy reuses origin.File rather than a second literal "--var".
	// Origin itself does not survive to the renderer — evaluating a ${var.x}
	// reference re-origins the value to the site that used it — which is why
	// SuppliedBy needs its own stamp rather than trusting Origin.
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
		// A numeric literal coerces to the declared kind wherever it appears,
		// exactly or not at all, and that includes a --var: without this,
		// `--var size=42.0` against `type: integer` is rejected while a
		// --var-file entry of `size: 42.0` is coerced — the identical literal
		// judged two ways depending on which input carried it. value.Coerce is
		// the one implementation of "exact or not at all".
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
			Summary:  s.noun() + " " + strconv.Quote(s.Name) + " cannot be set with --var",
			Detail:   "It is declared as " + s.Kind.String() + " at " + s.Origin.String() + ", and --var carries a single line of text.",
			Action:   "Set " + strconv.Quote(s.Name) + " in variables.yml or in environments/<environment>.yml, where YAML can express " + article(s.Kind) + " " + s.Kind.String() + ".",
			Origin:   origin,
		})
		return stamp(value.Unknown(s.Kind, value.SourceVariable)), ds
	}
}
