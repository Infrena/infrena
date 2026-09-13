package executor

import (
	"errors"
	"reflect"
	"testing"

	"github.com/infrata/infrata/internal/planner"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

func TestEventKindStringNamesEveryKindDistinctly(t *testing.T) {
	cases := []struct {
		kind EventKind
		want string
	}{
		{EventStarted, "started"},
		{EventSucceeded, "succeeded"},
		{EventFailed, "failed"},
		{EventRetrying, "retrying"},
		{EventSkipped, "skipped"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		got := tc.kind.String()
		if got != tc.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
		if seen[got] {
			t.Errorf("%q is returned by more than one EventKind — a renderer keying off this string would merge two different kinds", got)
		}
		seen[got] = true
	}
}

// TestEventCarriesNoValueType guards the property Event's GoDoc promises:
// nothing on the type can bypass value.Format's redaction because there is
// no value.Value leaf on it to bypass. The walk must be recursive over all
// reachable types, not just direct field types: a future addition of
// map[string]value.Value or []value.Value is a leak waiting to happen, but
// only structural checks can catch it; code review cannot. Rejects value.Value
// anywhere it is reachable from a field (direct, in containers, behind
// pointers, nested in structs), and rejects any / interface{} because
// dynamic types are invisible to static analysis.
func TestEventCarriesNoValueType(t *testing.T) {
	// First, prove the shallow check would fail on shapes that will leak.
	// These are test structs only — they don't pollute Event itself.
	type withSlice struct{ Values []value.Value }
	type withMap struct{ Attrs map[string]value.Value }
	type withPointer struct{ Val *value.Value }
	type withNested struct {
		Inner struct{ Val value.Value }
	}
	type withAny struct{ Data any }
	type withDirect struct{ Val value.Value }

	// Test that all these shapes would be caught by a proper recursive check.
	leakyShapes := []struct {
		name string
		typ  reflect.Type
	}{
		{"direct value.Value", reflect.TypeFor[withDirect]()},
		{"[]value.Value", reflect.TypeFor[withSlice]()},
		{"map[string]value.Value", reflect.TypeFor[withMap]()},
		{"*value.Value", reflect.TypeFor[withPointer]()},
		{"nested struct with value.Value", reflect.TypeFor[withNested]()},
		{"any field", reflect.TypeFor[withAny]()},
	}

	for _, shape := range leakyShapes {
		if !containsValueType(shape.typ) {
			t.Errorf("guard missed %s — a progress line with this field would leak unredacted secrets", shape.name)
		}
	}

	// Now verify the real Event passes the check.
	if containsValueType(reflect.TypeFor[Event]()) {
		t.Errorf("Event contains a value.Value leaf — every field on Event must already be pre-formatted text, or a future OnEvent can print an unredacted secret")
	}
}

// containsValueType recursively checks if typ can hold a value.Value anywhere
// in its structure. It returns true if:
// - typ is value.Value itself
// - typ is a slice, array, or pointer to something that contains value.Value
// - typ is a map with value.Value as key or value
// - typ is a struct with a field that contains value.Value
// - typ is any / interface{} (dynamic type, invisible to static analysis)
// It tracks visited types to prevent infinite recursion on self-referential types.
func containsValueType(typ reflect.Type) bool {
	return containsValueTypeRec(typ, make(map[reflect.Type]bool))
}

func containsValueTypeRec(typ reflect.Type, visited map[reflect.Type]bool) bool {
	if typ == nil {
		return false
	}

	// Prevent infinite recursion on self-referential types.
	if visited[typ] {
		return false
	}
	visited[typ] = true

	valueType := reflect.TypeFor[value.Value]()

	// Direct match.
	if typ == valueType {
		return true
	}

	// Interface{} / any can hold a value.Value at runtime.
	if typ.Kind() == reflect.Interface && typ.NumMethod() == 0 {
		return true
	}

	// Recursively check container element types.
	switch typ.Kind() {
	case reflect.Array, reflect.Slice:
		return containsValueTypeRec(typ.Elem(), visited)
	case reflect.Pointer:
		return containsValueTypeRec(typ.Elem(), visited)
	case reflect.Map:
		// Both key and value must be checked.
		return containsValueTypeRec(typ.Key(), visited) || containsValueTypeRec(typ.Elem(), visited)
	case reflect.Struct:
		// Check all fields recursively.
		for i := 0; i < typ.NumField(); i++ {
			if containsValueTypeRec(typ.Field(i).Type, visited) {
				return true
			}
		}
	}

	return false
}

// TestResultFailedIsKeyedByOpNodeID pins Result.Failed's documented key
// shape against the real OpNode.ID(), not a hand-typed string that could
// drift from what planner.OpNode actually produces.
func TestResultFailedIsKeyedByOpNodeID(t *testing.T) {
	n := planner.OpNode{
		Address: address.Address{Name: "db"},
		Kind:    planner.OpReplace,
		Phase:   planner.PhaseCreate,
	}
	r := Result{Failed: map[string]error{n.ID(): errors.New("boom")}}
	if _, ok := r.Failed["create:db"]; !ok {
		t.Errorf("Failed must be keyed by OpNode.ID() (e.g. %q), got keys %v", n.ID(), keysOf(r.Failed))
	}
}

func keysOf(m map[string]error) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
