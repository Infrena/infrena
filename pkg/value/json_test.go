package value

import (
	"encoding/json"
	"testing"
)

func roundTrip(t *testing.T, in Value) Value {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Value
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal of %s: %v", data, err)
	}
	return out
}

func TestRoundTripPreservesIntKind(t *testing.T) {
	out := roundTrip(t, Int(100, SourceDefault))
	if out.Kind != KindInt {
		t.Fatalf("Kind = %v, want KindInt (encoding/json would give float64)", out.Kind)
	}
	if got, ok := out.AsInt(); !ok || got != 100 {
		t.Errorf("AsInt() = %d, %v; want 100, true", got, ok)
	}
	if out.Source != SourceDefault {
		t.Errorf("Source = %v, want SourceDefault", out.Source)
	}
}

func TestRoundTripPreservesNestedProvenance(t *testing.T) {
	in := Map(map[string]Value{
		"engine":  String("postgres", SourceExplicit),
		"storage": Int(100, SourceDefault),
		"tags":    List([]Value{String("a", SourceModule)}, SourceModule),
	}, SourceExplicit)

	out := roundTrip(t, in)
	m, ok := out.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Raw is %T, want map[string]Value", out.Raw)
	}
	if m["storage"].Source != SourceDefault {
		t.Errorf("nested Source = %v, want SourceDefault", m["storage"].Source)
	}
	items, ok := m["tags"].Raw.([]Value)
	if !ok || len(items) != 1 || items[0].Source != SourceModule {
		t.Errorf("nested list lost provenance: %#v", m["tags"].Raw)
	}
}

func TestRoundTripPreservesUnknownAndSensitive(t *testing.T) {
	out := roundTrip(t, Unknown(KindString, SourceComputed).WithSensitive(true))
	if out.Known {
		t.Error("unknown must survive as unknown")
	}
	if out.Kind != KindString {
		t.Errorf("Kind = %v, want KindString", out.Kind)
	}
	if !out.Sensitive {
		t.Error("sensitivity must survive persistence")
	}
}

func TestRoundTripPreservesEquality(t *testing.T) {
	in := Bool(true, SourceProvider)
	if !in.Equal(roundTrip(t, in)) {
		t.Error("a persisted value must compare equal to its original, or every plan after a restart shows spurious changes")
	}
}
