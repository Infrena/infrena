package planner

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// deferred builds the value the compiler produces for a cross-resource
// reference: an unknown carrying the expression that will finish it. It goes
// through the real parser and the real evaluator with a scope that resolves
// nothing, which is exactly what internal/compiler/bind.go does at compile
// time — hand-assembling the Expr instead would let these fixtures drift from
// the shape the compiler actually emits, and the bug they exist to pin was
// invisible precisely because no planner fixture used a realistic value.
func deferred(t *testing.T, src string) value.Value {
	t.Helper()
	expr, ds := expressions.Parse(src, value.Origin{File: "infra.yml", Line: 1})
	if ds.HasErrors() {
		t.Fatalf("parsing %q: %s", src, rendered(t, ds))
	}
	v, ds := expressions.Evaluate(expr, expressions.ResourceScope{})
	if ds.HasErrors() {
		t.Fatalf("compile-time evaluation of %q: %s", src, rendered(t, ds))
	}
	if v.Known {
		t.Fatalf("%q resolved at compile time; this fixture is supposed to be deferred", src)
	}
	if v.Expr == nil {
		t.Fatalf("%q produced an unknown with no expression; there would be nothing for the planner to finish", src)
	}
	return v
}

// dependent is configured() plus the dependency edges the compiler derives
// from the references in its attributes. The planner orders resolution by
// these edges, so a fixture that omits them is not the fixture the compiler
// produces.
func dependent(name, typ string, attrs map[string]value.Value, deps ...string) *resource.ResolvedResource {
	r := configured(name, typ, attrs)
	for _, d := range deps {
		r.DependsOn = append(r.DependsOn, addr(d))
	}
	return r
}

// recordedDependent is recorded() plus the Dependencies an apply would have
// written for it.
//
// It matters that these fixtures use it. state's Dependencies is what the
// planner diffs configuration's DependsOn against, so a fixture that recorded
// a resource with edges in configuration and none in state is describing a
// state production no longer produces — and it would report an update on
// every run, which reads as invariant 2 being broken when what is actually
// broken is the fixture. This is the same class as the lifecycle fixtures
// that hand-built a ResourceState with Lifecycle set: seed the precondition
// the way production establishes it, or the test measures the author's
// assumptions instead of the code.
func recordedDependent(name, typ string, attrs map[string]value.Value, deps ...string) *resource.ResourceState {
	rs := recorded(name, typ, attrs)
	for _, d := range deps {
		rs.Dependencies = append(rs.Dependencies, addr(d))
	}
	return rs
}

// names renders an address slice for comparison. address.Address contains a
// slice, so it is not comparable with ==.
func names(addrs []address.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func operation(t *testing.T, p *Plan, name string) Operation {
	t.Helper()
	for _, op := range p.Operations {
		if op.Address.String() == name {
			return op
		}
	}
	t.Fatalf("no operation for %s in %+v", name, p.Operations)
	return Operation{}
}

// TestPlanConvergesWhenAReferencedAttributeIsAlreadyKnown is invariant 2 for
// the case that made it false: desired == actual, but one resource's
// configuration references another's computed attribute.
//
// Before the planner finished those references, the diff compared the recorded
// "net-1" against a desired value that was unknown at compile time and stayed
// unknown forever, so this plan proposed an update — and so did the plan after
// the apply that carried it out, and the one after that.
//
// The names matter: "database" sorts BEFORE "network", so deciding operations
// in address order reaches the dependent first and cannot have its dependency's
// answer yet. That is what makes resolutionOrder load-bearing here rather than
// incidentally satisfied.
func TestPlanConvergesWhenAReferencedAttributeIsAlreadyKnown(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"size":    value.Int(10, value.SourceDefault),
			"network": deferred(t, "${network.id}"),
		}, "network"),
	)
	live := []*resource.ResourceState{
		recorded("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16").WithSource(value.SourceProvider),
			"id":   str("net-1").WithSource(value.SourceProvider),
		}),
		recordedDependent("database", "test.database", map[string]value.Value{
			"engine":   str("postgres").WithSource(value.SourceProvider),
			"size":     value.Int(10, value.SourceProvider),
			"network":  str("net-1").WithSource(value.SourceProvider),
			"endpoint": str("db-1.db.test").WithSource(value.SourceProvider),
		}, "network"),
	}

	// Run repeatedly rather than once. Every input here reaches Compute
	// through a Go map, and Go randomises map iteration, so a single green
	// run cannot distinguish "converges" from "converged this time".
	for i := 0; i < 50; i++ {
		p, ds := Compute(cfg, stateOf(live...), present(live...), planOpts(t))
		if ds.HasErrors() {
			t.Fatalf("run %d: unexpected errors:\n%s", i, rendered(t, ds))
		}
		if p.HasChanges() {
			t.Fatalf("run %d: invariant 2 broken — desired equals actual but the plan proposes %d change(s): %+v",
				i, len(p.Operations), p.Operations)
		}
		if got := operation(t, p, "database").Kind; got != OpNoOp {
			t.Fatalf("run %d: database: kind %v, want %v", i, got, OpNoOp)
		}
		// The resolved value must be the dependency's actual attribute, not
		// merely "not unknown": a fabricated known value would also produce a
		// no-op diff here while corrupting what apply would write.
		after := operation(t, p, "database").After["network"]
		if !after.Known {
			t.Fatalf("run %d: database.network stayed unknown", i)
		}
		if got, _ := after.AsString(); got != "net-1" {
			t.Fatalf("run %d: database.network resolved to %q, want %q", i, got, "net-1")
		}
	}
}

