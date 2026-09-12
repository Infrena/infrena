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
		// A SKIPPED instance is not a reference target (PLAN.md §6.2). Leaving
		// it here resolves the reference silently and records an edge to a
		// resource that is never created: the plan reads correctly and the apply
		// fails waiting for a value nothing will produce. That is the failure
		// mode this whole rule exists to prevent, and it is why skipped
		// instances are marked rather than trusted to be harmless.
		if inst.Skipped {
			continue
		}
		declared[inst.Address.String()] = targetFor(inst, reg)
	}

	for _, inst := range exp.Instances {
		decl := inst.Decl
		self := inst.Address.String()

		resolved := &resource.ResolvedResource{
			Address: inst.Address,
			Type:    decl.Type,
			// Stage 5 leaves this empty when nothing named an instance, because it
			// has no business knowing which instances exist. THIS is where the
			// default is supplied, once, so a resource reaching the planner always
			// names the instance it belongs to — and a destroy, which has only
			// state, inherits that name from the apply that created it.
			Provider:  providerInstanceFor(inst, opts),
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
			resolved.Attrs[name] = bindAttribute(inst, attr, opts.Environment, declared, edges, &ds)
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
				// Skipped BEFORE undeclared: the name is in the file, so
				// "undeclared" would send the reader after a typo that is not
				// there. Only a resource that SURVIVES may complain — a skipped
				// one depending on another skipped one is two things leaving
				// together, which is fine.
				if origin, wasSkipped := inst.Scope.Skipped(target); wasSkipped {
					// See the matching branch in bindAttribute: a skipped
					// resource depending on a skipped resource reports nothing,
					// and must not fall through to "undeclared".
					if !inst.Skipped {
						ds.Add(skippedTargetDiag(target, opts.Environment, origin, decl.Origin,
							"depends_on names"))
					}
					continue
				}
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

		// THE ONE DROP POINT for a skipped resource (PLAN.md §6.2): after
		// reference binding, before anything downstream.
		//
		// After, because binding is what reports a surviving resource depending
		// on this one — drop it earlier and that diagnostic becomes "no such
		// resource". Before anything downstream, because from here on a skipped
		// resource is indistinguishable from one the user deleted, which is
		// exactly what invariant 1 should make of it: in state, absent from
		// configuration, therefore destroyed. That is not a side effect of the
		// feature, it IS the feature.
		//
		// Its own attributes were still bound above, and deliberately: a skipped
		// resource referring to a live one must not report anything, and the only
		// way to know the reference was legitimate is to resolve it.
		if inst.Skipped {
			continue
		}
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
	environment string,
	declared map[string]refTarget,
	edges map[string]value.Origin,
	ds *diag.Diagnostics,
) value.Value {
	if !attr.HasExpressions {
		return attr.Value
	}

	// A COMPOSITE carrying interpolations is walked leaf by leaf (PLAN.md
	// §10.1). Until M10 this was refused, and the refusal was right for the code
	// that existed: HasExpressions is set whenever any leaf holds "${" while the
	// leaf itself was never parsed, so returning the composite would have put
	// raw "${...}" text into a plan as though it were a literal.
	//
	// The per-leaf work below is unchanged and is what the walk calls, so a leaf
	// inside a map gets exactly the same treatment a bare string does: the same
	// module-output edges, the same reference checks, the same evaluation. That
	// is the point of sharing only the WALK.
	src, ok := attr.Value.AsString()
	if !ok {
		walked := expressions.WalkLeaves(attr.Value, func(leafSrc string, leafOrigin value.Origin) value.Value {
			return bindOneExpression(inst, leafSrc, leafOrigin, environment, declared, edges, ds)
		})
		// A composite one of whose leaves did not resolve is itself UNKNOWN
		// (PLAN.md §10.1). Left Known, the planner would diff a placeholder leaf
		// against the real value a previous apply recorded and report a change
		// every run — invariant 2 gone — and the executor would never revisit it,
		// so state could not even be written. Both were observed before this
		// line existed.
		if expressions.HasUnknownLeaf(walked) {
			walked.Known = false
		}
		return walked
	}

	return bindOneExpression(inst, src, attr.Origin, environment, declared, edges, ds)
}

// bindOneExpression is everything that happens to ONE interpolated string: parse,
// record module-output edges, qualify, check every reference, evaluate.
//
// Extracted from bindAttribute so a leaf inside a map goes through the identical
// path a bare attribute does. Two copies would mean a reference inside a map
// eventually being checked differently from the same reference outside one.
func bindOneExpression(
	inst modules.Instance,
	src string,
	origin value.Origin,
	environment string,
	declared map[string]refTarget,
	edges map[string]value.Origin,
	ds *diag.Diagnostics,
) value.Value {
	e, parseDiags := expressions.Parse(src, origin)
	ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(value.KindString, value.SourceComputed).WithOrigin(origin)
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
			recordEdge(edges, a.String(), origin)
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
					Origin: origin,
				})
				break
			}
			if origin, wasSkipped := inst.Scope.Skipped(ref.Target.Name); wasSkipped {
				// Only a resource that SURVIVES may complain. Two resources
				// excluded from the same environment referring to each other are
				// leaving together, which is nothing to report — and falling
				// through to "undeclared" for that case, which is what this did
				// before TestASkippedResourceMayReferToAnotherSkippedOne caught
				// it, is worse than saying nothing: it names a typo that is not
				// there, on a resource that is not being built.
				if !inst.Skipped {
					ds.Add(skippedTargetDiag(ref.Target.Name, environment, origin, origin,
						"${"+ref.String()+"} reads"))
				}
				break
			}
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "reference to undeclared resource or module " + strconv.Quote(target),
				Detail: "${" + ref.String() + "} names no resource and no module call that exists.\nKnown here:\n  " +
					strings.Join(inst.Scope.Names(), "\n  "),
				Action: "Correct the reference, or declare " + strconv.Quote(target) + ".",
				Origin: origin,
			})
		case target == self:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "resource " + strconv.Quote(inst.Decl.Name) + " refers to itself",
				Detail:   "${" + ref.String() + "} cannot be resolved: its own value would be required to compute it.",
				Origin:   origin,
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
				Origin: origin,
			})
		default:
			recordEdge(edges, target, origin)
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

