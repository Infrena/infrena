package compiler

import (
	"sort"
	"strconv"
	"strings"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/internal/expressions"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

// compileScope resolves variables at compile time but reports every resource
// attribute as unavailable. That is what turns a reference into an unknown
// carrying its expression, and simultaneously what makes the dependency edge
// discoverable. The apply-time scope in M3 resolves attributes too.
type compileScope struct {
	vars map[string]value.Value
}

// Variable resolves a compile-time variable by name.
func (s compileScope) Variable(name string) (value.Value, bool) {
	v, ok := s.vars[name]
	return v, ok
}

// Attribute always reports unavailable: at compile time no resource has been
// created yet, so every reference to one becomes an unknown.
func (s compileScope) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

// bindReferences is compiler stage 6. It parses and evaluates every attribute,
// records the dependency edges references imply, and rejects references that
// can never become knowable.
func bindReferences(project *config.ProjectDecl, opts Options) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	out := ResolvedConfig{
		Project:     project.Project,
		Environment: opts.Environment,
		Resources:   make(map[string]*resource.ResolvedResource, len(project.Resources)),
	}

	declared := make(map[string]bool, len(project.Resources))
	for _, r := range project.Resources {
		declared[r.Name] = true
	}

	scope := compileScope{vars: variableScope(opts)}

	for _, decl := range project.Resources {
		resolved := &resource.ResolvedResource{
			Address:   address.Address{Name: decl.Name},
			Type:      decl.Type,
			Attrs:     make(map[string]value.Value, len(decl.Attributes)),
			Lifecycle: resource.Lifecycle{PreventDestroy: decl.Lifecycle.PreventDestroy, Retain: decl.Lifecycle.Retain},
			Origin:    decl.Origin,
		}

		edges := map[string]value.Origin{}

		// Attribute names are visited in sorted order rather than Go's
		// randomised map order. Two attributes can reference the same target
		// (see recordEdge), and diagnostic order within a resource must not
		// change from run to run of the same configuration.
		for _, name := range sortedAttributeNames(decl.Attributes) {
			attr := decl.Attributes[name]
			resolved.Attrs[name] = bindAttribute(decl, attr, scope, declared, edges, &ds)
		}

		for _, target := range decl.DependsOn {
			if !declared[target] {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "depends_on names an undeclared resource " + strconv.Quote(target),
					Detail:   "Known resources:\n  " + strings.Join(sortedNames(declared), "\n  "),
					Action:   "Correct the name, or declare " + strconv.Quote(target) + ".",
					Origin:   decl.Origin,
				})
				continue
			}
			if target == decl.Name {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "resource " + strconv.Quote(decl.Name) + " depends on itself",
					Origin:   decl.Origin,
				})
				continue
			}
			recordEdge(edges, target, decl.Origin)
		}

		resolved.DependsOn = sortedAddresses(edges)
		out.Resources[resolved.Address.String()] = resolved
	}

	return out, ds
}

// bindAttribute resolves one attribute, recording any edges its references
// imply.
func bindAttribute(
	decl *config.ResourceDecl,
	attr config.AttributeDecl,
	scope compileScope,
	declared map[string]bool,
	edges map[string]value.Origin,
	ds *diag.Diagnostics,
) value.Value {
	if !attr.HasExpressions {
		return attr.Value
	}

	src, ok := attr.Value.AsString()
	if !ok {
		// A composite carrying an interpolation is not supported: the
		// language interpolates strings, not structures. HasExpressions is
		// set whenever any leaf of a list or map contains "${", but the leaf
		// itself was never parsed as an expression — it is still raw text.
		// Silently returning the composite here would let that raw,
		// unevaluated "${...}" text reach the plan as if it were a literal
		// value, so this must be reported rather than passed through.
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "interpolation inside a " + attr.Value.Kind.String() + " is not supported",
			Detail:   "Expressions may appear in string values only.",
			Origin:   attr.Origin,
		})
		return attr.Value
	}

	e, parseDiags := expressions.Parse(src, attr.Origin)
	ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(attr.Value.Kind, value.SourceComputed).WithOrigin(attr.Origin)
	}

	for _, ref := range e.References() {
		switch {
		case !declared[ref.Resource]:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference to undeclared resource " + strconv.Quote(ref.Resource),
				Detail: "${" + ref.String() + "} names a resource that does not exist.\nKnown resources:\n  " +
					strings.Join(sortedNames(declared), "\n  "),
				Action: "Correct the reference, or declare " + strconv.Quote(ref.Resource) + ".",
				Origin: attr.Origin,
			})
		case ref.Resource == decl.Name:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(decl.Name) + " refers to itself",
				Detail:   "${" + ref.String() + "} cannot be resolved: its own value would be required to compute it.",
				Origin:   attr.Origin,
			})
		default:
			recordEdge(edges, ref.Resource, attr.Origin)
		}
	}

	v, evalDiags := expressions.Evaluate(e, scope)
	ds.Extend(evalDiags)
	return v
}

// recordEdge records that decl depends on target, keeping the earliest origin
// (by source position) when more than one attribute — or an attribute and an
// explicit depends_on — names the same target. Without a tie-break, which
// origin survives would depend on Go's randomised map iteration order, and a
// diagnostic built from it would point somewhere different from run to run of
// the very same configuration.
func recordEdge(edges map[string]value.Origin, target string, origin value.Origin) {
	existing, ok := edges[target]
	if !ok || originLess(origin, existing) {
		edges[target] = origin
	}
}

// originLess reports whether a appears before b in its source file.
func originLess(a, b value.Origin) bool {
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Column < b.Column
}

// variableScope builds the compile-time variable scope. In M2 that is only
// --var; variables.yml and environment variables arrive in M4.
func variableScope(opts Options) map[string]value.Value {
	vars := make(map[string]value.Value, len(opts.Vars)+3)
	for k, v := range opts.Vars {
		vars[k] = value.String(v, value.SourceVariable)
	}
	// Always available, so configuration can name its own environment.
	vars["environment"] = value.String(opts.Environment, value.SourceEnvironment)
	if opts.Region != "" {
		vars["region"] = value.String(opts.Region, value.SourceEnvironment)
	}
	if opts.Account != "" {
		vars["account"] = value.String(opts.Account, value.SourceEnvironment)
	}
	return vars
}

// sortedAttributeNames returns an attribute map's keys in sorted order, so
// resolution and diagnostics do not depend on Go's randomised map order.
func sortedAttributeNames(attrs map[string]config.AttributeDecl) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedNames returns a name set's members in sorted order, for diagnostics
// that list known resources.
func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedAddresses returns an edge set as addresses, sorted canonically.
func sortedAddresses(edges map[string]value.Origin) []address.Address {
	out := make([]address.Address, 0, len(edges))
	for name := range edges {
		out = append(out, address.Address{Name: name})
	}
	address.Sort(out)
	return out
}