// TestPlanConvergesThroughAChainOfReferences extends the same invariant past
// one hop: application refers to database, which refers to network. A
// resolution pass that only looked one level deep, or that ordered resources
// by anything other than their dependencies, converges the first hop and
// leaves the second proposing an update forever.
func TestPlanConvergesThroughAChainOfReferences(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${network.id}"),
		}, "network"),
		dependent("application", "test.application", map[string]value.Value{
			"image":        str("web:1"),
			"database_url": deferred(t, "postgres://${database.endpoint}/app"),
		}, "database"),
	)
	live := []*resource.ResourceState{
		recorded("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
			"id":   str("net-1"),
		}),
		recordedDependent("database", "test.database", map[string]value.Value{
			"engine":   str("postgres"),
			"network":  str("net-1"),
			"endpoint": str("db-1.db.test"),
		}, "network"),
		recordedDependent("application", "test.application", map[string]value.Value{
			"image":        str("web:1"),
			"database_url": str("postgres://db-1.db.test/app"),
			"url":          str("app-1.test"),
		}, "database"),
	}

	p, ds := Compute(cfg, stateOf(live...), present(live...), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", rendered(t, ds))
	}
	if p.HasChanges() {
		t.Fatalf("invariant 2 broken across a reference chain: %+v", p.Operations)
	}
	url := operation(t, p, "application").After["database_url"]
	if got, _ := url.AsString(); got != "postgres://db-1.db.test/app" {
		t.Fatalf("application.database_url resolved to %q (known=%v), want the interpolated endpoint",
			got, url.Known)
	}
}

// TestPlanLeavesAReferenceDeferredWhenItsDependencyIsBeingCreated is the other
// half of the fix, and the one it would be easiest to break while making the
// half above work: on a configuration that has never been applied, a reference
// to a resource that does not exist yet is NOT knowable, and the plan must
// still say so.
//
// It also pins the mechanism rather than only the rendering. The dependency's
// After carries its computed "id" as an unknown with no expression, purely so
// the plan can print "(known after apply)". Handing that unknown back as a
// resolved answer would replace the dependent's own unknown — and with it the
// expression that will finish it — leaving an unknown that apply can no longer
// resolve and that resolveAfter drops entirely, so the database would be
// created with no network at all. The plan still LOOKS right in that case,
// which is why this asserts on Expr and not just on Known.
func TestPlanLeavesAReferenceDeferredWhenItsDependencyIsBeingCreated(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${network.id}"),
		}, "network"),
	)

	p, ds := Compute(cfg, nil, nil, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", rendered(t, ds))
	}

	network := operation(t, p, "network")
	if network.Kind != OpCreate {
		t.Fatalf("network: kind %v, want %v", network.Kind, OpCreate)
	}
	// A computed attribute the provider will assign: unknown, and carrying no
	// expression, because there is no expression to finish. It must also not
	// have become a change of its own.
	id := network.After["id"]
	if id.Known {
		t.Errorf("network.id was resolved to %v; nothing can know it before the provider assigns it", id.Raw)
	}
	if id.Expr != nil {
		t.Errorf("network.id acquired an expression (%s); a computed attribute is not a deferred reference", id.Expr)
	}

	database := operation(t, p, "database")
	if database.Kind != OpCreate {
		t.Fatalf("database: kind %v, want %v", database.Kind, OpCreate)
	}
	net := database.After["network"]
	if net.Known {
		t.Errorf("database.network resolved to %v before its network existed", net.Raw)
	}
	if net.Expr == nil {
		t.Fatal("database.network lost its expression: apply would drop the attribute and create the database with no network")
	}
	if got := net.Expr.String(); got != "${network.id}" {
		t.Errorf("database.network carries expression %q, want %q", got, "${network.id}")
	}
}

