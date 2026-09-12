package compiler

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/internal/modules"
	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

func bindReferences(exp *modules.Expansion, opts Options, reg *registry.Registry) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	out := ResolvedConfig{
		// The ROOT project name, carried on the Expansion. A module never has
		// its own, and every resource inside one needs the root's for its
		// provider defaults to resolve.
		Project:     exp.Project,
		Environment: opts.Environment,
		Resources:   make(map[string]*resource.ResolvedResource, len(exp.Instances)),
	}

	// Keyed by CANONICAL ADDRESS, not bare name: two modules may each declare a
	// `db`, and keying on the name would resolve both to one resource — a
	// silently wrong plan rather than an error. The value carries what a
	// reference to this target can be checked against.
	declared := make(map[string]refTarget, len(exp.Instances))
	for _, inst := range exp.Instances {
		declared[inst.Address.String()] = targetFor(inst, reg)
	}

	for _, inst := range exp.Instances {
		decl := inst.Decl
		self := inst.Address.String()

		resolved := &resource.ResolvedResource{
			Address:   inst.Address,
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
			resolved.Attrs[name] = bindAttribute(inst, attr, declared, edges, &ds)
		}

		for _, target := range decl.DependsOn {
			// A bare name in depends_on names a resource at the SAME level:
			// `db` written inside `module.net` is `module.net.db`. Stage 5 left
			// these bare deliberately, because only this stage knows the level.
			// A name that resolved to a module call was already fanned out by
			// stage 5 into ExtraDeps, one edge per resource the call produced —
			// nothing is addressed `prod` any more. The bare name is still here
			// because stage 5 does not rewrite DependsOn, so skipping it is how
			// this loop avoids reporting a call the user can see in their file.
			if _, isCall := inst.Scope.OutputNames(target); isCall {
				continue
			}
			key := address.Address{Module: inst.Address.Module, Name: target}.String()
			if _, ok := declared[key]; !ok {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "depends_on names an undeclared resource " + strconv.Quote(target),
					Detail:   "Known resources:\n  " + strings.Join(sortedTargets(declared), "\n  "),
					Action:   "Correct the name, or declare " + strconv.Quote(target) + ".",
					Origin:   decl.Origin,
				})
				continue
			}
			if key == self {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "resource " + strconv.Quote(decl.Name) + " depends on itself",
					Origin:   decl.Origin,
				})
				continue
			}
			recordEdge(edges, key, decl.Origin)
		}

		// Edges whose bare name expanded away: a sibling named a module call,
		// or a call's own depends_on was inherited by what it produced. They
		// arrive already qualified, which is why they are a separate slice —
		// one holding both kinds would make every consumer ask which it had.
		for _, a := range inst.ExtraDeps {
			recordEdge(edges, a.String(), decl.Origin)
		}

		resolved.DependsOn = sortedAddresses(edges)
		out.Resources[self] = resolved
	}

	return out, ds
}

// refTarget is what a reference can be checked against: the target's type, and
// the attribute names it offers.
//
// Every entry describes a PROVIDER RESOURCE. A module call is expanded away
// before this runs and can never appear in `declared` — which is exactly why a
// reference naming one needs the second source of truth in bindAttribute.
type refTarget struct {
	typeName string
	// names is empty for a type no provider registered. The check then skips,
	// so an unknown resource type produces stage 7's single "unknown resource
	// type" diagnostic rather than one "no such attribute" per reference to it.
	names []string
}

func (t refTarget) has(name string) bool {
	for _, n := range t.names {
		if n == name {
			return true
		}
	}
	return false
}

func targetFor(inst modules.Instance, reg *registry.Registry) refTarget {
	def, ok := reg.Definition(inst.Decl.Type)
	if !ok {
		return refTarget{typeName: inst.Decl.Type}
	}
	return refTarget{typeName: def.Type, names: attributeNames(def)}
}

