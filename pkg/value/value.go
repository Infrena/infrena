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
	Kind      Kind
	Known     bool
	Raw       any
	Source    ValueSource
	Sensitive bool
	// Expr is the expression that will produce this value, set when Known is
	// false because the value depends on a resource that does not exist yet.
	// The executor evaluates it once the dependency has been created.
	Expr   *Expr
	Origin Origin
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

// Equal reports whether two values hold the same datum of the same kind.
// Provenance, sensitivity and origin are deliberately excluded: they describe
// how a value was arrived at, not what the desired state is, so they must never
// cause a plan to show a change.
//
// An unknown value is never equal to anything, including another unknown. The
// planner relies on this: an attribute that cannot be proven unchanged must be
// reported as a change (spec §11).
func (v Value) Equal(other Value) bool {
	if !v.Known || !other.Known {
		return false
	}
	if v.Kind != other.Kind {
		return false
	}
	switch v.Kind {
	case KindList:
		a, _ := v.Raw.([]Value)
		b, _ := other.Raw.([]Value)
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
		a, _ := v.Raw.(map[string]Value)
		b, _ := other.Raw.(map[string]Value)
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
	default:
		return v.Raw == other.Raw
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
