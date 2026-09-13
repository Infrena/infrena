package planner

import (
	"bytes"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/refresh"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
	testprovider "github.com/infrata/infrata/providers/test"
)

// addr and str come from plan_test.go; do not redeclare them.

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register("test", testprovider.New(t.TempDir()+"/fake-cloud.json")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// planOpts fixes the clock so that the artifact is reproducible. It is named
// planOpts rather than opts so it cannot shadow Compute's parameter.
func planOpts(t *testing.T) Options {
	t.Helper()
	return Options{
		Environment: "dev",
		Now:         func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
		Registry:    testRegistry(t),
	}
}

func config(resources ...*resource.ResolvedResource) compiler.ResolvedConfig {
	c := compiler.ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources:   map[string]*resource.ResolvedResource{},
	}
	for _, r := range resources {
		c.Resources[r.Address.String()] = r
	}
	return c
}

func configured(name, typ string, attrs map[string]value.Value) *resource.ResolvedResource {
	return &resource.ResolvedResource{Address: addr(name), Type: typ, Provider: "test", Attrs: attrs}
}

// recorded builds a state entry. Its attributes carry SourceProvider so that
// every comparison in these tests also proves Equal ignores provenance.
func recorded(name, typ string, attrs map[string]value.Value) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    addr(name),
		Type:       typ,
		Provider:   "test",
		ProviderID: name + "-1",
		Attributes: attrs,
	}
}

func stateOf(resources ...*resource.ResourceState) *state.State {
	st := state.New("myapp", "dev")
	st.Serial = 7
	for _, r := range resources {
		st.Set(r)
	}
	return st
}

func present(resources ...*resource.ResourceState) refresh.Observations {
	obs := refresh.Observations{}
	for _, r := range resources {
		obs[r.Address.String()] = refresh.Observation{Address: r.Address, State: r}
	}
	return obs
}

func absent(addrs ...address.Address) refresh.Observations {
	obs := refresh.Observations{}
	for _, a := range addrs {
		obs[a.String()] = refresh.Observation{Address: a}
	}
	return obs
}

func merge(sets ...refresh.Observations) refresh.Observations {
	out := refresh.Observations{}
	for _, set := range sets {
		maps.Copy(out, set)
	}
	return out
}

func only(t *testing.T, p *Plan) Operation {
	t.Helper()
	if len(p.Operations) != 1 {
		t.Fatalf("expected exactly one operation, got %d: %+v", len(p.Operations), p.Operations)
	}
	return p.Operations[0]
}

func rendered(t *testing.T, ds diag.Diagnostics) string {
	t.Helper()
	var out strings.Builder
	ds.Render(&out)
	return out.String()
}

func provAttr(v value.Value) value.Value { return v.WithSource(value.SourceProvider) }

// --- one test per row of spec §11's decision table -------------------------

func TestInConfigNotInStateIsCreate(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	p, ds := Compute(cfg, stateOf(), refresh.Observations{}, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	op := only(t, p)
	if op.Kind != OpCreate {
		t.Fatalf("Kind = %s, want create", op.Kind)
	}
	if op.Before != nil {
		t.Error("Before must be nil for a create — spec §12.1")
	}
	if _, ok := op.After["cidr"]; !ok {
		t.Error("After must carry the configured attributes")
	}
	id, ok := op.After["id"]
	if !ok || id.Known {
		t.Error("a computed attribute must appear in After as unknown: it is known after apply")
	}
}

func TestNoDifferencesIsNoOp(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
		"id":   provAttr(str("net-1")),
	})

	p, ds := Compute(cfg, stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	op := only(t, p)
	if op.Kind != OpNoOp {
		t.Fatalf("Kind = %s, want noop (reasons: %+v)", op.Kind, op.Reasons)
	}
	if len(op.Reasons) != 0 {
		t.Errorf("a noop has no reasons, got %+v", op.Reasons)
	}
	if p.HasChanges() {
		t.Error("invariant 2: desired equals actual, so the plan proposes no changes")
	}
}

