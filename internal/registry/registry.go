// Package registry maps resource types to their definitions and providers.
// `infra explain` renders directly from it, so documentation cannot drift from
// the schemas it describes. Spec §8.2.
package registry

import (
	"fmt"
	"sort"

	"infra/pkg/provider"
	"infra/pkg/schema"
)

// Registry maps resource types to their definitions and providers.
type Registry struct {
	definitions map[string]*schema.ResourceDefinition
	providers   map[string]provider.Provider
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		definitions: map[string]*schema.ResourceDefinition{},
		providers:   map[string]provider.Provider{},
	}
}

// Register adds every definition a provider offers. A malformed schema or a
// type already claimed by another provider is an error here, at startup.
func (r *Registry) Register(p provider.Provider) error {
	defs := p.Definitions()

	// Validate everything before mutating, so a failed registration leaves the
	// registry untouched. `seen` catches a provider declaring the same type
	// twice in one call, which the registry-state check alone cannot see
	// because the first loop never mutates.
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("provider %s: %w", p.Name(), err)
		}
		if existing, ok := r.providers[d.Type]; ok {
			return fmt.Errorf("provider %s: resource type %q is already registered by provider %s", p.Name(), d.Type, existing.Name())
		}
		if seen[d.Type] {
			return fmt.Errorf("provider %s declares resource type %q more than once", p.Name(), d.Type)
		}
		seen[d.Type] = true
	}

	for _, d := range defs {
		r.definitions[d.Type] = d
		r.providers[d.Type] = p
	}
	return nil
}

// Definition returns the schema for a resource type, or false if not registered.
func (r *Registry) Definition(resourceType string) (*schema.ResourceDefinition, bool) {
	d, ok := r.definitions[resourceType]
	return d, ok
}

// Provider returns the provider for a resource type, or false if not registered.
func (r *Registry) Provider(resourceType string) (provider.Provider, bool) {
	p, ok := r.providers[resourceType]
	return p, ok
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
