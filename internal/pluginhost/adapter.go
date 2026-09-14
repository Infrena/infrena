package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// Plugin adapts a Client to provider.Plugin, so the registry cannot tell a
// subprocess from an in-process provider.
type Plugin struct {
	client *Client
	defs   []*schema.ResourceDefinition
	byType map[string]*schema.ResourceDefinition

	// dir is the project directory, handed to every instance this plugin
	// configures.
	//
	// Held HERE rather than passed through the engine's configuration maps. A
	// plugin needs it — a relative `cloud:` path resolves against the project, not
	// against whatever working directory the plugin inherited — but it is not part
	// of anybody's configuration, and a reserved key travelling through
	// internal/providers would be one every diagnostic and every `defaults:` check
	// had to learn to ignore.
	dir string
}

var _ provider.Plugin = (*Plugin)(nil)

// Name is the plugin's own name.
func (p *Plugin) Name() string { return p.client.Name() }

// Definitions are the schemas the plugin sent, already validated.
func (p *Plugin) Definitions() []*schema.ResourceDefinition { return p.defs }

// Version is what the plugin reported, for `plugins:` constraints and --verbose.
func (p *Plugin) Version() string { return p.client.PluginVersion() }

// Close shuts the plugin down.
func (p *Plugin) Close() error { return p.client.Close() }

// loadSchemas fetches and validates what a plugin offers.
//
// ON LOAD, through the same checks RegisterPlugin already applies to a built-in,
// plus one that only matters for a binary somebody else built: a plugin's types
// must be prefixed with its own name.
func (p *Plugin) loadSchemas(ctx context.Context) error {
	var result pluginproto.SchemasResult
	if err := p.client.call(ctx, pluginproto.MethodSchemas, nil, &result); err != nil {
		return fmt.Errorf("asking %s for its schemas: %w", p.Name(), err)
	}
	if len(result.Definitions) == 0 {
		return fmt.Errorf("the %s plugin offers no resource types at all", p.Name())
	}

	p.byType = make(map[string]*schema.ResourceDefinition, len(result.Definitions))
	for _, d := range result.Definitions {
		if d == nil {
			return fmt.Errorf("the %s plugin sent an empty resource definition", p.Name())
		}
		if err := d.Validate(); err != nil {
			return fmt.Errorf("the %s plugin sent an invalid schema: %w", p.Name(), err)
		}
		// A type name says where it came from, and two plugins cannot both claim
		// one. Without this a plugin could serve `aws.instance` and quietly take
		// over another plugin's resources; the registry's existing check refuses
		// the second one to load, which makes the outcome depend on ordering.
		if !strings.HasPrefix(d.Type, p.Name()+".") {
			return fmt.Errorf(
				"the %s plugin declares resource type %q, which is not prefixed with its own name\n"+
					"A plugin serves %s.* and nothing else, so that a type name says where it came "+
					"from and two plugins cannot claim the same one.",
				p.Name(), d.Type, p.Name())
		}
		if reserved := reservedAttributeOf(d); reserved != "" {
			// prevent_destroy and retain belong to infrena's lifecycle handling and
			// are accepted in a provider instance's `defaults:` for every resource
			// (§12.1), so an attribute of either name would make one key mean two
			// things. Refused here as well as in the registry, so the message names
			// the PLUGIN and arrives when its schemas load.
			return fmt.Errorf(
				"the %s plugin declares attribute %q on %s, which is reserved: every "+
					"resource accepts it as a lifecycle option",
				p.Name(), reserved, d.Type)
		}
		p.byType[d.Type] = d
	}
	p.defs = result.Definitions
	return nil
}