func TestUpdatableDifferenceIsUpdate(t *testing.T) {
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
		"size":   value.Int(20, value.SourceExplicit),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"size":     value.Int(10, value.SourceProvider),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "size" {
		t.Fatalf("Reasons = %+v, want one naming size", op.Reasons)
	}
	if op.Reasons[0].ForceNew {
		t.Error("size is updatable in place; it must not be marked ForceNew")
	}
	if op.Before == nil || op.After == nil {
		t.Error("an update populates both Before and After — spec §12.1")
	}
}

func TestForceNewDifferenceIsReplace(t *testing.T) {
	// engine is ForceNew on test.database.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
		"size":   value.Int(10, value.SourceExplicit),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"size":     value.Int(10, value.SourceProvider),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpReplace {
		t.Fatalf("Kind = %s, want replace — a changed ForceNew attribute promotes update to replace", op.Kind)
	}
	var forced *ChangeReason
	for i := range op.Reasons {
		if op.Reasons[i].ForceNew {
			forced = &op.Reasons[i]
		}
	}
	if forced == nil {
		t.Fatalf("Reasons = %+v, want one marked ForceNew so the plan can say what forced the replacement", op.Reasons)
	}
	if forced.Attribute != "engine" {
		t.Errorf("forcing attribute = %q, want engine", forced.Attribute)
	}
}

