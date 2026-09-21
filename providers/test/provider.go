package test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// ErrInjected wraps a deliberately injected failure so ClassifyError can
// recognise it.
type ErrInjected struct {
	Message string
	// Retryability is the classification the failure rule declared.
	Retryability Retryability
}

// Error returns the injected failure message.
func (e *ErrInjected) Error() string { return e.Message }

// Provider is the fake provider: its world lives in a hand-editable JSON
// cloud file so a human can mutate fake infrastructure and see drift
// detected.
type Provider struct {
	cloudPath string
	defs      []*schema.ResourceDefinition
	byType    map[string]*schema.ResourceDefinition

	// mu guards the whole load-mutate-save cycle. Every operation, Read
	// included, rewrites the cloud file, so without it concurrent callers
	// interleave and lose each other's writes. In-process only: there is no
	// cross-process lock on the cloud file.
	mu sync.Mutex
}

// New returns a fake provider backed by the cloud file at cloudPath.
func New(cloudPath string) *Provider {
	defs := definitions()
	byType := make(map[string]*schema.ResourceDefinition, len(defs))
	for _, d := range defs {
		byType[d.Type] = d
	}
	return &Provider{cloudPath: cloudPath, defs: defs, byType: byType}
}

// Provider satisfies the provider interface. Asserted at compile time so
// interface drift surfaces here rather than at integration.
var _ provider.Provider = (*Provider)(nil)

// Name returns the provider name.
func (p *Provider) Name() string { return "fake" }

// Definitions returns the resource definitions the fake provider supports.
func (p *Provider) Definitions() []*schema.ResourceDefinition { return p.defs }

// ClassifyError classifies an injected failure for retryability. Any other
// error is treated as not safe to retry.
func (p *Provider) ClassifyError(err error) provider.Retryability {
	if injected, ok := errors.AsType[*ErrInjected](err); ok {
		return injected.Retryability.Classify()
	}
	return provider.NotSafeToRetry
}

