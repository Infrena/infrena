package planner

import (
	"fmt"
	"sort"

	"infra/pkg/schema"
	"infra/pkg/value"
)

// diffAttributes reports every attribute that differs between desired
// configuration and the resource as the provider reports it.
//
// It walks the union of both maps so that an attribute deleted from
// configuration still registers as a change, and consults the schema so that
// computed attributes — which are outputs, not desired state — never do
// (spec §11).
func diffAttributes(def *schema.ResourceDefinition, desired, actual map[string]value.Value) []ChangeReason {
	var reasons []ChangeReason

	for _, name := range unionKeys(desired, actual) {
		attr, defined := def.Attribute(name)
		want, inConfig := desired[name]
		got, inActual := actual[name]

		if !inConfig {
			// The provider reports it but configuration does not set it. A
			// computed attribute is an output; an attribute the schema does
			// not define at all is the provider's own business.
			if !defined || attr.Computed {
				continue
			}
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "removed from configuration",
			})
			continue
		}

		// An unknown desired value cannot be proven unchanged, so it is always
		// a change. This runs before Equal — which would also return false —
		// so that the reason says why rather than merely that. Unknowns hide
		// inside composites, so the check recurses.
		if hasUnknown(want) {
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "known after apply",
			})
			continue
		}

		if !inActual {
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "not set on the resource",
			})
			continue
		}

		// value.Equal already ignores Source, Sensitive and Origin, and
		// recurses through composites. A second comparison with different
		// semantics is how two halves of an engine start disagreeing about
		// what changed.
		if want.Equal(got) {
			continue
		}

		// Reasons name attributes and types, never values: sensitivity is
		// per-leaf, so even a composite that is not itself marked sensitive
		// may contain a leaf that is.
		note := ""
		if got.Kind != want.Kind {
			note = fmt.Sprintf("kind changed from %s to %s", got.Kind, want.Kind)
		}
		reasons = append(reasons, ChangeReason{Attribute: name, ForceNew: attr.ForceNew, Note: note})
	}

	sortReasons(reasons)
	return reasons
}

// forcesReplacement reports whether any reason names a ForceNew attribute,
// which promotes an update to a replacement.
func forcesReplacement(reasons []ChangeReason) bool {
	for _, r := range reasons {
		if r.ForceNew {
			return true
		}
	}
	return false
}

// hasUnknown reports whether a value, or any leaf inside a composite, is not
// yet known.
//
// A map or list is Known even when one of its entries is not, so a top-level
// check alone would miss exactly the case that matters.
func hasUnknown(v value.Value) bool {
	if !v.Known {
		return true
	}
	switch v.Kind {
	case value.KindList:
		items, _ := v.Raw.([]value.Value)
		for _, item := range items {
			if hasUnknown(item) {
				return true
			}
		}
	case value.KindMap:
		entries, _ := v.Raw.(map[string]value.Value)
		for _, entry := range entries {
			if hasUnknown(entry) {
				return true
			}
		}
	}
	return false
}

// afterAttributes builds the After map for an operation.
//
// It starts from desired configuration and adds the computed attributes the
// schema defines. For NoOp and Update those survive in place, so they are
// carried across from the observed resource; for Create and Replace the object
// is built afresh, so they are unknown until the provider reports them.
func afterAttributes(def *schema.ResourceDefinition, desired, actual map[string]value.Value, kind OpKind) map[string]value.Value {
	out := make(map[string]value.Value, len(desired))
	for name, v := range desired {
		out[name] = v
	}
	if def == nil {
		return out
	}
	for name, attr := range def.Attributes {
		if !attr.Computed {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		if kind == OpCreate || kind == OpReplace {
			out[name] = value.Unknown(attr.Kind, value.SourceProvider)
			continue
		}
		if got, ok := actual[name]; ok {
			out[name] = got
		}
	}
	return out
}

// copyAttrs copies an attribute map so that a plan never aliases the state it
// was built from. Refresh and planning must not mutate what was loaded.
func copyAttrs(attrs map[string]value.Value) map[string]value.Value {
	out := make(map[string]value.Value, len(attrs))
	for name, v := range attrs {
		out[name] = v
	}
	return out
}

// unionKeys returns every key across the given maps, sorted. Every map-to-slice
// boundary sorts; determinism is invariant 6.
func unionKeys(maps ...map[string]value.Value) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for name := range m {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sortReasons orders reasons deterministically. They are already built in key
// order; sorting again means a caller that appends one cannot break invariant 6.
func sortReasons(reasons []ChangeReason) {
	sort.SliceStable(reasons, func(i, j int) bool {
		if reasons[i].Attribute != reasons[j].Attribute {
			return reasons[i].Attribute < reasons[j].Attribute
		}
		return reasons[i].Note < reasons[j].Note
	})
}