func TestObservedAbsentIsRecreate(t *testing.T) {
	// In config, in state, but the provider no longer has it: something
	// deleted it outside infra.
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(cfg, stateOf(live), absent(addr("net")), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpCreate {
		t.Fatalf("Kind = %s, want create — a resource deleted outside infra is recreated", op.Kind)
	}
	if op.Before != nil {
		t.Error("Before must be nil for a create, recreation included")
	}
	if len(op.Reasons) == 0 || !strings.Contains(op.Reasons[0].Note, "recreated") {
		t.Errorf("Reasons = %+v, want one explaining the recreation", op.Reasons)
	}
}

func TestNotInConfigButPresentIsDestroy(t *testing.T) {
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpDestroy {
		t.Fatalf("Kind = %s, want destroy — invariant 1", op.Kind)
	}
	if op.After != nil {
		t.Error("After must be nil for a destroy — spec §12.1")
	}
	if _, ok := op.Before["cidr"]; !ok {
		t.Error("Before must carry what is about to be destroyed")
	}
}

func TestNotInConfigAndAbsentIsForget(t *testing.T) {
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(config(), stateOf(live), absent(addr("net")), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpForget {
		t.Fatalf("Kind = %s, want forget — it is already gone, so only the state entry remains", op.Kind)
	}
	if op.After != nil {
		t.Error("After must be nil for a forget")
	}
}

// --- one test per rule that is a decision rather than a mechanic -----------

func TestUnknownDesiredValueIsAnUpdate(t *testing.T) {
	// Rule 1. An unknown cannot be proven unchanged, so it must show as a
	// change; treating it as unchanged under-reports, invisibly until apply.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine":  str("postgres"),
		"network": value.Unknown(value.KindString, value.SourceComputed),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":  provAttr(str("postgres")),
		"network": provAttr(str("net-1")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "network" {
		t.Fatalf("Reasons = %+v, want one naming network", op.Reasons)
	}
	if op.Reasons[0].Note != "known after apply" {
		t.Errorf("Note = %q, want \"known after apply\" — the reason must say why, not merely that", op.Reasons[0].Note)
	}
}

func TestUnknownInsideACompositeIsAnUpdate(t *testing.T) {
	// Rule 1, the case that is easy to miss: a map is Known even when one of
	// its entries is not, so a top-level check alone reports the wrong reason.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
		"tags": value.Map(map[string]value.Value{
			"env":     str("dev"),
			"release": value.Unknown(value.KindString, value.SourceComputed),
		}, value.SourceExplicit),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
		"tags": value.Map(map[string]value.Value{
			"env":     provAttr(str("dev")),
			"release": provAttr(str("v1")),
		}, value.SourceProvider),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "tags" {
		t.Fatalf("Reasons = %+v, want one naming tags", op.Reasons)
	}
	if op.Reasons[0].Note != "known after apply" {
		t.Errorf("Note = %q, want \"known after apply\" — the unknown check must recurse into composites", op.Reasons[0].Note)
	}
}

// TestUnknownForceNewAttributeIsAReplace pins the most destructive
// consequence of rule 1. Both tests above use non-ForceNew attributes, so
// neither exercises the case that actually matters: a value that cannot be
// proven unchanged always counts as a change, and when that value sits on a
// ForceNew attribute the change is a replacement — a live resource destroyed
// and recreated because the planner could not prove nothing changed.
func TestUnknownForceNewAttributeIsAReplace(t *testing.T) {
	// engine is ForceNew on test.database.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": value.Unknown(value.KindString, value.SourceComputed),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpReplace {
		t.Fatalf("Kind = %s, want replace — an unknown ForceNew attribute forces a replacement", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "engine" {
		t.Fatalf("Reasons = %+v, want one naming engine", op.Reasons)
	}
	if !op.Reasons[0].ForceNew {
		t.Error("the reason must be marked ForceNew so the plan can say what forced the replacement")
	}
	if op.Reasons[0].Note != "known after apply" {
		t.Errorf("Note = %q, want \"known after apply\"", op.Reasons[0].Note)
	}
}

func TestComputedAttributesDoNotDriveADiff(t *testing.T) {
	// Rule 2. id and endpoint are provider outputs, not desired state.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpNoOp {
		t.Fatalf("Kind = %s, want noop — a computed attribute absent from configuration is an output (reasons: %+v)", op.Kind, op.Reasons)
	}
	if got, ok := op.After["endpoint"]; !ok || !got.Known {
		t.Error("an in-place operation carries the computed attribute across; it survives")
	}
}

func TestRemovedAttributeIsAnUpdate(t *testing.T) {
	// The other half of rule 2: an attribute the schema defines, that is not
	// computed, and that configuration no longer sets, is a change.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("postgres"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"password": provAttr(str("s3cret")).WithSensitive(true),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "password" {
		t.Fatalf("Reasons = %+v, want exactly one naming password — endpoint is computed and must not appear", op.Reasons)
	}
}

func TestReplaceMarksComputedAttributesUnknown(t *testing.T) {
	// Rule 3's consequence: a replacement builds a new object, so the
	// provider's outputs are known after apply rather than carried across.
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"endpoint": provAttr(str("db-1.test")),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpReplace {
		t.Fatalf("Kind = %s, want replace", op.Kind)
	}
	endpoint, ok := op.After["endpoint"]
	if !ok {
		t.Fatal("After must mention the computed attribute")
	}
	if endpoint.Known {
		t.Error("a replacement re-derives computed attributes; they are known after apply, not carried across")
	}
}

func TestPreventDestroyIsAPlanTimeError(t *testing.T) {
	// Rule 4. The user learns before approving, not after.
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	live.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("removing a prevent_destroy resource from configuration must be an error at plan time")
	}
	if len(p.Operations) != 0 {
		t.Errorf("the plan must not contain an operation the engine has refused: %+v", p.Operations)
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "prevent_destroy") || !strings.Contains(out, "db") {
		t.Errorf("the diagnostic must name the guard and the resource:\n%s", out)
	}
}

func TestRetainForgetsWithoutDestroying(t *testing.T) {
	// Rule 5. The provider is never called.
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	live.Lifecycle = resource.Lifecycle{Retain: true}

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("retain is not an error: %+v", ds)
	}
	op := only(t, p)
	if op.Kind == OpDestroy {
		t.Fatal("retain must not destroy the resource")
	}
	if op.Kind != OpForget {
		t.Fatalf("Kind = %s, want forget", op.Kind)
	}
	if len(op.Reasons) == 0 || !strings.Contains(op.Reasons[0].Note, "without calling the provider") {
		t.Errorf("Reasons = %+v, want one saying the provider is not called", op.Reasons)
	}
}

