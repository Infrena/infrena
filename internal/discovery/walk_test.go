package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

func cloudWith(t *testing.T, rs map[string]*testprovider.CloudResource) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloud.json")
	c := &testprovider.Cloud{Resources: rs}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.Register("fake", testprovider.New(path)); err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestWalkNamesEverythingItFinds — the whole point of Walk over calling
// Discover directly.
func TestWalkNamesEverythingItFinds(t *testing.T) {
	reg := cloudWith(t, map[string]*testprovider.CloudResource{
		"db-9":  {Type: "fake.database", Attributes: map[string]any{"engine": "postgres", "name": "orders"}},
		"net-1": {Type: "fake.network", Attributes: map[string]any{"cidr": "10.0.0.0/16"}},
	})

	got, problems := Walk(context.Background(), reg, nil)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(got) != 2 {
		t.Fatalf("found %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "database-orders" {
		t.Errorf("the database's name = %q, want its name tag", got[0].Name)
	}
	if got[1].Name != "network-net-1" {
		t.Errorf("the network's name = %q, want its provider ID", got[1].Name)
	}
}

// TestWalkNamesDeterministically is why naming happens after the sort rather
// than during the walk.
//
// Unique is order-dependent by construction — the first `orders` keeps the
// unsuffixed name — so if names were assigned as results arrived, which
// resource got the clean name would depend on map iteration order. Two runs
// would then generate configuration with two resources' names swapped, and the
// diff would look like both had been renamed.
func TestWalkNamesDeterministically(t *testing.T) {
	rs := map[string]*testprovider.CloudResource{}
	for _, id := range []string{"db-1", "db-2", "db-3", "db-4", "db-5", "db-6"} {
		rs[id] = &testprovider.CloudResource{
			Type:       "fake.database",
			Attributes: map[string]any{"engine": "postgres", "name": "orders"},
		}
	}
	reg := cloudWith(t, rs)

	var first []string
	for i := range 20 {
		got, _ := Walk(context.Background(), reg, nil)
		var names []string
		for _, r := range got {
			names = append(names, r.ProviderID+"="+r.Name)
		}
		if i == 0 {
			first = names
			continue
		}
		for j := range names {
			if names[j] != first[j] {
				t.Fatalf("run %d assigned %v, run 0 assigned %v", i, names, first)
			}
		}
	}
	// The lowest-sorting ID keeps the unsuffixed name, because the sort runs
	// first. That is arbitrary but it must be STABLE.
	if first[0] != "db-1=database-orders" {
		t.Errorf("first[0] = %q, want db-1 to hold the unsuffixed name", first[0])
	}
}

// TestWalkSortsWhatAProviderReturns is what makes the sort in Walk
// load-bearing, and it exists because a sabotage proved the determinism test
// above could not tell.
//
// providers/test happens to return its own results already sorted, so with the
// only registered provider being that one, moving Walk's sort after the naming
// loop changes NOTHING and every test still passed. The contract is that Walk
// sorts, not that its callers happen to — a provider is free to return results
// in whatever order its API paginated them, and AWS will.
//
// Naming follows the sort, so the assertion is on both: the order returned AND
// which resource kept the unsuffixed name.
func TestWalkSortsWhatAProviderReturns(t *testing.T) {
	// All three carry the SAME name tag, so they collide — which is what makes
	// the ORDER of naming observable. Without a collision every resource is
	// named after its own ID and naming order cannot be detected at all.
	tagged := func(id string) provider.DiscoveredResource {
		return provider.DiscoveredResource{Type: "spy.thing", ProviderID: id,
			Attributes: map[string]value.Value{"name": value.String("orders", value.SourceProvider)}}
	}
	spy := &recordingProvider{returns: []provider.DiscoveredResource{
		tagged("zeta"), tagged("alpha"), tagged("mid"),
	}}
	reg := registry.New()
	if err := reg.Register("spy", spy); err != nil {
		t.Fatal(err)
	}

	got, problems := Walk(context.Background(), reg, nil)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ProviderID)
	}
	if strings.Join(ids, ",") != "alpha,mid,zeta" {
		t.Errorf("Walk returned %v, want them sorted — the provider returned them unsorted and "+
			"Walk is what fixes that", ids)
	}
	// Naming follows the SORTED order, not the arrival order: the
	// lowest-sorting ID keeps the unsuffixed name. Sort after naming instead
	// and `zeta` keeps it, because zeta arrived first — the returned slice is
	// still sorted, so only this assertion can tell the two apart.
	if got[0].Name != "thing-orders" {
		t.Errorf("%s is named %q; the first resource in sorted order must hold the unsuffixed "+
			"name, or which resource gets it depends on the order the provider paginated",
			got[0].ProviderID, got[0].Name)
	}
	for _, r := range got[1:] {
		if r.Name == "thing-orders" {
			t.Errorf("%s also holds the unsuffixed name", r.ProviderID)
		}
		if !strings.Contains(r.Name, r.ProviderID) {
			t.Errorf("%s is named %q, which does not contain its ID", r.ProviderID, r.Name)
		}
	}
}