// delay applies the cloud file's simulated latency. It is deliberately outside
// the mutex: the lock protects the load-mutate-save cycle, and holding it
// across a sleep would serialise the whole provider, silently disarming every
// concurrency test that relies on this latency.
func (p *Provider) delay(ctx context.Context) error {
	c, err := LoadCloud(p.cloudPath)
	if err != nil {
		return err
	}
	d := c.Delay()
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// begin loads the cloud and applies failure injection. Callers hold p.mu.
func (p *Provider) begin(ctx context.Context, op, addr string) (*Cloud, error) {
	c, err := LoadCloud(p.cloudPath)
	if err != nil {
		return nil, err
	}
	rule, failing := c.ShouldFail(op, addr)
	// ShouldFail advances persisted bookkeeping whenever a rule matches its op
	// and address, so the cloud is written back regardless of outcome. Saving
	// only on failure would reset a non-firing rule's Seen counter on the next
	// load, and an Nth greater than 1 could never be reached.
	if err := c.Save(p.cloudPath); err != nil {
		return nil, err
	}
	if failing {
		msg := rule.Message
		if msg == "" {
			msg = fmt.Sprintf("injected %s failure for %s", op, addr)
		}
		return nil, &ErrInjected{Message: msg, Retryability: rule.Retryability}
	}
	return c, nil
}

// Create creates a resource in the fake cloud and assigns it a provider ID.
func (p *Provider) Create(ctx context.Context, d *resource.DesiredResource) (*resource.ResourceState, error) {
	if err := p.delay(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "create", d.Address.String())
	if err != nil {
		return nil, err
	}

	id := c.AllocateID(idPrefix(d.Type))
	attrs := map[string]any{}
	for name, v := range d.Attrs {
		attrs[name] = toRaw(v)
	}
	maps.Copy(attrs, p.computedFor(d.Type, id))

	c.Resources[id] = &CloudResource{Type: d.Type, Address: d.Address.String(), Attributes: attrs}
	if err := c.Save(p.cloudPath); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	st := p.toState(d.Address.String(), d.Type, id, attrs)
	st.CreatedAt, st.UpdatedAt = now, now
	return st, nil
}

// Read returns the current state of a resource from the fake cloud. It
// returns (nil, nil) when the resource no longer exists there, so drift
// caused by a hand-edit or external deletion is observable.
func (p *Provider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	if err := p.delay(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "read", current.Address.String())
	if err != nil {
		return nil, err
	}
	obj, ok := c.Resources[current.ProviderID]
	if !ok {
		return nil, nil // deleted outside infrena
	}
	st := p.toState(current.Address.String(), obj.Type, current.ProviderID, obj.Attributes)
	carryForward(st, current)
	return st, nil
}

// Update applies desired attributes to an existing resource in the fake
// cloud without changing its provider ID.
func (p *Provider) Update(ctx context.Context, current *resource.ResourceState, d *resource.DesiredResource) (*resource.ResourceState, error) {
	if err := p.delay(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "update", d.Address.String())
	if err != nil {
		return nil, err
	}
	obj, ok := c.Resources[current.ProviderID]
	if !ok {
		return nil, fmt.Errorf("cannot update %s: %s no longer exists", d.Address, current.ProviderID)
	}
	for name, v := range d.Attrs {
		obj.Attributes[name] = toRaw(v)
	}
	if err := c.Save(p.cloudPath); err != nil {
		return nil, err
	}

	st := p.toState(d.Address.String(), obj.Type, current.ProviderID, obj.Attributes)
	carryForward(st, current)
	// An update stamps a new mtime. Everything else carried above is
	// unchanged, including Lifecycle: the executor overwrites that from the
	// plan on its way to state, so a provider setting it here would be a
	// second writer of a field it does not own.
	st.UpdatedAt = time.Now().UTC()
	return st, nil
}

// carryForward copies the fields the cloud file does not record — timestamps,
// dependency edges and lifecycle — from the previous state onto a freshly
// derived one.
//
// Read and Update both need this, and keeping it in one place stops them
// drifting apart — in particular over Dependencies, the only source of
// destroy-ordering edges for a resource that is no longer in configuration.
// Create deliberately does not call it: there is no previous state, and the
// executor knows the edges from configuration.
func carryForward(next, current *resource.ResourceState) {
	// The provider instance is carried from the state that was loaded, not
	// re-derived: toState writes p.Name(), which is the plugin, and a plugin
	// cannot know which of several instances of itself it is. Re-deriving it
	// would overwrite the instance name and take the destroy path's only clue
	// with it.
	next.Provider = current.Provider
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = current.UpdatedAt
	next.Lifecycle = current.Lifecycle
	// Copied, not aliased: a refresh must never mutate the state loaded from
	// disk.
	next.Dependencies = append([]address.Address(nil), current.Dependencies...)
}

// Delete removes a resource from the fake cloud.
func (p *Provider) Delete(ctx context.Context, current *resource.ResourceState) error {
	if err := p.delay(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "delete", current.Address.String())
	if err != nil {
		return err
	}
	delete(c.Resources, current.ProviderID)
	return c.Save(p.cloudPath)
}

// Discover enumerates the fake cloud.
//
// It reports every resource the cloud file holds, including ones this project
// never created: a CloudResource's `address` is empty for anything written by
// hand, and discovery deliberately does not care. Infrastructure that predates
// the tool is the only reason discovery exists.
//
// req.Types filters when non-empty. The filter is applied here rather than by
// the caller because a real provider answers `infrena discover aws.rds` with
// one API call per type asked for, and the fake provider must not model a
// cheaper contract than the real one.
//
// Results are sorted by ProviderID: they are printed to a user and diffed by
// scripts, and Go's map iteration is randomised.
func (p *Provider) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	if err := p.delay(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// "discover" is an op name a failure rule can match, so injected failures
	// reach this path like every other. begin also persists the rule
	// bookkeeping, which is why it is used rather than LoadCloud directly.
	c, err := p.begin(ctx, "discover", "")
	if err != nil {
		return nil, err
	}

	wanted := map[string]bool{}
	for _, t := range req.Types {
		wanted[t] = true
	}

	out := make([]provider.DiscoveredResource, 0, len(c.Resources))
	for id, obj := range c.Resources {
		if len(wanted) > 0 && !wanted[obj.Type] {
			continue
		}
		// toState applies the schema's sensitivity and stamps SourceProvider.
		// Reused rather than re-derived: a discovered secret that arrives
		// unmarked is one a generator will write into a file destined for
		// version control.
		st := p.toState("", obj.Type, id, obj.Attributes)
		out = append(out, provider.DiscoveredResource{
			Type:              obj.Type,
			ProviderID:        id,
			Attributes:        st.Attributes,
			SystemOwned:       obj.SystemOwned,
			SystemOwnedReason: obj.SystemOwnedReason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out, nil
}

// Import adopts one existing resource by provider ID.
//
// The type is checked against what the cloud holds rather than trusted.
// `import fake.network db-9` naming a real database would otherwise write state
// claiming a database is a network, and the next plan would propose replacing
// real infrastructure to resolve a disagreement the tool invented.
//
// No address is assigned here: naming is the caller's job, and a provider that
// invented one would be deciding what the user's configuration calls things.
func (p *Provider) Import(ctx context.Context, resourceType, id string) (*resource.ResourceState, error) {
	if err := p.delay(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	c, err := p.begin(ctx, "import", id)
	if err != nil {
		return nil, err
	}
	obj, ok := c.Resources[id]
	if !ok {
		return nil, fmt.Errorf("test provider: no resource with ID %q exists", id)
	}
	if obj.Type != resourceType {
		return nil, fmt.Errorf("test provider: %q is a %s, not a %s", id, obj.Type, resourceType)
	}
	return p.toState("", obj.Type, id, obj.Attributes), nil
}

// toState converts cloud JSON back into typed, provenance-carrying state. Every
// value a provider reports carries SourceProvider; the schema decides which are
// sensitive.
func (p *Provider) toState(addr, resourceType, id string, attrs map[string]any) *resource.ResourceState {
	def := p.byType[resourceType]
	out := map[string]value.Value{}
	for name, raw := range attrs {
		// A JSON null means the attribute is not set. Someone hand-editing the
		// cloud file may null a value out; turning that into the string
		// "<nil>" would silently corrupt it.
		if raw == nil {
			continue
		}
		v := fromRaw(raw)
		v = v.WithSource(value.SourceProvider)
		if def != nil {
			if a, ok := def.Attribute(name); ok && a.Sensitive {
				v = v.WithSensitive(true)
			}
		}
		out[name] = v
	}
	parsed, err := address.Parse(addr)
	if err != nil {
		parsed = address.Address{Name: addr}
	}
	return &resource.ResourceState{
		Address:    parsed,
		Type:       resourceType,
		Provider:   p.Name(),
		ProviderID: id,
		Attributes: out,
	}
}

func (p *Provider) computedFor(resourceType, id string) map[string]any {
	switch resourceType {
	case "fake.network":
		return map[string]any{"id": id}
	case "fake.database":
		return map[string]any{"endpoint": id + ".db.test"}
	case "fake.application":
		return map[string]any{"url": "https://" + id + ".test"}
	default:
		return nil
	}
}

func idPrefix(resourceType string) string {
	switch resourceType {
	case "fake.network":
		return "net"
	case "fake.database":
		return "db"
	case "fake.application":
		return "app"
	default:
		return strings.ReplaceAll(resourceType, ".", "-")
	}
}

// toRaw converts a typed Value into the plain JSON datum the cloud file holds,
// the inverse of fromRaw.
//
// Writing v.Raw directly would work for scalars but serialise a composite's
// []value.Value or map[string]value.Value through Value.MarshalJSON, putting
// the engine's internal wire objects into a file a human is meant to be able to
// hand-edit — and reading them back would produce nested garbage.
func toRaw(v value.Value) any {
	switch v.Kind {
	case value.KindList:
		items, _ := v.Raw.([]value.Value)
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, toRaw(item))
		}
		return out
	case value.KindMap:
		items, _ := v.Raw.(map[string]value.Value)
		out := make(map[string]any, len(items))
		for k, item := range items {
			out[k] = toRaw(item)
		}
		return out
	default:
		return v.Raw
	}
}

// fromRaw converts a JSON datum into a typed Value. JSON numbers arrive as
// float64; whole numbers become integers so they compare equal to configured
// integer attributes.
func fromRaw(raw any) value.Value {
	switch v := raw.(type) {
	case string:
		return value.String(v, value.SourceProvider)
	case bool:
		return value.Bool(v, value.SourceProvider)
	case float64:
		if v == float64(int64(v)) {
			return value.Int(int64(v), value.SourceProvider)
		}
		return value.Float(v, value.SourceProvider)
	case int64:
		return value.Int(v, value.SourceProvider)
	case []any:
		items := make([]value.Value, 0, len(v))
		for _, item := range v {
			items = append(items, fromRaw(item))
		}
		return value.List(items, value.SourceProvider)
	case map[string]any:
		items := map[string]value.Value{}
		for k, item := range v {
			items[k] = fromRaw(item)
		}
		return value.Map(items, value.SourceProvider)
	case nil:
		// Unreachable for a top-level attribute (toState skips nulls), but a
		// null nested inside a list or map lands here. Return the zero Value,
		// whose KindInvalid fails loudly downstream rather than masquerading
		// as the string "<nil>".
		return value.Value{}
	default:
		return value.String(fmt.Sprint(v), value.SourceProvider)
	}
}

// CloudPath is where this instance's world lives.
//
// Exported for one reason: it is the only observable consequence of an instance's
// configuration, so it is what a test asserting "the resolved value reached the
// provider" has to read. Nothing in the engine calls it.
func (p *Provider) CloudPath() string { return p.cloudPath }