func TestRetainWinsOverPreventDestroy(t *testing.T) {
	// A resource carrying both is forgotten, not refused: retain destroys
	// nothing, so it already satisfies what prevent_destroy protects.
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	live.Lifecycle = resource.Lifecycle{Retain: true, PreventDestroy: true}

	p, ds := Compute(config(), stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("retain satisfies prevent_destroy; this must not error: %+v", ds)
	}
	if op := only(t, p); op.Kind != OpForget {
		t.Errorf("Kind = %s, want forget", op.Kind)
	}
}

// --- judgement calls the signature forces ----------------------------------

func TestReadErrorFailsPlanningRatherThanAssumingAbsence(t *testing.T) {
	inConfig := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})
	orphan := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))

	obs := refresh.Observations{
		"net": {Address: addr("net"), Err: errors.New("connection refused")},
		"db":  {Address: addr("db"), Err: errors.New("connection refused")},
	}

	p, ds := Compute(cfg, stateOf(inConfig, orphan), obs, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("a read error must fail planning for that resource")
	}
	if len(p.Operations) != 0 {
		t.Errorf("a resource that could not be read gets no operation; a failed read is not evidence of deletion: %+v", p.Operations)
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "connection refused") {
		t.Errorf("the diagnostic must carry the provider's error:\n%s", out)
	}
}

// TestReadErrorOnOneResourceDoesNotAbortPlanningForOthers is the other half
// of TestReadErrorFailsPlanningRatherThanAssumingAbsence, which errors on
// every resource in its fixture and so cannot distinguish "the failed
// resource is skipped" from "any read error aborts the whole plan" — a
// regression promoting a per-resource error to a whole-plan abort would pass
// it just as well. Here db fails to read and net genuinely changed (cidr is
// ForceNew), so net must still get its operation.
func TestReadErrorOnOneResourceDoesNotAbortPlanningForOthers(t *testing.T) {
	broken := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	healthy := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})
	cfg := config(
		configured("db", "test.database", map[string]value.Value{"engine": str("postgres")}),
		configured("net", "test.network", map[string]value.Value{"cidr": str("10.1.0.0/16")}),
	)

	obs := merge(
		refresh.Observations{"db": {Address: addr("db"), Err: errors.New("connection refused")}},
		present(healthy),
	)

	p, ds := Compute(cfg, stateOf(broken, healthy), obs, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("db's read error must still be reported")
	}

	for _, op := range p.Operations {
		if op.Address.String() == "db" {
			t.Fatalf("db failed to read; it must get no operation, got %+v", op)
		}
	}

	var netOp *Operation
	for i := range p.Operations {
		if p.Operations[i].Address.String() == "net" {
			netOp = &p.Operations[i]
		}
	}
	if netOp == nil {
		t.Fatalf("net must still get an operation even though db failed to read: %+v", p.Operations)
	}
	if netOp.Kind != OpReplace {
		t.Fatalf("Kind = %s, want replace — net genuinely changed (cidr is ForceNew) and must still be planned", netOp.Kind)
	}
}

func TestMissingObservationFallsBackToRecordedState(t *testing.T) {
	// Refresh not having covered an address is not evidence of absence.
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	live := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})

	p, ds := Compute(cfg, stateOf(live), refresh.Observations{}, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	if op := only(t, p); op.Kind != OpNoOp {
		t.Errorf("Kind = %s, want noop — with no observation the recorded state stands in", op.Kind)
	}
}

func TestChangeReasonsNeverCarryValues(t *testing.T) {
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine":   str("postgres"),
		"password": str("hunter2").WithSensitive(true),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine":   provAttr(str("postgres")),
		"password": provAttr(str("s3cret")).WithSensitive(true),
	})

	p, _ := Compute(cfg, stateOf(live), present(live), planOpts(t))
	op := only(t, p)
	if op.Kind != OpUpdate {
		t.Fatalf("Kind = %s, want update", op.Kind)
	}
	for _, r := range op.Reasons {
		for _, leak := range []string{"hunter2", "s3cret"} {
			if strings.Contains(r.Attribute+" "+r.Note, leak) {
				t.Errorf("reason %+v leaks a value; reasons name attributes, never data", r)
			}
		}
	}
	if len(op.Reasons) != 1 || op.Reasons[0].Attribute != "password" {
		t.Errorf("Reasons = %+v, want one naming password", op.Reasons)
	}
}

