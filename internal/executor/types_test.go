package executor

import (
	"errors"
	"reflect"
	"testing"

	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/value"
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
// no value.Value leaf on it to bypass. A hand-maintained checklist would not
// catch a field added later that reintroduces one; reflection does.
func TestEventCarriesNoValueType(t *testing.T) {
	valueType := reflect.TypeOf(value.Value{})
	typ := reflect.TypeOf(Event{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == valueType {
			t.Errorf("Event.%s is a value.Value — every field on Event must already be pre-formatted text, or a future OnEvent can print an unredacted secret", f.Name)
		}
	}
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
