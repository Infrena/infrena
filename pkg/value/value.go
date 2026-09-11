package value

import "fmt"

// ValueSource records where a resolved value came from. Spec §5.1 and §43 of
// PLAN.md. Provenance is what lets plans mark defaults, import generate minimal
// configuration, and `infra explain` describe behaviour accurately.
type ValueSource string

const (
	SourceExplicit    ValueSource = "explicit"
	SourceDefault     ValueSource = "default"
	SourceEnvironment ValueSource = "environment"
	SourceVariable    ValueSource = "variable"
	SourceModule      ValueSource = "module"
	SourceComputed    ValueSource = "computed"
	SourceProvider    ValueSource = "provider"
)

// Origin locates a value in the source configuration, including the module
// instantiation chain. Compile-time module flattening (spec §7.2) means error
// messages depend entirely on this to say which module a problem came from.
type Origin struct {
	File   string
	Line   int
	Column int
	Module []string
}

func (o Origin) String() string {
	if o.File == "" {
		return "<generated>"
	}
	if o.Line == 0 {
		return o.File
	}
	return fmt.Sprintf("%s:%d:%d", o.File, o.Line, o.Column)
}

// Value is a resolved configuration or state value.
//
// Raw holds a Go value matching Kind: string, int64, float64, bool, []Value or
// map[string]Value. Composites hold Values recursively so provenance is
// per-leaf. When Known is false, Raw is nil.
type Value struct {
	Kind   Kind
	Known  bool
	Raw    any
	Source ValueSource
	// Scope records which precedence level supplied this value; Source
	// records what kind of thing it is. See scope.go.
	Scope     Scope
	Sensitive bool
	// Expr is the expression that will produce this value, set when Known is
	// false because the value depends on a resource that does not exist yet.
	// The executor evaluates it once the dependency has been created.
	Expr   *Expr
	Origin Origin
	// SuppliedBy names the INPUT that supplied this value, as the user
	// named it — "--var" for the flag, or a --var-file path exactly as
	// typed. It exists because Origin does not survive to the renderer:
	// internal/expressions/eval.go's OpVarRef case re-origins every
	// variable reference to the referencing expression's site (the line in
	// infra.yml where ${var} is written), which is correct for diagnostics
	// but means Origin means "where this was referenced" by the time a
	// value reaches a plan, not "where it came from". SuppliedBy is a
	// separate field precisely so that overwrite cannot erase it — see
	// value.Annotate's ScopeCLIOverride special case.
	//
	// Amendment 6 (owner ruling, contract.md), landed after Amendment 5
	// tried to reuse Origin for this and was reverted: a --var-file value
	// rendered "[variable, from --var]" pre-amendment (correct precedence,
	// wrong label) and rendered "[variable, from <infra.yml's own path>]"
	// with Amendment 5 (a value naming its own re-origined reference site).
	// Only meaningful at ScopeCLIOverride today; empty everywhere else.
	SuppliedBy string
}

func String(s string, src ValueSource) Value {
	return Value{Kind: KindString, Known: true, Raw: s, Source: src}
}

func Int(i int64, src ValueSource) Value {
	return Value{Kind: KindInt, Known: true, Raw: i, Source: src}
}

func Float(f float64, src ValueSource) Value {
	return Value{Kind: KindFloat, Known: true, Raw: f, Source: src}
}

func Bool(b bool, src ValueSource) Value {
	return Value{Kind: KindBool, Known: true, Raw: b, Source: src}
}

func List(items []Value, src ValueSource) Value {
	return Value{Kind: KindList, Known: true, Raw: items, Source: src}
}

func Map(items map[string]Value, src ValueSource) Value {
	return Value{Kind: KindMap, Known: true, Raw: items, Source: src}
}

// Unknown builds a value whose datum is not yet determined but whose type is.
func Unknown(k Kind, src ValueSource) Value {
	return Value{Kind: k, Known: false, Source: src}
}

func (v Value) WithSensitive(s bool) Value {
	v.Sensitive = s
	return v
}

func (v Value) WithSource(src ValueSource) Value {
	v.Source = src
	return v
}

func (v Value) WithOrigin(o Origin) Value {
	v.Origin = o
	return v
}

// WithSuppliedBy returns a copy of the value recorded as supplied by input s
// — "--var", or a --var-file path as typed. See Value.SuppliedBy.
func (v Value) WithSuppliedBy(s string) Value {
	v.SuppliedBy = s
	return v
}

