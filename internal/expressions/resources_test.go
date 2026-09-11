package expressions

import (
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

func origin() value.Origin { return value.Origin{File: "infra.yml", Line: 3} }

// deferredRef builds what the compiler stores for an unresolved reference: an
// unknown carrying the expression that will finish it. It goes through the
// real parser and evaluator, against a scope that resolves nothing, rather
// than hand-assembling an Expr.
func deferredRef(t *testing.T, src string) value.Value {
	t.Helper()
	expr, ds := Parse(src, origin())
	if ds.HasErrors() {
		t.Fatalf("parsing %q: %v", src, ds)
	}
	v, ds := Evaluate(expr, ResourceScope{})
	if ds.HasErrors() {
		t.Fatalf("evaluating %q against an empty scope: %v", src, ds)
	}
	if v.Known || v.Expr == nil {
		t.Fatalf("%q did not produce a deferred value (known=%v, expr=%v)", src, v.Known, v.Expr)
	}
	return v
}

// TestResolveDeferredFinishesAReferenceWhoseTargetIsKnown is the base case
// both phases depend on.
func TestResolveDeferredFinishesAReferenceWhoseTargetIsKnown(t *testing.T) {
	scope := ResourceScope{
		"network": {"id": value.String("net-1", value.SourceProvider)},
	}
	attrs := map[string]value.Value{
		"engine":  value.String("postgres", value.SourceExplicit),
		"network": deferredRef(t, "${network.id}"),
	}

	out, unresolved, ds := ResolveDeferred(attrs, scope)
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none", unresolved)
	}
	got, ok := out["network"].AsString()
	if !ok || got != "net-1" {
		t.Errorf("network = %q (known=%v), want %q", got, out["network"].Known, "net-1")
	}
	if !out["engine"].Equal(attrs["engine"]) || out["engine"].Origin.String() != attrs["engine"].Origin.String() {
		t.Errorf("a known attribute was altered: %+v", out["engine"])
	}
	// The input must be untouched: at plan time these attributes belong to the
	// compiled configuration, which ConfigHash is taken over and which every
	// other resource is diffed against.
	if attrs["network"].Known {
		t.Error("ResolveDeferred mutated its input map")
	}
}

// TestResolveDeferredLeavesAComputedAttributeAlone is the distinction that
// broke twice in this milestone in opposite directions.
//
// An unknown with no expression is not a deferred reference. It is a computed
// schema attribute the provider assigns itself, staged as unknown so a plan can
// render "(known after apply)". Evaluating it calls Evaluate(nil, scope), which
// returns an unknown with NO error — so it would silently be reported as
// unresolved, and the executor turns "unresolved" into a hard failure. Every
// create of any resource with an unset computed attribute would fail.
func TestResolveDeferredLeavesAComputedAttributeAlone(t *testing.T) {
	staged := value.Unknown(value.KindString, value.SourceProvider)
	if staged.Expr != nil {
		t.Fatal("value.Unknown now attaches an expression; this test's premise no longer holds")
	}
	attrs := map[string]value.Value{"id": staged}

	// A scope that could answer for it if it were ever asked, so that a
	// regression shows up as a wrong ANSWER and not merely as a wrong error.
	scope := ResourceScope{"id": {"id": value.String("wrong", value.SourceProvider)}}

	out, unresolved, ds := ResolveDeferred(attrs, scope)
	if len(ds) != 0 {
		t.Errorf("diagnostics for a computed attribute: %v", ds)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v — a computed attribute is not an unfinished reference", unresolved)
	}
	if out["id"].Known {
		t.Errorf("id was resolved to %v; only the provider can assign it", out["id"].Raw)
	}
	if out["id"].Expr != nil {
		t.Errorf("id acquired an expression: %s", out["id"].Expr)
	}
}

// TestResolveDeferredWillNotAnswerFromAnUnknownAttribute is the same
// distinction seen from the referring side, and is what keeps a plan's
// deferred values apply-able.
//
// At plan time the scope holds each operation's After, and a create's After
// carries its computed attributes as unknowns with no expression. Handing one
// back as an answer would replace the referring value's own unknown — and its
// expression — with an expression-less unknown, which apply drops entirely
// (internal/executor/resolve.go), creating the resource with the attribute
// missing. The plan would still print "(known after apply)" either way, which
// is why this asserts on Expr.
func TestResolveDeferredWillNotAnswerFromAnUnknownAttribute(t *testing.T) {
	scope := ResourceScope{
		"network": {"id": value.Unknown(value.KindString, value.SourceProvider)},
	}
	attrs := map[string]value.Value{"network": deferredRef(t, "${network.id}")}

	out, unresolved, ds := ResolveDeferred(attrs, scope)
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if len(unresolved) != 1 || unresolved[0] != "network" {
		t.Errorf("unresolved = %v, want [network]", unresolved)
	}
	if out["network"].Known {
		t.Errorf("network resolved to %v from an attribute nobody knows yet", out["network"].Raw)
	}
	if out["network"].Expr == nil {
		t.Fatal("network lost its expression; apply could no longer finish it")
	}
	if got := out["network"].Expr.String(); got != "${network.id}" {
		t.Errorf("network carries %q, want %q", got, "${network.id}")
	}
}

