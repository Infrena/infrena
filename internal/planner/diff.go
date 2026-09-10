package planner

import (
	"fmt"
	"sort"
	"strconv"

	"infra/internal/diag"
	"infra/pkg/address"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// diffAttributes reports every attribute that differs between desired
// configuration and the resource as the provider reports it.
//
// It walks the union of both maps so that an attribute deleted from
// configuration still registers as a change, and consults the schema so that
// computed attributes — which are outputs, not desired state — never do
// (spec §11). It also returns diagnostics: an attribute configuration sets
// that the schema does not define at all is a plan-time error, not a
// silently mislabelled update — see the comment at that branch.
func diffAttributes(addr address.Address, def *schema.ResourceDefinition, desired, actual map[string]value.Value) ([]ChangeReason, diag.Diagnostics) {
	var reasons []ChangeReason
	var ds diag.Diagnostics

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

		// Configuration sets an attribute the schema does not define at all.
		// This path should be unreachable through the real pipeline —
		// internal/compiler/schema.go rejects it at compile time — but
		// Compute is a pure function of its four arguments, not of "the
		// caller went through Compile", and the failure mode of guessing is
		// the bad kind: with attr left at its zero value, ForceNew would
		// silently read false, so a changed attribute that actually forces a
		// replacement would be reported as an in-place Update instead. The
		// planner already refuses to guess one level up, at the resource
		// type: an unrecognised type is a hard error via Definition's own ok
		// (see operationFor), not a silent skip. Treating an unrecognised
		// attribute differently — quietly ignoring it — would be the same
		// mistake one boundary further in, so this mirrors that diagnostic
		// rather than a bare `continue`.
		if !defined {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "attribute " + strconv.Quote(name) + " is not defined by this resource's schema",
				Detail: "Configuration sets an attribute the provider's schema does not recognise. The planner " +
					"cannot tell whether a change to it would force a replacement, so it cannot be diffed safely.",
				Action:  "Remove " + strconv.Quote(name) + ", or check it for a typo against the resource type's schema.",
				Related: []address.Address{addr},
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
	return reasons, ds
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
// check alone would miss exactly the case that matters. A composite whose Raw
// does not match its Kind fails closed the same way value.Equal does, and for
// the same reason: a malformed value cannot be PROVEN fully known, so it is
// treated as unknown rather than as empty. Discarding the type assertion's ok
// here — `items, _ := v.Raw.([]value.Value)` — is the exact defect that let
// value.Equal report two malformed values as equal earlier in this milestone;
// in the planner the same shape of bug would let an unprovable value slip
// through as "nothing unknown" and be silently NoOp'd or Updated instead of
// flagged as a change.
func hasUnknown(v value.Value) bool {
	if !v.Known {
		return true
	}
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return true
		}
		for _, item := range items {
			if hasUnknown(item) {
				return true
			}
		}
	case value.KindMap:
		entries, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return true
		}
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
