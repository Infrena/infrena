// Package providers resolves the `providers:` block into the instance table the
// rest of the engine dispatches on (PLAN.md §12.1).
//
// It runs between compiler stages 4 and 5: after variables, because an instance's
// configuration interpolates them, and before module expansion, because stage 5
// needs to know which instance a resource belongs to.
package providers

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/value"
)

// Instance is one resolved provider instance.
type Instance struct {
	Name   string
	Plugin string
	// Config is handed to the plugin. Defaults are attribute defaults for every
	// resource that uses this instance. Separate for the reason §12.1 gives: one
	// configures the provider, the other defaults a resource.
	Config   map[string]value.Value
	Defaults map[string]value.Value
	Default  bool
	Origin   value.Origin
}

// Table is every instance, keyed by name, with exactly one Default.
type Table map[string]Instance

// DefaultName returns the name of the default instance, or "" when there are none.
func (t Table) DefaultName() string {
	for _, i := range t {
		if i.Default {
			return i.Name
		}
	}
	return ""
}

// Names lists every instance, sorted, for diagnostics that suggest what the user
// might have meant.
func (t Table) Names() []string {
	out := make([]string, 0, len(t))
	for name := range t {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Resolve turns declarations into the instance table, evaluating every
// interpolation in each instance's configuration.
//
// AFTER VARIABLES, which is the whole point: `iam-role: ${var.aws_role}` with the
// variable set per environment is how one project reaches a different account in
// production than in dev.
func Resolve(decls []config.ProviderDecl, scope variables.Scope) (Table, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := make(Table, len(decls))

	for _, d := range decls {
		inst := Instance{
			Name:     d.Name,
			Plugin:   d.Plugin,
			Default:  d.Default,
			Origin:   d.Origin,
			Config:   resolveAll(d.Name, "configuration", d.Config, scope, &ds),
			Defaults: resolveAll(d.Name, "`defaults`", d.Defaults, scope, &ds),
		}
		out[inst.Name] = inst
	}
	return out, ds
}

// resolveAll evaluates one instance's map of declarations, in sorted key order so
// that diagnostics about several bad values come out the same way every run.
func resolveAll(
	instance, what string, decls map[string]config.AttributeDecl,
	scope variables.Scope, ds *diag.Diagnostics,
) map[string]value.Value {
	out := make(map[string]value.Value, len(decls))
	keys := make([]string, 0, len(decls))
	for k := range decls {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		d := decls[k]
		if !d.HasExpressions {
			out[k] = d.Value
			continue
		}
		if src, ok := d.Value.AsString(); ok {
			out[k] = resolveOne(instance, what, k, src, d.Origin, scope, ds)
			continue
		}
		// A composite, through §10.1's shared walk rather than a second one.
		out[k] = expressions.WalkLeaves(d.Value, func(src string, origin value.Origin) value.Value {
			return resolveOne(instance, what, k, src, origin, scope, ds)
		})
	}
	return out
}

// resolveOne evaluates a single interpolated string from a provider's block.
//
// A RESOURCE REFERENCE IS REFUSED, and that is the rule worth the code. A
// provider's configuration is needed before any resource exists, so
// `iam-role: ${some_db.arn}` would have the provider create the thing its own
// credentials depend on. Checked on the PARSED expression rather than by noticing
// an unknown afterwards, because the two are not the same message: "unknown value"
// reads as an engine bug, while naming the reference says what cannot work and why.
func resolveOne(
	instance, what, key, src string, origin value.Origin,
	scope variables.Scope, ds *diag.Diagnostics,
) value.Value {
	e, parseDiags := expressions.Parse(src, origin)
	ds.Extend(parseDiags)
	if e == nil {
		return value.Unknown(value.KindString, value.SourceExplicit).WithOrigin(origin)
	}

	if refs := e.References(); len(refs) > 0 {
		names := make([]string, 0, len(refs))
		for _, r := range refs {
			names = append(names, strconv.Quote(r.String()))
		}
		sort.Strings(names)
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: "provider " + strconv.Quote(instance) + "'s " + what + " key " +
				strconv.Quote(key) + " refers to a resource",
			Detail: "It names " + strings.Join(names, ", ") + ". A provider is configured before " +
				"any resource exists, so this would require the provider to create the thing its " +
				"own configuration depends on.",
			Action: "Use a variable instead, set per environment — that is how an instance reaches " +
				"a different account in production than in dev.",
			Origin: origin,
		})
		return value.Unknown(value.KindString, value.SourceExplicit).WithOrigin(origin)
	}

	v, evalDiags := expressions.Evaluate(e, providerScope{vars: scope})
	ds.Extend(evalDiags)
	return v
}

// providerScope resolves variables and nothing else.
//
// Attribute always declines, which is belt-and-braces behind resolveOne's
// reference check: that check reports the case with a message a user can act on,
// and this makes the value unknown rather than wrong if one ever reaches here by
// another route.
type providerScope struct{ vars variables.Scope }

func (s providerScope) Variable(name string) (value.Value, bool) { return s.vars.Variable(name) }
func (s providerScope) Attribute(value.Reference) (value.Value, bool) {
	return value.Value{}, false
}
