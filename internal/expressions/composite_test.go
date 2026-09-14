package expressions

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

// leafStr builds a string leaf. Named to avoid colliding with funcs_test.go's
// own helper, which this package already has.
func leafStr(s string) value.Value { return value.String(s, value.SourceExplicit) }

// PLAN.md §10.1 — the walk. What happens to a leaf is the caller's; this is
// only that every leaf is reached, structure survives, and keys are left alone.

// upper replaces a leaf with a marker naming what it saw, so a test can assert
// WHICH leaves were visited rather than only that something changed.
func marker(src string, _ value.Origin) value.Value {
	return value.String("<"+src+">", value.SourceExplicit)
}

func TestALeafInsideAMapIsReached(t *testing.T) {
	in := value.Map(map[string]value.Value{
		"environment": leafStr("${var.environment}"),
		"team":        leafStr("payments"),
	}, value.SourceExplicit)

	out := WalkLeaves(in, marker)
	m, ok := out.Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("the walk changed the shape: %#v", out.Raw)
	}
	if got, _ := m["environment"].AsString(); got != "<${var.environment}>" {
		t.Errorf("the interpolated leaf was not reached: %q", got)
	}
	// A leaf with no interpolation is UNTOUCHED, not reparsed. Parsing a literal
	// that happens to contain a brace would change it.
	if got, _ := m["team"].AsString(); got != "payments" {
		t.Errorf("a literal leaf was rewritten: %q", got)
	}
}

func TestNestingWorksToAnyDepth(t *testing.T) {
	in := value.Map(map[string]value.Value{
		"outer": value.List([]value.Value{
			leafStr("plain"),
			value.Map(map[string]value.Value{"deep": leafStr("${var.x}")}, value.SourceExplicit),
		}, value.SourceExplicit),
	}, value.SourceExplicit)

	out := WalkLeaves(in, marker)
	outer := out.Raw.(map[string]value.Value)["outer"].Raw.([]value.Value)
	inner := outer[1].Raw.(map[string]value.Value)
	if got, _ := inner["deep"].AsString(); got != "<${var.x}>" {
		t.Errorf("a leaf three levels down was not reached: %q", got)
	}
	// Refusing depth two while allowing depth one would be a rule nobody could
	// predict, so the shallow sibling must survive too.
	if got, _ := outer[0].AsString(); got != "plain" {
		t.Errorf("a shallow literal was rewritten: %q", got)
	}
}

// TestAKeyIsNeverInterpolated. Keys are literal, so what a file DECLARES can be
// read without resolving anything.
func TestAKeyIsNeverInterpolated(t *testing.T) {
	in := value.Map(map[string]value.Value{
		"${var.notakey}": leafStr("value"),
	}, value.SourceExplicit)

	out := WalkLeaves(in, marker)
	m := out.Raw.(map[string]value.Value)
	if _, ok := m["${var.notakey}"]; !ok {
		t.Errorf("the key was rewritten; a configuration's shape must not depend on a value: %#v", m)
	}
}

// TestTheWalkDoesNotMutateItsInput. The value may be a variable's, shared with
// the scope: mutating it would make a second reference to that variable see the
// resolved result, which would depend on evaluation order.
func TestTheWalkDoesNotMutateItsInput(t *testing.T) {
	inner := map[string]value.Value{"a": leafStr("${var.x}")}
	in := value.Map(inner, value.SourceExplicit)

	_ = WalkLeaves(in, marker)

	if got, _ := inner["a"].AsString(); got != "${var.x}" {
		t.Errorf("the input map was mutated: %q", got)
	}
}

// TestANonCompositeIsReturnedAsIs — an integer has no leaves.
func TestANonCompositeIsReturnedAsIs(t *testing.T) {
	in := value.Int(7, value.SourceExplicit)
	if out := WalkLeaves(in, marker); out.Kind != value.KindInt {
		t.Errorf("an integer became %v", out.Kind)
	}
}

// TestTheLeafGetsItsOwnOrigin. Each leaf was decoded separately and kept its own
// position, which is what lets a diagnostic point at the line inside the map
// rather than at the attribute that contains it.
func TestTheLeafGetsItsOwnOrigin(t *testing.T) {
	leafOrigin := value.Origin{File: "f.yml", Line: 42, Column: 7}
	in := value.Map(map[string]value.Value{
		"a": leafStr("${var.x}").WithOrigin(leafOrigin),
	}, value.SourceExplicit).WithOrigin(value.Origin{File: "f.yml", Line: 1, Column: 1})

	var seen value.Origin
	WalkLeaves(in, func(_ string, o value.Origin) value.Value {
		seen = o
		return value.Value{}
	})
	if seen.Line != 42 {
		t.Errorf("the leaf was handed origin line %d, want its own (42) rather than the map's",
			seen.Line)
	}
}

// TestTheWalkPreservesLeafSensitivity is this milestone's governing rule at the
// layer the walk can actually reach.
//
// pkg/value models sensitivity per leaf and value.Format redacts at that
// granularity. The walk must not flatten either: a map with one secret leaf
// stays a map with one secret leaf, so a plan shows the other keys.
//
// Both halves, because neither alone is enough — a wholly-redacted map hides
// what a reader needs, and a wholly-visible one leaks.
func TestTheWalkPreservesLeafSensitivity(t *testing.T) {
	in := value.Map(map[string]value.Value{
		"team":     leafStr("payments"),
		"password": leafStr("hunter2").WithSensitive(true),
		"resolved": leafStr("${var.x}"),
	}, value.SourceExplicit)

	out := WalkLeaves(in, func(string, value.Origin) value.Value {
		return value.String("resolved-value", value.SourceExplicit)
	})

	rendered := value.Format(out, value.FormatOptions{})
	if strings.Contains(rendered, "hunter2") {
		t.Fatalf("the secret survived rendering: %s", rendered)
	}
	if !strings.Contains(rendered, "payments") {
		t.Errorf("the whole map was redacted, hiding keys a reader needs: %s", rendered)
	}
	if !strings.Contains(rendered, value.Redacted) {
		t.Errorf("the secret leaf was not redacted: %s", rendered)
	}
	// And the walk did its job alongside: the interpolated leaf resolved.
	if !strings.Contains(rendered, "resolved-value") {
		t.Errorf("the interpolated leaf was not replaced: %s", rendered)
	}
}
