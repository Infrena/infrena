package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/providers"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// bindSchemas is compiler stage 7. It resolves each resource's type to its
// definition, validates what configuration set, and fills in defaults.
func bindSchemas(
	cfg *ResolvedConfig, reg *registry.Registry, opts Options, table providers.Table,
) diag.Diagnostics {
	var ds diag.Diagnostics

	for _, addr := range cfg.Addresses() {
		r := cfg.Resources[addr.String()]

		def, ok := reg.Definition(r.Type)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "unknown resource type " + strconv.Quote(r.Type),
				Detail:   "Known types:\n  " + strings.Join(reg.Types(), "\n  "),
				Action:   "Correct the type, or check that the provider offering it is available.",
				Origin:   r.Origin,
			})
			continue
		}

		// BEFORE everything else that touches these keys. Canonicalisation is the one
		// place aliases are resolved (PLAN.md §14.1), so checkConfiguredAttributes,
		// the defaults, checkRequired and markSensitive all see the plugin's own names
		// — and so does every stage downstream of the compiler.
		canonicaliseAttributes(r.Attrs, def, &ds)
		canonicaliseIgnoreChanges(r, def, &ds)
		checkConfiguredAttributes(r.Attrs, def, r.Origin, &ds)
		// BEFORE the plugin's own defaults, because §12.1's ladder has the
		// instance's `defaults:` beating them: whichever runs first wins, since
		// neither overwrites a value already present.
		applyInstanceDefaults(r.Attrs, def, table[r.Provider])
		applyDefaults(r.Attrs, def, r.Origin, &ds)
		checkRequired(r.Attrs, def, r.Origin, &ds)
		markSensitive(r.Attrs, def)
	}

	return ds
}

// canonicaliseAttributes rewrites configuration's attribute keys to the names the plugin
// declared, resolving aliases and case (PLAN.md §14.1).
//
// THE ONE PLACE THIS HAPPENS for resource attributes. Everything downstream — the
// defaults, checkRequired, markSensitive, the planner's diff, the host's
// undeclared-attribute refusal, the generator, state, the plan artifact and the plugin
// wire — keeps looking attributes up by exact name, because after this they ARE exact.
// Two of those would be actively unsafe otherwise: markSensitive applies sensitivity by
// exact name, so an unresolved alias would leave a secret unmarked and print it in clear,
// and the planner would see one attribute under two spellings and propose a change
// forever.
//
// A name that resolves to nothing is LEFT ALONE rather than reported here, so that
// checkConfiguredAttributes reports it once with the list of attributes the type has.
func canonicaliseAttributes(
	attrs map[string]value.Value, def *schema.ResourceDefinition, ds *diag.Diagnostics,
) {
	written := make([]string, 0, len(attrs))
	for name := range attrs {
		written = append(written, name)
	}
	// Sorted so that two spellings of one attribute are reported against the same pair
	// on every run, and so the surviving key is the same one every time.
	sort.Strings(written)

	claimed := map[string]string{} // canonical -> the spelling that claimed it
	for _, name := range written {
		canonical, ok := def.Canonical(name)
		if !ok || canonical == name {
			if ok {
				claimed[canonical] = name
			}
			continue
		}
		if first, taken := claimed[canonical]; taken {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary: strconv.Quote(first) + " and " + strconv.Quote(name) +
					" are the same attribute",
				Detail: "Both name " + strconv.Quote(canonical) + " on " + def.Type +
					". Setting one attribute twice leaves it ambiguous which value applies.",
				Action: "Remove one of them.",
				Origin: attrs[name].Origin,
			})
			continue
		}
		claimed[canonical] = name
		attrs[canonical] = attrs[name]
		delete(attrs, name)
	}
}

// canonicaliseIgnoreChanges resolves `ignore_changes:` entries to the plugin's own
// attribute names, and refuses one that names nothing (PLAN.md §14.2).
//
// THE SAME BOUNDARY as every other spelling a user writes, for the same reason: the
// planner compares these names against attribute keys that are canonical by then, so an
// alias left unresolved would silently ignore NOTHING — the user would have written
// `ignore_changes: [taskRevision]`, seen it accepted, and watched the next apply revert
// the very attribute they protected. Silence is the failure mode this guards against, so
// a name matching no attribute is an ERROR rather than a warning.
//
// Sorted afterwards so a plan artifact is byte-stable however the list was written
// (invariant 6).
func canonicaliseIgnoreChanges(
	r *resource.ResolvedResource, def *schema.ResourceDefinition, ds *diag.Diagnostics,
) {
	if len(r.Lifecycle.IgnoreChanges) == 0 {
		return
	}
	out := make([]string, 0, len(r.Lifecycle.IgnoreChanges))
	seen := map[string]bool{}

	for _, written := range r.Lifecycle.IgnoreChanges {
		canonical, ok := def.Canonical(written)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary: "`ignore_changes` names " + strconv.Quote(written) +
					", which " + def.Type + " has no attribute for",
				Detail: "Ignoring an attribute that does not exist protects nothing, and the next " +
					"apply would revert whatever was meant to be left alone.\nAttributes of " +
					def.Type + ":\n  " + strings.Join(attributeNames(def), "\n  "),
				Action: "Correct the name, or remove it from `ignore_changes`.",
				Origin: r.Origin,
			})
			continue
		}
		if seen[canonical] {
			// Two spellings of one attribute. Harmless — ignoring twice is ignoring —
			// so it is deduplicated rather than refused.
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	sort.Strings(out)
	r.Lifecycle.IgnoreChanges = out
}

