package value

import "testing"

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