// TestResolveDeferredKeepsAnUnresolvedValueExactlyAsItWas pins the rule that
// makes running this at plan time safe: resolution either replaces an unknown
// with a fully known value or changes nothing at all. Anything else would make
// a plan for a configuration that was never applied differ from the plan the
// same configuration produced before resolution existed.
func TestResolveDeferredKeepsAnUnresolvedValueExactlyAsItWas(t *testing.T) {
	attrs := map[string]value.Value{
		"url":     deferredRef(t, "postgres://${database.endpoint}/app"),
		"network": deferredRef(t, "${network.id}"),
	}

	out, unresolved, ds := ResolveDeferred(attrs, ResourceScope{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if len(unresolved) != 2 || unresolved[0] != "network" || unresolved[1] != "url" {
		t.Errorf("unresolved = %v, want [network url] — sorted, so diagnostics cannot reorder between runs", unresolved)
	}
	for name, want := range attrs {
		got := out[name]
		if got.Known != want.Known || got.Kind != want.Kind || got.Expr != want.Expr {
			t.Errorf("%s changed: got %+v, want %+v", name, got, want)
		}
	}
}

// TestResolveDeferredIsSortedOverManyAttributes pins the sort rather than
// trusting a lucky map iteration. Sortedness is checked by looping over
// deliberately reversed input, because Go's randomised map order passes an
// unsorted implementation often enough — measured at 38% with six keys — that
// a single run proves nothing.
func TestResolveDeferredIsSortedOverManyAttributes(t *testing.T) {
	attrs := map[string]value.Value{}
	want := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for i := len(want) - 1; i >= 0; i-- {
		attrs[want[i]] = deferredRef(t, "${nowhere.id}")
	}

	for i := 0; i < 50; i++ {
		_, unresolved, _ := ResolveDeferred(attrs, ResourceScope{})
		if len(unresolved) != len(want) {
			t.Fatalf("run %d: unresolved = %v, want %v", i, unresolved, want)
		}
		for j := range want {
			if unresolved[j] != want[j] {
				t.Fatalf("run %d: unresolved = %v, want %v", i, unresolved, want)
			}
		}
	}
}

// TestResourceScopeNeverAnswersForAVariable pins the deliberate decision
// documented on ResourceScope: a variable reference surviving into a deferred
// value means a compiler invariant broke, and it must surface as an error
// rather than be quietly answered.
func TestResourceScopeNeverAnswersForAVariable(t *testing.T) {
	if v, ok := (ResourceScope{}).Variable("environment"); ok {
		t.Errorf("Variable answered with %+v; it must report unavailable so a broken invariant surfaces", v)
	}
}

// TestResourceScopeReportsAMissingResourceUnavailable covers the ordinary
// case of a dependency that has not been decided or applied yet, and the
// no-such-attribute case beside it.
func TestResourceScopeReportsAMissingResourceUnavailable(t *testing.T) {
	scope := ResourceScope{"network": {"id": value.String("net-1", value.SourceProvider)}}

	if _, ok := scope.Attribute(value.LocalRef("database", "endpoint")); ok {
		t.Error("Attribute answered for a resource the scope does not hold")
	}
	if _, ok := scope.Attribute(value.LocalRef("network", "missing")); ok {
		t.Error("Attribute answered for an attribute the resource does not have")
	}
	if _, ok := scope.Attribute(value.LocalRef("network", "id")); !ok {
		t.Error("Attribute did not answer for an attribute it holds")
	}
}

// TestResourceScopeWillNotAnswerWithAnUnknownAttribute pins Attribute's own
// contract: it answers only with a value that is actually known.
//
// ResolveDeferred's rule that an unfinished value is left exactly as it was
// currently masks a violation of this — the referring value keeps its own
// expression either way — so the guarantee has to be tested where it is made
// rather than through its caller. It matters on its own terms: an unknown is
// not an answer, and anything else reaching for this scope (a future explain,
// or a caller that does keep the evaluator's output) would otherwise be handed
// one resource's "I do not know yet" as though it were another's value.
func TestResourceScopeWillNotAnswerWithAnUnknownAttribute(t *testing.T) {
	scope := ResourceScope{
		"network": {"id": value.Unknown(value.KindString, value.SourceProvider)},
	}
	if v, ok := scope.Attribute(value.LocalRef("network", "id")); ok {
		t.Errorf("Attribute answered %+v for an attribute nobody knows yet", v)
	}
}

// TestAttributeResolvesWithinItsModuleAndNotAgainstASameNamedRootResource is
// the M5 landmine M3 filed against this file, made executable.
//
// The snapshot holds TWO resources both logically named "db": one at the root
// and one inside module "net", with DIFFERENT endpoints. A reference carrying
// the module path must resolve to the module's.
//
// Against the bare-name lookup this replaces —
// s[(address.Address{Name: ref.Resource}).String()] — the module reference
// resolves to the ROOT db and hands back "root.example.com". Nothing errors:
// the plan is written with another resource's endpoint in it, and apply
// carries it out. This is why the fixture gives the two DIFFERENT values; two
// identical ones would pass against the broken lookup.
func TestAttributeResolvesWithinItsModuleAndNotAgainstASameNamedRootResource(t *testing.T) {
	scope := ResourceScope{
		"db": {
			"endpoint": value.String("root.example.com", value.SourceProvider),
		},
		"module.net.db": {
			"endpoint": value.String("net.example.com", value.SourceProvider),
		},
	}

	inModule := value.LocalRef("db", "endpoint").InModule("net")
	got, ok := scope.Attribute(inModule)
	if !ok {
		t.Fatalf("%s did not resolve; a reference inside a module must find that module's resource", inModule)
	}
	if s, _ := got.AsString(); s != "net.example.com" {
		t.Errorf("%s resolved to %q, want \"net.example.com\": the lookup is keyed on the bare name, "+
			"so it matched the root resource of the same name", inModule, s)
	}

	atRoot, ok := scope.Attribute(value.LocalRef("db", "endpoint"))
	if !ok {
		t.Fatal("the root db did not resolve")
	}
	if s, _ := atRoot.AsString(); s != "root.example.com" {
		t.Errorf("root db resolved to %q, want \"root.example.com\"", s)
	}
}

// TestAttributeDoesNotResolveAModuleReferenceAgainstAnAbsentModule is the
// other answer, and it stops the test above from passing against a lookup that
// simply ignores the module path in the other direction.
func TestAttributeDoesNotResolveAModuleReferenceAgainstAnAbsentModule(t *testing.T) {
	scope := ResourceScope{
		"db": {"endpoint": value.String("root.example.com", value.SourceProvider)},
	}
	ref := value.LocalRef("db", "endpoint").InModule("net")
	if v, ok := scope.Attribute(ref); ok {
		s, _ := v.AsString()
		t.Errorf("%s resolved to %q against a snapshot with no module \"net\"; a reference inside a "+
			"module must not fall back to a root resource of the same name", ref, s)
	}
}

// TestAttributeRefusesAnUnqualifiedReference is the test that fails if stage 6
// never qualifies (contract Amendment 7).
//
// Carrying a module path on Reference is only half the fix. If bindAttribute
// parses and evaluates without calling Qualify, every reference stays
// scope-relative with an empty module path, and one reaches this lookup looking
// exactly like the reference below. The snapshot holds ONLY the module's db, so
// the lookup misses — which is correct, and is what makes the failure visible.
//
// The real target is the tempting wrong fix. Faced with "my module's reference
// does not resolve", the fastest repair is a bare-name fallback in Attribute:
// try the canonical key, then try matching on Name alone. That makes the symptom
// go away and silently restores Ruling 1's landmine in full — two modules each
// declaring a `db` would match each other's. This test fails against that
// fallback, so the missing Qualify has to be fixed where it actually is.
func TestAttributeRefusesAnUnqualifiedReference(t *testing.T) {
	scope := ResourceScope{
		"module.net.db": {"endpoint": value.String("net.example.com", value.SourceProvider)},
	}

	// As parsed, before qualification: no module path.
	unqualified := value.LocalRef("db", "endpoint")
	if len(unqualified.Target.Module) != 0 {
		t.Fatalf("LocalRef is not scope-relative: %v", unqualified.Target.Module)
	}
	if v, ok := scope.Attribute(unqualified); ok {
		s, _ := v.AsString()
		t.Errorf("an unqualified %s resolved to %q against a snapshot holding only module.net.db. "+
			"Either stage 6's Qualify was skipped and this lookup is covering for it, or Attribute has "+
			"grown a bare-name fallback — which is Ruling 1's cross-module mismatch restored.", unqualified, s)
	}

	// The same reference, qualified as stage 6 will qualify it, DOES resolve.
	// Without this half the test above is satisfied by a lookup that resolves
	// nothing at all.
	qualified := unqualified.InModule("net")
	v, ok := scope.Attribute(qualified)
	if !ok {
		t.Fatalf("%s did not resolve; qualification is what makes a module's reference findable", qualified)
	}
	if s, _ := v.AsString(); s != "net.example.com" {
		t.Errorf("%s resolved to %q", qualified, s)
	}
}
