package value

import (
	"strings"
	"testing"
)

func TestConstructorsRecordKindAndSource(t *testing.T) {
	v := String("postgres", SourceExplicit)
	if v.Kind != KindString {
		t.Errorf("Kind = %v, want KindString", v.Kind)
	}
	if v.Source != SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit", v.Source)
	}
	if !v.Known {
		t.Error("a literal value must be Known")
	}
	if got, ok := v.AsString(); !ok || got != "postgres" {
		t.Errorf("AsString() = %q, %v; want \"postgres\", true", got, ok)
	}
}

func TestUnknownCarriesKindButNoValue(t *testing.T) {
	v := Unknown(KindString, SourceComputed)
	if v.Known {
		t.Error("Unknown must not be Known")
	}
	if v.Kind != KindString {
		t.Errorf("Kind = %v, want KindString — unknown values must keep their kind so type checking still runs", v.Kind)
	}
	if v.Raw != nil {
		t.Errorf("Raw = %v, want nil", v.Raw)
	}
}

func TestEqualIgnoresProvenanceAndOrigin(t *testing.T) {
	a := String("postgres", SourceExplicit)
	b := String("postgres", SourceDefault)
	b.Origin = Origin{File: "other.yml", Line: 9}
	if !a.Equal(b) {
		t.Error("Equal must compare kind and datum only; provenance and origin are not part of desired state")
	}
	if a.Equal(String("mysql", SourceExplicit)) {
		t.Error("different data must not be equal")
	}
}

func TestUnknownIsNeverEqual(t *testing.T) {
	u := Unknown(KindString, SourceComputed)
	if u.Equal(String("x", SourceExplicit)) || u.Equal(Unknown(KindString, SourceComputed)) {
		t.Error("an unknown value can never be proven equal to anything — spec §11")
	}
}

func TestCompositesHoldValuesRecursively(t *testing.T) {
	m := Map(map[string]Value{
		"engine":  String("postgres", SourceExplicit),
		"storage": Int(100, SourceDefault),
	}, SourceExplicit)

	raw, ok := m.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Map Raw is %T, want map[string]Value — provenance must survive per leaf", m.Raw)
	}
	if raw["storage"].Source != SourceDefault {
		t.Error("per-leaf provenance was lost")
	}
}

func TestWithSensitiveIsSticky(t *testing.T) {
	v := String("hunter2", SourceVariable).WithSensitive(true)
	if !v.Sensitive {
		t.Error("WithSensitive(true) must mark the value sensitive")
	}
	if v.WithSource(SourceModule).Sensitive == false {
		t.Error("changing source must not clear sensitivity")
	}
}

// TestEqualFailsClosedOnMalformedValues pins the conservative rule for values
// whose Raw does not match their Kind. Both behaviours below were measured
// against the previous implementation before this test existed: the composite
// cases returned true, and the KindInvalid case panicked.
//
// Equal is the core of the diff, so "equal" means the planner emits no
// operation. A false equal is a real change that is never planned.
func TestEqualFailsClosedOnMalformedValues(t *testing.T) {
	cases := []struct {
		name string
		a, b Value
	}{
		{
			// Previously true: the discarded type assertion made both look empty.
			name: "list kind, wrong Raw type, different contents",
			a:    Value{Kind: KindList, Known: true, Raw: []any{"one"}},
			b:    Value{Kind: KindList, Known: true, Raw: []any{"two", "three"}},
		},
		{
			name: "map kind, wrong Raw type, different contents",
			a:    Value{Kind: KindMap, Known: true, Raw: map[string]any{"k": "v1"}},
			b:    Value{Kind: KindMap, Known: true, Raw: map[string]any{"k": "v2", "j": "x"}},
		},
		{
			name: "one side well-formed, the other not",
			a:    Value{Kind: KindList, Known: true, Raw: []Value{String("x", SourceExplicit)}},
			b:    Value{Kind: KindList, Known: true, Raw: []any{"x"}},
		},
		{
			// Previously panicked: == on an uncomparable type.
			name: "kind left at the zero value with a composite Raw",
			a:    Value{Known: true, Raw: map[string]Value{"password": String("s", SourceProvider)}},
			b:    Value{Known: true, Raw: map[string]Value{"password": String("s", SourceProvider)}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Must not panic, and must not claim equality it cannot prove.
			if tc.a.Equal(tc.b) {
				t.Error("a value whose Raw does not match its Kind must never compare equal — " +
					"the planner reads equality as 'no operation', so a false equal loses a real change")
			}
			if tc.b.Equal(tc.a) {
				t.Error("Equal must be symmetric in failing closed")
			}
		})
	}
}