// checkConfiguredAttributes rejects what configuration must not set.
// Redundancy note (measured): removing the sort below fails nothing. It
// orders the per-attribute diagnostics this function emits for one resource,
// and no fixture has two rejected attributes on one resource. Kept for the
// same reason as bind.go's sortedAttributeNames: unstable diagnostic order
// is a user-visible defect no test would notice.
func checkConfiguredAttributes(attrs map[string]value.Value, def *schema.ResourceDefinition, origin value.Origin, ds *diag.Diagnostics) {
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		v := attrs[name]

		attr, known := def.Attribute(name)
		if !known {
			action := "Remove the attribute, or correct its name."
			if name == "count" {
				// `count` is NOT a reserved word here, and deliberately so: a
				// provider may legitimately have an attribute of that name — a
				// service's replica count, say — and refusing it at decode time
				// would break perfectly good configuration for the sake of a
				// hint. So the hint lives here instead, where it is only
				// reached by a `count` this type genuinely has no attribute
				// for, which is almost always somebody writing Terraform.
				action = "infrena has no `count`: an instance's identity is its KEY, never its " +
					"position, because removing one entry from a count would shift every later " +
					"instance and propose destroying resources that had not changed. Write " +
					"`for_each:` over a list or map, and use ${each.key} where you would have " +
					"used an index."
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + " has no attribute " + strconv.Quote(name),
				Detail:   "Attributes of " + def.Type + ":\n  " + strings.Join(attributeNames(def), "\n  "),
				Action:   action,
				Origin:   v.Origin,
			})
			continue
		}

		// Computed AND Optional is the third state §14.1 adds: configuration may set
		// it, and the provider picks when configuration does not. Only a computed
		// attribute that is NOT optional is refused.
		if attr.Computed && !attr.Optional {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(name) + " is computed and cannot be set",
				Detail:   "The provider determines this value. It can be referenced by other resources, but not configured.",
				Origin:   v.Origin,
			})
			continue
		}

		// An unknown carries the kind it will have, but an unknown produced by
		// a failed parse carries a placeholder. Checking it would report the
		// same problem twice, so kind checking applies to known values only.
		if !v.Known {
			// The compile-time evaluator (Task 6) has no schema access, so
			// every reference-derived unknown carries a KindString
			// placeholder regardless of what the attribute actually holds —
			// this is the first stage that knows the real Kind. Left
			// uncorrected, value.Unknown's own contract ("an unknown value
			// still carries its Kind") silently does not hold, all the way
			// through planning and rendering. Correcting it here is cheap
			// and makes every later stage trustworthy without having to
			// special-case the origin of the unknown.
			if v.Kind != attr.Kind {
				v.Kind = attr.Kind
				attrs[name] = v
			}
			continue
		}

		if v.Kind != attr.Kind {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  strconv.Quote(name) + " must be " + attr.Kind.String() + ", got " + v.Kind.String(),
				Origin:   v.Origin,
			})
			continue
		}

		// WHERE DECLARATIVE VALIDATION WILL GO, and one thing to keep when it
		// does. A per-attribute `Validate func(value.Value) error` lived here and
		// is gone with §31.1: a function cannot cross a pipe, and nothing ever set
		// it. Its replacement is declarative — an enum, a range, a pattern — added
		// when the first real attribute needs one.
		//
		// The rule that cost something to learn: a validator's message must not be
		// printed for an attribute the schema declares Sensitive. The natural way
		// to write one is fmt.Errorf("... got %q", s), which puts the secret on
		// stderr verbatim. This is the only place that knows both the message and
		// the sensitivity, so it is the only place that can hold them apart —
		// report that the value was rejected, say the message is withheld because
		// the attribute is sensitive, and point at the provider's documented
		// constraints instead.
	}
}

// applyDefaults fills absent optional attributes, marking each SourceDefault.
// It never overwrites a value configuration supplied: an explicit value always
// wins over an implicit one. A default the value model cannot express, or one
// that does not match its attribute's declared kind, is a provider bug and is
// reported rather than silently filled in — see checkedDefault.
//
// Redundancy note (measured): removing the sort below fails nothing. Filling
// defaults is order-independent — each attribute is independent of the
// others — so the sort exists only so that any DIAGNOSTICS a malformed default
// produces come out in a stable order. No fixture has two bad defaults on one
// resource, which is the only way to observe it.
func applyDefaults(attrs map[string]value.Value, def *schema.ResourceDefinition, origin value.Origin, ds *diag.Diagnostics) {
	names := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		attr := def.Attributes[name]
		if attr.Default == nil || attr.Computed {
			continue
		}
		if _, present := attrs[name]; present {
			continue
		}
		raw := attr.Default
		v, ok := checkedDefault(raw, attr.Kind)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + ": the default for " + strconv.Quote(name) + " is not a " + attr.Kind.String(),
				Detail: fmt.Sprintf(
					"The default is a %T, which is not a %s. A provider default must be the "+
						"kind its attribute declares. This is a provider bug, not a "+
						"configuration error.",
					raw, attr.Kind,
				),
				Action: "This is a defect in the provider; please report it.",
				Origin: origin,
			})
			continue
		}
		attrs[name] = v
	}
}

