package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/internal/registry"
	"infra/pkg/schema"
	"infra/pkg/value"
)

// bindSchemas is compiler stage 7. It resolves each resource's type to its
// definition, validates what configuration set, and fills in defaults.
func bindSchemas(cfg *ResolvedConfig, reg *registry.Registry, opts Options) diag.Diagnostics {
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

		checkConfiguredAttributes(r.Attrs, def, r.Origin, &ds)
		applyDefaults(r.Attrs, def, defaultContextFor(cfg, r.Type, opts), r.Origin, &ds)
		checkRequired(r.Attrs, def, r.Origin, &ds)
		markSensitive(r.Attrs, def)
	}

	return ds
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
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + " has no attribute " + strconv.Quote(name),
				Detail:   "Attributes of " + def.Type + ":\n  " + strings.Join(attributeNames(def), "\n  "),
				Action:   "Remove the attribute, or correct its name.",
				Origin:   v.Origin,
			})
			continue
		}

		if attr.Computed {
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

		if attr.Validate != nil {
			if err := attr.Validate(v); err != nil {
				// A validator is handed the whole Value and the natural way
				// to write one is fmt.Errorf("... got %q", s). For an
				// attribute the schema declares Sensitive — two fields away
				// from the Validate func being called — that message would
				// carry the secret onto stderr verbatim. The provider is not
				// doing anything wrong; this is the only place that knows
				// both the message and the sensitivity, so it is the only
				// place that can hold them apart.
				summary := strconv.Quote(name) + " is not valid"
				if !attr.Sensitive {
					summary += ": " + err.Error()
				}
				d := diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  summary,
					Origin:   v.Origin,
				}
				if attr.Sensitive {
					d.Detail = "The provider rejected this value. Its message is withheld " +
						"because the attribute is sensitive and the message may quote it."
					d.Action = "Check the value against the provider's documented constraints for " +
						strconv.Quote(name) + "."
				}
				ds.Add(d)
			}
		}
	}
}

// applyDefaults fills absent optional attributes, marking each SourceDefault.
// It never overwrites a value configuration supplied: an explicit value always
// wins over an implicit one. A default resolver returning a datum the value
// model cannot express, or one that does not match its attribute's declared
// kind, is a provider bug and is reported rather than silently filled in —
// see checkedDefault.
//
// Redundancy note (measured): removing the sort below fails nothing. Filling
// defaults is order-independent — each attribute is independent of the
// others — so the sort exists only so that any DIAGNOSTICS a bad default
// resolver produces come out in a stable order. No fixture has two bad
// defaults on one resource, which is the only way to observe it.
func applyDefaults(attrs map[string]value.Value, def *schema.ResourceDefinition, ctx schema.DefaultContext, origin value.Origin, ds *diag.Diagnostics) {
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
		raw, ok := attr.Default(ctx)
		if !ok {
			continue
		}
		v, ok := checkedDefault(raw, attr.Kind)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + ": the default for " + strconv.Quote(name) + " is not a " + attr.Kind.String(),
				Detail: fmt.Sprintf(
					"The default resolver returned a %T, which is not a %s. "+
						"A provider default must produce the kind its attribute declares. "+
						"This is a provider bug, not a configuration error.",
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

// fromDefault wraps a resolver's datum as a value marked SourceDefault, which
// is what lets a plan print [default] and import generate minimal config. It
// reports ok=false when raw is a type the value model cannot express.
//
// Falling back to an unknown here — as a naive conversion might — would be
// worse than reporting the failure: a default is supposed to be always
// computable at plan time (nothing ever resolves it later, unlike a
// reference), so an unknown default would show as a change on every single
// plan forever, with no way for the user to fix it.
func fromDefault(raw any, kind value.Kind) (value.Value, bool) {
	switch v := raw.(type) {
	case string:
		return value.String(v, value.SourceDefault), true
	case int64:
		return value.Int(v, value.SourceDefault), true
	case int:
		return value.Int(int64(v), value.SourceDefault), true
	case float64:
		return value.Float(v, value.SourceDefault), true
	case bool:
		return value.Bool(v, value.SourceDefault), true
	case []value.Value:
		return value.List(v, value.SourceDefault), true
	case map[string]value.Value:
		return value.Map(v, value.SourceDefault), true
	default:
		return value.Value{}, false
	}
}

// checkedDefault converts a resolver's datum and confirms it produced the kind
// the attribute declares.
//
// Matching a Go type is not the same as matching the declared kind: a resolver
// for a float attribute that returns int64 builds a perfectly valid KindInt
// value, which would then sail past the kind check that exists to catch
// exactly this — because that check runs on configuration, before a default is
// ever filled in, not on the default itself. The declared kind is the
// contract; the Go type is only how it happens to arrive.
func checkedDefault(raw any, kind value.Kind) (value.Value, bool) {
	v, ok := fromDefault(raw, kind)
	if !ok || v.Kind != kind {
		return value.Value{}, false
	}
	return v, true
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

// defaultContextFor builds the context a default resolver is allowed to see:
// environment, region, account, project and type — never another resource's
// attributes, so a default can never depend on an unknown.
//
// Environment and EnvironmentType come from opts, not cfg. In practice the two
// agree — bindReferences (stage 6) sets ResolvedConfig.Environment from the
// same Options.Environment — but opts is the explicit signal bindSchemas was
// handed for this purpose, and Options is what carries Region and Account too;
// reading Environment from a different input than its siblings is the kind of
// inconsistency that drifts silently.
func defaultContextFor(cfg *ResolvedConfig, resourceType string, opts Options) schema.DefaultContext {
	return schema.DefaultContext{
		Environment:     opts.Environment,
		EnvironmentType: environmentType(opts.Environment),
		Region:          opts.Region,
		Account:         opts.Account,
		Project:         cfg.Project,
		Type:            resourceType,
	}
}

// environmentType classifies an environment for default resolution. Explicit
// declaration via `environment: type:` arrives with the environment system in
// M4; until then the name is the only signal available.
func environmentType(name string) string {
	if name == "production" || name == "prod" {
		return "production"
	}
	return name
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