// TestPlanResolvesAReferenceForAResourceBeingCreated covers the create half of
// After, which the converge tests never reach: adding a resource to a
// configuration that has already been applied creates it against dependencies
// that already exist, so its reference IS knowable and the plan must show the
// value rather than "(known after apply)".
//
// A plan that hid a value it could have known would still apply correctly —
// the executor finishes the reference either way — which is exactly why this
// needs its own test: the failure is a plan that under-reports what it knows,
// not a broken apply.
func TestPlanResolvesAReferenceForAResourceBeingCreated(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${network.id}"),
		}, "network"),
	)
	// Only the network has ever been applied.
	live := []*resource.ResourceState{
		recorded("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
			"id":   str("net-1"),
		}),
	}

	p, ds := Compute(cfg, stateOf(live...), present(live...), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", rendered(t, ds))
	}
	if got := operation(t, p, "network").Kind; got != OpNoOp {
		t.Fatalf("network: kind %v, want %v", got, OpNoOp)
	}

	database := operation(t, p, "database")
	if database.Kind != OpCreate {
		t.Fatalf("database: kind %v, want %v", database.Kind, OpCreate)
	}
	net := database.After["network"]
	if !net.Known {
		t.Fatalf("database.network is still deferred, but its network already exists and its id is recorded")
	}
	if got, _ := net.AsString(); got != "net-1" {
		t.Errorf("database.network = %q, want %q", got, "net-1")
	}
	// Its own computed attribute is still genuinely unknown: only the
	// reference resolved, not everything.
	if database.After["endpoint"].Known {
		t.Errorf("database.endpoint = %v; the provider has not assigned it yet", database.After["endpoint"].Raw)
	}
}

// TestPlanLeavesAReferenceDeferredWhenItsDependencyIsBeingReplaced covers the
// case that makes resolving against the PLANNED value, rather than against the
// recorded one, the correct rule.
//
// network's cidr is ForceNew, so changing it replaces the network and its id
// will not survive. Resolving ${network.id} against what state records would
// produce the old id, report the database unchanged, and then leave it
// pointing at a network that no longer exists — a plan that lies, and a
// configuration that never converges. The reference must stay deferred, and
// the database must be reported as changing.
func TestPlanLeavesAReferenceDeferredWhenItsDependencyIsBeingReplaced(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.9.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${network.id}"),
		}, "network"),
	)
	live := []*resource.ResourceState{
		recorded("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
			"id":   str("net-1"),
		}),
		recorded("database", "test.database", map[string]value.Value{
			"engine":   str("postgres"),
			"network":  str("net-1"),
			"endpoint": str("db-1.db.test"),
		}),
	}

	p, ds := Compute(cfg, stateOf(live...), present(live...), planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", rendered(t, ds))
	}

	if got := operation(t, p, "network").Kind; got != OpReplace {
		t.Fatalf("network: kind %v, want %v", got, OpReplace)
	}

	database := operation(t, p, "database")
	if database.Kind != OpUpdate {
		t.Fatalf("database: kind %v, want %v — its network is being replaced, so its reference cannot be unchanged",
			database.Kind, OpUpdate)
	}
	net := database.After["network"]
	if net.Known {
		t.Errorf("database.network resolved to %v from a network that is about to be replaced", net.Raw)
	}
	if net.Expr == nil {
		t.Error("database.network lost its expression: apply could not finish it against the replacement network")
	}
}

