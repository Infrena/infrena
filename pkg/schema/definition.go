package schema

import (
	"fmt"
	"sort"

	"github.com/infrata/infrata/pkg/value"
)

// Requirement declares infrastructure a resource needs in order to exist. The
// planner reports unsatisfied requirements before apply rather than letting a
// provider API call fail. PLAN.md §17.
type Requirement struct {
	Name        string   // "cluster", "network", "execution_role"
	Types       []string // resource types that satisfy it
	Optional    bool
	Description string
}

// Capabilities describes which operations a resource type supports.
type Capabilities struct {
	Create, Read, Update, Delete, Import bool
}

// ImportSpec describes the provider ID form for a resource type. M1 uses
// Description only, to render `infra explain`. Parse is declared now so the
// field does not change shape in Phase 2, and may be nil until then.
type ImportSpec struct {
	Description string
	Parse       func(id string) (map[string]value.Value, error)
}

// ResourceDefinition describes a provider resource type.
type ResourceDefinition struct {
	Type         string
	Description  string
	Attributes   map[string]Attribute
	Requirements []Requirement
	Capabilities Capabilities
	ImportID     ImportSpec
}

// Attribute returns the attribute with the given name, or ok=false if not found.
func (d *ResourceDefinition) Attribute(name string) (Attribute, bool) {
	a, ok := d.Attributes[name]
	return a, ok
}

// RequiredAttributes returns required attribute names in sorted order. Sorting
// matters: diagnostics built from this list must be identical across runs, and
// Go map iteration order is randomised.
func (d *ResourceDefinition) RequiredAttributes() []string {
	return d.filterNames(func(a Attribute) bool { return a.Required })
}

// ForceNewAttributes returns force-new attribute names in sorted order.
func (d *ResourceDefinition) ForceNewAttributes() []string {
	return d.filterNames(func(a Attribute) bool { return a.ForceNew })
}

func (d *ResourceDefinition) filterNames(keep func(Attribute) bool) []string {
	var out []string
	for name, attr := range d.Attributes {
		if keep(attr) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Validate checks a definition for internal contradictions. Providers call it
// at registration so a malformed schema fails at startup rather than during a
// plan.
func (d *ResourceDefinition) Validate() error {
	if d.Type == "" {
		return fmt.Errorf("resource definition has no Type")
	}
	names := make([]string, 0, len(d.Attributes))
	for name := range d.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		attr := d.Attributes[name]
		switch {
		case attr.Kind == value.KindInvalid:
			return fmt.Errorf("%s: attribute %q has no Kind", d.Type, name)
		case attr.Required && attr.Computed:
			return fmt.Errorf("%s: attribute %q is both Required and Computed; configuration may not set a computed attribute", d.Type, name)
		case attr.Computed && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Computed and also has a Default; the provider supplies computed values", d.Type, name)
		case attr.Required && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Required and also has a Default; a default makes it optional", d.Type, name)
		}
	}

	for _, req := range d.Requirements {
		if req.Name == "" {
			return fmt.Errorf("%s: a requirement has no Name", d.Type)
		}
		if len(req.Types) == 0 {
			return fmt.Errorf("%s: requirement %q names no satisfying types, so it can never be satisfied", d.Type, req.Name)
		}
	}
	return nil
}