func TestMissingSchemaIsAnErrorNotASilentUpdate(t *testing.T) {
	cfg := config(configured("thing", "aws.rds", map[string]value.Value{
		"engine": str("postgres"),
	}))
	p, ds := Compute(cfg, stateOf(), refresh.Observations{}, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("without a schema the planner cannot tell ForceNew from updatable; it must say so")
	}
	if len(p.Operations) != 0 {
		t.Errorf("no operation may be proposed for a type the planner cannot reason about: %+v", p.Operations)
	}
}

// TestAttributeNotDefinedByTheSchemaIsAnErrorNotASilentUpdate is the same
// defence one level down from TestMissingSchemaIsAnErrorNotASilentUpdate: an
// unrecognised resource TYPE is already a hard error via Definition's own ok,
// so an unrecognised ATTRIBUTE must not quietly fall through to the zero
// schema.Attribute — that would report ForceNew: false unconditionally,
// mislabelling a possible replacement as an in-place update. This should be
// unreachable through the real pipeline (internal/compiler/schema.go rejects
// it at compile time), but Compute does not get to assume its caller went
// through Compile.
func TestAttributeNotDefinedByTheSchemaIsAnErrorNotASilentUpdate(t *testing.T) {
	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine":     str("postgres"),
		"not_a_real": str("mystery"),
	}))
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})

	p, ds := Compute(cfg, stateOf(live), present(live), planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("an attribute the schema does not define must be a plan-time error, not a silently mislabelled update")
	}
	if len(p.Operations) != 0 {
		t.Errorf("no operation may be proposed when the planner cannot fully reason about a resource's attributes: %+v", p.Operations)
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "not_a_real") {
		t.Errorf("the diagnostic must name the unrecognised attribute:\n%s", out)
	}
}

func TestNilRegistryIsAnErrorNotADegradation(t *testing.T) {
	opts := planOpts(t)
	opts.Registry = nil
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	if _, ds := Compute(cfg, stateOf(), refresh.Observations{}, opts); !ds.HasErrors() {
		t.Error("planning without a registry must fail loudly, not quietly report every replacement as an update")
	}
}

func TestPlanningAgainstAnotherEnvironmentsStateIsAnError(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	prod := state.New("myapp", "prod")

	_, ds := Compute(cfg, prod, refresh.Observations{}, planOpts(t))
	if !ds.HasErrors() {
		t.Fatal("planning one environment's configuration against another's state must be refused")
	}
	out := rendered(t, ds)
	if !strings.Contains(out, "prod") || !strings.Contains(out, "dev") {
		t.Errorf("the diagnostic must name both environments:\n%s", out)
	}
}

func TestComputeDoesNotMutateItsInputs(t *testing.T) {
	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	st := stateOf(live)
	before, err := st.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	cfg := config(configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
	}))
	p, _ := Compute(cfg, st, present(live), planOpts(t))

	// Mutating the plan must not reach back into state.
	only(t, p).Before["engine"] = str("tampered")

	after, err := st.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("planning must never mutate the state it was given:\n%s\n%s", before, after)
	}
}

func TestPlanRecordsItsInputFingerprints(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	st := stateOf()

	p, _ := Compute(cfg, st, refresh.Observations{}, planOpts(t))
	if p.Version != PlanVersion {
		t.Errorf("Version = %d, want %d", p.Version, PlanVersion)
	}
	if p.Project != "myapp" || p.Environment != "dev" {
		t.Errorf("plan identifies itself as %s/%s", p.Project, p.Environment)
	}
	if p.ConfigHash == "" {
		t.Error("ConfigHash must be populated so M6 can detect a stale plan")
	}
	if p.StateHash == "" {
		t.Error("StateHash must be populated for the same reason")
	}
	if p.StateSerial != st.Serial {
		t.Errorf("StateSerial = %d, want %d", p.StateSerial, st.Serial)
	}
	if !p.CreatedAt.Equal(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %s; the clock is injected so plans are reproducible", p.CreatedAt)
	}
}

