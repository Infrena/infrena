// Package schema describes provider resource types as inspectable data.
//
// Schemas are plain values rather than struct tags so that `infra explain`,
// validation and default resolution all read one source, and so that
// environment-aware default resolvers can be functions. Spec §8.1.
package schema

import "infra/pkg/value"

// DefaultContext is everything a default resolver is allowed to see.
//
// It deliberately excludes other resources' attributes: defaults must never
// depend on unknown values, so that they are always computable at plan time.
// Spec §7.3.
type DefaultContext struct {
	Environment     string
	EnvironmentType string // e.g. "production"
	Region          string
	Account         string
	Project         string
	Type            string
}

// DefaultFunc returns a default datum and whether one applies. Returning false
// means the attribute stays absent.
type DefaultFunc func(DefaultContext) (any, bool)

// Attribute describes a resource attribute.
type Attribute struct {
	Kind        value.Kind
	Required    bool
	Computed    bool // the provider sets it; configuration may not
	Sensitive   bool
	ForceNew    bool // a change replaces the resource rather than updating it
	Default     DefaultFunc
	Description string
	Validate    func(value.Value) error
}
