package executor

import (
	"errors"
	"reflect"
	"testing"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
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

// TestEventCarriesNoValueType guards the property Event's doc comment
// promises: nothing on the type can bypass value.Format's redaction, because
// there is no value.Value leaf on it to bypass with. The walk is recursive over
// every reachable type, not just direct field types — a later
// map[string]value.Value or []value.Value leaks just as well, and only a
// structural check catches that — and rejects any / interface{} too, since a
// dynamic type is invisible to static analysis.
func TestEventCarriesNoValueType(t *testing.T) {
	// Shapes that would leak, proving the check is not merely a direct-field
	// one. Test structs only; they do not pollute Event itself.
	type withSlice struct{ Values []value.Value }
	type withMap struct{ Attrs map[string]value.Value }
	type withPointer struct{ Val *value.Value }
	type withNested struct {
		Inner struct{ Val value.Value }
	}
	type withAny struct{ Data any }
	type withDirect struct{ Val value.Value }

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

	if containsValueType(reflect.TypeFor[Event]()) {
		t.Errorf("Event contains a value.Value leaf — every field on Event must already be pre-formatted text, or a future OnEvent can print an unredacted secret")
	}
}

// containsValueType reports whether typ can hold a value.Value anywhere in its
// structure: directly, in a container, behind a pointer, nested in a struct, or
// dynamically behind any / interface{}.
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

	if typ == valueType {
		return true
	}

	// Interface{} / any can hold a value.Value at runtime.
	if typ.Kind() == reflect.Interface && typ.NumMethod() == 0 {
		return true
	}

	switch typ.Kind() {
	case reflect.Array, reflect.Slice:
		return containsValueTypeRec(typ.Elem(), visited)
	case reflect.Pointer:
		return containsValueTypeRec(typ.Elem(), visited)
	case reflect.Map:
		return containsValueTypeRec(typ.Key(), visited) || containsValueTypeRec(typ.Elem(), visited)
	case reflect.Struct:
		for field := range typ.Fields() {
			if containsValueTypeRec(field.Type, visited) {
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
