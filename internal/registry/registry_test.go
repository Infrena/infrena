package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// stubProvider is the minimum a registry test needs. Behavioural provider tests live
// with the fake provider.
type stubProvider struct {
	name string
	defs []*schema.ResourceDefinition
}

func (s stubProvider) Name() string                              { return s.name }
func (s stubProvider) Definitions() []*schema.ResourceDefinition { return s.defs }
func (s stubProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }
func (s stubProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (s stubProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (s stubProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func def(t string) *schema.ResourceDefinition {
	return &schema.ResourceDefinition{
		Type:       t,
		Attributes: map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
	}
}

func TestRegisterAndLookup(t *testing.T) {
	r := New()
	if err := r.Register("test", stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("fake.database")}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Definition("fake.database"); !ok {
		t.Error("definition not found after registration")
	}
	p, ok := r.ProviderFor("fake.database", "test")
	if !ok || p.Name() != "test" {
		t.Error("provider not found after registration")
	}
	if _, ok := r.Definition("test.missing"); ok {
		t.Error("unregistered type must not be found")
	}
}

func TestRegisterRejectsDuplicateType(t *testing.T) {
	r := New()
	p := stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("fake.database")}}
	if err := r.Register("test", p); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register("other", stubProvider{name: "other", defs: []*schema.ResourceDefinition{def("fake.database")}})
	if err == nil || !strings.Contains(err.Error(), "fake.database") {
		t.Errorf("duplicate registration error = %v; want one naming the type", err)
	}
}

func TestRegisterRejectsDuplicateTypeWithinOneProvider(t *testing.T) {
	r := New()
	err := r.Register("test", stubProvider{name: "test", defs: []*schema.ResourceDefinition{
		def("fake.database"), def("fake.database"),
	}})
	if err == nil {
		t.Fatal("a provider declaring the same type twice must be rejected, not silently clobbered")
	}
	if len(r.Types()) != 0 {
		t.Error("a failed registration must leave the registry untouched")
	}
}

func TestRegisterValidatesDefinitions(t *testing.T) {
	r := New()
	broken := &schema.ResourceDefinition{
		Type:       "test.broken",
		Attributes: map[string]schema.Attribute{"x": {Kind: value.KindString, Required: true, Computed: true}},
	}
	if err := r.Register("test", stubProvider{name: "test", defs: []*schema.ResourceDefinition{broken}}); err == nil {
		t.Error("a malformed schema must fail at registration, not during a plan")
	}
}

// A Reference names another type, and whether that type exists is a fact about
// the whole set of a plugin's definitions — checkable only here, with the whole
// set in hand at once, not inside a single definition's own Validate().
func TestRegisterRefusesADanglingReference(t *testing.T) {
	r := New()
	subnet := &schema.ResourceDefinition{
		Type: "test.subnet",
		Attributes: map[string]schema.Attribute{
			"vpc_id": {Kind: value.KindString, Required: true,
				References: &schema.Reference{Type: "test.vpc", Attribute: "id"}},
		},
	}
	err := r.Register("test", stubProvider{name: "test", defs: []*schema.ResourceDefinition{subnet}})
	if err == nil {
		t.Fatal("a reference to a type this plugin does not declare must fail at registration")
	}
	if !strings.Contains(err.Error(), "test.vpc") {
		t.Errorf("error = %q, want it to name the missing type", err)
	}
	if _, ok := r.Definition("test.subnet"); ok {
		t.Error("a failed registration must leave the registry untouched")
	}
}

func TestRegisterRefusesTheModuleNamespace(t *testing.T) {
	r := New()
	err := r.Register("rogue", stubProvider{name: "rogue", defs: []*schema.ResourceDefinition{
		def("module.app_stack"),
	}})
	if err == nil {
		t.Fatal("a provider claiming `module.` would silently shadow every module call " +
			"of that name")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error = %q, want it to say the namespace is reserved", err)
	}
	if _, ok := r.Definition("module.app_stack"); ok {
		t.Error("a failed registration must leave the registry untouched")
	}
}

// The boundary, and the half that keeps the guard honest. The rule is a `module.`
// prefix, not the substring: a provider legitimately offering `test.module_group`,
// `aws.module` or `modulearium.thing` must still register.
//
// The cheapest implementation that satisfies the refusal test is
// strings.Contains(d.Type, "module"), which passes it while quietly reserving far
// more than the namespace it was meant to protect — including every type of a
// provider whose name happens to start with "module".
func TestRegisterAcceptsATypeMerelyContainingModule(t *testing.T) {
	for _, typ := range []string{"test.module_group", "aws.module", "modulearium.thing"} {
		r := New()
		if err := r.Register("legit", stubProvider{name: "legit", defs: []*schema.ResourceDefinition{
			def(typ),
		}}); err != nil {
			t.Errorf("Register(%q) = %v, want it accepted: the guard reserves the `module.` prefix, not the word", typ, err)
		}
		if _, ok := r.Definition(typ); !ok {
			t.Errorf("%q did not reach the registry", typ)
		}
	}
}

func TestTypesIsSorted(t *testing.T) {
	r := New()
	_ = r.Register("test", stubProvider{name: "test", defs: []*schema.ResourceDefinition{
		def("fake.network"), def("fake.application"), def("fake.database"),
	}})
	got := r.Types()
	want := []string{"fake.application", "fake.database", "fake.network"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Types() = %v, want %v — explain output must be stable", got, want)
		}
	}
}
