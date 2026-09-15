package schema

import (
	"fmt"
	"sort"

	"github.com/infrena/infrena/pkg/value"
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

// ImportSpec describes the provider ID form for a resource type, for `infra
// explain` to render.
//
// It once also declared `Parse func(id string) (map[string]value.Value, error)`,
// "so the field does not change shape in Phase 2". Phase 2 came and went without
// anything setting it or calling it, and §31.1 made it impossible: a function
// cannot cross a pipe. A plugin that needs to interpret an import ID does so
// inside its own `import` implementation, where it already has the ID.
type ImportSpec struct {
	Description string
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
		case attr.Optional && !attr.Computed:
			// Every non-required attribute is already optional, so Optional alone
			// says nothing — and an author setting it expects it to mean something.
			return fmt.Errorf("%s: attribute %q is Optional without Computed, which says nothing: every attribute that is not Required is already optional. Optional exists to pair with Computed (PLAN.md §14.1)", d.Type, name)
		case attr.Computed && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Computed and also has a Default; the provider supplies computed values", d.Type, name)
		case attr.Required && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Required and also has a Default; a default makes it optional", d.Type, name)
		case attr.Fields != nil && attr.Kind != value.KindMap:
			return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; Fields describes a map's known keys", d.Type, name)
		}
		if attr.Fields != nil {
			if err := validateFields(d.Type, name, attr.Fields); err != nil {
				return err
			}
		}
	}

	if err := d.checkSpellings(); err != nil {
		return err
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

// validateFields checks a declared map's known keys, recursively — Validate's
// own per-attribute loop only ever looks at the TOP-LEVEL attributes, and
// without this a nested attribute's Kind and its own References go
// unchecked entirely.
//
// A nested References is REFUSED rather than merely left unchecked:
// internal/compiler/bind.go's projectRefs only ever reads a top-level
// attribute's References (PLAN.md §14.3), so one declared inside Fields is
// consulted by nothing — a declaration nothing consults is exactly the
// defect class this whole feature keeps running into, and refusing it at
// load time is simpler than teaching every consumer of References to
// recurse into a shape most of them have no reason to know about.
func validateFields(typeName, path string, fields map[string]Attribute) error {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		nested := fields[n]
		nestedPath := path + "." + n
		switch {
		case nested.Kind == value.KindInvalid:
			return fmt.Errorf("%s: attribute %q has no Kind", typeName, nestedPath)
		case nested.References != nil:
			return fmt.Errorf("%s: attribute %q declares References, but a nested attribute's "+
				"References is never consulted — only a top-level attribute's is ever projected "+
				"(PLAN.md §14.3)", typeName, nestedPath)
		case nested.Fields != nil && nested.Kind != value.KindMap:
			return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; Fields "+
				"describes a map's known keys", typeName, nestedPath)
		}
		if nested.Fields != nil {
			if err := validateFields(typeName, nestedPath, nested.Fields); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateAll validates each definition on its own and then the relationships
// BETWEEN them, which no single definition can check: a Reference names another
// type, and whether that type exists is a fact about the whole set.
//
// A plugin with a dangling relationship does not load. §14.1 took the same line
// for colliding alias spellings, for the same reason — a silent runtime surprise
// about which relationship won is worse than a plugin that refuses to start.
func ValidateAll(defs []*ResourceDefinition) error {
	byType := make(map[string]*ResourceDefinition, len(defs))
	for _, d := range defs {
		if err := d.Validate(); err != nil {
			return err
		}
		byType[d.Type] = d
	}

	// Sorted, so a plugin with two broken relationships reports the same one
	// first on every run (invariant 6).
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, t := range types {
		d := byType[t]
		names := make([]string, 0, len(d.Attributes))
		for n := range d.Attributes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ref := d.Attributes[n].References
			if ref == nil {
				continue
			}
			target, ok := byType[ref.Type]
			if !ok {
				return fmt.Errorf("%s: attribute %q refers to type %q, which this plugin does not declare",
					d.Type, n, ref.Type)
			}
			if _, ok := target.Attributes[ref.Attribute]; !ok {
				return fmt.Errorf("%s: attribute %q refers to %s.%s, and %s has no attribute %q",
					d.Type, n, ref.Type, ref.Attribute, ref.Type, ref.Attribute)
			}
		}
	}
	return nil
}
