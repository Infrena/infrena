package registry

import (
	"context"
	"strings"
	"testing"

	"infra/pkg/provider"
	"infra/pkg/resource"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// stubProvider is the minimum a registry test needs. Behavioural provider tests
// live with the fake provider in Task 9.
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
	if err := r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("test.database")}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Definition("test.database"); !ok {
		t.Error("definition not found after registration")
	}
	p, ok := r.Provider("test.database")
	if !ok || p.Name() != "test" {
		t.Error("provider not found after registration")
	}
	if _, ok := r.Definition("test.missing"); ok {
		t.Error("unregistered type must not be found")
	}
}

func TestRegisterRejectsDuplicateType(t *testing.T) {
	r := New()
	p := stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("test.database")}}
	if err := r.Register(p); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(stubProvider{name: "other", defs: []*schema.ResourceDefinition{def("test.database")}})
	if err == nil || !strings.Contains(err.Error(), "test.database") {
		t.Errorf("duplicate registration error = %v; want one naming the type", err)
	}
}

func TestRegisterRejectsDuplicateTypeWithinOneProvider(t *testing.T) {
	r := New()
	err := r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{
		def("test.database"), def("test.database"),
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
	if err := r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{broken}}); err == nil {
		t.Error("a malformed schema must fail at registration, not during a plan")
	}
}

func TestTypesIsSorted(t *testing.T) {
	r := New()
	_ = r.Register(stubProvider{name: "test", defs: []*schema.ResourceDefinition{
		def("test.network"), def("test.application"), def("test.database"),
	}})
	got := r.Types()
	want := []string{"test.application", "test.database", "test.network"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Types() = %v, want %v — explain output must be stable", got, want)
		}
	}
}
