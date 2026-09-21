// Package registry maps resource types to their definitions and providers.
// `infrena explain` renders directly from it, so documentation cannot drift from
// the schemas it describes.
package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// Registry maps resource types to their definitions, and (type, instance) pairs to
// the provider serving them.
//
// Two dimensions, and only one of them is per instance. A resource type's
// schema belongs to the plugin and is identical across every instance of it, so
// definitions stay keyed by type alone. What differs per instance is the
// provider object — its credentials, its account, its endpoint — and that is
// what a resource's `provider:` selects.
type Registry struct {
	definitions map[string]*schema.ResourceDefinition
	// pluginOf records which plugin declared each type, so a second plugin
	// claiming it is refused while a second instance of the same plugin is not.
	pluginOf map[string]string
	// instances is instance name -> resource type -> provider.
	instances map[string]map[string]provider.Provider
	// pluginFor records each instance's plugin, for diagnostics.
	pluginFor map[string]string
	// plugins holds each registered plugin by name: the factory half of
	// registration, which can be asked for a configured instance later.
	//
	// A name may map to nil, meaning a plugin known only because a caller handed
	// over an already-constructed provider to Register. Such a name can be
	// reported and counted but cannot construct anything, which is why
	// RegisterInstance says so rather than dereferencing a nil.
	plugins map[string]provider.Plugin

	// loader supplies a plugin the registry has not been given, which is what
	// makes plugin loading follow from configuration rather than from a list
	// somebody maintains: `plugin: aws`, or a resource of type `aws.instance`,
	// is itself the instruction to go and find infrena-plugin-aws.
	//
	// nil where a caller hands over its own providers directly, so every use of
	// it is guarded.
	loader Loader
}

// Loader finds and starts a plugin by name. internal/pluginhost implements it.
//
// An interface here rather than a dependency on internal/pluginhost, because
// the registry is the low-level map every other package already imports and a
// cycle through the host would be immediate.
type Loader interface {
	Load(ctx context.Context, name string) (provider.Plugin, error)
}

// SetLoader supplies the loader used for plugins not already registered.
func (r *Registry) SetLoader(l Loader) { r.loader = l }

// EnsurePlugin registers the named plugin, loading it if it is not here yet.
//
// Idempotent: a second call for a plugin already registered does nothing, which is
// what lets every caller that needs a plugin simply say so without first checking.
func (r *Registry) EnsurePlugin(ctx context.Context, name string) error {
	if _, known := r.plugins[name]; known {
		// Known at all is enough, including known only by name because a caller
		// handed over a constructed provider: that caller has already supplied
		// the schemas, and loading a binary over the top would replace a
		// provider somebody chose deliberately.
		return nil
	}
	if r.loader == nil {
		return fmt.Errorf("no plugin named %s is registered, and there is no way to load one", name)
	}
	p, err := r.loader.Load(ctx, name)
	if err != nil {
		return err
	}
	// A name registered by a constructed provider is nil here; replacing it
	// with a real factory is an upgrade, not a conflict, so RegisterPlugin's
	// duplicate check is deliberately not reached.
	delete(r.plugins, name)
	return r.RegisterPlugin(p)
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		definitions: map[string]*schema.ResourceDefinition{},
		pluginOf:    map[string]string{},
		instances:   map[string]map[string]provider.Provider{},
		pluginFor:   map[string]string{},
		plugins:     map[string]provider.Plugin{},
	}
}

// Register adds every definition a provider offers, under an instance name.
//
// instance is what a resource's `provider:` selects. Two instances of one
// plugin both register and serve the same types, while two different plugins
// claiming one type is an error, because nothing could then say which owns it.
//
// Nothing is mutated until checkDefinitions has passed, so a failed
// registration leaves the registry untouched.
func (r *Registry) Register(instance string, p provider.Provider) error {
	if instance == "" {
		return fmt.Errorf("provider %s: an instance name is required", p.Name())
	}
	if existing, taken := r.pluginFor[instance]; taken {
		return fmt.Errorf("provider instance %q is already registered, by plugin %s", instance, existing)
	}

	defs := p.Definitions()
	if err := r.checkDefinitions(p.Name(), defs); err != nil {
		return err
	}

	byType := make(map[string]provider.Provider, len(defs))
	for _, d := range defs {
		// The definition is written unconditionally. A second instance of one
		// plugin re-registers an identical schema, so last-write-wins is not a
		// choice between two things — it is the same thing twice.
		r.definitions[d.Type] = d
		r.pluginOf[d.Type] = p.Name()
		byType[d.Type] = p
	}
	r.instances[instance] = byType
	r.pluginFor[instance] = p.Name()
	if _, known := r.plugins[p.Name()]; !known {
		// Known by name only: a constructed provider carries no factory, so
		// this entry can be listed and counted but not asked for another
		// instance.
		r.plugins[p.Name()] = nil
	}
	return nil
}

