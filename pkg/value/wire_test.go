package value

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestKindWireNamesAreFrozen pins the persisted spelling of every Kind.
//
// These names are an on-disk contract. Changing one silently invalidates every
// state file ever written, with no version bump and no error — the decoder just
// reports "unknown value kind". The literals below are duplicated deliberately:
// deriving them from kindWireNames would assert nothing.
func TestKindWireNamesAreFrozen(t *testing.T) {
	frozen := map[Kind]string{
		KindString: "string",
		KindInt:    "integer",
		KindFloat:  "float",
		KindBool:   "boolean",
		KindList:   "list",
		KindMap:    "map",
	}

	if len(kindWireNames) != len(frozen) {
		t.Fatalf("kindWireNames has %d entries, frozen contract has %d; a new Kind needs a state version bump", len(kindWireNames), len(frozen))
	}
	for kind, want := range frozen {
		got, err := kindToWireName(kind)
		if err != nil {
			t.Errorf("kindToWireName(%v): %v", kind, err)
			continue
		}
		if got != want {
			t.Errorf("kind %v persists as %q, want %q", kind, got, want)
		}
		back, err := kindFromWireName(want)
		if err != nil || back != kind {
			t.Errorf("kindFromWireName(%q) = %v, %v; want %v, nil", want, back, err, kind)
		}
	}
}

// TestKindWireNameIsIndependentOfString is the structural half of the same
// guard: the persisted name must come from the frozen table, never from
// Kind.String(), which is now the configuration language's spelling and
// answers for KindInvalid where this table must refuse it.
//
// The two spellings agree today, so no round trip can tell them apart. What can
// is that kindToWireName rejects a Kind absent from the table, whereas
// Kind.String() answers "invalid" for it and would have written that to disk.
func TestKindWireNameIsIndependentOfString(t *testing.T) {
	if _, err := kindToWireName(KindInvalid); err == nil {
		t.Fatal("kindToWireName accepted KindInvalid; the encoding is still falling back to Kind.String()")
	}
	if KindInvalid.String() != "invalid" {
		t.Fatalf("Kind.String() for KindInvalid = %q; this test assumes it answers where the table does not", KindInvalid.String())
	}

	if _, err := json.Marshal(Value{Kind: KindInvalid, Known: true, Raw: "x"}); err == nil {
		t.Error("marshalling a KindInvalid value must fail rather than write a kind the decoder cannot read")
	}
}

// TestValueWireShapeIsFrozen pins the JSON object a Value serialises to.
func TestValueWireShapeIsFrozen(t *testing.T) {
	data, err := json.Marshal(Int(42, SourceDefault).WithSensitive(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"kind":"integer","known":true,"raw":42,"source":"default","sensitive":true}`
	if strings.TrimSpace(string(data)) != want {
		t.Errorf("value wire shape changed.\n got %s\nwant %s", data, want)
	}
}
