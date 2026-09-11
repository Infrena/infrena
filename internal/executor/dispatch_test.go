package executor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/infrata/infrata/internal/planner"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
	testprovider "github.com/infrata/infrata/providers/test"
)

// poisonProvider is a Provider double whose every method fails the test
// immediately. It exists to prove a negative: that dispatch never reaches
// the provider at all for an operation kind that must not call it.
type poisonProvider struct {
	t            *testing.T
	resourceType string
}

func (p poisonProvider) Name() string { return "poison" }
func (p poisonProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{Type: p.resourceType}}
}
func (p poisonProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil, nil
}
func (p poisonProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil, nil
}
func (p poisonProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil, nil
}
func (p poisonProvider) Delete(context.Context, *resource.ResourceState) error {
	p.t.Fatal("dispatch must not call the provider for this operation")
	return nil
}
func (p poisonProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (p poisonProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p poisonProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

var _ provider.Provider = poisonProvider{}

func mustCreate(t *testing.T, prov *testprovider.Provider, name, resourceType string, attrs map[string]value.Value) *resource.ResourceState {
	t.Helper()
	st, err := prov.Create(context.Background(), &resource.DesiredResource{
		Address: address.Address{Name: name},
		Type:    resourceType,
		Attrs:   attrs,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return st
}

func TestDispatchForgetMakesNoProviderCall(t *testing.T) {
	node := planner.OpNode{Address: address.Address{Name: "old"}, Kind: planner.OpForget, Phase: planner.PhaseDestroy}
	current := &resource.ResourceState{Address: node.Address, Type: "test.network", ProviderID: "net-1"}

	result, err := dispatch(context.Background(), poisonProvider{t: t}, node, current, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result != nil {
		t.Errorf("result = %+v, want nil — forget records nothing new, it only drops the existing entry", result)
	}
}

func TestDispatchCreateCallsProviderCreate(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))

	node := planner.OpNode{Address: address.Address{Name: "net"}, Kind: planner.OpCreate, Phase: planner.PhaseCreate}
	desired := &resource.DesiredResource{
		Address: node.Address,
		Type:    "test.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
	}

	got, err := dispatch(context.Background(), prov, node, nil, desired)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got == nil || got.ProviderID == "" {
		t.Fatalf("got = %+v, want a resource state with a provider ID", got)
	}
	if cidr, _ := got.Attributes["cidr"].AsString(); cidr != "10.0.0.0/16" {
		t.Errorf("cidr = %q", cidr)
	}
}

func TestDispatchUpdateReusesProviderID(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))
	current := mustCreate(t, prov, "net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	})

	node := planner.OpNode{Address: current.Address, Kind: planner.OpUpdate, Phase: planner.PhaseCreate}
	desired := &resource.DesiredResource{
		Address: current.Address,
		Type:    "test.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.1.0.0/16", value.SourceExplicit)},
	}

	got, err := dispatch(context.Background(), prov, node, current, desired)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.ProviderID != current.ProviderID {
		t.Errorf("ProviderID = %q, want unchanged %q — update must not allocate a new object", got.ProviderID, current.ProviderID)
	}
	if cidr, _ := got.Attributes["cidr"].AsString(); cidr != "10.1.0.0/16" {
		t.Errorf("cidr = %q, want the new value", cidr)
	}
}

func TestDispatchDestroyRemovesTheResource(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))
	current := mustCreate(t, prov, "net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	})

	node := planner.OpNode{Address: current.Address, Kind: planner.OpDestroy, Phase: planner.PhaseDestroy}
	got, err := dispatch(context.Background(), prov, node, current, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil — Delete returns no state", got)
	}

	after, err := prov.Read(context.Background(), current)
	if err != nil {
		t.Fatalf("Read after destroy: %v", err)
	}
	if after != nil {
		t.Error("resource still exists at the provider after dispatch destroyed it")
	}
}

func TestDispatchReplaceCreatePhaseAllocatesANewObject(t *testing.T) {
	dir := t.TempDir()
	prov := testprovider.New(filepath.Join(dir, "cloud.json"))
	original := mustCreate(t, prov, "net", "test.network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	})

	// A replace is two nodes at the SAME address, distinguished only by
	// Phase — OpNode.ID() cannot tell them apart (contract). Drive both
	// through dispatch the way Task 8's worker pool will: destroy first,
	// then create, both carrying Kind == OpReplace.
	destroyNode := planner.OpNode{Address: original.Address, Kind: planner.OpReplace, Phase: planner.PhaseDestroy}
	if _, err := dispatch(context.Background(), prov, destroyNode, original, nil); err != nil {
		t.Fatalf("destroy phase: %v", err)
	}

	createNode := planner.OpNode{Address: original.Address, Kind: planner.OpReplace, Phase: planner.PhaseCreate}
	desired := &resource.DesiredResource{
		Address: original.Address,
		Type:    "test.network",
		Attrs:   map[string]value.Value{"cidr": value.String("10.2.0.0/16", value.SourceExplicit)},
	}
	got, err := dispatch(context.Background(), prov, createNode, nil, desired)
	if err != nil {
		t.Fatalf("create phase: %v", err)
	}

	// The defect this catches: an implementation keying its behaviour off
	// node.Kind alone would see OpReplace both times and could dispatch the
	// create phase as an Update reusing original's ProviderID instead of a
	// fresh Create — silently patching the OLD object rather than replacing
	// it, defeating the entire point of a replace. A reused ID here must
	// fail this assertion.
	if got.ProviderID == original.ProviderID {
		t.Fatalf("ProviderID = %q, same as the destroyed original %q — the create phase must allocate a new object, not update the old one", got.ProviderID, original.ProviderID)
	}
	if got.ProviderID == "" {
		t.Fatal("create phase produced no provider ID")
	}
}