// RegisterPlugin adds a plugin's schemas, and no instance of it.
//
// This is the half of registration that needs no configuration, and running it
// alone breaks the cycle: the compiler can resolve a type against Definition
// before any provider object exists, and therefore before the variables an
// instance's configuration interpolates have been resolved.
//
// A registry holding only plugins dispatches nothing. RegisterInstance supplies
// the other half once the configuration is resolved.
func (r *Registry) RegisterPlugin(p provider.Plugin) error {
	if existing, taken := r.plugins[p.Name()]; taken && existing != nil {
		return fmt.Errorf("plugin %s is already registered", p.Name())
	}
	if err := r.checkDefinitions(p.Name(), p.Definitions()); err != nil {
		return err
	}
	for _, d := range p.Definitions() {
		r.definitions[d.Type] = d
		r.pluginOf[d.Type] = p.Name()
	}
	r.plugins[p.Name()] = p
	return nil
}

// RegisterInstance constructs one instance of a registered plugin from its
// resolved configuration, and registers the provider object.
//
// config comes from internal/providers, which resolved it against variables,
// which needed a compile, which needed the schemas RegisterPlugin supplied.
func (r *Registry) RegisterInstance(instance, plugin string, config map[string]value.Value) error {
	if instance == "" {
		return fmt.Errorf("plugin %s: an instance name is required", plugin)
	}
	p, known := r.plugins[plugin]
	if !known {
		return fmt.Errorf("no plugin named %s is registered; available: %s",
			plugin, strings.Join(r.PluginNames(), ", "))
	}
	if p == nil {
		return fmt.Errorf("plugin %s was registered as a constructed provider, so it cannot "+
			"build a further instance", plugin)
	}
	built, err := p.New(provider.Config{Instance: instance, Values: config})
	if err != nil {
		return err
	}
	// A duplicate instance name is Register's refusal, not a silent
	// replacement: nothing constructs an instance twice, so reaching that
	// refusal means two things believe they own one name.
	return r.Register(instance, built)
}

// HasInstance reports whether an instance is already registered.
func (r *Registry) HasInstance(instance string) bool {
	_, ok := r.pluginFor[instance]
	return ok
}