// skippedTargetDiag reports a surviving resource depending on one that `skip` or
// `only` removed from this environment (PLAN.md §6.2).
//
// NOT "no such resource", which is what this used to be and what the cheapest
// implementation still produces: the name is in the file, usually a few lines
// away, so that message sends a reader hunting for a typo that does not exist.
//
// It names three things, and each is needed. The TARGET, so the reader knows
// which name. The ENVIRONMENT, because the same configuration is correct
// elsewhere and that is the whole point of the feature. And the ORIGIN OF THE
// FILTER — finding the reference is trivial, finding the `only:` three resources
// away is the part that costs time.
func skippedTargetDiag(target, environment string, filter, at value.Origin, lead string) diag.Diagnostic {
	return diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary: lead + " " + strconv.Quote(target) + ", which is skipped in environment " +
			strconv.Quote(environment),
		Detail: strconv.Quote(target) + " is excluded from this environment by the `skip`/`only` at " +
			describeSkipOrigin(filter) + ", so the value this needs will never exist here.",
		Action: "Skip this resource in the same environments, or widen the filter on " +
			strconv.Quote(target) + ".",
		Origin: at,
	}
}

func describeSkipOrigin(o value.Origin) string {
	if o.File == "" {
		return "its own declaration"
	}
	return o.String()
}

// providerInstanceFor settles which instance a resource belongs to.
//
// Stage 5 resolved the resource's own `provider:` and the module call it inherited
// from (modules.providerFor); what is left is the default, which only the compiler
// knows. Filled HERE rather than at dispatch, because a resource whose instance is
// decided at dispatch time is one whose state cannot say which account it is in —
// and a destroy has nothing but state.
func providerInstanceFor(inst modules.Instance, opts Options) string {
	if inst.ProviderInstance != "" {
		return inst.ProviderInstance
	}
	if opts.DefaultProvider != "" {
		return opts.DefaultProvider
	}
	// A project with no `providers:` block at all. Every project written before
	// §12.1 is this one, and the implicit instance is named after the only plugin
	// there is — which is also what a state file written then already records.
	return "test"
}