// sortedTargets lists the declared addresses for a diagnostic, sorted.
func sortedTargets(set map[string]refTarget) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// bindAttribute resolves one attribute, recording any edges its references
// imply.
func bindAttribute(
	inst modules.Instance,
	attr config.AttributeDecl,
	declared map[string]refTarget,
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

	// Qualify BEFORE walking the references. A reference written inside a
	// module is SCOPE-RELATIVE until this point — `${db.id}` means "the db in
	// this module" — and Qualify is what makes it absolute, using the scope
	// stage 5 recorded for this instantiation.
	//
	// Qualify also FOLDS a resolved module output into a literal carrying its
	// value. That is why a VALID output reference never reaches the loop below
	// and an INVALID one does: the fold removes exactly the cases that need no
	// checking.
	// BEFORE Qualify, and this order is the point. Qualify FOLDS a resolved
	// module output into a literal carrying its value, so `${thedb.endpoint}`
	// leaves no reference behind — and the edge it implies would be lost. The
	// plan still renders correctly, because the value is there; the APPLY fails,
	// because the executor schedules both in the same wave and the output is
	// still unknown when the reader runs.
	//
	// A module call has many addresses, so one reference becomes many edges:
	// the reader cannot begin until everything the call produced exists.
	for _, ref := range e.References() {
		b, ok := inst.Scope.Lookup(ref.Target.Name)
		if !ok || b.Kind != modules.BindsModule {
			continue
		}
		for _, a := range b.Addresses {
			recordEdge(edges, a.String(), attr.Origin)
		}
	}

	e = inst.Scope.Qualify(e)

	self := inst.Address.String()
	for _, ref := range e.References() {
		// The canonical address, not the bare name: two modules may each
		// declare a `db`, and `declared` is keyed by the same rendering.
		target := ref.Target.String()
		t, known := declared[target]
		switch {
		case !known:
			// A module call is expanded away and is never in `declared`, so a
			// reference to one lands here. Reporting it as an undeclared
			// resource would name a call the user can plainly see in their own
			// file; the scope knows better.
			if outs, isCall := inst.Scope.OutputNames(ref.Target.Name); isCall {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "module call " + strconv.Quote(ref.Target.Name) + " has no output " + strconv.Quote(ref.Attribute),
					Detail: "${" + ref.String() + "} reads an output the module does not publish.\nOutputs it declares:\n  " +
						strings.Join(outs, "\n  "),
					Action: "Reference one of those, or add " + strconv.Quote(ref.Attribute) + " to the module's `outputs:`.",
					Origin: attr.Origin,
				})
				break
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference to undeclared resource or module " + strconv.Quote(target),
				Detail: "${" + ref.String() + "} names no resource and no module call that exists.\nKnown here:\n  " +
					strings.Join(inst.Scope.Names(), "\n  "),
				Action: "Correct the reference, or declare " + strconv.Quote(target) + ".",
				Origin: attr.Origin,
			})
		case target == self:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(inst.Decl.Name) + " refers to itself",
				Detail:   "${" + ref.String() + "} cannot be resolved: its own value would be required to compute it.",
				Origin:   attr.Origin,
			})
		case len(t.names) > 0 && !t.has(ref.Attribute):
			// The attribute axis. Nothing checked this before M5: a typo here
			// passed `validate`, produced a clean plan, and failed halfway
			// through `apply` after real infrastructure existed, with a message
			// naming the symptom rather than the cause.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  t.typeName + " has no attribute " + strconv.Quote(ref.Attribute),
				Detail: "${" + ref.String() + "} reads an attribute that does not exist.\nAttributes of " +
					t.typeName + ":\n  " + strings.Join(t.names, "\n  "),
				Action: "Correct the attribute name.",
				Origin: attr.Origin,
			})
		default:
			recordEdge(edges, target, attr.Origin)
		}
	}

	// In(Dir) is the only place a directory's own variables enter stage 6: a
	// resource declared in resources/db/ sees resources/db/vars/** on top of the
	// project's (PLAN.md §4.1). It narrows the VARIABLES only — the reference
	// resolution above deliberately uses inst.Scope, because a resource's
	// address does not depend on the directory it was declared in and neither
	// may the names it can refer to.
	v, evalDiags := expressions.Evaluate(e, inst.Scope.In(inst.Decl.Dir))
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

// sortedAttributeNames returns an attribute map's keys in sorted order, so
// resolution and diagnostics do not depend on Go's randomised map order.
//
// Redundancy note (measured): removing this sort fails nothing in the suite.
// Nothing downstream re-sorts it — what it orders is the sequence in which
// per-attribute diagnostics are emitted for ONE resource — and no fixture
// today has two failing attributes on one resource, which is the only shape
// that could observe it. It is kept: the alternative is diagnostics that
// reorder themselves between runs, which is invisible in a test suite and
// obvious to a user diffing two outputs.
func sortedAttributeNames(attrs map[string]config.AttributeDecl) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedAddresses returns an edge set as addresses, sorted canonically.
//
// LOAD-BEARING, unlike the two sorts above, and pinned:
// TestBindSortsDependsOnEveryTime (bind_test.go) fails without it. This is
// what puts ResolvedResource.DependsOn in canonical order, built from a map,
// and it is what makes ResolvedConfig.Hash's own sort.Strings(deps)
// redundant — invariant 6 (plan determinism) rests on one of the two, and
// each now has its own test so that removing either is caught.
func sortedAddresses(edges map[string]value.Origin) []address.Address {
	out := make([]address.Address, 0, len(edges))
	for name := range edges {
		// The key is a canonical address, so parse it back rather than
		// wrapping it as a bare Name: `module.net.db` must come out with its
		// module path intact, not as a root resource whose name has dots in it.
		a, err := address.Parse(name)
		if err != nil {
			a = address.Address{Name: name}
		}
		out = append(out, a)
	}
	address.Sort(out)
	return out
}
