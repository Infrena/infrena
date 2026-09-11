package test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
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
	// interleave and lose each other's writes. This is the in-process half
	// only: the cross-process file lock belongs to M3.
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
func (p *Provider) Name() string { return "test" }

// Definitions returns the resource definitions the fake provider supports.
func (p *Provider) Definitions() []*schema.ResourceDefinition { return p.defs }

// ClassifyError classifies an injected failure for retryability. Any other
// error is treated as not safe to retry.
func (p *Provider) ClassifyError(err error) provider.Retryability {
	var injected *ErrInjected
	if errors.As(err, &injected) {
		return injected.Retryability.Classify()
	}
	return provider.NotSafeToRetry
}

// delay applies the cloud file's simulated latency. It is deliberately outside
// the mutex: the lock protects the load-mutate-save cycle against the file, and
// holding it across a sleep would serialise the whole provider, silently
// disarming every concurrency test in M2 and M3.
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
	for name, computed := range p.computedFor(d.Type, id) {
		attrs[name] = computed
	}

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
		return nil, nil // deleted outside infra
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
	// unchanged — including Lifecycle, which the executor overwrites from the
	// plan on its way to state (internal/executor/apply.go), so a provider
	// setting it here would be a second writer of a field it does not own.
	st.UpdatedAt = time.Now().UTC()
	return st, nil
}

// carryForward copies the fields the cloud file does not record — timestamps,
// dependency edges and lifecycle — from the previous state onto a freshly
// derived one.
//
// Read and Update both need this, and keeping it in one place is what stops
// them drifting apart: Update previously omitted Dependencies, which spec §14
// makes the only source of destroy-ordering edges for a resource that is no
// longer in configuration. Create deliberately does not call it — on a create
// there is no previous state, and the executor knows the edges from
// configuration.
func carryForward(next, current *resource.ResourceState) {
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

// Discover is not implemented until Phase 2.
func (p *Provider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, fmt.Errorf("test provider: discovery arrives in Phase 2: %w", provider.ErrNotImplemented)
}

// Import is not implemented until Phase 2.
func (p *Provider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, fmt.Errorf("test provider: import arrives in Phase 2: %w", provider.ErrNotImplemented)
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
	case "test.network":
		return map[string]any{"id": id}
	case "test.database":
		return map[string]any{"endpoint": id + ".db.test"}
	case "test.application":
		return map[string]any{"url": "https://" + id + ".test"}
	default:
		return nil
	}
}

func idPrefix(resourceType string) string {
	switch resourceType {
	case "test.network":
		return "net"
	case "test.database":
		return "db"
	case "test.application":
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
// the engine's internal wire objects into a file spec §8.4 requires a human to
// be able to hand-edit — and reading them back would produce nested garbage.
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
