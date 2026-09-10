package executor

import (
	"testing"

	"infra/internal/expressions"
	"infra/internal/planner"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// planTimeScope reproduces the compiler's compile-time scope: it resolves
// "environment" the way every real compile does (internal/compiler/bind.go,
// variableScope), and reports every resource attribute as unavailable —
// which is what turns a reference into a deferred unknown carrying its
// expression (spec §6). Building fixtures through the real parser and
// evaluator, rather than a hand-built *value.Expr, is what proves
// resolveAfter is handed the exact shape of Value the compiler actually
// produces.
type planTimeScope struct{ environment string }

func (s planTimeScope) Variable(name string) (value.Value, bool) {
	if name == "environment" {
		return value.String(s.environment, value.SourceEnvironment), true
	}
	return value.Value{}, false
}

func (s planTimeScope) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

// deferredValue parses and evaluates src the way the compiler would, and
// fails the test if the result is anything other than a genuine deferred
// unknown — the only shape resolveAfter is meant to handle.
func deferredValue(t *testing.T, src string) value.Value {
	t.Helper()
	e, ds := expressions.Parse(src, value.Origin{File: "infra.yml", Line: 1})
	if ds.HasErrors() {
		t.Fatalf("Parse(%q): %+v", src, ds)
	}
	v, ds := expressions.Evaluate(e, planTimeScope{environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("Evaluate(%q): %+v", src, ds)
	}
	if v.Known {
		t.Fatalf("Evaluate(%q) is Known; the fixture must be unresolvable at compile time", src)
	}
	return v
}

func TestResolveAfterFillsUnknownFromDependency(t *testing.T) {
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		Type:    "test.application",
		After: map[string]value.Value{
			"network_id": deferredValue(t, "${net.id}"),
		},
	}
	snapshot := map[string]*resource.ResourceState{
		"net": {
			Address:    address.Address{Name: "net"},
			Attributes: map[string]value.Value{"id": value.String("net-1", value.SourceProvider)},
		},
	}

	got, ds := resolveAfter(op, snapshot, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	v := got["network_id"]
	if !v.Known {
		t.Fatal("network_id must be Known once its dependency has been applied")
	}
	if s, _ := v.AsString(); s != "net-1" {
		t.Errorf("network_id = %q, want \"net-1\"", s)
	}
}

func TestResolveAfterLeavesKnownValuesUntouched(t *testing.T) {
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		After: map[string]value.Value{
			"name": value.String("fixed", value.SourceExplicit),
		},
	}

	got, ds := resolveAfter(op, map[string]*resource.ResourceState{}, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := got["name"].AsString(); s != "fixed" {
		t.Errorf("name = %q, want \"fixed\" — an already-Known value must pass through untouched", s)
	}
}

func TestResolveAfterReportsUnresolvedDependencyAsDiagnostic(t *testing.T) {
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		After: map[string]value.Value{
			"network_id": deferredValue(t, "${net.id}"),
		},
	}

	// The dependency is missing from the snapshot entirely — the case a bug
	// in dependency ordering (spec §14) would produce.
	got, ds := resolveAfter(op, map[string]*resource.ResourceState{}, "dev")
	if !ds.HasErrors() {
		t.Fatal("expected a diagnostic: network_id cannot resolve without its dependency")
	}
	if got["network_id"].Known {
		t.Error("network_id must remain unknown, not silently fabricated")
	}
}

func TestResolveAfterMixesVariableAndResourceReference(t *testing.T) {
	// Unknownness is contagious (spec §6): the whole concat goes unknown
	// because net.id is unresolvable at compile time, even though
	// "environment" resolves immediately. The stored Expr is the ORIGINAL
	// tree, so re-evaluating it at apply time must resolve BOTH the
	// variable and the resource reference again — this is the test that
	// proves runtimeScope.Variable actually does that (Task 3 folds resolved parts into the deferred expression, so a residual
	// contains no variable reference and this scope needs no variables.)
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		After: map[string]value.Value{
			"name": deferredValue(t, "${environment}-${net.id}"),
		},
	}
	snapshot := map[string]*resource.ResourceState{
		"net": {
			Address:    address.Address{Name: "net"},
			Attributes: map[string]value.Value{"id": value.String("net-1", value.SourceProvider)},
		},
	}

	got, ds := resolveAfter(op, snapshot, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := got["name"].AsString(); s != "dev-net-1" {
		t.Errorf("name = %q, want \"dev-net-1\"", s)
	}
}

func TestResolveAfterUsesLiveSnapshotNotPlanTimeBefore(t *testing.T) {
	// Before carries what the plan diffed against — a stale, plan-time
	// snapshot. resolveAfter must never read it: only op.After (the
	// expression) and the live snapshot (the dependency's real, post-apply
	// attributes) may determine the resolved value.
	op := &planner.Operation{
		Address: address.Address{Name: "app"},
		Before: map[string]value.Value{
			"network_id": value.String("stale-would-be-wrong", value.SourceComputed),
		},
		After: map[string]value.Value{
			"network_id": deferredValue(t, "${net.id}"),
		},
	}
	snapshot := map[string]*resource.ResourceState{
		"net": {
			Address:    address.Address{Name: "net"},
			Attributes: map[string]value.Value{"id": value.String("net-1", value.SourceProvider)},
		},
	}

	got, ds := resolveAfter(op, snapshot, "dev")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := got["network_id"].AsString(); s != "net-1" {
		t.Errorf("network_id = %q, want the live value \"net-1\", not Before's stale one", s)
	}
}