// TestHasUnknownFailsClosedOnMalformedComposites is a white-box test of
// hasUnknown directly: a composite Value whose Raw does not match its Kind
// must read as unknown, the same conservative rule value.Equal applies to
// malformed values and for the same reason — a value that cannot be PROVEN
// fully known must not be treated as fully known. Discarding the type
// assertion's ok is exactly the shape of bug that made value.Equal report two
// malformed values as equal earlier in this milestone.
func TestHasUnknownFailsClosedOnMalformedComposites(t *testing.T) {
	cases := []struct {
		name string
		v    value.Value
	}{
		{"list with wrong Raw type", value.Value{Kind: value.KindList, Known: true, Raw: "not a []value.Value"}},
		{"map with wrong Raw type", value.Value{Kind: value.KindMap, Known: true, Raw: "not a map[string]value.Value"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !hasUnknown(tc.v) {
				t.Error("a malformed composite must read as unknown, not as fully known")
			}
		})
	}
}

// --- dependent counts, for the destructive-change warning ------------------

func TestDestroyReportsDependentsFromState(t *testing.T) {
	// The case the state-side lookup exists for: the dependents are not in
	// configuration either, so a config-side implementation reports zero and
	// the warning spec §20 requires silently disappears.
	//
	// Three dependents, inserted out of sorted order, not one: dependentsOf
	// walks st.Resources directly — a plain Go map with randomised iteration
	// order — so with fewer than three entries, or entries already visited in
	// sorted order, this test could pass whether or not the implementation
	// actually sorts. Three distinct names whose insertion order differs from
	// their sorted order is what turns a deleted address.Sort call into a
	// failing test instead of an assertion that cannot fail.
	net := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})
	zebra := recorded("zebra-db", "test.database", map[string]value.Value{"engine": provAttr(str("postgres"))})
	alpha := recorded("alpha-db", "test.database", map[string]value.Value{"engine": provAttr(str("postgres"))})
	middle := recorded("middle-db", "test.database", map[string]value.Value{"engine": provAttr(str("postgres"))})
	for _, db := range []*resource.ResourceState{zebra, alpha, middle} {
		db.Dependencies = []address.Address{addr("net")}
	}

	all := []*resource.ResourceState{net, zebra, alpha, middle}
	p, ds := Compute(config(), stateOf(all...), present(all...), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	var destroyNet *Operation
	for i := range p.Operations {
		if p.Operations[i].Address.String() == "net" {
			destroyNet = &p.Operations[i]
		}
	}
	if destroyNet == nil {
		t.Fatalf("no operation for net: %+v", p.Operations)
	}
	if destroyNet.Kind != OpDestroy {
		t.Fatalf("Kind = %s, want destroy", destroyNet.Kind)
	}
	want := []string{"alpha-db", "middle-db", "zebra-db"}
	if len(destroyNet.Dependents) != len(want) {
		t.Fatalf("Dependents = %v, want %v — a destroy's edges live in state, not configuration", destroyNet.Dependents, want)
	}
	for i := range want {
		if destroyNet.Dependents[i].String() != want[i] {
			t.Fatalf("Dependents = %v, want %v — sorted, every map->slice boundary sorts", destroyNet.Dependents, want)
		}
	}
}

