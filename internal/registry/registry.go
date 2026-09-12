// Package registry maps resource types to their definitions and providers.
// `infra explain` renders directly from it, so documentation cannot drift from
// the schemas it describes. Spec §8.2.
package registry

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/schema"
)

// Registry maps resource types to their definitions, and (type, instance) pairs to
// the provider serving them.
//
// TWO DIMENSIONS, and only one of them is new. A resource type's SCHEMA belongs to
// the plugin and is identical across every instance of it — two `aws` instances
// describe `aws.instance` the same way — so definitions stay keyed by type and the
// six callers of Definition need no instance. What differs per instance is the
// PROVIDER OBJECT: its credentials, its account, its endpoint. That is what a
// resource's `provider:` selects (PLAN.md §12.1).
type Registry struct {
	definitions map[string]*schema.ResourceDefinition
	// pluginOf records which plugin declared each type, so a SECOND plugin
	// claiming it is still refused while a second INSTANCE of the same plugin is
	// not. That distinction is the whole change: the old check refused both,
	// because it could not tell them apart.
	pluginOf map[string]string
	// instances is instance name -> resource type -> provider.
	instances map[string]map[string]provider.Provider
	// pluginFor records each instance's plugin, for diagnostics.
	pluginFor map[string]string
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		definitions: map[string]*schema.ResourceDefinition{},
		pluginOf:    map[string]string{},
		instances:   map[string]map[string]provider.Provider{},
		pluginFor:   map[string]string{},
	}
}

// Register adds every definition a provider offers, under an INSTANCE NAME.
//
// instance is what a resource's `provider:` selects. Two instances of one plugin
// both register successfully and serve the same types — that is the point of the
// milestone — while two different PLUGINS claiming one type is still an error,
// because nothing could then say which owns it.
//
// Validate everything before mutating, so a failed registration leaves the
// registry untouched. `seen` catches a provider declaring the same type twice in
// one call, which the registry-state check alone cannot see because the first loop
// never mutates.
func (r *Registry) Register(instance string, p provider.Provider) error {
	if instance == "" {
		return fmt.Errorf("provider %s: an instance name is required", p.Name())
	}
	if existing, taken := r.pluginFor[instance]; taken {
		return fmt.Errorf("provider instance %q is already registered, by plugin %s", instance, existing)
	}

	defs := p.Definitions()
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("provider %s: %w", p.Name(), err)
		}
		if strings.HasPrefix(d.Type, "module.") {
			// Compiler stage 5 selects module instantiations by this prefix
			// (PLAN.md §11). A provider claiming the namespace would turn a
			// user's `type: module.app_stack` into a silently shadowed provider
			// resource — a plan that is wrong and looks fine.
			//
			// Belt-and-braces: stage 5 runs before stage 7, so a module type from
			// a user's config is always expanded away before Definition() is
			// reached. What this stops is the provider side, and it stops it at
			// startup in that provider's own tests.
			//
			// In the FIRST loop with the other checks, because this loop is
			// deliberately validate-before-mutate.
			return fmt.Errorf("provider %s: resource type %q is reserved: the `module.` namespace "+
				"is how configuration instantiates a module", p.Name(), d.Type)
		}
		if owner, claimed := r.pluginOf[d.Type]; claimed && owner != p.Name() {
			return fmt.Errorf("provider %s: resource type %q is already declared by plugin %s",
				p.Name(), d.Type, owner)
		}
		if seen[d.Type] {
			return fmt.Errorf("provider %s declares resource type %q more than once", p.Name(), d.Type)
		}
		seen[d.Type] = true
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
	return nil
}

// Definition returns the schema for a resource type, or false if not registered.
func (r *Registry) Definition(resourceType string) (*schema.ResourceDefinition, bool) {
	d, ok := r.definitions[resourceType]
	return d, ok
}

// ProviderFor returns the provider serving a resource type in one instance.
//
// Both dimensions are required, and there is deliberately no single-argument form:
// a lookup that fell back to "whichever instance offers this type" would send a
// resource to an account nobody chose, and would do it silently. The old
// Provider(type) was exactly that shape, which is why it is gone rather than kept
// as a convenience.
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
	Name     string
	Plugin   string
	Provider provider.Provider
}

// Instances lists every registered instance, sorted by name.
//
// Replaces Providers(): discovery asks each INSTANCE what exists, because two
// instances of one plugin hold different infrastructure — which is the entire
// reason they are separate.
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
