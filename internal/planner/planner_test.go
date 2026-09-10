package planner

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"infra/internal/compiler"
	"infra/internal/diag"
	"infra/internal/refresh"
	"infra/internal/registry"
	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
	testprovider "infra/providers/test"
)

// addr and str come from plan_test.go; do not redeclare them.

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register(testprovider.New(t.TempDir() + "/fake-cloud.json")); err != nil {
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
	return &resource.ResolvedResource{Address: addr(name), Type: typ, Attrs: attrs}
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
		for k, v := range set {
			out[k] = v
		}
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

// --- dependent counts, for the destructive-change warning ------------------

func TestDestroyReportsDependentsFromState(t *testing.T) {
	// The case the state-side lookup exists for: the dependent is not in
	// configuration either, so a config-side implementation reports zero and
	// the warning spec §20 requires silently disappears.
	net := recorded("net", "test.network", map[string]value.Value{
		"cidr": provAttr(str("10.0.0.0/16")),
	})
	db := recorded("db", "test.database", map[string]value.Value{
		"engine": provAttr(str("postgres")),
	})
	db.Dependencies = []address.Address{addr("net")}

	p, ds := Compute(config(), stateOf(net, db), present(net, db), planOpts(t))
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
	if len(destroyNet.Dependents) != 1 || destroyNet.Dependents[0].String() != "db" {
		t.Errorf("Dependents = %v, want [db] — a destroy's edges live in state, not configuration", destroyNet.Dependents)
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

	for i := 0; i < 20; i++ {
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