// New configures one instance and returns it as a provider.
func (p *Plugin) New(cfg provider.Config) (provider.Provider, error) {
	var result pluginproto.ConfigureResult
	err := p.client.call(context.Background(), pluginproto.MethodConfigure, pluginproto.ConfigureParams{
		Instance: cfg.Instance,
		Config:   cfg.Values,
		// The host's own, not the caller's. ProjectDir is context infrena supplies,
		// so a configuration file cannot tell a plugin its project lives elsewhere.
		Dir: p.dir,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &remoteProvider{plugin: p, handle: result.Handle, instance: cfg.Instance}, nil
}

// remoteProvider is one configured instance living in a plugin process.
//
// Every method here is the same shape: send what the plugin owns, and rebuild what
// comes back under the host's own rules. That is the point of the type — there is
// deliberately no path from a plugin's answer to the engine that skips it.
type remoteProvider struct {
	plugin   *Plugin
	handle   string
	instance string
}

var _ provider.Provider = (*remoteProvider)(nil)

func (r *remoteProvider) Name() string                              { return r.plugin.Name() }
func (r *remoteProvider) Definitions() []*schema.ResourceDefinition { return r.plugin.Definitions() }

// ClassifyError reads the classification the plugin sent with its error.
//
// provider.ClassifyError takes an `error`, which does not cross a pipe, so the SDK
// asks the plugin to classify on its own side and the answer travels on the error.
// An error from anywhere else is not safe to retry: a host-side failure — a broken
// pipe, a plugin that died — is exactly the case where retrying a Create could
// create a second resource.
func (r *remoteProvider) ClassifyError(err error) provider.Retryability {
	if pe, ok := errors.AsType[*ProviderError](err); ok {
		return pe.Retryability
	}
	return provider.NotSafeToRetry
}

// Read reports the remote system's view, with bookkeeping re-attached.
//
// The host sends only type, provider ID and attributes, and puts every field the
// plugin does not own back on the result itself. §31's carry-forward contract
// stops being a plugin obligation: a field never sent cannot be dropped, and
// losing Lifecycle makes a prevent_destroy guard vanish silently.
func (r *remoteProvider) Read(ctx context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	var result pluginproto.ResourceResult
	err := r.plugin.client.call(ctx, pluginproto.MethodRead, r.params(current.Address, current.Type, current.ProviderID, current.Attributes), &result)
	if err != nil {
		return nil, err
	}
	if result.Absent {
		return nil, nil
	}
	return r.rebuild(current.Type, result, current)
}

func (r *remoteProvider) Create(ctx context.Context, desired *resource.DesiredResource) (*resource.ResourceState, error) {
	var result pluginproto.ResourceResult
	err := r.plugin.client.call(ctx, pluginproto.MethodCreate, r.params(desired.Address, desired.Type, "", desired.Attrs), &result)
	if err != nil {
		return nil, err
	}
	if result.Absent {
		return nil, r.nothingHappened("create", desired.Type)
	}
	// A create has no PRIOR state to re-attach bookkeeping from, so the desired
	// resource is where it comes from. The ADDRESS is the load-bearing part: a
	// resource state with no address is dropped by state.Set, so the run reports
	// "0 applied" for an operation whose provider really did create something.
	// internal/cli's apply tests caught exactly that.
	//
	// Lifecycle is deliberately NOT taken from desired here: the executor stamps it
	// onto the returned state from the plan, which is the one place that knows what
	// the configuration asked for.
	return r.rebuild(desired.Type, result, &resource.ResourceState{
		Address: desired.Address,
	})
}

func (r *remoteProvider) Update(ctx context.Context, current *resource.ResourceState, desired *resource.DesiredResource) (*resource.ResourceState, error) {
	var result pluginproto.ResourceResult
	err := r.plugin.client.call(ctx, pluginproto.MethodUpdate, pluginproto.UpdateParams{
		Handle:  r.handle,
		Current: r.params(current.Address, current.Type, current.ProviderID, current.Attributes),
		Desired: r.params(desired.Address, desired.Type, current.ProviderID, desired.Attrs),
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.Absent {
		return nil, r.nothingHappened("update", current.Type)
	}
	return r.rebuild(current.Type, result, current)
}

func (r *remoteProvider) Delete(ctx context.Context, current *resource.ResourceState) error {
	return r.plugin.client.call(ctx, pluginproto.MethodDelete,
		r.params(current.Address, current.Type, current.ProviderID, current.Attributes), nil)
}

func (r *remoteProvider) Discover(ctx context.Context, req provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	var result pluginproto.DiscoverResult
	err := r.plugin.client.call(ctx, pluginproto.MethodDiscover, pluginproto.DiscoverParams{
		Handle: r.handle, Types: req.Types,
	}, &result)
	if err != nil {
		return nil, err
	}
	out := make([]provider.DiscoveredResource, 0, len(result.Found))
	for _, f := range result.Found {
		def, ok := r.plugin.byType[f.Type]
		if !ok {
			// Discovery is a survey, not a mutation, so an unknown type is
			// skipped rather than failing the whole run: a plugin may know about
			// resources it does not model.
			continue
		}
		attrs, err := r.check(def, f.Attributes)
		if err != nil {
			return nil, err
		}
		out = append(out, provider.DiscoveredResource{Type: f.Type, ProviderID: f.ProviderID, Attributes: attrs})
	}
	return out, nil
}

func (r *remoteProvider) Import(ctx context.Context, resourceType, id string) (*resource.ResourceState, error) {
	var result pluginproto.ResourceResult
	err := r.plugin.client.call(ctx, pluginproto.MethodImport, pluginproto.ImportParams{
		Handle: r.handle, Type: resourceType, ID: id,
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.Absent {
		return nil, fmt.Errorf("%s: no %s with id %q", r.plugin.Name(), resourceType, id)
	}
	return r.rebuild(resourceType, result, nil)
}

func (r *remoteProvider) params(addr address.Address, resourceType, providerID string, attrs map[string]value.Value) pluginproto.ResourceParams {
	return pluginproto.ResourceParams{
		Handle:     r.handle,
		Type:       resourceType,
		Address:    addr.String(),
		ProviderID: providerID,
		Attributes: attrs,
	}
}

// nothingHappened turns a plugin's empty success into the error it really is.
//
// A nil result with no error is indistinguishable from "nothing happened" to
// everything above the provider interface. If the call DID take effect, that
// resource is now real and tracked nowhere — unfindable by a later plan or
// destroy. Saying so is the only useful thing left to do.
func (r *remoteProvider) nothingHappened(method, resourceType string) error {
	return fmt.Errorf(
		"%s reported no result from %s of %s, so infrena cannot record what now exists.\n"+
			"If the operation did take effect, that resource exists and is not in state: "+
			"check %s directly before re-running.\n"+
			"This is a defect in the plugin — a successful %s must report the resource it "+
			"acted on.",
		r.plugin.Name(), method, resourceType, r.plugin.Name(), method)
}

// rebuild turns a plugin's answer into state the engine may trust.
//
// carry is the state the host already held, or nil for a resource that did not
// exist before. Everything the plugin does not own comes from there.
func (r *remoteProvider) rebuild(resourceType string, result pluginproto.ResourceResult, carry *resource.ResourceState) (*resource.ResourceState, error) {
	def, ok := r.plugin.byType[resourceType]
	if !ok {
		return nil, fmt.Errorf("%s returned a %s, which it does not declare", r.plugin.Name(), resourceType)
	}
	attrs, err := r.check(def, result.Attributes)
	if err != nil {
		return nil, err
	}

	out := &resource.ResourceState{
		Type:       resourceType,
		ProviderID: result.ProviderID,
		Attributes: attrs,
		// UNCONDITIONALLY, because this object IS one instance and knows which:
		// a plugin cannot know which instance of itself it is, and the alternative
		// is relying on a later caller to stamp it. The executor does stamp it for
		// create and update, but `import` writes straight to state — so a resource
		// adopted into a project came back with no instance at all, and the next
		// plan could not find a provider for it.
		Provider: r.instance,
	}
	if carry != nil {
		// The bookkeeping the plugin was never sent and therefore cannot have
		// lost. Dependencies is the only source of destroy-ordering edges once a
		// resource leaves configuration (§14); Lifecycle is prevent_destroy.
		out.Address = carry.Address
		if carry.Provider != "" {
			out.Provider = carry.Provider
		}
		out.Dependencies = carry.Dependencies
		out.Lifecycle = carry.Lifecycle
		out.CreatedAt = carry.CreatedAt
		out.UpdatedAt = carry.UpdatedAt
		if out.ProviderID == "" {
			out.ProviderID = carry.ProviderID
		}
	}
	return out, nil
}

// check enforces the two rules about what a plugin may say about a value.
//
// Together they mean a plugin cannot put a secret somewhere it does not belong,
// and cannot claim a value came from somewhere it did not.
func (r *remoteProvider) check(def *schema.ResourceDefinition, attrs map[string]value.Value) (map[string]value.Value, error) {
	if len(attrs) == 0 {
		return map[string]value.Value{}, nil
	}
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]value.Value, len(attrs))
	for _, name := range names {
		attr, declared := def.Attribute(name)
		if !declared {
			// Not silently persisted into state, where it would appear in a plan
			// as a change to an attribute nothing can explain.
			return nil, fmt.Errorf(
				"%s returned attribute %s on a %s, which its own schema does not declare\n"+
					"Declared: %s",
				r.plugin.Name(), strconv.Quote(name), def.Type, strings.Join(declaredNames(def), ", "))
		}
		v := attrs[name]
		// SENSITIVITY IS FORCED FROM THE SCHEMA, never merely accepted from the
		// plugin. A plugin that forgets the flag must not be able to put a
		// password into a plan, a report or a generated file (§36).
		if attr.Sensitive {
			v = v.WithSensitive(true)
		}
		// PROVENANCE IS THE HOST'S. A plugin reports a fact about the remote
		// system, so the value came from the provider — it cannot claim a user
		// wrote it, which is what would make a plan credit the wrong file.
		v = v.WithSource(value.SourceProvider).WithScope(value.ScopeUnset)
		out[name] = v
	}
	return out, nil
}

// reservedAttributeOf returns the reserved name a definition collides with, or "".
//
// The list comes from internal/registry, so the two checks cannot come to disagree
// about which names the engine owns.
func reservedAttributeOf(d *schema.ResourceDefinition) string {
	for _, name := range registry.ReservedAttributes {
		if _, declared := d.Attributes[name]; declared {
			return name
		}
	}
	return ""
}

func declaredNames(def *schema.ResourceDefinition) []string {
	out := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