// TestDestroyBeforeReflectsObservedStateNotStaleRecordedState pins that a
// removal's Before comes from what the provider actually reports, not from
// the possibly-stale record in state — the same source the in-place diff
// path already uses, and consistent with how "present" was decided in the
// first place: it would be strange to trust the observation to decide IF the
// resource still exists, then show the plan a different resource's attributes
// than the ones that decision was based on.
func TestDestroyBeforeReflectsObservedStateNotStaleRecordedState(t *testing.T) {
	recordedState := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")), // what state remembers
	})
	drifted := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.9.0.0/16")), // what the provider reports now
	})

	p, ds := Compute(config(), stateOf(recordedState), present(drifted), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	op := only(t, p)
	if op.Kind != OpDestroy {
		t.Fatalf("Kind = %s, want destroy", op.Kind)
	}
	got, ok := op.Before["cidr"].AsString()
	if !ok || got != "10.9.0.0/16" {
		t.Errorf("Before[cidr] = %+v, want the observed value 10.9.0.0/16 — Before must reflect what the "+
			"provider actually reports, not the possibly-stale state record", op.Before["cidr"])
	}
}

func TestConfiguredOperationsReportDependentsFromConfigSorted(t *testing.T) {
	db := configured("db", "test.database", map[string]value.Value{
		"engine": str("mysql"),
	})
	var dependents []*resource.ResolvedResource
	for _, name := range []string{"zebra-app", "alpha-app", "middle-app"} {
		app := configured(name, "test.application", map[string]value.Value{
			"image": str("app:1"),
		})
		app.DependsOn = []address.Address{addr("db")}
		dependents = append(dependents, app)
	}

	live := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})

	cfg := config(append(dependents, db)...)
	p, ds := Compute(cfg, stateOf(live), present(live), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}

	for _, op := range p.Operations {
		if op.Address.String() != "db" {
			continue
		}
		if op.Kind != OpReplace {
			t.Fatalf("Kind = %s, want replace", op.Kind)
		}
		want := []string{"alpha-app", "middle-app", "zebra-app"}
		if len(op.Dependents) != len(want) {
			t.Fatalf("Dependents = %v, want %v", op.Dependents, want)
		}
		for i := range want {
			if op.Dependents[i].String() != want[i] {
				t.Fatalf("Dependents = %v, want %v — sorted, every map->slice boundary sorts", op.Dependents, want)
			}
		}
		return
	}
	t.Fatalf("no operation for db: %+v", p.Operations)
}

func TestResourcesWithoutDependentsReportNone(t *testing.T) {
	cfg := config(configured("net", "test.network", map[string]value.Value{
		"cidr": str("10.0.0.0/16"),
	}))
	p, _ := Compute(cfg, stateOf(), refresh.Observations{}, planOpts(t))
	if got := only(t, p).Dependents; len(got) != 0 {
		t.Errorf("Dependents = %v, want none", got)
	}
}

// --- invariant 6 -----------------------------------------------------------

