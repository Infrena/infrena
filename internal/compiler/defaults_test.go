package compiler

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/providers"
	"github.com/infrata/infrata/pkg/value"
)

// instanceWith is a provider instance carrying nothing but a `defaults:` block, for the
// two tests that exercise applyInstanceDefaults directly.
func instanceWith(defaults map[string]value.Value) providers.Instance {
	return providers.Instance{Name: "test", Plugin: "test", Defaults: defaults}
}

// PLAN.md §12.1's ladder, at the one place it is decided:
//
//	explicit on the resource  →  the instance's `defaults:`  →  the plugin's schema

// compiled runs the whole pipeline over a one-file project, failing on diagnostics.
func compiled(t *testing.T, body string) ResolvedConfig {
	t.Helper()
	cfg, ds := Compile(loadFiles(t, body), testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	return cfg
}

const defaultsProject = `
project: p
environments:
  dev: {}
providers:
  - plugin: fake
    defaults:
      size: 200
      tags:
        team: platform
        tier: shared
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  plain:
    type: fake.database
    engine: postgres
  own:
    type: fake.database
    engine: postgres
    size: 50
    tags:
      team: payments
      owner: storefront
`

// TestAnInstanceDefaultFillsAnAbsentAttribute, and BEATS the plugin's schema default.
//
// `size` ships a schema default of 10. Asserting only that the attribute is present
// would pass against a build that ignored `defaults:` entirely, so the value is what
// the assertion is about.
func TestAnInstanceDefaultBeatsThePluginsSchemaDefault(t *testing.T) {
	cfg := compiled(t, defaultsProject)
	v := cfg.Resources["plain"].Attrs["size"]
	if got, _ := v.AsInt(); got != 200 {
		t.Errorf("size = %v, want 200 from the instance's `defaults:` — 10 is the plugin's "+
			"schema default, which the instance outranks", v)
	}
	// And it says where it came from, or a plan sends the reader to the plugin's
	// documentation for a value written in their own file.
	if v.Scope != value.ScopeInstanceDefault {
		t.Errorf("Scope = %s, want ScopeInstanceDefault", v.Scope)
	}
	// Marked a default, which is what makes a plan print [default] and generation
	// leave it out.
	if v.Source != value.SourceDefault {
		t.Errorf("Source = %s, want SourceDefault", v.Source)
	}
}

// TestAResourcesOwnValueReplacesTheInstanceDefaultWHOLE.
//
// The two maps share `team` and each has a key the other lacks, so a merge would
// visibly produce a third thing — which is the failure this asserts against. §12.1
// says replaced, not merged, and §10.2's `merge()` is how a user asks for the union.
// Merging silently would leave no way to REMOVE an inherited key.
func TestAResourcesOwnValueReplacesTheInstanceDefaultWhole(t *testing.T) {
	cfg := compiled(t, defaultsProject)
	tags, ok := cfg.Resources["own"].Attrs["tags"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags Raw is %T", cfg.Resources["own"].Attrs["tags"].Raw)
	}
	if got, _ := tags["team"].AsString(); got != "payments" {
		t.Errorf("team = %v, want the resource's own value", tags["team"])
	}
	if got, _ := tags["owner"].AsString(); got != "storefront" {
		t.Errorf("owner = %v, want the resource's own key kept", tags["owner"])
	}
	// The key only the DEFAULT holds must be gone. Its presence is exactly what a
	// merge would produce.
	if _, merged := tags["tier"]; merged {
		t.Errorf("`tier` survived from the instance default, so the maps were merged "+
			"rather than replaced: %v", tags)
	}
	if len(tags) != 2 {
		t.Errorf("tags has %d keys, want the resource's two: %v", len(tags), tags)
	}
	// And the scalar half of the same rule.
	if got, _ := cfg.Resources["own"].Attrs["size"].AsInt(); got != 50 {
		t.Errorf("size = %v, want the resource's own 50", cfg.Resources["own"].Attrs["size"])
	}
	if cfg.Resources["own"].Attrs["size"].Scope == value.ScopeInstanceDefault {
		t.Error("an explicitly written value is credited to the instance default")
	}
}

// TestAnInstanceDefaultReachesOnlyTheTypesThatDeclareIt.
//
// `tags` is declared by fake.database and by neither of its siblings. Applying it to a
// network would produce "no such attribute" from the resource's own type — a file that
// does not load, pointing at nothing the user wrote.
func TestAnInstanceDefaultReachesOnlyTheTypesThatDeclareIt(t *testing.T) {
	cfg := compiled(t, defaultsProject)
	if _, present := cfg.Resources["net"].Attrs["tags"]; present {
		t.Error("a network was given `tags`, which fake.network does not declare")
	}
	// The other half, or this test would pass against a build applying nothing.
	if _, present := cfg.Resources["plain"].Attrs["tags"]; !present {
		t.Error("the database was not given `tags`")
	}
}

// TestAComputedAttributeIsNeverFilledFromDefaults. `defaults:` naming one is refused in
// stage 4.5, so this guards the other direction: a key that is computed on ONE type and
// settable on another must not reach the type that computes it.
//
// Nothing in the fake provider's schema is shaped that way today, so the guard is
// asserted at the unit below rather than through a fixture that cannot exist.
func TestAComputedAttributeIsNeverFilledFromDefaults(t *testing.T) {
	def, ok := testRegistry(t).Definition("fake.database")
	if !ok {
		t.Fatal("no fake.database definition")
	}
	attrs := map[string]value.Value{}
	applyInstanceDefaults(attrs, def, instanceWith(map[string]value.Value{
		"endpoint": value.String("db.example.com", value.SourceExplicit),
		"engine":   value.String("postgres", value.SourceExplicit),
	}))
	if _, present := attrs["endpoint"]; present {
		t.Error("a computed attribute was filled from `defaults:`; the resource would then " +
			"fail to load with \"is computed and cannot be set\"")
	}
	// And the settable neighbour in the same call did arrive, so the skip is about
	// being computed rather than about the loop stopping.
	if got, _ := attrs["engine"].AsString(); got != "postgres" {
		t.Errorf("engine = %v, want the settable default applied", attrs["engine"])
	}
}

// TestADefaultOfTheWrongKindIsNotAppliedToAMismatchedType — the same shape as above:
// stage 4.5 refuses a kind no type accepts, so what is left is a kind accepted by one
// type and not another.
func TestADefaultOfTheWrongKindIsNotAppliedToAMismatchedType(t *testing.T) {
	def, ok := testRegistry(t).Definition("fake.database")
	if !ok {
		t.Fatal("no fake.database definition")
	}
	attrs := map[string]value.Value{}
	applyInstanceDefaults(attrs, def, instanceWith(map[string]value.Value{
		"size": value.String("200", value.SourceExplicit),
	}))
	if _, present := attrs["size"]; present {
		t.Error("a string was applied to an integer attribute, which the kind check on " +
			"configured attributes would then report against a value the user never wrote")
	}
}

// The LIFECYCLE half of §12.1's `defaults:`. Every resource accepts prevent_destroy and
// retain, so the instance's block may supply either for all of them at once — which is
// the whole "prevent_destroy in production, not in dev" example the feature exists for.

const lifecycleDefaults = `
project: p
environments:
  dev: {}
providers:
  - plugin: fake
    defaults:
      prevent_destroy: true
resources:
  inherits:
    type: fake.network
    cidr: 10.0.0.0/16
  opts_out:
    type: fake.network
    cidr: 10.1.0.0/16
    lifecycle:
      prevent_destroy: false
`

// TestALifecycleDefaultReachesEveryResource, including the types whose schema declares
// no such attribute — because none of them do. The engine owns these names.
func TestALifecycleDefaultReachesEveryResource(t *testing.T) {
	cfg := compiled(t, lifecycleDefaults)
	if !cfg.Resources["inherits"].Lifecycle.PreventDestroy {
		t.Error("the instance's `prevent_destroy: true` did not reach a resource that says " +
			"nothing about it")
	}
}

// TestAResourcesOwnFalseBeatsALifecycleDefaultOfTrue is the sharp edge, and the reason
// LifecycleDecl tracks whether each key was WRITTEN.
//
// `prevent_destroy: false` is indistinguishable from absent in a bare bool, so the
// obvious implementation makes an explicit opt-out silently inert — and gets it wrong in
// the direction that REFUSES a destroy the user asked for, which is the direction a user
// cannot work around.
func TestAResourcesOwnFalseBeatsALifecycleDefaultOfTrue(t *testing.T) {
	cfg := compiled(t, lifecycleDefaults)
	if cfg.Resources["opts_out"].Lifecycle.PreventDestroy {
		t.Error("`prevent_destroy: false` written on the resource lost to the instance's " +
			"default of true, so there is no way to opt one resource out")
	}
}

// TestALifecycleDefaultOfFalseLeavesAResourcesOwnTrueAlone — the same rule the other way
// round, which a build that simply always preferred the resource's value would also
// pass. What it discriminates is a build that copied the default over unconditionally.
func TestALifecycleDefaultOfFalseLeavesAResourcesOwnTrueAlone(t *testing.T) {
	cfg := compiled(t, `
project: p
environments:
  dev: {}
providers:
  - plugin: fake
    defaults:
      prevent_destroy: false
resources:
  guarded:
    type: fake.network
    cidr: 10.0.0.0/16
    lifecycle:
      prevent_destroy: true
`)
	if !cfg.Resources["guarded"].Lifecycle.PreventDestroy {
		t.Error("an instance default of false overrode a resource's own `prevent_destroy: true`")
	}
}

// TestABadDefaultsKeyIsStillRefusedWhenTheProviderWasSuppliedDirectly.
//
// Found by a sabotage that moved the `defaults:` check back inside the branch that
// CONSTRUCTS an instance, and broke nothing: every test in this package registers an
// already-built provider, so the whole fail-closed rule was switched off in exactly the
// place the feature is most likely to be exercised next.
//
// The check is about the SCHEMAS. Who built the provider object has nothing to do with
// whether `tag:` is an attribute.
func TestABadDefaultsKeyIsStillRefusedWhenTheProviderWasSuppliedDirectly(t *testing.T) {
	_, ds := Compile(loadFiles(t, `
project: p
environments:
  dev: {}
providers:
  - plugin: fake
    defaults:
      tag:
        team: payments
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`), testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a `defaults:` key nothing accepts must be refused however the provider " +
			"object got into the registry")
	}
	if !strings.Contains(rendered(ds), "tag") {
		t.Errorf("the diagnostic does not name the key:\n%s", rendered(ds))
	}
}
