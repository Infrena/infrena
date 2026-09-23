package compiler

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/diag"

	"github.com/infrena/infrena/internal/providers"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func testRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register("fake", testprovider.New(t.TempDir()+"/fake-cloud.json")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// testTable is the instance table a project with no `providers:` block gets: one
// implicit instance, default, named after the only plugin there is.
//
// bindReferences takes it as an argument rather than deriving it, because the
// derivation belongs to stage 4.5 (internal/providers.Prepare) and a stage that
// re-derived it could come to disagree with the one that built the providers.
func testTable() providers.Table {
	return providers.Table{"fake": providers.Instance{Name: "fake", Plugin: "fake", Default: true}}
}

func oneResource(typ string, attrs map[string]value.Value) *ResolvedConfig {
	return &ResolvedConfig{
		Project:     "myapp",
		Environment: "dev",
		Resources: map[string]*resource.ResolvedResource{
			"r": {Address: address.Address{Name: "r"}, Type: typ, Attrs: attrs},
		},
	}
}

// TestSchemaRejectsUnknownType covers the TYPO: a type the loaded plugin does
// not offer, which is what the list of known types answers. A type whose PLUGIN
// never loaded is a different fact wanting the opposite message, and is covered
// by the test below.
func TestSchemaRejectsUnknownType(t *testing.T) {
	cfg := oneResource("fake.netwrok", nil)
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())
	if !ds.HasErrors() {
		t.Fatal("an unregistered type must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "fake.database") {
		t.Errorf("the diagnostic should list known types:\n%s", out.String())
	}
}

// TestSchemaSaysWhenAPluginNeverLoaded, rather than offering a list that cannot
// contain what was asked for.
//
// With nothing loaded the list is EMPTY, so the known-types message renders a
// heading above a blank line and advises correcting a spelling that was never
// wrong. A project whose resources live only inside modules hits exactly this
// shape, where the cause is that nothing at the root asked for the plugin.
func TestSchemaSaysWhenAPluginNeverLoaded(t *testing.T) {
	cfg := oneResource("aws.rds", nil)
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())
	if !ds.HasErrors() {
		t.Fatal("a type from an unloaded plugin must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	got := out.String()
	if !strings.Contains(got, "never loaded") {
		t.Errorf("the diagnostic does not say the plugin never loaded:\n%s", got)
	}
	if strings.Contains(got, "Known types:") {
		t.Errorf("a list of known types cannot answer this and must not be offered:\n%s", got)
	}
}

func TestSchemaRejectsUnknownAttribute(t *testing.T) {
	cfg := oneResource("fake.network", map[string]value.Value{
		"cidr":     value.String("10.0.0.0/16", value.SourceExplicit),
		"nonsense": value.Bool(true, value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable()); !ds.HasErrors() {
		t.Error("an attribute the schema does not define must be an error")
	}
}

func TestSchemaRejectsSettingAComputedAttribute(t *testing.T) {
	cfg := oneResource("fake.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
		"id":   value.String("net-1", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable()); !ds.HasErrors() {
		t.Error("configuration must not set a computed attribute")
	}
}

func TestSchemaRejectsAMissingRequiredAttribute(t *testing.T) {
	cfg := oneResource("fake.network", nil) // cidr is required
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable()); !ds.HasErrors() {
		t.Error("a missing required attribute must be an error")
	}
}

func TestSchemaRejectsAWrongKind(t *testing.T) {
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.String("large", value.SourceExplicit), // size is an integer
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())
	if !ds.HasErrors() {
		t.Fatal("a string where an integer is required must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "integer") {
		t.Errorf("the diagnostic should name the expected kind:\n%s", out.String())
	}
}

func TestSchemaSkipsKindCheckOnUnknowns(t *testing.T) {
	// An unknown carries the kind it will have; a mismatch there is not a
	// user error and reporting it would be noise on every reference.
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Unknown(value.KindString, value.SourceComputed),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable()); ds.HasErrors() {
		t.Errorf("an unknown must not trip kind checking: %+v", ds)
	}
}

func TestSchemaFillsDefaults(t *testing.T) {
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable()); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	size, ok := cfg.Resources["r"].Attrs["size"]
	if !ok {
		t.Fatal("the default for size was not filled in")
	}
	if size.Source != value.SourceDefault {
		t.Errorf("Source = %v, want SourceDefault — a plan must be able to print [default]", size.Source)
	}
	if n, _ := size.AsInt(); n != 10 {
		t.Errorf("size = %d, want the default of 10", n)
	}
}

// There is one schema default per attribute, the same in every environment.
// Environment variation belongs to `environments:`, not to a plugin's defaults.
func TestADefaultDoesNotVaryByEnvironment(t *testing.T) {
	// The direction that matters: the SAME value in every environment. A default
	// that quietly differed would be a second, invisible mechanism for
	// environment variation.
	var seen []int64
	for _, env := range []string{"dev", "staging", "production", "prod"} {
		cfg := oneResource("fake.database", map[string]value.Value{
			"engine": value.String("postgres", value.SourceExplicit),
		})
		bindSchemas(cfg, testRegistry(t), Options{Environment: env}, testTable())
		n, _ := cfg.Resources["r"].Attrs["size"].AsInt()
		seen = append(seen, n)
	}
	for i, n := range seen {
		if n != seen[0] {
			t.Fatalf("the default differs by environment: %v — there is one schema default per attribute, and "+
				"environment %d got %d rather than %d", seen, i, n, seen[0])
		}
	}
	// "production" and "prod" are in that list deliberately: an environment
	// classifier would match on those names, so that is where a surviving copy
	// of one would show.
	if seen[0] != 10 {
		t.Errorf("default = %d, want 10", seen[0])
	}
}

func TestSchemaDefaultNeverOverwritesAnExplicitValue(t *testing.T) {
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(50, value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())

	size := cfg.Resources["r"].Attrs["size"]
	if n, _ := size.AsInt(); n != 50 {
		t.Errorf("size = %d, want the explicit 50", n)
	}
	if size.Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit — an explicit value always wins", size.Source)
	}
}

func TestSchemaMarksSensitiveAttributes(t *testing.T) {
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine":   value.String("postgres", value.SourceExplicit),
		"password": value.String("hunter2", value.SourceExplicit),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())

	if !cfg.Resources["r"].Attrs["password"].Sensitive {
		t.Error("an attribute the schema marks Sensitive must come out sensitive")
	}
}

