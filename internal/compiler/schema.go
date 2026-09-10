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
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  strconv.Quote(name) + " is not valid: " + err.Error(),
					Origin:   v.Origin,
				})
			}
		}
	}
}

// applyDefaults fills absent optional attributes, marking each SourceDefault.
// It never overwrites a value configuration supplied: an explicit value always
// wins over an implicit one. A default resolver returning a datum the value
// model cannot express is a provider bug and is reported rather than silently
// filled in — see fromDefault.
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
		v, ok := fromDefault(raw, attr.Kind)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  def.Type + "." + name + " default resolver returned an unsupported value",
				Detail: fmt.Sprintf(
					"The default resolver returned a %T, which the value model cannot represent as a %s. "+
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
func attributeNames(def *schema.ResourceDefinition) []string {
	out := make([]string, 0, len(def.Attributes))
	for name := range def.Attributes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