func TestPlanIsDeterministicAcrossTwentyRuns(t *testing.T) {
	// Invariant 6: identical configuration, state and observations produce an
	// equivalent plan. The clock deliberately advances on every call, so this
	// also proves the canonical form does not depend on CreatedAt.
	var (
		desired []*resource.ResolvedResource
		live    []*resource.ResourceState
		gone    []address.Address
	)

	// Creates.
	for _, name := range []string{"net-a", "net-b", "net-c"} {
		desired = append(desired, configuredNetwork(name, "10.0.0.0/16"))
	}
	// NoOps.
	for _, name := range []string{"keep-a", "keep-b", "keep-c"} {
		desired = append(desired, configuredNetwork(name, "10.1.0.0/16"))
		live = append(live, recordedNetwork(name, "10.1.0.0/16"))
	}
	// Updates and replacements.
	for i, name := range []string{"db-a", "db-b", "db-c", "db-d"} {
		engine, size := "postgres", int64(20)
		if i%2 == 0 {
			engine = "mysql" // ForceNew: a replacement
		}
		desired = append(desired, &resource.ResolvedResource{
			Provider:  "test",
			Address:   addr(name),
			Type:      "test.database",
			DependsOn: []address.Address{addr("keep-a")},
			Attrs: map[string]value.Value{
				"engine": str(engine),
				"size":   value.Int(size, value.SourceExplicit),
				"tags": value.Map(map[string]value.Value{
					"env":  str("dev"),
					"team": str("platform"),
				}, value.SourceExplicit),
			},
		})
		live = append(live, &resource.ResourceState{
			Address:      addr(name),
			Type:         "test.database",
			Provider:     "test",
			ProviderID:   name + "-1",
			Dependencies: []address.Address{addr("keep-a")},
			Attributes: map[string]value.Value{
				"engine":   provAttr(str("postgres")),
				"size":     value.Int(10, value.SourceProvider),
				"endpoint": provAttr(str(name + ".test")),
				"tags": value.Map(map[string]value.Value{
					"env":  provAttr(str("dev")),
					"team": provAttr(str("platform")),
				}, value.SourceProvider),
			},
		})
	}
	// Destroys and forgets.
	for _, name := range []string{"old-a", "old-b"} {
		live = append(live, recordedNetwork(name, "10.9.0.0/16"))
	}
	forgotten := recordedNetwork("old-c", "10.9.0.0/16")
	live = append(live, forgotten)
	gone = append(gone, forgotten.Address)

	cfg := config(desired...)
	st := stateOf(live...)
	obs := merge(present(live...), absent(gone...))

	opts := planOpts(t)
	tick := 0
	opts.Now = func() time.Time {
		tick++
		return time.Date(2026, 9, 10, 12, 0, tick, 0, time.UTC)
	}

	first, ds := Compute(cfg, st, obs, opts)
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %+v", ds)
	}
	if !first.HasChanges() {
		t.Fatal("the fixture should propose changes")
	}
	want, err := first.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	for i := range 20 {
		next, _ := Compute(cfg, st, obs, opts)
		got, err := next.Canonical()
		if err != nil {
			t.Fatalf("Canonical: %v", err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("run %d produced a different plan; invariant 6 requires byte-identical output\nwant: %s\ngot:  %s", i, want, got)
		}
		if len(next.Operations) != len(first.Operations) {
			t.Fatalf("run %d produced %d operations, want %d", i, len(next.Operations), len(first.Operations))
		}
	}
}

func configuredNetwork(name, cidr string) *resource.ResolvedResource {
	return configured(name, "test.network", map[string]value.Value{"cidr": str(cidr)})
}

func recordedNetwork(name, cidr string) *resource.ResourceState {
	return recorded(name, "test.network", map[string]value.Value{
		"cidr": provAttr(str(cidr)),
		"id":   provAttr(str(name + "-1")),
	})
}

// TestEnvironmentMismatchProducesNoOperations is the reason the guard exists.
// The diagnostic is necessary but not sufficient: without the early return,
// Compute happily builds a complete list of operations destroying everything
// recorded in one environment's state and creating everything in the other's
// configuration — a well-formed plan a user could save to a file and hand to
// apply. This asserts the operation list is EMPTY, not merely that an error
// was reported, because the error was always reported.
//
// The state's resource carries a nil Attributes map — a deviation from the
// task brief, which named the field Attrs; resource.ResourceState has no such
// field, only Attributes (pkg/resource/resource.go). Fixed here so the test
// compiles; the nil value itself is unchanged from the brief's intent.
func TestEnvironmentMismatchProducesNoOperations(t *testing.T) {
	cfg := compiler.ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources: map[string]*resource.ResolvedResource{
			"network": {
				Address: address.Address{Name: "network"},
				Type:    "test.network",
				Attrs:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
	st := &state.State{
		Version:     1,
		Serial:      3,
		Project:     "myapp",
		Environment: "production",
		Resources: map[string]*resource.ResourceState{
			"database": {
				Address:    address.Address{Name: "database"},
				Type:       "test.database",
				Attributes: nil,
			},
		},
	}

	p, ds := Compute(cfg, st, nil, planOpts(t))

	if !ds.HasErrors() {
		t.Fatal("planning dev configuration against production state must be an error")
	}
	if len(p.Operations) != 0 {
		t.Fatalf("a mismatched-environment plan must contain no operations, got %d: %+v", len(p.Operations), p.Operations)
	}
	if p.HasChanges() {
		t.Error("a plan with no operations must report no changes")
	}
}