// Equal reports whether two values hold the same datum of the same kind.
// Provenance, sensitivity and origin are deliberately excluded: they describe
// how a value was arrived at, not what the desired state is, so they must never
// cause a plan to show a change.
//
// THAT INCLUDES Scope, and this comment is the only thing saying so — the rule
// holds today by construction (the comparisons below touch Known, Kind and Raw
// and nothing else), which means a future field can be added to this method
// with no signpost that it must not be. Two values that differ only in which
// precedence level supplied them are THE SAME VALUE: a `--var replicas=20`
// that matches what variables.yml already said must plan as no change.
// Comparing Scope breaks acceptance invariant 2 (no-op plan) permanently and
// silently, which is the phantom-diff shape M3 spent a Critical fixing.
// Pinned by TestEqualIgnoresScopeForEveryPairOfScopes (scope_test.go).
//
// SAME RULE FOR SuppliedBy (Amendment 6): a `--var replicas=20` and a
// `--var-file f.yml` entry of `replicas: 20` are the same input, whichever
// one supplied it. Pinned by TestEqualIgnoresSuppliedByForEveryScope
// (suppliedby_test.go).
//
// An unknown value is never equal to anything, including another unknown. The
// planner relies on this: an attribute that cannot be proven unchanged must be
// reported as a change (spec §11).
//
// The same conservative rule governs malformed values, and for the same
// reason. A Value whose Raw does not match its Kind cannot be PROVEN equal to
// anything, so it is not. The earlier version discarded the type assertion's
// ok — `a, _ := v.Raw.([]Value)` — which turned every malformed composite into
// an empty one, and two malformed values with entirely different contents
// compared EQUAL. Measured, not assumed:
//
//	KindList with wrong Raw types, different contents: Equal = true
//	KindMap  with wrong Raw types, different contents: Equal = true
//
// Equal is the core of the diff. "Equal" there means the planner emits no
// operation, so a real change to a real resource would silently never be
// planned — acceptance invariant 2 failing in the direction that loses work
// rather than the direction that does too much.
//
// The kind switch is likewise an ALLOWLIST rather than a `default` that
// compares Raw directly. KindInvalid is the zero value of Kind, so a Value
// whose Kind was never set reached `v.Raw == other.Raw`, and == on an
// uncomparable type is a runtime panic, not a compile error:
//
//	PANIC: comparing uncomparable type map[string]value.Value
//
// Equal is called once per attribute per resource on every plan, so that
// panic is reachable from any malformed value anywhere in configuration or
// state.
func (v Value) Equal(other Value) bool {
	if !v.Known || !other.Known {
		return false
	}
	if v.Kind != other.Kind {
		return false
	}
	switch v.Kind {
	case KindList:
		a, aok := v.Raw.([]Value)
		b, bok := other.Raw.([]Value)
		if !aok || !bok {
			return false
		}
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if !a[i].Equal(b[i]) {
				return false
			}
		}
		return true
	case KindMap:
		a, aok := v.Raw.(map[string]Value)
		b, bok := other.Raw.(map[string]Value)
		if !aok || !bok {
			return false
		}
		if len(a) != len(b) {
			return false
		}
		for k, av := range a {
			bv, ok := b[k]
			if !ok || !av.Equal(bv) {
				return false
			}
		}
		return true
	case KindString, KindInt, KindFloat, KindBool:
		return v.Raw == other.Raw
	default:
		// KindInvalid, or a kind added later that nobody taught this method
		// about. Never equal: see the doc comment.
		return false
	}
}

func (v Value) AsString() (string, bool) {
	s, ok := v.Raw.(string)
	return s, ok && v.Known
}

func (v Value) AsInt() (int64, bool) {
	i, ok := v.Raw.(int64)
	return i, ok && v.Known
}

func (v Value) AsBool() (bool, bool) {
	b, ok := v.Raw.(bool)
	return b, ok && v.Known
}

// AsFloat reads a float value. Like AsInt, it is a plain type assertion, so it
// answers false for a KindInt value — that value's Raw is an int64, and YAML
// tags `1` as !!int even where a float was declared. A caller doing arithmetic
// across both numeric kinds must handle int64 itself rather than assume this
// coerces.
func (v Value) AsFloat() (float64, bool) {
	f, ok := v.Raw.(float64)
	return f, ok && v.Known
}

