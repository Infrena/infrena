package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/discovery"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// importableProvider discovers NOTHING and imports anything asked of it.
//
// That is the shape of the case these tests exist for: a provider whose scan
// scope does not cover a resource it can nonetheless read by ID. An AWS instance
// scanning the regions `discover_regions` names is exactly this — the resource
// is real, the account is right, and a survey that was never meant to be
// exhaustive does not list it.
type importableProvider struct {
	name  string
	types []string
	// absent is a provider ID this provider will deny, so a genuinely missing
	// resource can be told from one merely outside the scan scope.
	absent string
}

func (p importableProvider) Name() string { return p.name }

func (p importableProvider) Definitions() []*schema.ResourceDefinition {
	out := make([]*schema.ResourceDefinition, 0, len(p.types))
	for _, t := range p.types {
		out = append(out, &schema.ResourceDefinition{
			Type:         t,
			Attributes:   map[string]schema.Attribute{"name": {Kind: value.KindString}},
			Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true, Import: true},
		})
	}
	return out
}

func (p importableProvider) Import(_ context.Context, resourceType, id string) (*resource.ResourceState, error) {
	if id == p.absent {
		return nil, errNoSuchResource
	}
	return &resource.ResourceState{
		Type:       resourceType,
		Provider:   p.name,
		ProviderID: id,
		Attributes: map[string]value.Value{
			"name": value.String("from-import", value.SourceProvider),
		},
	}, nil
}

// Discover returns nothing, which is the premise.
func (importableProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, nil
}