// PluginNames lists every registered plugin, sorted — including ones known by name
// only. Factories reports the subset that can construct an instance.
func (r *Registry) PluginNames() []string {
	out := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Factories lists the plugins that can construct an instance, sorted.
//
// The distinction matters to exactly one decision: what the implicit instance of a
// project with no `providers:` block is called. See internal/providers.Prepare.
func (r *Registry) Factories() []string {
	out := make([]string, 0, len(r.plugins))
	for name, p := range r.plugins {
		if p != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// checkDefinitions validates a set of definitions before anything is mutated, so a
// failed registration leaves the registry untouched.
//
// ValidateAll runs first: it covers each definition's own Validate and the
// relationships between them — a Reference naming a type or attribute this
// plugin never declares — which is only checkable with the whole set in hand.
//
// `seen` catches a plugin declaring the same type twice in one call, which the
// registry-state check alone cannot: nothing has been written yet.
func (r *Registry) checkDefinitions(pluginName string, defs []*schema.ResourceDefinition) error {
	if err := schema.ValidateAll(defs); err != nil {
		return fmt.Errorf("provider %s: %w", pluginName, err)
	}
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if strings.HasPrefix(d.Type, "module.") {
			// Module expansion selects instantiations by this prefix, so a
			// provider claiming the namespace would turn a user's
			// `type: module.app_stack` into a silently shadowed provider
			// resource: a plan that is wrong and looks fine. Caught at
			// registration, which is that provider's own test run.
			return fmt.Errorf("provider %s: resource type %q is reserved: the `module.` namespace "+
				"is how configuration instantiates a module", pluginName, d.Type)
		}
		if owner, claimed := r.pluginOf[d.Type]; claimed && owner != pluginName {
			return fmt.Errorf("provider %s: resource type %q is already declared by plugin %s",
				pluginName, d.Type, owner)
		}
		if reserved := reservedAttributeOf(d); reserved != "" {
			// Every resource accepts the lifecycle names as options, so a
			// plugin declaring an attribute of the same name would make one
			// key mean two things: an engine lifecycle flag and a provider
			// attribute.
			return fmt.Errorf("provider %s: resource type %q declares attribute %q, which is "+
				"reserved: every resource accepts it as a lifecycle option",
				pluginName, d.Type, reserved)
		}
		if seen[d.Type] {
			return fmt.Errorf("provider %s declares resource type %q more than once", pluginName, d.Type)
		}
		seen[d.Type] = true
	}

	return nil
}

// ReservedAttributes are the attribute names the engine owns on every resource.
//
// Exported because internal/providers validates a `defaults:` key against the
// same list: a key naming one of these is valid there and belongs to no schema,
// so the two checks have to agree about which names those are.
var ReservedAttributes = []string{"prevent_destroy", "retain"}

// reservedAttributeOf returns the reserved name a definition collides with, or "".
func reservedAttributeOf(d *schema.ResourceDefinition) string {
	for _, name := range ReservedAttributes {
		if _, declared := d.Attributes[name]; declared {
			return name
		}
	}
	return ""
}

// Definition returns the schema for a resource type, or false if not registered.
func (r *Registry) Definition(resourceType string) (*schema.ResourceDefinition, bool) {
	d, ok := r.definitions[resourceType]
	return d, ok
}

// ProviderFor returns the provider serving a resource type in one instance.
//
// Both dimensions are required, and there is deliberately no single-argument
// form: a lookup that fell back to "whichever instance offers this type" would
// silently send a resource to an account nobody chose.
func (r *Registry) ProviderFor(resourceType, instance string) (provider.Provider, bool) {
	byType, ok := r.instances[instance]
	if !ok {
		return nil, false
	}
	p, ok := byType[resourceType]
	return p, ok
}

// Instance pairs an instance name with the provider serving it.
type Instance struct {
	// Name is the instance name a resource's `provider:` selects.
	Name string
	// Plugin is the plugin this instance was built from.
	Plugin   string
	Provider provider.Provider
}

// Instances lists every registered instance, sorted by name. Discovery asks
// each instance rather than each plugin, because two instances of one plugin
// hold different infrastructure.
func (r *Registry) Instances() []Instance {
	names := make([]string, 0, len(r.instances))
	for name := range r.instances {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Instance, 0, len(names))
	for _, name := range names {
		// Any type of the instance resolves to the same provider object.
		var p provider.Provider
		for _, candidate := range r.instances[name] {
			p = candidate
			break
		}
		out = append(out, Instance{Name: name, Plugin: r.pluginFor[name], Provider: p})
	}
	return out
}

// DeclaredConcurrency reports, per plugin name, the ceiling that plugin said
// its API tolerates. A plugin that made no claim is absent from the map.
//
// It reads the plugin rather than an instance because that is what the
// executor's per-provider bound keys on: two instances of one plugin are two
// accounts on one API, and the ceiling the plugin declared is a statement about
// that API.
func (r *Registry) DeclaredConcurrency() map[string]int {
	// Nil-safe: some callers build executor options before a registry exists,
	// and a concurrency ceiling must not be the read that panics an apply.
	if r == nil {
		return nil
	}
	var out map[string]int
	for name, p := range r.plugins {
		// nil is a plugin known only because a caller handed over a built
		// provider rather than a factory — see the `plugins` field.
		if p == nil {
			continue
		}
		l, ok := p.(interface{ MaxConcurrency() int })
		if !ok {
			continue
		}
		if n := l.MaxConcurrency(); n > 0 {
			if out == nil {
				out = map[string]int{}
			}
			out[name] = n
		}
	}
	return out
}

// InstanceNames lists every instance, sorted, for diagnostics that suggest what a
// user might have meant.
func (r *Registry) InstanceNames() []string {
	out := make([]string, 0, len(r.instances))
	for name := range r.instances {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Types returns every registered type in sorted order.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.definitions))
	for t := range r.definitions {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// TypesOf returns the resource types one instance offers, sorted.
func (r *Registry) TypesOf(instance string) []string {
	byType, ok := r.instances[instance]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(byType))
	for t := range byType {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