// Coerce converts v to the given numeric Kind, applying the ONE exactness
// rule everywhere a declared numeric kind meets a literal of the other
// numeric kind: internal/config's stage 2 on a variable's bounds, stage 2 on
// its default, and stage 4 on a supplied value resolved against a schema
// (M4 contract Amendment 4). The rule mentions only Kind and Value and must
// never differ between those call sites — a precision bug in one is a
// precision bug in the others — so it lives here once rather than once per
// caller. Contrast pkg/value's OWN Kind-name table (ParseKind, Kind.String):
// those were free to diverge across call sites and so stayed separate; this
// one must never diverge, so it is one function.
//
// ok is false, and v is returned UNCHANGED, in three cases:
//
//   - v is already Kind k: this is then a no-op success (ok=true), not a
//     failure — a caller need not special-case "nothing to coerce".
//   - v.Kind or k is not KindInt or KindFloat, OR they are numeric but not a
//     cross-kind pair (e.g. v is a string where k is KindFloat): this is a
//     TYPE MISMATCH, not a lossy conversion, and Coerce does not report it —
//     it normalises, it does not judge. The caller's own diagnostic says
//     "must be a float"; Coerce saying so too would be a second, competing
//     description of the same problem.
//   - the cross-kind conversion would lose information: a fractional part
//     rounded away converting float to int, or an int64 above 2^53 that does
//     not survive round-tripping through float64 (float64 represents every
//     integer up to 2^53 exactly; past that, adjacent representable values
//     are two apart, so some integers have no exact float64 form).
//
// On success, ONLY Kind and Raw change. Source, Scope, Sensitive, Origin,
// SuppliedBy and Expr all survive untouched — v is taken and returned by
// value, so every field neither this function nor its caller names is
// carried through automatically, including one added to Value after this
// was written. A
// coercion that silently cleared Sensitive would be the M3 Critical again: a
// secret recorded by dropping what was known about it, this time on the way
// into a plan instead of out of a provider.
func Coerce(v Value, k Kind) (Value, bool) {
	if v.Kind == k {
		return v, true
	}
	switch {
	case k == KindInt && v.Kind == KindFloat:
		if !v.Known {
			// An unknown's Kind is a CLAIM about what it will become; there is
			// no datum to round, so retyping one is exact by definition.
			// Without this, AsFloat below reports ok=false for a value holding
			// nothing, and Coerce answers "cannot convert exactly" about a
			// conversion that cannot lose anything. Schema.Coerce then renders
			// that as "the value supplied by ... is (unknown), which cannot be
			// converted to int without changing it" — a complaint about a
			// value that has not arrived yet.
			//
			// Guarded by the surrounding case, not hoisted above the switch:
			// an unknown is exact to retype ONLY across a numeric pair. An
			// unknown string retyped to an int would still be the wrong kind,
			// known or not, and that is a type error the caller reports, not
			// a lossy conversion this function judges. Raw is set to nil
			// explicitly rather than left alone: Value's contract is that Raw
			// is nil when Known is false, and an unknown that kept a stale
			// datum of the OLD kind would be a value whose Raw contradicts
			// its Kind — the exact shape Equal and Format each shipped a bug
			// over.
			v.Kind, v.Raw = k, nil
			return v, true
		}
		f, ok := v.AsFloat()
		if !ok {
			return v, false
		}
		n := int64(f)
		// Exactness both ways: float64(n) == f rejects a fractional part, and
		// it also rejects a float too large to survive the round trip.
		if float64(n) != f {
			return v, false
		}
		v.Kind, v.Raw = KindInt, n
		return v, true

	case k == KindFloat && v.Kind == KindInt:
		if !v.Known {
			// See the symmetric comment in the case above.
			v.Kind, v.Raw = k, nil
			return v, true
		}
		n, ok := v.AsInt()
		if !ok {
			return v, false
		}
		f := float64(n)
		// An int64 above 2^53 does not survive this.
		if int64(f) != n {
			return v, false
		}
		v.Kind, v.Raw = KindFloat, f
		return v, true
	}
	// Any other kind pairing (a non-numeric v, a non-numeric k, or both
	// numeric but neither cross-kind case above) is a type error, not a
	// lossy conversion — Coerce does not report it; the caller's own
	// diagnostic does.
	return v, false
}