func (importableProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, nil
}
func (importableProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (importableProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (importableProvider) Delete(context.Context, *resource.ResourceState) error { return nil }
func (importableProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = importableProvider{}

var errNoSuchResource = &noSuchResourceError{}

type noSuchResourceError struct{}

func (*noSuchResourceError) Error() string { return "no such resource in this account" }

func registryWith(t *testing.T, instances map[string]importableProvider) *registry.Registry {
	t.Helper()
	reg := registry.New()
	for name, p := range instances {
		if err := reg.Register(name, p); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}
	return reg
}

// TestASelectorDiscoveryDidNotReturnIsFetchedDirectly is the whole point.
//
// Discovery is bounded, so a resource outside the scan scope used to be
// unimportable at any price, INCLUDING by naming it exactly. Naming a `type.id`
// is a statement that you know the resource exists; discarding it in favour of a
// survey that was never meant to be exhaustive is the wrong way round.
func TestASelectorDiscoveryDidNotReturnIsFetchedDirectly(t *testing.T) {
	reg := registryWith(t, map[string]importableProvider{
		"main": {name: "main", types: []string{"cloud.bucket"}},
	})

	got, err := narrowToSelectors(context.Background(), reg,
		nil, []string{"cloud.bucket.b-1"}, "")
	if err != nil {
		t.Fatalf("a named resource outside the scan scope must be fetched, not refused: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].ProviderID != "b-1" || got[0].Type != "cloud.bucket" || got[0].Provider != "main" {
		t.Errorf("result = %+v, want cloud.bucket b-1 from main", got[0])
	}
	if got[0].Name == "" {
		t.Error("a directly fetched resource must still be named, or import has nothing to call it")
	}
	if _, ok := got[0].Attributes["name"]; !ok {
		t.Error("the provider's attributes did not survive, so `--generate` would write an empty resource")
	}
}

// TestADirectlyFetchedNameCannotCollideWithADiscoveredOne. Names are proposals a
// user reads before importing, and two resources sharing one would mean the
// second silently replaced the first in generated configuration.
func TestADirectlyFetchedNameCannotCollideWithADiscoveredOne(t *testing.T) {
	reg := registryWith(t, map[string]importableProvider{
		"main": {name: "main", types: []string{"cloud.bucket"}},
	})
	found := []discovery.Result{
		{Name: "bucket-b-1", Type: "cloud.bucket", Provider: "main", ProviderID: "b-1"},
	}

	got, err := narrowToSelectors(context.Background(), reg, found,
		[]string{"cloud.bucket.b-1", "cloud.bucket.b-2"}, "")
	if err != nil {
		t.Fatalf("narrowToSelectors: %v", err)
	}
	names := map[string]bool{}
	for _, r := range got {
		if names[r.Name] {
			t.Errorf("two resources were both named %q", r.Name)
		}
		names[r.Name] = true
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (one discovered, one fetched)", len(got))
	}
}

// TestADirectFetchRefusesWhenTwoInstancesCouldServeIt mirrors the rule already
// applied to a discovered ID found in two accounts. A provider ID is unique
// within an account rather than across them, so asking an arbitrary instance
// could adopt a resource from the wrong one — not a mistake a later diagnostic
// can undo.
func TestADirectFetchRefusesWhenTwoInstancesCouldServeIt(t *testing.T) {
	reg := registryWith(t, map[string]importableProvider{
		// Both are instances of ONE plugin, which is what the registry requires:
		// two plugins declaring the same type is a different error entirely.
		"main":  {name: "cloud", types: []string{"cloud.bucket"}},
		"acct2": {name: "cloud", types: []string{"cloud.bucket"}},
	})

	_, err := narrowToSelectors(context.Background(), reg, nil, []string{"cloud.bucket.b-1"}, "")
	if err == nil {
		t.Fatal("two instances offer this type; picking one adopts from an account the user did not name")
	}
	for _, want := range []string{"acct2", "main", "--provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q, so the user cannot act on it:\n%v", want, err)
		}
	}

	// And --provider is the way out the message names, so it has to work.
	got, err := narrowToSelectors(context.Background(), reg, nil, []string{"cloud.bucket.b-1"}, "acct2")
	if err != nil {
		t.Fatalf("--provider did not resolve the ambiguity, so the suggested action is a broken promise: %v", err)
	}
	if len(got) != 1 || got[0].Provider != "acct2" {
		t.Errorf("got %+v, want one result from acct2", got)
	}
}

// TestADirectFetchSplitsTheTypeUsingTheRegistryNotTheDots is why splitSelector
// exists in the shape it does.
//
// Selectors are matched WHOLE everywhere else in import precisely because a
// provider ID may contain dots — a hostname, an ARN, a resource path. A direct
// fetch has to split, since the provider must be told type and ID separately,
// and splitting at the last dot would hand it `cloud.bucket.files.example` as a
// type and `com` as an ID.
func TestADirectFetchSplitsTheTypeUsingTheRegistryNotTheDots(t *testing.T) {
	reg := registryWith(t, map[string]importableProvider{
		"main": {name: "main", types: []string{"cloud.bucket", "cloud.bucket.replica"}},
	})

	got, err := narrowToSelectors(context.Background(), reg, nil,
		[]string{"cloud.bucket.files.example.com"}, "")
	if err != nil {
		t.Fatalf("an ID containing dots must survive: %v", err)
	}
	if got[0].Type != "cloud.bucket" || got[0].ProviderID != "files.example.com" {
		t.Errorf("split as type=%q id=%q, want cloud.bucket / files.example.com", got[0].Type, got[0].ProviderID)
	}

	// Longest match wins, so the more specific registered type is preferred over
	// the one that is merely a prefix of it.
	got, err = narrowToSelectors(context.Background(), reg, nil,
		[]string{"cloud.bucket.replica.r-9"}, "")
	if err != nil {
		t.Fatalf("narrowToSelectors: %v", err)
	}
	if got[0].Type != "cloud.bucket.replica" || got[0].ProviderID != "r-9" {
		t.Errorf("split as type=%q id=%q, want cloud.bucket.replica / r-9", got[0].Type, got[0].ProviderID)
	}
}

// TestAResourceThatReallyDoesNotExistStillFails. Falling back to a direct fetch
// must not turn every typo into a successful import of nothing, and the error
// has to carry what the provider said rather than a generic refusal.
func TestAResourceThatReallyDoesNotExistStillFails(t *testing.T) {
	reg := registryWith(t, map[string]importableProvider{
		"main": {name: "main", types: []string{"cloud.bucket"}, absent: "gone"},
	})

	_, err := narrowToSelectors(context.Background(), reg, nil, []string{"cloud.bucket.gone"}, "")
	if err == nil {
		t.Fatal("a resource the provider denies must fail the import")
	}
	if !strings.Contains(err.Error(), "no such resource") {
		t.Errorf("the provider's own explanation was dropped:\n%v", err)
	}

	// An unregistered type is a different mistake and must read as one.
	_, err = narrowToSelectors(context.Background(), reg, nil, []string{"other.thing.x-1"}, "")
	if err == nil {
		t.Fatal("a selector naming no registered type must fail")
	}
	if !strings.Contains(err.Error(), "other.thing.x-1") {
		t.Errorf("the error does not name the selector that failed:\n%v", err)
	}
}

var _ = address.Address{}
