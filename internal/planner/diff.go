package planner

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// diffAttributes reports every attribute that differs between desired
// configuration and the resource as the provider reports it.
//
// It walks the union of both maps so that an attribute deleted from
// configuration still registers as a change, and consults the schema so that
// computed attributes — outputs, not desired state — never do. An attribute
// configuration sets that the schema does not define is a plan-time error
// rather than a silently mislabelled update.
func diffAttributes(
	addr address.Address, def *schema.ResourceDefinition,
	desired, actual map[string]value.Value, ignored []string,
) ([]ChangeReason, diag.Diagnostics) {
	var reasons []ChangeReason
	var ds diag.Diagnostics

	for _, name := range unionKeys(desired, actual) {
		// Ignored before anything else, including the undefined-attribute
		// error below. ignore_changes says this attribute belongs to someone
		// else, so the planner has nothing to say about it either way.
		if slices.Contains(ignored, name) {
			continue
		}
		attr, defined := def.Attribute(name)
		want, inConfig := desired[name]
		got, inActual := actual[name]

		if !inConfig {
			// The provider reports it but configuration does not set it. A
			// computed attribute is an output; an attribute the schema does
			// not define is the provider's own business.
			//
			// This covers the optional-and-computed case, which is the point
			// of it: when configuration sets no value the provider's choice
			// is authoritative. Without it the plan proposes unsetting what
			// the cloud just chose, which the cloud then chooses again,
			// forever.
			if !defined || attr.Computed {
				continue
			}
			// An empty collection is not something to remove. Clouds fill in
			// empty lists and maps nobody asked for, and configuration that
			// does not mention one is not requesting a removal.
			if emptyCollection(got) {
				continue
			}
			reasons = append(reasons, ChangeReason{
				Attribute: name,
				ForceNew:  attr.ForceNew,
				Note:      "removed from configuration",
			})
			continue
		}

		// Configuration sets an attribute the schema does not define. The
		// compiler rejects this earlier, but Compute is a pure function of
		// its arguments rather than of "the caller compiled first", and
		// guessing fails badly: with a zero-value attribute ForceNew reads
		// false, so a change that really forces a replacement would be
		// reported as an in-place update. An unrecognised resource type is
		// already a hard error one level up, for the same reason.
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

		// equalBesidesProviderEmpties is value.Equal plus exactly one
		// forgiveness, not a rival implementation: a second comparison with
		// its own semantics is how two halves of an engine start disagreeing
		// about what changed.
		if equalBesidesProviderEmpties(want, got) {
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

	// Redundant today, since unionKeys already returns sorted names. Kept
	// because the loop appends from three branches and a fourth added later
	// might not follow that sequence.
	sortReasons(reasons)
	return reasons, ds
}

// emptyCollection reports whether v is a list or map the provider reports as
// holding nothing.
//
// The distinction it draws is between "the user removed this" and "the cloud
// synthesised an empty one". Only the second is forgiven anywhere in this
// file, and only because an empty collection carries no information to
// remove.
func emptyCollection(v value.Value) bool {
	if !v.Known {
		return false
	}
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		return ok && len(items) == 0
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		return ok && len(m) == 0
	default:
		return false
	}
}

// equalBesidesProviderEmpties is value.Equal with one exception: a key the
// provider reports and configuration does not mention is ignored when its
// value is an empty collection.
//
// Clouds synthesise empty collections nobody asked for, nested inside
// composite attributes — an empty address list inside a network interface,
// say. Read as a difference, and on a ForceNew attribute, that makes a plan
// over an untouched account propose replacing running infrastructure.
//
// diffAttributes applies this rule to a top-level attribute by asking the
// schema whether it is computed. Inside a composite there is no per-leaf
// schema to ask, so emptiness is the evidence: an empty collection has
// nothing in it to remove.
//
// Deliberately narrow in three ways:
//   - only a key missing from configuration is forgiven, never one that
//     differs;
//   - only an empty collection, so a list the user really did shorten still
//     reads as a change;
//   - a key configuration sets and the provider does not report is still a
//     change, because that direction is the user asking for something.
func equalBesidesProviderEmpties(want, got value.Value) bool {
	if want.Equal(got) {
		return true
	}
	if want.Kind != got.Kind || !want.Known || !got.Known {
		return false
	}

	switch want.Kind {
	case value.KindMap:
		wm, wok := want.Raw.(map[string]value.Value)
		gm, gok := got.Raw.(map[string]value.Value)
		if !wok || !gok {
			return false
		}
		for k, gv := range gm {
			wv, inWant := wm[k]
			if !inWant {
				// The one forgiveness.
				if emptyCollection(gv) {
					continue
				}
				return false
			}
			if !equalBesidesProviderEmpties(wv, gv) {
				return false
			}
		}
		// Anything configuration sets that the provider does not report is a
		// change, and is not this function's business to forgive.
		for k := range wm {
			if _, inGot := gm[k]; !inGot {
				return false
			}
		}
		return true

	case value.KindList:
		wl, wok := want.Raw.([]value.Value)
		gl, gok := got.Raw.([]value.Value)
		// Length is meaning in a list: a shorter one is a removal.
		if !wok || !gok || len(wl) != len(gl) {
			return false
		}
		for i := range wl {
			if !equalBesidesProviderEmpties(wl[i], gl[i]) {
				return false
			}
		}
		return true

	default:
		return false
	}
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
// Configuration wins, always: these reasons describe moving state towards
// config, never the reverse. If state won, clearing prevent_destroy from
// configuration could never take effect and the guard would be permanently
// unremovable — making the advice to clear it in order to destroy a lie.
// State's copy is authoritative only once the resource has left
// configuration, where there is no declared lifecycle to compare against.
//
// ForceNew is false on both. Lifecycle is metadata, and nothing about the
// external object changes when it does; marking it would turn switching a
// guard on into destroying the resource in the act of protecting it.
//
// These notes are two booleans the user typed, which is why they may be
// shown literally where a ChangeReason otherwise carries no values.
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
// thing and cannot collide with a real attribute: depends_on is a structural
// key in the resource body, so it never reaches the attribute map.
const dependsOnAttribute = "depends_on"

// dependencyReasons reports a change to this resource's dependency edges, so
// that a depends_on-only change is a visible operation instead of a silent
// no-op.
//
// State's Dependencies is the only source of destroy-ordering edges once a
// resource leaves configuration, and the executor writes it only when an
// operation actually runs. Without this diff, adding a dependency to an
// existing resource plans as no change, nothing is written, and the edge
// never reaches state: a later destroy could delete the new dependency first
// and strand what depends on it.
//
// Configuration wins, always, as for lifecycle. If state won, an edge could
// be added but never removed.
//
// ForceNew is false: dependency edges are metadata, and marking them would
// destroy and recreate a resource because something else started pointing at
// it.
//
// Both slices are sorted by construction, so they are compared in order
// rather than as sets. Two orderings of the same edges would be a
// canonicalisation bug upstream, and treating them as equal here would hide
// it.
//
// The note names addresses, never values, so nothing sensitive reaches it.
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
// A map or list is known even when one of its entries is not, so a top-level
// check alone would miss the case that matters. A composite whose Raw does
// not match its Kind fails closed, like value.Equal: a malformed value cannot
// be proven fully known, so it counts as unknown rather than as empty.
// Discarding the type assertion's ok would let such a value slip through as
// "nothing unknown" and be planned as a no-op instead of a change.
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
func afterAttributes(
	def *schema.ResourceDefinition, desired, actual map[string]value.Value,
	kind OpKind, ignored []string,
) map[string]value.Value {
	out := make(map[string]value.Value, len(desired))
	maps.Copy(out, desired)

	// An ignored attribute keeps what is really there. Otherwise the
	// operation carries configuration's value into After, the executor
	// records it, and state claims something the provider is not running:
	// the plan says nothing changed while rewriting the record of what did.
	//
	// Not on a create, where there is no actual resource yet and
	// configuration's value is the only one there is.
	if kind != OpCreate && kind != OpReplace {
		for _, name := range ignored {
			if got, ok := actual[name]; ok {
				out[name] = got
			} else {
				delete(out, name)
			}
		}
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
	maps.Copy(out, attrs)
	return out
}

// unionKeys returns every key across the given maps, sorted. Every
// map-to-slice boundary sorts, so that output is deterministic.
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
// order; sorting again means a caller that appends one cannot break that.
func sortReasons(reasons []ChangeReason) {
	sort.SliceStable(reasons, func(i, j int) bool {
		if reasons[i].Attribute != reasons[j].Attribute {
			return reasons[i].Attribute < reasons[j].Attribute
		}
		return reasons[i].Note < reasons[j].Note
	})
}