// TestEqualStillComparesWellFormedValues guards the other direction: failing
// closed must not make ordinary equal values compare unequal.
func TestEqualStillComparesWellFormedValues(t *testing.T) {
	if !String("eu-west-1", SourceExplicit).Equal(String("eu-west-1", SourceDefault)) {
		t.Error("identical strings must be equal regardless of provenance")
	}
	if !Int(20, SourceExplicit).Equal(Int(20, SourceProvider)) {
		t.Error("identical ints must be equal regardless of provenance")
	}
	list := func() Value {
		return Value{Kind: KindList, Known: true, Raw: []Value{String("a", SourceExplicit), Int(1, SourceExplicit)}}
	}
	if !list().Equal(list()) {
		t.Error("identical well-formed lists must be equal")
	}
	m := func() Value {
		return Value{Kind: KindMap, Known: true, Raw: map[string]Value{"a": String("x", SourceExplicit)}}
	}
	if !m().Equal(m()) {
		t.Error("identical well-formed maps must be equal")
	}
	if String("a", SourceExplicit).Equal(String("b", SourceExplicit)) {
		t.Error("different strings must not be equal")
	}
}

func TestCoerceRetypesAnUnknown(t *testing.T) {
	// An unknown carries a Kind as a CLAIM about what it will become, with no
	// datum to lose, so retyping one is exact by definition. The shipped
	// Coerce falls through to AsInt/AsFloat, which report ok=false for a value
	// with no datum, so it currently answers "cannot convert exactly" to a
	// conversion that cannot lose anything.
	got, ok := Coerce(Unknown(KindInt, SourceVariable), KindFloat)
	if !ok {
		t.Fatal("an unknown has no datum to lose; retyping it is exact")
	}
	if got.Kind != KindFloat {
		t.Errorf("Kind = %v, want KindFloat", got.Kind)
	}
	if got.Known {
		t.Error("it must stay unknown: Coerce changes a value's type, never whether it is known")
	}
	if got.Raw != nil {
		t.Errorf("Raw = %v, want nil — an unknown holds no datum (see Value's doc comment)", got.Raw)
	}
}

func TestCoerceLeavesAnUnknownOfANonNumericKindAlone(t *testing.T) {
	// The other direction. Retyping is exact only because there is no datum;
	// it is not a licence to reinterpret an unknown string as a number, which
	// would be a type error whether or not the datum had arrived yet.
	if _, ok := Coerce(Unknown(KindString, SourceVariable), KindInt); ok {
		t.Error("an unknown string is still the wrong kind for an integer; the caller reports that, not Coerce")
	}
}

// TestOriginInModuleDoesNotAliasBetweenInstantiations is the property the whole
// primitive exists for. ONE decoded module source is instantiated many times,
// and every instantiation re-roots the SAME origins. Appending in place would
// let the second instantiation extend the first's path, so a diagnostic would
// name a module the user never wrote there.
func TestOriginInModuleDoesNotAliasBetweenInstantiations(t *testing.T) {
	shared := Origin{File: "module.yml", Line: 3, Column: 5}

	one := shared.InModule("one")
	two := shared.InModule("two")

	if got := strings.Join(one.Module, "."); got != "one" {
		t.Errorf("first instantiation = %q, want %q", got, "one")
	}
	if got := strings.Join(two.Module, "."); got != "two" {
		t.Errorf("second instantiation = %q, want %q — the two aliased", got, "two")
	}
	if shared.Module != nil {
		t.Errorf("the shared origin was mutated: %v", shared.Module)
	}

	// Nesting reads outermost-first, matching the address it belongs to.
	nested := shared.InModule("inner").InModule("outer")
	if got := strings.Join(nested.Module, "."); got != "outer.inner" {
		t.Errorf("nested = %q, want %q — an origin's path must read like the address", got, "outer.inner")
	}
}

// TestValueInModuleRecursesIntoComposites. Provenance is per-leaf, so a map with
// one bad key produces a diagnostic pointing at THAT key's origin. An origin
// that stopped at the outer Value could not say which instantiation it came
// from, which is the whole point of stamping.
func TestValueInModuleRecursesIntoComposites(t *testing.T) {
	leaf := String("x", SourceExplicit).WithOrigin(Origin{File: "module.yml", Line: 9})
	m := Map(map[string]Value{"k": leaf}, SourceExplicit).WithOrigin(Origin{File: "module.yml", Line: 8})

	got := m.InModule("net")

	if p := strings.Join(got.Origin.Module, "."); p != "net" {
		t.Errorf("outer origin = %q, want %q", p, "net")
	}
	inner, ok := got.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("map lost its shape: %T", got.Raw)
	}
	if p := strings.Join(inner["k"].Origin.Module, "."); p != "net" {
		t.Errorf("leaf origin = %q, want %q — a per-leaf diagnostic could not name the instantiation", p, "net")
	}
	// The source value must be untouched: it is shared between instantiations.
	orig := m.Raw.(map[string]Value)
	if orig["k"].Origin.Module != nil {
		t.Errorf("the source value was stamped in place: %v", orig["k"].Origin.Module)
	}
}