func TestSchemaDoesNotDeclassifyAnAlreadySensitiveValue(t *testing.T) {
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithSensitive(true),
	})
	bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())

	if !cfg.Resources["r"].Attrs["engine"].Sensitive {
		t.Error("schema binding must add sensitivity, never clear it — engine is not a sensitive attribute but this value arrived classified")
	}
}

// wrongKindProvider is a fake provider whose one resource type declares a
// float attribute but whose default resolver returns an int64 — the mistake
// checkedDefault exists to catch: matching a Go type the switch recognises is
// not the same as matching the declared Kind.
type wrongKindProvider struct{}

func (wrongKindProvider) Name() string { return "wrongkind" }

func (wrongKindProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type: "bad.thing",
		Attributes: map[string]schema.Attribute{
			"ratio": {
				Kind:        value.KindFloat,
				Default:     int64(1), // wrong: declares KindFloat, holds an int64
				Description: "A ratio that should be a float",
			},
		},
	}}
}

func (wrongKindProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, nil
}

func (wrongKindProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, nil
}

func (wrongKindProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, nil
}

func (wrongKindProvider) Delete(context.Context, *resource.ResourceState) error { return nil }

func (wrongKindProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (wrongKindProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (wrongKindProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

func TestSchemaRejectsADefaultThatDoesNotMatchItsDeclaredKind(t *testing.T) {
	reg := registry.New()
	if err := reg.Register("test", wrongKindProvider{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cfg := oneResource("bad.thing", map[string]value.Value{})

	ds := bindSchemas(cfg, reg, Options{Environment: "dev"}, testTable())
	if !ds.HasErrors() {
		t.Fatal("a default that does not match its attribute's declared kind must be an error")
	}
	if v, ok := cfg.Resources["r"].Attrs["ratio"]; ok {
		t.Errorf("the wrong-kinded default must not be filled in, got %+v", v)
	}
}

func TestSchemaReportsEveryProblemAtOnce(t *testing.T) {
	cfg := oneResource("fake.network", map[string]value.Value{
		"nonsense_one": value.Bool(true, value.SourceExplicit),
		"nonsense_two": value.Bool(true, value.SourceExplicit),
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3 (two unknown attributes and one missing required)", len(ds))
	}
}

func TestSchemaStampsProviderDefaultsWithTheirScope(t *testing.T) {
	// fake.database's `size` is optional with a default of 10. Filling it is the
	// only rung of the precedence chain that stages 3 and 4 never see.
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	size, ok := cfg.Resources["r"].Attrs["size"]
	if !ok {
		t.Fatal("the default for `size` was not filled in at all")
	}
	if size.Source != value.SourceDefault {
		t.Errorf("Source = %v, want SourceDefault", size.Source)
	}
	if size.Scope != value.ScopeProviderDefault {
		t.Errorf("Scope = %v, want ScopeProviderDefault — provider defaults are the floor of the precedence chain, and a chain that cannot show its own floor is not explainable", size.Scope)
	}
}

func TestSchemaDoesNotStampValuesConfigurationSupplied(t *testing.T) {
	// The other direction, and the one that matters more: applyDefaults'
	// contract is that an explicit value always beats an implicit one, so a
	// stamp that leaked onto explicit values would make a plan claim the
	// provider supplied something the user wrote. That is a precedence lie,
	// and unlike a missing stamp it is invisible — the value is right and only
	// its provenance is wrong.
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
		"size":   value.Int(50, value.SourceExplicit),
	})
	ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	size := cfg.Resources["r"].Attrs["size"]
	if n, _ := size.AsInt(); n != 50 {
		t.Fatalf("size = %d, want the explicit 50 — applyDefaults must never overwrite a configured value", n)
	}
	if size.Scope != value.ScopeUnset {
		t.Errorf("Scope = %v, want ScopeUnset: nothing in stage 7 supplied this value, so stage 7 must not claim it did", size.Scope)
	}
}

func TestSchemaStampingADefaultDoesNotMakeItCompareUnequal(t *testing.T) {
	// value.Equal ignores Scope. If it ever stopped doing so, every resource
	// with a filled default would diff against the same value from configuration
	// or state, and the no-op plan would fail for most resources in most
	// projects.
	cfg := oneResource("fake.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	})
	if ds := bindSchemas(cfg, testRegistry(t), Options{Environment: "dev"}, testTable()); ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	stamped := cfg.Resources["r"].Attrs["size"]
	plain := value.Int(10, value.SourceDefault)
	if !stamped.Equal(plain) {
		t.Error("a stamped default must still be Equal to the same datum with no Scope — provenance describes how a value was arrived at, not what the desired state is")
	}
}

// nestedDef declares one attribute at the top level and the same spelling one
// level down, so a test can ask whether a name resolves the same way at both
// depths.
func nestedDef() *schema.ResourceDefinition {
	return &schema.ResourceDefinition{
		Type: "fake.service",
		Attributes: map[string]schema.Attribute{
			"cloudRun": {Kind: value.KindString, Optional: true},
			"template": {
				Kind:     value.KindMap,
				Optional: true,
				Fields: map[string]schema.Attribute{
					"cloudRun":    {Kind: value.KindString, Optional: true},
					"serviceName": {Kind: value.KindString, Optional: true, Aliases: []string{"service"}},
				},
			},
		},
	}
}

// TestNestedNamesCanonicaliseLikeTopLevelOnes. A spelling that resolves at the
// top level has to resolve at any depth, or the same file accepts one spelling
// outside a block and demands another inside it, with no diagnostic either way.
func TestNestedNamesCanonicaliseLikeTopLevelOnes(t *testing.T) {
	def := nestedDef()
	attrs := map[string]value.Value{
		"cloudrun": value.String("top", value.SourceExplicit),
		"template": value.Map(map[string]value.Value{
			"cloudrun": value.String("nested", value.SourceExplicit),
			"service":  value.String("aliased", value.SourceExplicit),
		}, value.SourceExplicit),
	}

	var ds diag.Diagnostics
	canonicaliseAttributes(attrs, def, &ds)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	if _, ok := attrs["cloudRun"]; !ok {
		t.Errorf("the top-level name did not canonicalise: keys are %v", keysOf(attrs))
	}
	inner, ok := attrs["template"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("template is not a map: %#v", attrs["template"].Raw)
	}
	if _, ok := inner["cloudRun"]; !ok {
		t.Errorf("a nested name that differs only in case did not canonicalise: keys are %v", keysOf(inner))
	}
	if _, ok := inner["serviceName"]; !ok {
		t.Errorf("a nested alias did not canonicalise: keys are %v. An Aliases list a plugin "+
			"declares on a nested field is inert, so the schema promises a spelling that does nothing", keysOf(inner))
	}
}

// TestANestedNameThatResolvesToNothingIsReported is the half that matters more.
// An unresolved nested key is not rewritten and not refused: it sits inside a
// composite value, which the plugin-boundary check never descends into, so it
// reaches the provider and goes out on the wire as written.
func TestANestedNameThatResolvesToNothingIsReported(t *testing.T) {
	def := nestedDef()
	attrs := map[string]value.Value{
		"template": value.Map(map[string]value.Value{
			"nosuchkey": value.String("x", value.SourceExplicit),
		}, value.SourceExplicit),
	}

	var ds diag.Diagnostics
	canonicaliseAttributes(attrs, def, &ds)
	checkConfiguredAttributes(attrs, def, value.Origin{}, &ds)

	if !ds.HasErrors() {
		t.Fatal("a nested key the type does not declare produced no diagnostic, so it reaches " +
			"the provider unchecked")
	}
	var buf strings.Builder
	ds.Render(&buf)
	for _, want := range []string{"nosuchkey", "template"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, buf.String())
		}
	}
}

// An open map is one whose keys the provider does not know — tags, labels — and
// nil Fields is how a schema says so. Those keys must pass through untouched:
// canonicalising or refusing them would mangle data the provider takes verbatim.
func TestAnOpenMapsKeysAreLeftAlone(t *testing.T) {
	def := &schema.ResourceDefinition{
		Type: "fake.service",
		Attributes: map[string]schema.Attribute{
			"tags": {Kind: value.KindMap, Optional: true}, // no Fields: open
		},
	}
	attrs := map[string]value.Value{
		"tags": value.Map(map[string]value.Value{
			"Name":          value.String("web", value.SourceExplicit),
			"anythingAtAll": value.String("y", value.SourceExplicit),
		}, value.SourceExplicit),
	}

	var ds diag.Diagnostics
	canonicaliseAttributes(attrs, def, &ds)
	checkConfiguredAttributes(attrs, def, value.Origin{}, &ds)

	if ds.HasErrors() {
		var buf strings.Builder
		ds.Render(&buf)
		t.Fatalf("an open map's keys were refused:\n%s", buf.String())
	}
	inner := attrs["tags"].Raw.(map[string]value.Value)
	for _, want := range []string{"Name", "anythingAtAll"} {
		if _, ok := inner[want]; !ok {
			t.Errorf("key %q was rewritten or dropped: keys are %v", want, keysOf(inner))
		}
	}
}
