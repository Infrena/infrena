package value

import "fmt"

// ValueSource records where a resolved value came from.
//
// Provenance is what lets a plan mark a value as a default, lets import
// generate minimal configuration, and lets `infrena explain` describe
// behaviour accurately.
type ValueSource string

const (
	// SourceExplicit is a value written in configuration.
	SourceExplicit ValueSource = "explicit"
	// SourceDefault is a value a provider's schema supplied because
	// configuration did not.
	SourceDefault ValueSource = "default"
	// SourceEnvironment is a value an environment's overrides supplied.
	SourceEnvironment ValueSource = "environment"
	// SourceVariable is a value that came from a variable.
	SourceVariable ValueSource = "variable"
	// SourceModule is a value a module call passed to its module.
	SourceModule ValueSource = "module"
	// SourceComputed is a value the provider will choose during apply, and
	// which is therefore unknown until then.
	SourceComputed ValueSource = "computed"
	// SourceProvider is a value a provider reported for a resource that
	// exists.
	SourceProvider ValueSource = "provider"
)

// Origin locates a value in the source configuration, including the module
// instantiation chain.
//
// Modules are flattened at compile time, so error messages depend entirely on
// this to say which module a problem came from.
type Origin struct {
	File   string
	Line   int
	Column int
	Module []string
}

// InModule returns the origin as seen from inside a module instantiation,
// prepending name to the module path. It copies the slice so that two
// instantiations of one decoded module source cannot alias each other's path —
// the same guarantee, in the same shape, as address.Address.InModule.
//
// Outermost first, so an Origin's module path reads identically to the Address
// of the resource it belongs to. A diagnostic that spelled the path one way and
// the address another would be two spellings of one thing.
func (o Origin) InModule(name string) Origin {
	next := make([]string, 0, len(o.Module)+1)
	next = append(next, name)
	next = append(next, o.Module...)
	o.Module = next
	return o
}

// String renders the origin as file:line:column, or as the file alone when no
// line is known. A value with no file at all reads as "<generated>".
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
	// SuppliedBy names the input that supplied this value as the user named
	// it: "--var", or a --var-file path exactly as typed. It is separate from
	// Origin because evaluating a variable reference re-origins the value to
	// the site that referenced it, so Origin ends up saying where a value was
	// used rather than where it came from.
	//
	// Only set for values supplied on the command line.
	SuppliedBy string
}

// String builds a known string value.
func String(s string, src ValueSource) Value {
	return Value{Kind: KindString, Known: true, Raw: s, Source: src}
}

// Int builds a known integer value.
func Int(i int64, src ValueSource) Value {
	return Value{Kind: KindInt, Known: true, Raw: i, Source: src}
}

// Float builds a known floating-point value.
func Float(f float64, src ValueSource) Value {
	return Value{Kind: KindFloat, Known: true, Raw: f, Source: src}
}

// Bool builds a known boolean value.
func Bool(b bool, src ValueSource) Value {
	return Value{Kind: KindBool, Known: true, Raw: b, Source: src}
}

// List builds a known list value. Items keep their own provenance.
func List(items []Value, src ValueSource) Value {
	return Value{Kind: KindList, Known: true, Raw: items, Source: src}
}

// Map builds a known map value. Entries keep their own provenance.
func Map(items map[string]Value, src ValueSource) Value {
	return Value{Kind: KindMap, Known: true, Raw: items, Source: src}
}

// Unknown builds a value whose datum is not yet determined but whose type is.
func Unknown(k Kind, src ValueSource) Value {
	return Value{Kind: k, Known: false, Source: src}
}

// WithSensitive returns a copy of the value marked sensitive or not.
func (v Value) WithSensitive(s bool) Value {
	v.Sensitive = s
	return v
}

// WithSource returns a copy of the value attributed to src.
func (v Value) WithSource(src ValueSource) Value {
	v.Source = src
	return v
}

// WithOrigin returns a copy of the value located at o.
func (v Value) WithOrigin(o Origin) Value {
	v.Origin = o
	return v
}

// InModule re-roots this value's origin into the named module instantiation,
// and every leaf's origin with it.
//
// Composites recurse because provenance is per-leaf, and are rebuilt rather
// than stamped in place: one decoded module source is instantiated many times
// and its literals are shared between them.
//
// There is deliberately no Expr counterpart. Parsing stamps a declaration's
// origin onto every expression node built from it, so re-rooting the
// declaration re-roots the whole expression with it.
func (v Value) InModule(name string) Value {
	v.Origin = v.Origin.InModule(name)
	switch raw := v.Raw.(type) {
	case []Value:
		out := make([]Value, len(raw))
		for i, e := range raw {
			out[i] = e.InModule(name)
		}
		v.Raw = out
	case map[string]Value:
		out := make(map[string]Value, len(raw))
		for k, e := range raw {
			out[k] = e.InModule(name)
		}
		v.Raw = out
	}
	return v
}

// WithSuppliedBy returns a copy of the value recorded as supplied by input s:
// "--var", or a --var-file path as typed. See Value.SuppliedBy.
func (v Value) WithSuppliedBy(s string) Value {
	v.SuppliedBy = s
	return v
}

// Equal reports whether two values hold the same datum of the same kind.
//
// Provenance, sensitivity, origin, scope and SuppliedBy are excluded: they
// describe how a value was arrived at rather than what the desired state is,
// and a plan must not show a change for them.
//
// Anything that cannot be proven equal is reported unequal, which is the safe
// direction for a diff. An unknown is never equal to anything, including
// another unknown, and neither is a malformed value whose Raw does not match
// its Kind.
//
// The kind switch is an allowlist rather than a default case comparing Raw
// directly, because == on an uncomparable type panics at run time.
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

// AsString reads a known string value. ok is false for any other kind, and
// for a value that is not yet known.
func (v Value) AsString() (string, bool) {
	s, ok := v.Raw.(string)
	return s, ok && v.Known
}

// AsInt reads a known integer value. ok is false for any other kind,
// including a float, and for a value that is not yet known.
func (v Value) AsInt() (int64, bool) {
	i, ok := v.Raw.(int64)
	return i, ok && v.Known
}

// AsBool reads a known boolean value. ok is false for any other kind, and for
// a value that is not yet known.
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

// Coerce converts v to the given numeric Kind. It lives here rather than at
// each call site so that the exactness rule cannot drift between them.
//
// ok is false, and v is returned unchanged, when the kinds are not a
// cross-kind numeric pair, and when the conversion would lose information: a
// fractional part rounded away, or an int64 above 2^53, which has no exact
// float64 form. A type mismatch is not reported as an error here; the
// caller's own diagnostic already describes it.
//
// Converting to the kind a value already has is a no-op success. On success
// only Kind and Raw change, so a coercion cannot drop Sensitive and leak a
// secret into a plan.
func Coerce(v Value, k Kind) (Value, bool) {
	if v.Kind == k {
		return v, true
	}
	switch {
	case k == KindInt && v.Kind == KindFloat:
		if !v.Known {
			// An unknown has no datum to round, so retyping one is exact.
			// Inside the case rather than above the switch, because that is
			// only true across a numeric pair. Raw is cleared to hold the
			// contract that Raw is nil when Known is false.
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
	// Any other pairing is a type error rather than a lossy conversion, and
	// the caller's own diagnostic reports it.
	return v, false
}
