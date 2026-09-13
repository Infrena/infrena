package planner

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
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

	// Redundant with unionKeys above, which already returns names sorted, so
	// reasons are appended in sorted order — measured: neutralising this
	// sort alone leaves the whole suite green, while neutralising unionKeys'
	// fails TestRenderIsDeterministicAcrossRepeatedCalls. Kept because the
	// loop appends from three separate branches and a fourth added later
	// could easily not come from unionKeys' sequence.
	sortReasons(reasons)
	return reasons, ds
}

// lifecyclePrefix marks a ChangeReason as describing a lifecycle setting
// rather than a provider attribute. Reasons carry no other type tag, and the
// renderer needs to tell the two apart: a lifecycle reason has no entry in
// Before or After, so the attribute-diff loop would never print it.
const lifecyclePrefix = "lifecycle."

// lifecycleReasons reports the lifecycle settings that differ between
// configuration and what state records, so that changing only a lifecycle
// setting is a visible operation instead of a silent no-op.
//
// Configuration wins, always — these reasons describe moving state towards
// config, never the reverse. Spec §15 makes lifecycle something the user
// declares in configuration, and §7's "explicit config always wins over an
// implicit default" settles the direction: if state won, clearing
// prevent_destroy from configuration could never take effect, the guard would
// be permanently unremovable, and removalOperation's own suggested fix
// ("clear prevent_destroy if you really mean to destroy it") would be a lie.
// State's copy is authoritative in exactly one situation — when the resource
// has left configuration, which is when removalOperation reads it and there is
// no config lifecycle to compare against at all.
//
// ForceNew is deliberately false on both: lifecycle is metadata infra records
// about a resource, and nothing about the external object changes when it
// does. Marking either ForceNew would promote a guard being switched on into
// a replacement, i.e. destroying the resource in the act of protecting it.
//
// A returned reason never carries a value that could be sensitive: these are
// two booleans the user typed in configuration, which is why they may be shown
// literally where ChangeReason's doc otherwise forbids values.
func lifecycleReasons(desired, recorded resource.Lifecycle) []ChangeReason {
	var reasons []ChangeReason
	add := func(name string, want, got bool) {
		if want == got {
			return
		}
		reasons = append(reasons, ChangeReason{
			Attribute: lifecyclePrefix + name,
			Note:      strconv.FormatBool(got) + " -> " + strconv.FormatBool(want),
		})
	}
	add("prevent_destroy", desired.PreventDestroy, recorded.PreventDestroy)
	add("retain", desired.Retain, recorded.Retain)
	return reasons
}

// dependsOnAttribute marks a ChangeReason as describing this resource's
// dependency edges rather than a provider attribute — the same role
// lifecyclePrefix plays, for the same reason: the reason has no entry in
// Before or After, so the attribute-diff loop would never print it.
//
// It is a bare name rather than a prefixed one because it names exactly one
// thing, and it cannot collide with a real attribute: `depends_on` is a
// structural key in the resource body (internal/config/decode.go switches on
// it alongside `type` and `lifecycle`), so it never reaches r.Attributes and
// diffAttributes can never produce a reason with this name.
const dependsOnAttribute = "depends_on"

// dependencyReasons reports a change to this resource's dependency edges, so
// that a depends_on-only change is a visible operation instead of a silent
// no-op.
//
// This exists for the same reason lifecycleReasons does, and its absence was
// the same bug one field over. state's Dependencies is the ONLY source of
// destroy-ordering edges once a resource leaves configuration (spec §14), and
// the executor only writes it when an operation actually runs. Without this
// diff, adding a dependency to a resource that already exists plans as "No
// changes", nothing is written, and the new edge never reaches state at all:
// a later destroy could delete the new dependency first and strand the
// resource that depends on it. Recording dependencies on create alone made
// invariant 4 hold for resources created afterwards and not for resources
// whose dependencies change, which is worse than not holding at all —
// half-implemented behaviour is what this project refuses (see --var-file,
// which errors rather than being silently ignored).
//
// Configuration wins, always, exactly as for lifecycle: these reasons describe
// moving state towards config, never the reverse. If state won, an edge could
// be added but never removed.
//
// ForceNew is deliberately false. Dependency edges are metadata infra records
// about a resource; nothing about the external object changes when they do,
// and marking it ForceNew would destroy and recreate a resource because
// something else started pointing at it.
//
// Both slices are sorted by construction — compiler's sortedAddresses builds
// DependsOn, and the executor stamps that same slice into state — so this
// compares them in order rather than as sets. That is deliberate: two
// orderings of the same edges would be a bug upstream in canonicalisation
// (the property TestBindSortsDependsOnEveryTime pins), and silently treating
// them as equal here would hide it.
//
// The Note names addresses, never values, so nothing sensitive can reach it.
func dependencyReasons(desired, recorded []address.Address) []ChangeReason {
	if len(desired) == len(recorded) {
		same := true
		for i := range desired {
			if desired[i].String() != recorded[i].String() {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}
	return []ChangeReason{{
		Attribute: dependsOnAttribute,
		Note:      formatAddresses(recorded) + " -> " + formatAddresses(desired),
	}}
}

// formatAddresses renders an address list for a ChangeReason's Note.
func formatAddresses(addrs []address.Address) string {
	if len(addrs) == 0 {
		return "[]"
	}
	names := make([]string, 0, len(addrs))
	for _, a := range addrs {
		names = append(names, a.String())
	}
	return "[" + strings.Join(names, ", ") + "]"
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
		if slices.ContainsFunc(items, hasUnknown) {
			return true
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
	maps.Copy(out, desired)
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
	maps.Copy(out, attrs)
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