// TestPlanIsUnchangedForAConfigurationThatWasNeverApplied guards the fix's
// blast radius from the other direction: with no state and no observations,
// resolution has nothing to resolve against and must therefore change nothing
// at all about the plan. The comparison is the plan's own canonical form, so
// a difference anywhere in any operation fails, not only in the attributes
// this change touches.
func TestPlanIsUnchangedForAConfigurationThatWasNeverApplied(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${network.id}"),
		}, "network"),
	)

	first, ds := Compute(cfg, nil, nil, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", rendered(t, ds))
	}
	want, err := first.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	// Invariant 6, on the path this change added: identical inputs, identical
	// plan, repeatedly, so that a resolution order derived from map iteration
	// would show up rather than passing by luck.
	for i := 0; i < 50; i++ {
		p, ds := Compute(cfg, nil, nil, planOpts(t))
		if ds.HasErrors() {
			t.Fatalf("run %d: unexpected errors:\n%s", i, rendered(t, ds))
		}
		got, err := p.Canonical()
		if err != nil {
			t.Fatalf("run %d: Canonical: %v", i, err)
		}
		if string(got) != string(want) {
			t.Fatalf("run %d: plan differs (invariant 6):\n--- want ---\n%s\n--- got ---\n%s", i, want, got)
		}
	}
}

// TestPlanIsDeterministicWhenReferencesResolve is the same invariant on the
// applied path, where the new code actually does work: fifty plans over
// identical configuration, state and observations must be byte-identical.
func TestPlanIsDeterministicWhenReferencesResolve(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
		}),
		dependent("database", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${network.id}"),
			"tags":    value.Map(map[string]value.Value{"team": str("core"), "tier": str("db")}, value.SourceExplicit),
		}, "network"),
	)
	live := []*resource.ResourceState{
		recorded("network", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
			"id":   str("net-1"),
		}),
		recorded("database", "test.database", map[string]value.Value{
			"engine":   str("postgres"),
			"network":  str("net-1"),
			"endpoint": str("db-1.db.test"),
			"tags":     value.Map(map[string]value.Value{"team": str("core"), "tier": str("db")}, value.SourceProvider),
		}),
	}

	var want string
	for i := 0; i < 50; i++ {
		p, ds := Compute(cfg, stateOf(live...), present(live...), planOpts(t))
		if ds.HasErrors() {
			t.Fatalf("run %d: unexpected errors:\n%s", i, rendered(t, ds))
		}
		got, err := p.Canonical()
		if err != nil {
			t.Fatalf("run %d: Canonical: %v", i, err)
		}
		if i == 0 {
			want = string(got)
			continue
		}
		if string(got) != want {
			t.Fatalf("run %d: plan differs (invariant 6):\n--- want ---\n%s\n--- got ---\n%s", i, want, got)
		}
	}
}

// TestResolutionOrderPutsDependenciesFirst pins the ordering itself, not only
// its effect. It uses names whose alphabetical order is the REVERSE of their
// dependency order, so an implementation that fell back to address order —
// which is what Compute used before, and what resolutionOrder deliberately
// returns for a malformed graph — produces a visibly different sequence.
func TestResolutionOrderPutsDependenciesFirst(t *testing.T) {
	cfg := config(
		configured("zeta", "test.network", map[string]value.Value{"cidr": str("10.0.0.0/16")}),
		dependent("alpha", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${zeta.id}"),
		}, "zeta"),
	)

	for i := 0; i < 50; i++ {
		got := names(resolutionOrder(cfg, nil))
		want := []string{"zeta", "alpha"}
		if !equalStrings(got, want) {
			t.Fatalf("run %d: got %v, want %v (a dependency must be decided before its dependent)", i, got, want)
		}
	}
}

// TestResolutionOrderIncludesAddressesOnlyInState makes sure the reordering
// did not quietly drop the resources a plan proposes destroying: they appear
// in state and not in configuration, and before this change they reached
// Compute through planAddresses alone.
func TestResolutionOrderIncludesAddressesOnlyInState(t *testing.T) {
	cfg := config(configured("network", "test.network", map[string]value.Value{"cidr": str("10.0.0.0/16")}))
	st := stateOf(
		recorded("network", "test.network", map[string]value.Value{"cidr": str("10.0.0.0/16")}),
		recorded("gone", "test.database", map[string]value.Value{"engine": str("postgres")}),
		recorded("alsogone", "test.database", map[string]value.Value{"engine": str("postgres")}),
	)

	for i := 0; i < 50; i++ {
		got := names(resolutionOrder(cfg, st))
		want := []string{"network", "alsogone", "gone"}
		if !equalStrings(got, want) {
			t.Fatalf("run %d: got %v, want %v (removals last, in address order)", i, got, want)
		}
	}
}

