package schema

import (
	"fmt"
	"sort"

	"github.com/infrena/infrena/pkg/value"
)

// Requirement declares infrastructure a resource needs in order to exist. The
// planner reports unsatisfied requirements before apply rather than letting a
// provider API call fail.
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

// ImportSpec describes the provider ID form for a resource type, for
// `infrena explain` to render.
//
// It carries a description and nothing else. A schema has to cross a pipe to a
// plugin process and a function cannot, so there is deliberately no parser field
// here: a plugin that needs to interpret an import ID does so inside its own
// import implementation, where it already has the ID.
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

// Attribute returns the attribute declared under name, or ok=false when there is
// none. The name must already be canonical; see Canonical to resolve a spelling
// a user wrote.
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

// ForceNewAttributes returns force-new attribute names in sorted order, for the
// same reason RequiredAttributes sorts.
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
			return fmt.Errorf("%s: attribute %q is Optional without Computed, which says nothing: every attribute that is not Required is already optional. Optional exists to pair with Computed", d.Type, name)
		case attr.Computed && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Computed and also has a Default; the provider supplies computed values", d.Type, name)
		case attr.Required && attr.Default != nil:
			return fmt.Errorf("%s: attribute %q is Required and also has a Default; a default makes it optional", d.Type, name)
		case attr.Fields != nil && attr.Kind != value.KindMap:
			return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; Fields describes a map's known keys", d.Type, name)
		case attr.Elem != nil && attr.Kind != value.KindList:
			return fmt.Errorf("%s: attribute %q declares Elem but its Kind is not a list; Elem describes each element of a list", d.Type, name)
		}
		if attr.Elem != nil {
			if err := validateElem(d.Type, name, attr.Elem); err != nil {
				return err
			}
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

// validateElem checks a list element's own declaration, and whatever it nests.
//
// An element is an attribute in every way that matters here: it has a Kind, it
// may carry Fields when it is a map, and it may carry an Elem of its own when
// it is a list of lists.
//
// Required, Optional, Computed and Default are NOT refused on one, even though
// an element is not configured independently of the list that holds it and
// nothing reads them here. A key nested inside a map is in exactly the same
// position and validateFields accepts them there, so refusing them here would
// be an asymmetry with no rule behind it — and an invisible one, since the two
// edges are declared the same way.
//
// It would also reject almost every provider. The natural conversion from a
// provider's own catalog marks anything neither required nor output as
// Optional and Computed, which is right for an ordinary attribute and lands on
// elements as a side effect. Refusing that costs a provider author a hunt
// through a shared default several call sites away, and buys nothing: the flag
// is ignored either way.
func validateElem(typeName, path string, elem *Attribute) error {
	described := path + "[]"
	switch {
	case elem.Kind == value.KindInvalid:
		return fmt.Errorf("%s: the element of %q declares no Kind", typeName, path)
	case elem.Fields != nil && elem.Kind != value.KindMap:
		return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; Fields "+
			"describes a map's known keys", typeName, described)
	case elem.Elem != nil && elem.Kind != value.KindList:
		return fmt.Errorf("%s: attribute %q declares Elem but its Kind is not a list; Elem "+
			"describes each element of a list", typeName, described)
	}
	if elem.Elem != nil {
		if err := validateElem(typeName, described, elem.Elem); err != nil {
			return err
		}
	}
	if elem.Fields != nil {
		return validateFields(typeName, described, elem.Fields)
	}
	return nil
}

// validateFields checks a declared map's known keys, recursively. Validate's own
// per-attribute loop looks only at top-level attributes, so without this a nested
// attribute's Kind and References go unchecked entirely.
//
// A nested References is refused rather than merely left unchecked: only a
// top-level attribute's References is ever projected into a dependency, so one
// declared inside Fields would be consulted by nothing. Refusing it at load time
// is simpler than teaching every consumer of References to recurse into a shape
// most of them have no reason to know about.
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
				"References is never consulted — only a top-level attribute's is ever projected",
				typeName, nestedPath)
		case nested.Fields != nil && nested.Kind != value.KindMap:
			return fmt.Errorf("%s: attribute %q declares Fields but its Kind is not a map; Fields "+
				"describes a map's known keys", typeName, nestedPath)
		case nested.Elem != nil && nested.Kind != value.KindList:
			return fmt.Errorf("%s: attribute %q declares Elem but its Kind is not a list; Elem "+
				"describes each element of a list", typeName, nestedPath)
		}
		if nested.Elem != nil {
			if err := validateElem(typeName, nestedPath, nested.Elem); err != nil {
				return err
			}
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
// between them, which no single definition can check: a Reference names another
// type, and whether that type exists is a fact about the whole set.
//
// A plugin with a dangling relationship does not load, on the same reasoning as
// a colliding alias spelling — a silent runtime surprise about which
// relationship won is worse than a plugin that refuses to start.
func ValidateAll(defs []*ResourceDefinition) error {
	byType := make(map[string]*ResourceDefinition, len(defs))
	for _, d := range defs {
		// A nil entry is refused rather than dereferenced. This is exported, so
		// a caller that has not screened its input can reach it, and a public
		// function that panics on malformed input pushes that hazard onto every
		// caller in turn.
		if d == nil {
			return fmt.Errorf("a resource definition is missing")
		}
		if err := d.Validate(); err != nil {
			return err
		}
		byType[d.Type] = d
	}

	// Sorted, so a plugin with two broken relationships reports the same one
	// first on every run.
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
