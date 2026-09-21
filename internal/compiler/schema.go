package compiler

import (
	"fmt"
	"slices"
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
			ds.Add(unknownTypeDiagnostic(r, reg))
			continue
		}

		// Before everything else that touches these keys. Canonicalisation is the
		// one place aliases are resolved, so checkConfiguredAttributes, the
		// defaults, checkRequired and markSensitive all see the plugin's own
		// names — and so does every stage downstream of the compiler.
		canonicaliseAttributes(r.Attrs, def, &ds)
		canonicaliseIgnoreChanges(r, def, &ds)
		checkConfiguredAttributes(r.Attrs, def, r.Origin, &ds)
		// Before the plugin's own defaults, because an instance's `defaults:`
		// beats them: whichever runs first wins, since neither overwrites a value
		// already present.
		applyInstanceDefaults(r.Attrs, def, table[r.Provider])
		applyDefaults(r.Attrs, def, r.Origin, &ds)
		checkRequired(r.Attrs, def, r.Origin, &ds)
		markSensitive(r.Attrs, def)
	}

	return ds
}

// unknownTypeDiagnostic explains an unresolvable resource type. Its whole job is
// to tell apart the two very different reasons a type does not resolve.
//
// A typo in a type a loaded plugin does not offer is answered by the list of
// types. A plugin that never loaded is not: the list is the wrong answer, and
// when nothing loaded at all it is empty, so the message offers a correction for
// a spelling that was never wrong while the real fact goes unsaid.
//
// The case that produces the second: a project whose resources live only inside
// modules. Which plugins to load is worked out from the resource types the root
// declares, and a `module.` call is not one of them; module files are not read
// until stage 5, by which time the registry is built. The fix is a `providers:`
// block, which is the other source that list is read from.
func unknownTypeDiagnostic(r *resource.ResolvedResource, reg *registry.Registry) diag.Diagnostic {
	d := diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary:  "unknown resource type " + strconv.Quote(r.Type),
		Origin:   r.Origin,
	}
	plugin, _, _ := strings.Cut(r.Type, ".")
	loaded := reg.PluginNames()

	if slices.Contains(loaded, plugin) {
		// The plugin IS here and does not offer this type, which is the case
		// the list of types answers.
		d.Detail = "The " + plugin + " plugin is loaded and does not offer it.\n\nKnown types:\n  " +
			strings.Join(reg.Types(), "\n  ")
		// NOT `infrena explain <the misspelling>`, which is the obvious
		// phrasing and is advice that cannot work: the command would fail the
		// same way for the same reason.
		d.Action = "Correct the type. `infrena explain <type>` describes any of the types above."
		return d
	}

	// The plugin never loaded. Say that, rather than offering a list that
	// cannot contain what was asked for.
	detail := "The " + plugin + " plugin was never loaded, so nothing could offer " +
		strconv.Quote(r.Type) + ". "
	switch {
	case len(loaded) == 0:
		detail += "No provider plugins were loaded at all."
	default:
		detail += "Loaded plugins: " + strings.Join(loaded, ", ") + "."
	}

	if len(r.Origin.Module) > 0 {
		// The module case, which is the one that reads as the module's fault.
		detail += "\n\nWhich plugins to load is worked out from the resource types " +
			"declared at the root of the project, and a `module.` call is not one of them. " +
			"Every resource using " + plugin + " is inside a module, so nothing at the root " +
			"asked for it."
		d.Action = "Name it explicitly:\n\n  providers:\n    - plugin: " + plugin +
			"\n\nThat block is optional only because a root resource type usually implies " +
			"its plugin."
		d.Detail = detail
		return d
	}

	d.Detail = detail
	d.Action = "Install the " + plugin + " plugin, or correct the type. " +
		"`infrena plugins list` reports what is installed and where it was loaded from."
	return d
}