// TestOperationsAreEmittedInAddressOrder pins the SECOND ordering the
// resolution pass introduced, and the one nothing else asserts.
//
// Compute now decides operations in dependency order and emits them in address
// order. Only the deciding order was tested; emitting in resolution order
// instead passed the entire suite, even though the plan artifact's
// byte-stability and invariant 6 both rest on emission order, and even though
// a reader who meets resolutionOrder first could reasonably take it for THE
// order.
//
// The fixture is built so the two orders contradict each other: zeta must be
// DECIDED first because alpha depends on it, and alpha must be EMITTED first
// because it sorts first. A fixture whose dependency order happened to agree
// with its alphabetical order would pass either way — which is exactly how two
// earlier tests in this file passed against a broken scope.
func TestOperationsAreEmittedInAddressOrder(t *testing.T) {
	cfg := config(
		configured("zeta", "test.network", map[string]value.Value{"cidr": str("10.0.0.0/16")}),
		dependent("alpha", "test.database", map[string]value.Value{
			"engine":  str("postgres"),
			"network": deferred(t, "${zeta.id}"),
		}, "zeta"),
	)
	// A resource only in state sorts between the two and is decided LAST of
	// all — resolutionOrder puts removals after every configured resource —
	// so it pins emission order against the removal tail as well.
	st := stateOf(
		recorded("zeta", "test.network", map[string]value.Value{
			"cidr": str("10.0.0.0/16"),
			"id":   str("net-1"),
		}),
		recorded("mu", "test.database", map[string]value.Value{"engine": str("postgres")}),
	)

	// Deciding order for this fixture is [zeta alpha mu]; emitted order must
	// be alphabetical regardless.
	wantDecided := []string{"zeta", "alpha", "mu"}
	wantEmitted := []string{"alpha", "mu", "zeta"}

	for i := 0; i < 50; i++ {
		if got := names(resolutionOrder(cfg, st)); !equalStrings(got, wantDecided) {
			t.Fatalf("run %d: resolution order = %v, want %v — this fixture only pins emission order while the two disagree",
				i, got, wantDecided)
		}

		p, ds := Compute(cfg, st, present(st.Resources["zeta"], st.Resources["mu"]), planOpts(t))
		if ds.HasErrors() {
			t.Fatalf("run %d: unexpected errors:\n%s", i, rendered(t, ds))
		}
		var got []string
		for _, op := range p.Operations {
			got = append(got, op.Address.String())
		}
		if !equalStrings(got, wantEmitted) {
			t.Fatalf("run %d: operations emitted as %v, want %v (sorted by canonical address, spec §12.1)",
				i, got, wantEmitted)
		}
	}
}

// TestDiagnosticsAreEmittedInAddressOrder pins the same contract for what a
// user reads. Decisions are made in dependency order, so a diagnostic raised
// while deciding a dependency would otherwise be reported before one raised for
// a resource that sorts ahead of it — and which order a plan reports its
// problems in is part of invariant 6 too.
//
// Both resources name a type no provider defines, so each produces exactly one
// diagnostic, and alpha depends on zeta so the two orders disagree.
func TestDiagnosticsAreEmittedInAddressOrder(t *testing.T) {
	cfg := config(
		configured("zeta", "test.nosuchtype", map[string]value.Value{"cidr": str("10.0.0.0/16")}),
		dependent("alpha", "test.nosuchtype", map[string]value.Value{"engine": str("postgres")}, "zeta"),
	)

	for i := 0; i < 50; i++ {
		_, ds := Compute(cfg, nil, nil, planOpts(t))
		if len(ds) != 2 {
			t.Fatalf("run %d: got %d diagnostics, want 2:\n%s", i, len(ds), rendered(t, ds))
		}
		out := rendered(t, ds)
		alpha := strings.Index(out, "alpha")
		zeta := strings.Index(out, "zeta")
		if alpha < 0 || zeta < 0 {
			t.Fatalf("run %d: diagnostics do not name both resources:\n%s", i, out)
		}
		if alpha > zeta {
			t.Fatalf("run %d: zeta's diagnostic is reported before alpha's; diagnostics follow address order, not resolution order:\n%s",
				i, out)
		}
	}
}