// TestWalkAsksAProviderOnlyAboutItsOwnTypes. A real Discover is API calls;
// asking a provider about a type it does not offer is a round trip whose answer
// is known in advance.
func TestWalkAsksAProviderOnlyAboutItsOwnTypes(t *testing.T) {
	spy := &recordingProvider{}
	reg := registry.New()
	if err := reg.Register("spy", spy); err != nil {
		t.Fatal(err)
	}

	if _, problems := Walk(context.Background(), reg, []string{"spy.thing"}); len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(spy.asked) != 1 {
		t.Fatalf("provider asked %d times, want 1: %v", len(spy.asked), spy.asked)
	}
	if len(spy.asked[0]) != 1 || spy.asked[0][0] != "spy.thing" {
		t.Errorf("asked for %v, want only the requested type", spy.asked[0])
	}

	// And a request naming NO type this provider offers must not reach it at
	// all. Asserting only the positive case passes against a Walk that asks
	// every provider everything and filters afterwards.
	spy.asked = nil
	if _, problems := Walk(context.Background(), reg, []string{"other.thing"}); len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(spy.asked) != 0 {
		t.Errorf("a provider offering none of the requested types was still asked: %v", spy.asked)
	}
}

// TestWalkReportsAProviderThatCannotLook. "Found nothing" and "could not look"
// are different answers, and a user deciding whether their account is empty
// needs to know which one they got.
func TestWalkReportsAProviderThatCannotLook(t *testing.T) {
	reg := registry.New()
	if err := reg.Register("spy", &recordingProvider{fail: errors.New("credentials expired")}); err != nil {
		t.Fatal(err)
	}

	got, problems := Walk(context.Background(), reg, nil)
	if len(got) != 0 {
		t.Errorf("a failing provider returned results: %+v", got)
	}
	if len(problems) != 1 {
		t.Fatalf("want 1 problem reported, got %v", problems)
	}
	if !strings.Contains(problems[0].Error(), "credentials expired") {
		t.Errorf("the problem does not carry the provider's own message: %v", problems[0])
	}
	if !strings.Contains(problems[0].Error(), "spy") {
		t.Errorf("the problem does not name the provider that failed: %v", problems[0])
	}
}

// recordingProvider records what it was asked, so a test can assert the
// question rather than only the answer.
type recordingProvider struct {
	asked   [][]string
	fail    error
	returns []provider.DiscoveredResource
}

func (p *recordingProvider) Name() string { return "spy" }
func (p *recordingProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:       "spy.thing",
		Attributes: map[string]schema.Attribute{"a": {Kind: value.KindString}},
	}}
}
func (p *recordingProvider) Discover(_ context.Context, req provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	p.asked = append(p.asked, req.Types)
	if p.fail != nil {
		return nil, p.fail
	}
	return p.returns, nil
}
func (p *recordingProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *recordingProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *recordingProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *recordingProvider) Delete(context.Context, *resource.ResourceState) error {
	return provider.ErrNotImplemented
}
func (p *recordingProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (p *recordingProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}