// applyInstanceDefaults fills absent attributes from the instance's `defaults:`
// block — the middle rung of §12.1's ladder.
//
// WHOLE, not merged: a resource writing `tags: {team: payments}` replaces the
// instance's map rather than adding to it. That is the documented rule and the reason
// §10.2 has `merge()` — a user who wants both writes
// `${merge(var.tags, {team: payments})}` and can see, in the file, which keys they get.
// Merging silently would mean no way to REMOVE an inherited key.
//
// Marked SourceDefault so a plan prints [default] and generation omits it, and
// ScopeInstanceDefault so a reader is sent to `providers:` rather than to the plugin.
// Shallow, so a leaf that came from a variable keeps saying so.
//
// Nothing is reported here. A key that fits no resource type at all is refused once,
// per instance, by internal/providers.checkDefaults; a key that fits SOME of them is
// correct and ordinary — `tags:` on everything taggable — so a type that does not
// declare it is silently skipped rather than reported once per resource.
func applyInstanceDefaults(
	attrs map[string]value.Value, def *schema.ResourceDefinition, inst providers.Instance,
) {
	for _, name := range sortedDefaultKeys(inst.Defaults) {
		attr, declared := def.Attribute(name)
		if !declared || (attr.Computed && !attr.Optional) {
			continue
		}
		if _, present := attrs[name]; present {
			continue
		}
		v := inst.Defaults[name]
		if v.Kind != attr.Kind {
			continue
		}
		attrs[name] = v.WithSource(value.SourceDefault).WithScope(value.ScopeInstanceDefault)
	}
}

// sortedDefaultKeys orders an instance's default keys, so that filling them is
// deterministic even though each is independent of the others.
func sortedDefaultKeys(defaults map[string]value.Value) []string {
	out := make([]string, 0, len(defaults))
	for name := range defaults {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// fromDefault wraps a default datum as a value marked SourceDefault, which
// is what lets a plan print [default] and import generate minimal config. It
// reports ok=false when raw is a type the value model cannot express.
//
// Falling back to an unknown here — as a naive conversion might — would be
// worse than reporting the failure: a default is supposed to be always
// computable at plan time (nothing ever resolves it later, unlike a
// reference), so an unknown default would show as a change on every single
// plan forever, with no way for the user to fix it.
//
// The matching Scope — ScopeProviderDefault, the floor of PLAN.md §7's
// precedence chain — is stamped once by checkedDefault rather than in each arm
// here, so it cannot be applied to six kinds and missed on the seventh.
// checkedDefault converts a default datum and confirms it is the kind the
// attribute declares.
//
// Matching a Go type is not the same as matching the declared kind: a datum for
// a float attribute written as int64 builds a perfectly valid KindInt
// value, which would then sail past the kind check that exists to catch
// exactly this — because that check runs on configuration, before a default is
// ever filled in, not on the default itself. The declared kind is the
// contract; the Go type is only how it happens to arrive.
func checkedDefault(raw any, kind value.Kind) (value.Value, bool) {
	v, ok := schema.DatumValue(raw, kind)
	if !ok {
		return value.Value{}, false
	}
	return v.WithScope(value.ScopeProviderDefault), true
}

// checkRequired reports every required attribute configuration did not
// supply, after defaults have had a chance to fill in the optional ones.
func checkRequired(attrs map[string]value.Value, def *schema.ResourceDefinition, origin value.Origin, ds *diag.Diagnostics) {
	for _, name := range def.RequiredAttributes() {
		if _, ok := attrs[name]; !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + " requires " + strconv.Quote(name),
				Detail:   def.Attributes[name].Description,
				Action:   "Set " + name + " on this resource.",
				Origin:   origin,
			})
		}
	}
}

// markSensitive adds schema-declared sensitivity. It never clears sensitivity a
// value already carries: a secret interpolated into an ordinary attribute stays
// classified.
func markSensitive(attrs map[string]value.Value, def *schema.ResourceDefinition) {
	for name, v := range attrs {
		if attr, ok := def.Attribute(name); ok && attr.Sensitive {
			attrs[name] = v.WithSensitive(true)
		}
	}
}

// attributeNames returns a definition's attribute names in sorted order, for
// diagnostics that list what a resource type supports.
//
// Redundancy note (measured): removing this sort fails nothing, because the
// list it orders appears inside one diagnostic's text and every fixture that
// triggers it has too few attributes for the orders to differ. It is the
// most user-visible of this file's three: "did you mean" style suggestions
// that shuffle between runs read as a broken tool.
func attributeNames(def *schema.ResourceDefinition) []string {
	out := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