// TestResolutionOrderSurvivesADependencyOutsideConfiguration pins the guard on
// the edge-building loop.
//
// graph.Edge panics on an ID that was never added — deliberately, since a graph
// in this system is always built from addresses its builder already validated
// — so a DependsOn naming something absent from configuration must be skipped
// rather than recorded. Without the skip this panics, and a panic here reaches
// `apply`, which would abort mid-run still holding the environment's lock: the
// worst available failure mode for the worst available moment.
//
// The compiler rejects such a dependency (internal/compiler/bind.go's
// "depends_on names an undeclared resource"), so this is not reachable through
// the real pipeline. It is reachable by any caller that hands Compute a config
// it did not build with Compile, which Compute's own contract explicitly allows
// — the same reasoning the environment-mismatch guard in Compute rests on.
//
// The panic is recovered on purpose. An unrecovered one tears down the test
// binary and reports failures in packages this test has nothing to do with,
// which is precisely the signal that makes a mutation result unreadable.
func TestResolutionOrderSurvivesADependencyOutsideConfiguration(t *testing.T) {
	cfg := config(
		configured("network", "test.network", map[string]value.Value{"cidr": str("10.0.0.0/16")}),
		dependent("database", "test.database", map[string]value.Value{
			"engine": str("postgres"),
		}, "network", "vanished"),
	)

	order, panicked := recovered(func() []address.Address { return resolutionOrder(cfg, nil) })
	if panicked != nil {
		t.Fatalf("resolutionOrder panicked on a dependency absent from configuration: %v", panicked)
	}
	if got, want := names(order), []string{"network", "database"}; !equalStrings(got, want) {
		t.Errorf("resolution order = %v, want %v — the absent dependency must be skipped, not dropped along with the real one", got, want)
	}

	// And the whole plan still comes out, rather than the process ending.
	plan, panicked := recovered(func() *Plan {
		p, _ := Compute(cfg, nil, nil, planOpts(t))
		return p
	})
	if panicked != nil {
		t.Fatalf("Compute panicked on a dependency absent from configuration: %v", panicked)
	}
	if len(plan.Operations) != 2 {
		t.Errorf("got %d operations, want 2", len(plan.Operations))
	}
}

// TestResolutionOrderFallsBackWithoutLosingRemovals reaches the cycle branch.
//
// It is reachable for the same reason the guard above is: Compute is a pure
// function of its arguments, and a hand-built ResolvedConfig — a test's, or a
// future caller reading configuration back from somewhere other than Compile —
// is not obliged to be acyclic. The branch must fall back to the order Compute
// used before it ordered anything, which is planAddresses: every address in
// configuration OR state. Falling back to configuration alone would silently
// drop every removal, so a resource deleted from a configuration that also
// happened to contain a cycle would never be destroyed — invariant 1 lost to a
// fallback for an unrelated problem.
func TestResolutionOrderFallsBackWithoutLosingRemovals(t *testing.T) {
	cfg := config(
		dependent("alpha", "test.database", map[string]value.Value{"engine": str("postgres")}, "zeta"),
		dependent("zeta", "test.network", map[string]value.Value{"cidr": str("10.0.0.0/16")}, "alpha"),
	)
	st := stateOf(recorded("mu", "test.database", map[string]value.Value{"engine": str("postgres")}))

	order, panicked := recovered(func() []address.Address { return resolutionOrder(cfg, st) })
	if panicked != nil {
		t.Fatalf("resolutionOrder panicked on a cyclic configuration: %v", panicked)
	}
	if got, want := names(order), []string{"alpha", "mu", "zeta"}; !equalStrings(got, want) {
		t.Fatalf("resolution order = %v, want %v — the fallback is planAddresses order, which includes state-only removals",
			got, want)
	}

	// The removal must actually reach the plan, which is the property the
	// order above exists to protect.
	p, ds := Compute(cfg, st, nil, planOpts(t))
	if ds.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", rendered(t, ds))
	}
	if got := operation(t, p, "mu").Kind; got != OpForget && got != OpDestroy {
		t.Errorf("mu: kind %v, want a removal — it is in state and not in configuration (invariant 1)", got)
	}
}

// recovered runs fn and reports whatever it panicked with, so that a mutation
// which reintroduces a panic fails the test that covers it instead of killing
// the test binary and blaming unrelated packages.
func recovered[T any](fn func() T) (out T, panicked any) {
	defer func() { panicked = recover() }()
	return fn(), nil
}