// canonicaliseAttributes rewrites configuration's attribute keys to the names the
// plugin declared, resolving aliases and case.
//
// The one place this happens for resource attributes. Everything downstream — the
// defaults, checkRequired, markSensitive, the planner's diff, the generator,
// state, the plan artifact and the plugin wire — looks attributes up by exact
// name, because after this they are exact. Two of those would be actively unsafe
// otherwise: markSensitive applies sensitivity by exact name, so an unresolved
// alias would leave a secret unmarked and print it in clear, and the planner would
// see one attribute under two spellings and propose a change forever.
//
// A name that resolves to nothing is left alone rather than reported here, so that
// checkConfiguredAttributes reports it once with the list of attributes the type
// has.
func canonicaliseAttributes(
	attrs map[string]value.Value, def *schema.ResourceDefinition, ds *diag.Diagnostics,
) {
	written := make([]string, 0, len(attrs))
	for name := range attrs {
		written = append(written, name)
	}
	// Sorted so that two spellings of one attribute are reported against the same
	// pair on every run, and the surviving key is the same one every time.
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
// attribute names, and refuses one that names nothing.
//
// The planner compares these names against attribute keys that are canonical by
// then, so an alias left unresolved would silently ignore nothing: the user would
// have written `ignore_changes: [taskRevision]`, seen it accepted, and watched the
// next apply revert the very attribute they protected. Because the failure mode is
// silence, a name matching no attribute is an error rather than a warning.
//
// Sorted afterwards so a plan artifact is byte-stable however the list was
// written.
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
			// Two spellings of one attribute. Harmless — ignoring twice is
			// ignoring — so it is deduplicated rather than refused.
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	sort.Strings(out)
	r.Lifecycle.IgnoreChanges = out
}

// checkConfiguredAttributes rejects what configuration must not set.
//
// The sort below orders the per-attribute diagnostics emitted for one resource.
// No test observes it, and it is kept anyway: unstable diagnostic order is a
// user-visible defect that a test suite cannot see.
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
				// `count` is deliberately not a reserved word: a provider may
				// legitimately have an attribute of that name — a service's
				// replica count, say — so the hint lives here, where it is only
				// reached by a `count` the type genuinely has no attribute
				// for.
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

		// Computed and optional together is a third state: configuration may set
		// it, and the provider picks when configuration does not. Only a computed
		// attribute that is not optional is refused.
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
			// The compile-time evaluator has no schema access, so every
			// reference-derived unknown carries a KindString placeholder
			// whatever the attribute actually holds, and this is the first stage
			// that knows the real kind. Left uncorrected, the guarantee that an
			// unknown still carries its Kind silently does not hold, all the way
			// through planning and rendering.
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

		// Where declarative per-attribute validation will go — an enum, a range, a
		// pattern — when the first real attribute needs one. A validator function
		// cannot cross the plugin pipe, so it has to be data.
		//
		// The rule to keep when it lands: a validator's message must not be
		// printed for an attribute the schema declares Sensitive, because the
		// natural way to write one puts the rejected value in the message. This is
		// the only place that knows both the message and the sensitivity, so it is
		// the only place that can hold them apart.
	}
}

// applyDefaults fills absent optional attributes, marking each SourceDefault. It
// never overwrites a value configuration supplied: an explicit value always wins
// over an implicit one. A default the value model cannot express, or one that
// does not match its attribute's declared kind, is a provider bug and is reported
// rather than silently filled in — see checkedDefault.
//
// Filling defaults is order-independent; the sort exists only so that any
// diagnostics a malformed default produces come out in a stable order.
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
// block — the middle rung of the precedence ladder.
//
// Whole, not merged: a resource writing `tags: {team: payments}` replaces the
// instance's map rather than adding to it. That is why `merge()` exists — a user
// who wants both writes `${merge(var.tags, {team: payments})}` and can see, in the
// file, which keys they get. Merging silently would leave no way to remove an
// inherited key.
//
// Marked SourceDefault so a plan prints [default] and generation omits it, and
// ScopeInstanceDefault so a reader is sent to `providers:` rather than to the
// plugin. Shallow, so a leaf that came from a variable keeps saying so.
//
// Nothing is reported here. A key that fits no resource type at all is refused
// once per instance by internal/providers; a key that fits some of them is
// ordinary — `tags:` on everything taggable — so a type that does not declare it
// is silently skipped rather than reported once per resource.
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

// checkedDefault converts a default datum and confirms it is the kind the
// attribute declares. It reports ok=false for a datum the value model cannot
// express, or one whose kind does not match.
//
// Matching a Go type is not the same as matching the declared kind: a datum for a
// float attribute written as int64 builds a perfectly valid KindInt value, which
// would then sail past the kind check that exists to catch exactly this — that
// check runs on configuration, before a default is ever filled in, not on the
// default itself. The declared kind is the contract; the Go type is only how it
// happens to arrive.
//
// Reporting the failure rather than falling back to an unknown is deliberate: a
// default is always computable at plan time, so an unknown default would show as
// a change on every plan forever with no way for the user to fix it.
//
// ScopeProviderDefault, the floor of the precedence chain, is stamped once here
// rather than in each kind's arm, so it cannot be applied to six kinds and missed
// on the seventh.
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
// diagnostics that list what a resource type supports. Suggestions that shuffle
// between runs read as a broken tool.
func attributeNames(def *schema.ResourceDefinition) []string {
	out := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
