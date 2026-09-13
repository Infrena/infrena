package executor

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/infrata/infrata/internal/compiler"
	"github.com/infrata/infrata/internal/planner"
	"github.com/infrata/infrata/internal/refresh"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// realisticProvider is the only Provider double in this package with an
// actual schema.
//
// Every other fixture here returns []*schema.ResourceDefinition{{Type:
// ...}} — no Attributes map at all, no Computed, no ForceNew, no Sensitive.
// That is a deliberate minimum for tests about scheduling, retries,
// persistence and failure isolation, none of which read a schema: the
// executor never looks at one (the only production reference to a
// definition's attributes anywhere below the planner is summary.go reading
// values, not shapes). Padding those fixtures out would add noise without
// adding reach.
//
// What the empty schemas DID hide is upstream: what the PLANNER stages into
// an operation's After depends entirely on the schema, and every test here
// builds its plan by hand. planner.afterAttributes turns each unset
// Computed attribute into value.Unknown with a NIL Expr, purely so the plan
// can render "(known after apply)". A hand-built After never contains one,
// so nothing in this package's own tests could reach the code that has to
// cope with it — and a Critical lived there: resolveAfter evaluated those
// unknowns and reported them as broken dependency ordering, which made
// `infra apply` fail for every resource type with an unset computed
// attribute, which is very nearly all of them.
//
// So this fixture has a realistic schema, and the test below builds its plan
// through planner.Compute rather than by hand. That is the whole point: the
// precondition is established by production, not manufactured by the test.
// resolve_test.go pins the same decision as a unit, by constructing the
// value directly; both are wanted, and only this one proves the shape
// actually arises.
type realisticProvider struct {
	resourceType string
}

func (p *realisticProvider) Name() string { return "realistic" }

func (p *realisticProvider) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:        p.resourceType,
		Description: "A resource shaped like a real one",
		Attributes: map[string]schema.Attribute{
			"cidr":     {Kind: value.KindString, Required: true, ForceNew: true, Description: "Address range"},
			"password": {Kind: value.KindString, Sensitive: true, Description: "Administrator password"},
			"id":       {Kind: value.KindString, Computed: true, Description: "Assigned identifier"},
			"endpoint": {Kind: value.KindString, Computed: true, Description: "Connection endpoint"},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Update: true, Delete: true},
	}}
}

// Create assigns the computed attributes itself, the way a real provider
// does — the caller never supplies them, and a fixture that echoed back
// only what it was given could not tell "the executor dropped the computed
// key correctly" from "there was never a computed key".
func (p *realisticProvider) Create(_ context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	attrs := map[string]value.Value{}
	maps.Copy(attrs, d.Attrs)
	attrs["id"] = value.String(d.Address.Name+"-1", value.SourceProvider)
	attrs["endpoint"] = value.String(d.Address.Name+".example", value.SourceProvider)
	return &resource.ResourceState{
		Address:    d.Address,
		Type:       d.Type,
		Provider:   p.Name(),
		ProviderID: d.Address.Name + "-1",
		Attributes: attrs,
	}, nil
}

func (p *realisticProvider) Read(_ context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	return current.Clone(), nil
}

func (p *realisticProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *realisticProvider) Delete(context.Context, *resource.ResourceState) error { return nil }

func (p *realisticProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}

func (p *realisticProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func (p *realisticProvider) ClassifyError(error) provider.Retryability {
	return provider.NotSafeToRetry
}

var _ provider.Provider = (*realisticProvider)(nil)

// TestApplyOfAPlannedCreateWithComputedAttributes applies a plan the PLANNER
// produced from a real schema, rather than one this test assembled.
//
// The distinction is the finding. Hand-built plans in this package never
// carry an unset computed attribute, because a test author writes the keys
// they are thinking about; the planner writes every Computed attribute the
// schema declares, as an unknown with no expression. That difference is
// what let resolveAfter's nil-Expr bug reach a release candidate.
func TestApplyOfAPlannedCreateWithComputedAttributes(t *testing.T) {
	const resourceType = "realistic.thing"
	reg := registry.New()
	if err := reg.Register("test", &realisticProvider{resourceType: resourceType}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cfg := compiler.ResolvedConfig{
		Project:     "proj",
		Environment: "dev",
		Resources: map[string]*resource.ResolvedResource{
			"net": {
				Address:  addr("net"),
				Type:     resourceType,
				Provider: "test",
				Attrs: map[string]value.Value{
					"cidr":     value.String("10.0.0.0/16", value.SourceExplicit),
					"password": value.String("hunter2", value.SourceExplicit),
				},
			},
		},
	}

	st := state.New("proj", "dev")
	p, planDiags := planner.Compute(cfg, st, refresh.Observations{}, planner.Options{
		Environment: "dev",
		Registry:    reg,
		Now:         func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
	})
	if planDiags.HasErrors() {
		t.Fatalf("planning: %+v", planDiags)
	}

	// Guard, not decoration: if the planner ever stops staging computed
	// attributes as unknowns, this test silently stops testing anything and
	// would keep passing. Assert the fixture reaches the shape under test.
	if len(p.Operations) != 1 {
		t.Fatalf("Operations = %d, want 1", len(p.Operations))
	}
	id, ok := p.Operations[0].After["id"]
	if !ok || id.Known || id.Expr != nil {
		t.Fatalf(`After["id"] = %+v (present=%v), want an unknown with no expression — the shape this test exists to exercise`, id, ok)
	}

	g, err := planner.BuildExecution(p, noDeps)
	if err != nil {
		t.Fatalf("BuildExecution: %v", err)
	}
	backend := newLockedBackend(t, "dev")

	result, ds := Apply(context.Background(), p, g, st, Options{
		Parallelism: 2, PerProvider: 2, Registry: reg, Backend: backend, Environment: "dev",
	})
	if ds.HasErrors() {
		t.Fatalf("apply reported errors for an ordinary create with computed attributes: %+v", ds)
	}
	if len(result.Applied) != 1 || result.Applied[0].String() != "net" {
		t.Fatalf("Applied = %v, want [net]", result.Applied)
	}

	got, ok := st.Get(addr("net"))
	if !ok {
		t.Fatal("net was not recorded in state")
	}
	if s, _ := got.Attributes["id"].AsString(); s != "net-1" {
		t.Errorf(`state id = %q, want "net-1" — the provider's assigned value must be what is recorded`, s)
	}
	if s, _ := got.Attributes["endpoint"].AsString(); s != "net.example" {
		t.Errorf(`state endpoint = %q, want "net.example"`, s)
	}
	if s, _ := got.Attributes["cidr"].AsString(); s != "10.0.0.0/16" {
		t.Errorf(`state cidr = %q, want "10.0.0.0/16"`, s)
	}
	// Schema-declared sensitivity is the provider's to re-apply on read; what
	// the executor must not do is lose the value itself.
	if s, _ := got.Attributes["password"].AsString(); s != "hunter2" {
		t.Errorf("state password was not recorded")
	}
}
