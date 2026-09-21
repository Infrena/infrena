package compiler

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/internal/modules"
	"github.com/infrena/infrena/internal/providers"
	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// bindReferences is compiler stage 6. It resolves every expression against the
// resources the expansion produced, records the dependency edges those
// references imply, and drops the resources this environment skips.
func bindReferences(
	exp *modules.Expansion, opts Options, reg *registry.Registry, table providers.Table,
) (ResolvedConfig, diag.Diagnostics) {
	var ds diag.Diagnostics

	out := ResolvedConfig{
		// The root project name, carried on the Expansion. A module never has its
		// own, and every resource inside one needs the root's for its provider
		// defaults to resolve.
		Project:     exp.Project,
		Environment: opts.Environment,
		Resources:   make(map[string]*resource.ResolvedResource, len(exp.Instances)),
	}

	// Keyed by canonical address, not bare name: two modules may each declare a
	// `db`, and keying on the name would resolve both to one resource — a
	// silently wrong plan rather than an error. The value carries what a reference
	// to this target can be checked against.
	declared := make(map[string]refTarget, len(exp.Instances))
	for _, inst := range exp.Instances {
		// A skipped instance is not a reference target. Leaving it here resolves
		// the reference silently and records an edge to a resource that is never
		// created: the plan reads correctly and the apply fails waiting for a
		// value nothing will produce.
		if inst.Skipped {
			continue
		}
		declared[inst.Address.String()] = targetFor(inst, reg)
	}

	for _, inst := range exp.Instances {
		decl := inst.Decl
		self := inst.Address.String()

		// Once, into a local: the lifecycle rung below needs the same instance the
		// resource is recorded as belonging to, and computing it twice is how the two
		// come to disagree.
		instance := providerInstanceFor(inst, table, &ds)

		resolved := &resource.ResolvedResource{
			Address: inst.Address,
			Type:    decl.Type,
			// Stage 5 leaves this empty when nothing named an instance, because it
			// has no business knowing which instances exist. This is where the
			// default is supplied, once, so a resource reaching the planner always
			// names the instance it belongs to — and a destroy, which has only
			// state, inherits that name from the apply that created it.
			Provider: instance,
			Attrs:    make(map[string]value.Value, len(decl.Attributes)),
			// The instance's `defaults:` may supply a lifecycle flag, and this is
			// where the lifecycle is assembled. Stage 7 handles the schema half: a
			// lifecycle option is not a schema attribute, so it has nothing to
			// resolve against a definition and no business waiting for one.
			Lifecycle: lifecycleFor(decl.Lifecycle, table[instance]),
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
			// A bare name in depends_on names a resource at the same level: `db`
			// written inside `module.net` is `module.net.db`. Stage 5 leaves these
			// bare because only this stage knows the level. A name that resolved
			// to a module call was already fanned out into ExtraDeps, one edge per
			// resource the call produced, but the bare name is still here — so it
			// is skipped rather than reported as a resource that does not exist.
			if _, isCall := inst.Scope.OutputNames(target); isCall {
				continue
			}
			key := address.Address{Module: inst.Address.Module, Name: target}.String()
			if _, ok := declared[key]; !ok {
				// Skipped before undeclared: the name is in the file, so
				// "undeclared" would send the reader after a typo that is not
				// there. Only a resource that survives may complain — a skipped
				// one depending on another skipped one is two things leaving
				// together.
				if origin, wasSkipped := inst.Scope.Skipped(target); wasSkipped {
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

		// Edges whose bare name expanded away: a sibling named a module call, or a
		// call's own depends_on inherited by what it produced. They arrive already
		// qualified, which is why they are a separate slice — one holding both
		// kinds would make every consumer ask which it had.
		for _, a := range inst.ExtraDeps {
			recordEdge(edges, a.String(), decl.Origin)
		}

		resolved.DependsOn = sortedAddresses(edges)

		// The one drop point for a skipped resource: after reference binding,
		// before anything downstream.
		//
		// After, because binding is what reports a surviving resource depending on
		// this one — drop it earlier and that diagnostic becomes "no such
		// resource". Before anything downstream, because from here on a skipped
		// resource is indistinguishable from one the user deleted: in state,
		// absent from configuration, therefore destroyed. That is the feature, not
		// a side effect of it.
		//
		// Its own attributes were still bound above, deliberately: a skipped
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
// Every entry describes a provider resource. A module call is expanded away
// before this runs and can never appear in `declared` — which is exactly why a
// reference naming one needs the second source of truth in bindAttribute.
type refTarget struct {
	typeName string
	// names is empty for a type no provider registered. The check then skips,
	// so an unknown resource type produces stage 7's single "unknown resource
	// type" diagnostic rather than one "no such attribute" per reference to it.
	names []string
	// def is carried so a reference can be canonicalised, not merely checked:
	// `${vpc.cidr}` must become `${vpc.CidrBlock}` here, because the evaluator
	// resolves against a plain map of attribute values with no schema in reach
	// (expressions.ResourceScope). Stage 6 is the last place that knows both.
	def *schema.ResourceDefinition
}

func (t refTarget) has(name string) bool {
	return slices.Contains(t.names, name)
}

// canonical resolves a reference's attribute spelling to the plugin's own name.
// Unknown types carry no definition, so they resolve to nothing and the caller's
// existing "skip the attribute axis" behaviour is unchanged.
func (t refTarget) canonical(name string) (string, bool) {
	if t.def == nil {
		return "", false
	}
	return t.def.Canonical(name)
}

func targetFor(inst modules.Instance, reg *registry.Registry) refTarget {
	def, ok := reg.Definition(inst.Decl.Type)
	if !ok {
		return refTarget{typeName: inst.Decl.Type}
	}
	return refTarget{typeName: def.Type, names: attributeNames(def), def: def}
}

// canonicaliseRefs rewrites every resource reference in an expression to the
// attribute name its target's plugin declared.
//
// In place, on the AST, because Expr.References() returns a collected copy and
// mutating that changes nothing. It runs before the attribute-axis check, so a
// reference written as an alias is rewritten and then found rather than reported
// as a typo.
//
// A reference whose target or attribute resolves to nothing is left exactly as
// written, so the diagnostics report the name the user actually typed.
func canonicaliseRefs(e *value.Expr, declared map[string]refTarget) {
	if e == nil {
		return
	}
	if e.Op == value.OpResourceRef {
		if t, known := declared[e.Ref.Target.String()]; known {
			if canonical, ok := t.canonical(e.Ref.Attribute); ok {
				e.Ref.Attribute = canonical
			}
		}
	}
	for _, arg := range e.Args {
		canonicaliseRefs(arg, declared)
	}
}

// projectRefs fills in the attribute of every whole-resource reference, from the
// declaration on the attribute that consumes it.
//
// `vpc_id: ${vpc}` becomes `${vpc.id}` here and nowhere else, which is what lets
// the planner, the executor, the plan artifact and the wire format stay exactly as
// they are: downstream sees an ordinary two-part reference and cannot tell the
// difference.
//
// In place on the AST, and before canonicaliseRefs, so that a projected name is
// then canonicalised like any other — a plugin may declare its reference against
// an attribute spelling that is itself an alias.
//
// The engine never guesses. An attribute with no declaration is an error naming
// the fix, not a fallback to "probably the id": a wrong value shipped silently is
// the failure this feature exists to prevent.
//
// consumingTypeRegistered separates two situations a nil consuming would
// conflate: the consuming resource's own type is unregistered, where this must say
// nothing rather than pile a diagnostic about a schema it does not have, versus a
// registered type whose attribute genuinely declares no reference, which is an
// error naming the fix.
//
// scope is consulted for exactly one thing: whether the reference's target is a
// module call rather than a resource. A module call has outputs, not schema'd
// attributes, so there is nothing to project onto, and checking that ahead of the
// consuming attribute's own declaration is what keeps the diagnostic about the
// module rather than about a missing output or a missing declaration.
//
// handled records every reference this function has had its say about, including
// the cases where it deliberately says nothing, so the checks in bindOneExpression
// do not process the same still-empty attribute again and pile a second,
// unrelated diagnostic onto the symptom. A reference this function actually
// projects is not marked: it now carries a real attribute and must flow through
// every check like one written out by hand.
func projectRefs(
	e *value.Expr,
	scope *modules.Scope,
	consuming *schema.Attribute,
	consumingTypeRegistered bool,
	consumingName string,
	origin value.Origin,
	ds *diag.Diagnostics,
	handled map[string]bool,
) {
	if e == nil {
		return
	}
	if e.Op == value.OpResourceRef && e.Ref.Attribute == "" {
		switch {
		case !consumingTypeRegistered:
			// Nothing to check, and nothing to say: stage 7 already reports the
			// unregistered type itself, and that diagnostic should stand alone.
			handled[e.Ref.String()] = true
		case isModuleCallTarget(scope, e.Ref.Target.Name):
			// A module exposes outputs, not schema'd attributes, so there is
			// nothing on the call itself to project — regardless of what the
			// consuming attribute declares.
			outs, _ := scope.OutputNames(e.Ref.Target.Name)
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${" + e.Ref.Target.String() + "} names a module call, not a resource",
				Detail: "A module exposes outputs, not attributes, so there is nothing on " +
					strconv.Quote(e.Ref.Target.String()) + " itself for `" + consumingName +
					"` to hold.\nOutputs it declares:\n  " + strings.Join(outs, "\n  "),
				Action: "Name the output you mean, as ${" + e.Ref.Target.String() + ".<output>}.",
				Origin: origin,
			})
			handled[e.Ref.String()] = true
		case consuming == nil || consuming.References == nil:
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "${" + e.Ref.Target.String() + "} passes a resource to an attribute that declares no reference",
				Detail: "`" + consumingName + "` does not say which of " +
					strconv.Quote(e.Ref.Target.String()) + "'s attributes it holds, so there is " +
					"nothing to pick — write ${" + e.Ref.Target.String() + ".<attribute>} instead. " +
					"The provider declares that, not infrena.",
				Action: "Name the attribute you mean, as ${" + e.Ref.Target.String() + ".<attribute>}.",
				Origin: origin,
			})
			handled[e.Ref.String()] = true
		default:
			e.Ref.Attribute = consuming.References.Attribute
		}
	}
	for _, arg := range e.Args {
		projectRefs(arg, scope, consuming, consumingTypeRegistered, consumingName, origin, ds, handled)
	}
}

// isModuleCallTarget reports whether name is bound, at scope, to a module call
// rather than a resource. A module call is expanded away before `declared` is
// built and can never appear there, which is why projectRefs needs this second
// source of truth.
func isModuleCallTarget(scope *modules.Scope, name string) bool {
	b, ok := scope.Lookup(name)
	return ok && b.Kind == modules.BindsModule
}

// checkReferredType reports a reference into a resource of the wrong type.
//
// It runs for a reference written in full as well as for a projected one. Naming
// the attribute explicitly escapes the projection, which is what the projection is
// sugar for, but it must not escape the check: reaching into the wrong resource is
// the same mistake whichever spelling it wears.
//
// It runs only when the consuming attribute declares a reference, because there
// is nothing to check against otherwise.
func checkReferredType(
	ref value.Reference,
	consuming *schema.Attribute,
	consumingName string,
	declared map[string]refTarget,
	origin value.Origin,
	ds *diag.Diagnostics,
) {
	if consuming == nil || consuming.References == nil {
		return
	}
	target, known := declared[ref.Target.String()]
	if !known || target.typeName == "" {
		// An undeclared target already has its own diagnostic; do not tell the
		// same reader about a type mismatch with a resource that does not exist.
		return
	}
	if target.typeName == consuming.References.Type {
		return
	}
	ds.Add(diag.Diagnostic{
		Severity: diag.SeverityError,
		Summary: consumingName + " refers to " + consuming.References.Type +
			", and " + strconv.Quote(ref.Target.String()) + " is " + target.typeName,
		Detail: "${" + ref.String() + "} reaches into a resource of the wrong type.",
		Action: "Pass a " + consuming.References.Type +
			", or name the attribute you mean on a resource of that type.",
		Origin: origin,
	})
}

// checkReferredFields reports a path into a declared map attribute that names a
// key the map does not have.
//
// It runs only for the steps past the attribute axis, and only once that axis has
// confirmed ref.Attribute exists. It walks ref.Path against the target
// attribute's declared Fields one key step at a time, descending into the nested
// Attribute a match names, so a path several keys deep is checked at every level.
//
// Nil Fields, at the top or at any nested level, stops the walk without complaint:
// that is what an open map means, and it is not this function's place to decide a
// provider was wrong to leave one open. A StepIndex stops the walk the same way —
// Fields describes a map's keys, not a list's shape.
func checkReferredFields(ref value.Reference, t refTarget, origin value.Origin, ds *diag.Diagnostics) bool {
	if t.def == nil {
		return false
	}
	attr, ok := t.def.Attribute(ref.Attribute)
	if !ok {
		return false
	}
	fields := attr.Fields
	described := t.typeName + "." + ref.Attribute
	for _, step := range ref.Path {
		if fields == nil || step.Kind != value.StepKey {
			return false
		}
		next, known := fields[step.Key]
		if !known {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  described + " has no key " + strconv.Quote(step.Key),
				Detail: "${" + ref.String() + "} reads a key that does not exist.\nKeys of " +
					described + ":\n  " + strings.Join(sortedFieldNames(fields), "\n  "),
				Action: "Correct the key name.",
				Origin: origin,
			})
			return true
		}
		fields = next.Fields
		described += "." + step.Key
	}
	return false
}

// sortedFieldNames lists a declared map's known keys for a diagnostic, sorted:
// Go randomises map iteration, and the same configuration must produce the same
// diagnostic on every run.
func sortedFieldNames(fields map[string]schema.Attribute) []string {
	out := make([]string, 0, len(fields))
	for name := range fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

	// The consuming attribute's own declaration, looked up once for every leaf
	// this attribute has: it is what projectRefs checks a whole-resource reference
	// against. Whether the type was registered is tracked separately, because an
	// unregistered type and a registered type whose attribute is simply not found
	// are both "nothing to project against" here but must not be reported alike.
	var consuming *schema.Attribute
	consumingTypeRegistered := false
	if def := declared[inst.Address.String()].def; def != nil {
		consumingTypeRegistered = true
		// Canonicalise first. attr.Name is the user's spelling and Attribute is an
		// exact lookup, so without this an attribute written as one of its aliases
		// finds nothing and `vpc: ${vpc}` reports "declares no reference" about an
		// attribute that plainly declares one. Aliases exist so that the friendly
		// spelling is the one people write, which makes this the common path, not a
		// corner. canonicaliseRefs below is the same rule on the reference side.
		if canonical, ok := def.Canonical(attr.Name); ok {
			if a, ok := def.Attribute(canonical); ok {
				consuming = &a
			}
		}
	}

	// A composite carrying interpolations is walked leaf by leaf, and each leaf
	// goes through the same bindOneExpression a bare string does: the same
	// module-output edges, the same reference checks, the same evaluation. Only
	// the walk is shared.
	src, ok := attr.Value.AsString()
	if !ok {
		walked := expressions.WalkLeaves(attr.Value, func(leafSrc string, leafOrigin value.Origin) value.Value {
			return bindOneExpression(inst, leafSrc, leafOrigin, environment, declared, edges, consuming, consumingTypeRegistered, attr.Name, ds)
		})
		// A composite one of whose leaves did not resolve is itself unknown. Left
		// known, the planner would diff a placeholder leaf against the real value
		// a previous apply recorded and report a change every run, and the
		// executor would never revisit it, so state could not even be written.
		if expressions.HasUnknownLeaf(walked) {
			walked.Known = false
		}
		return walked
	}

	return bindOneExpression(inst, src, attr.Origin, environment, declared, edges, consuming, consumingTypeRegistered, attr.Name, ds)
}

// bindOneExpression is everything that happens to one interpolated string: parse,
// record module-output edges, qualify, check every reference, evaluate.
//
// Separate from bindAttribute so that a leaf inside a map goes through the
// identical path a bare attribute does; two copies would eventually check a
// reference inside a map differently from the same reference outside one.
func bindOneExpression(
	inst modules.Instance,
	src string,
	origin value.Origin,
	environment string,
	declared map[string]refTarget,
	edges map[string]value.Origin,
	consuming *schema.Attribute,
	consumingTypeRegistered bool,
	consumingName string,
	ds *diag.Diagnostics,
) value.Value {
	e, parseDiags := expressions.Parse(src, origin)
	ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(value.KindString, value.SourceComputed).WithOrigin(origin)
	}

	// Module-output edges are recorded before Qualify runs, and the order is the
	// point. Qualify folds a resolved module output into a literal carrying its
	// value, so `${thedb.endpoint}` leaves no reference behind and the edge it
	// implies would be lost. The plan would still render correctly, because the
	// value is there; the apply would fail, because the executor schedules both in
	// the same wave and the output is still unknown when the reader runs.
	//
	// A module call has many addresses, so one reference becomes many edges: the
	// reader cannot begin until everything the call produced exists.
	//
	// Qualify itself makes a reference absolute. One written inside a module is
	// scope-relative until then — `${db.id}` means "the db in this module" — and
	// it uses the scope stage 5 recorded for this instantiation. Because it folds
	// resolved outputs away, a valid output reference never reaches the loop
	// further down and an invalid one does.
	for _, ref := range e.References() {
		b, ok := inst.Scope.Lookup(ref.Target.Name)
		if !ok || b.Kind != modules.BindsModule {
			continue
		}
		// A reference naming ONE instance of a `for_each` call depends on that
		// instance alone. Depending on everything the call produced would be
		// conservatively safe for ordering and wrong in two ways that matter:
		// it serialises instances that have nothing to do with each other, and
		// two instances referring to each other's outputs would form a cycle
		// the configuration does not contain.
		targets := b.Addresses
		if ref.Target.Key != "" {
			if keyed, ok := b.KeyedAddresses[ref.Target.Key]; ok {
				targets = keyed
			}
		}
		for _, a := range targets {
			recordEdge(edges, a.String(), origin)
		}
	}

	e = inst.Scope.Qualify(e)

	// Before canonicaliseRefs: a projected name must then be canonicalised like
	// any other, in case a plugin declares its reference against an attribute
	// spelling that is itself an alias.
	//
	// handled is projectRefs's report card — every reference it already had
	// something to say about, including its deliberate silences — so the checks
	// below do not process the same still-empty attribute a second time.
	handled := map[string]bool{}
	projectRefs(e, inst.Scope, consuming, consumingTypeRegistered, consumingName, origin, ds, handled)

	// Before the attribute axis is checked below, so an alias is rewritten and
	// then found rather than reported as a typo naming an attribute that exists.
	canonicaliseRefs(e, declared)

	self := inst.Address.String()
	for _, ref := range e.References() {
		if handled[ref.String()] {
			// projectRefs already reported on this exact reference, or
			// deliberately said nothing about it, and it still carries the empty
			// attribute that got it there.
			continue
		}
		// The canonical address, not the bare name: two modules may each
		// declare a `db`, and `declared` is keyed by the same rendering.
		target := ref.Target.String()
		t, known := declared[target]
		switch {
		case !known:
			// A module call is expanded away and is never in `declared`, so a
			// reference to one lands here. Reporting it as an undeclared resource
			// would name a call the user can plainly see in their own file.
			if outs, isCall := inst.Scope.OutputNames(ref.Target.Name); isCall {
				// A `for_each` call has no single set of outputs, so a reference
				// naming no instance arrives here with an attribute the module
				// does declare, and the generic message would contradict itself:
				// "has no output endpoint. Outputs it declares: endpoint".
				if b, bound := inst.Scope.Lookup(ref.Target.Name); bound && len(b.Keys) > 0 {
					ds.Add(diag.Diagnostic{
						Severity: diag.SeverityError,
						Summary: "module call " + strconv.Quote(ref.Target.Name) + " declares `for_each`, " +
							"so a reference must name one instance",
						Detail: "${" + ref.String() + "} names the whole set. Instances:\n  " +
							strings.Join(callInstances(ref.Target.Name, b.Keys), "\n  "),
						Action: "Write ${" + ref.Target.Name + "[\"" + b.Keys[0] + "\"]." + ref.Attribute + "}.",
						Origin: origin,
					})
					break
				}
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
				// Only a resource that survives may complain. Two resources
				// excluded from the same environment referring to each other are
				// leaving together, which is nothing to report; falling through to
				// "undeclared" there would name a typo that is not present, on a
				// resource that is not being built.
				if !inst.Skipped {
					ds.Add(skippedTargetDiag(ref.Target.Name, environment, origin, origin,
						"${"+ref.String()+"} reads"))
				}
				break
			}
			// A for_each resource exists but has no address of its own: only its
			// instances do. "Undeclared" is true of the address and false of the
			// resource, and sends the reader looking for a typo in a name that is
			// right there in the file.
			if b, bound := inst.Scope.Lookup(ref.Target.Name); bound && len(b.Instances) > 0 {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary: "resource " + strconv.Quote(ref.Target.Name) + " declares `for_each`, so " +
						"a reference must name one instance",
					Detail: "${" + ref.String() + "} names the whole set. Instances:\n  " +
						strings.Join(instanceKeys(b.Instances), "\n  "),
					Action: "Write ${" + ref.Target.Name + "[\"" + b.Instances[0].Key + "\"]." + ref.Attribute + "}.",
					Origin: origin,
				})
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
		case consuming != nil && consuming.References != nil && t.typeName != "" && t.typeName != consuming.References.Type:
			// The type axis, deliberately before the attribute axis below: once
			// the target is the wrong type, whether it happens to have an
			// attribute of that name is not the reader's problem.
			checkReferredType(ref, consuming, consumingName, declared, origin, ds)
		case len(t.names) > 0 && !t.has(ref.Attribute):
			// The attribute axis. Unchecked, a typo here passes `validate`,
			// produces a clean plan, and fails halfway through `apply` after real
			// infrastructure exists, naming the symptom rather than the cause.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  t.typeName + " has no attribute " + strconv.Quote(ref.Attribute),
				Detail: "${" + ref.String() + "} reads an attribute that does not exist.\nAttributes of " +
					t.typeName + ":\n  " + strings.Join(t.names, "\n  "),
				Action: "Correct the attribute name.",
				Origin: origin,
			})
		case checkReferredFields(ref, t, origin, ds):
			// The shape axis, inside a declared map. Diagnostic already added;
			// nothing downstream should treat a path into the wrong key as a
			// dependency worth recording.
		default:
			recordEdge(edges, target, origin)
		}
	}

	// In(Dir) is the only place a directory's own variables enter stage 6: a
	// resource declared in resources/db/ sees resources/db/vars/** on top of the
	// project's. It narrows the variables only — the reference resolution above
	// deliberately uses inst.Scope, because a resource's address does not depend
	// on the directory it was declared in, and neither may the names it can refer
	// to.
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
// What it orders is the sequence of per-attribute diagnostics for one resource.
// No test observes that, and it is kept anyway: diagnostics that reorder
// themselves between runs are invisible in a test suite and obvious to a user
// diffing two outputs.
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
// Load-bearing, unlike the sort above: this is what puts
// ResolvedResource.DependsOn in canonical order, built from a map, and plan
// determinism rests on it. ResolvedConfig.Hash sorts again, and both have their
// own test, so removing either is caught.
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
// `only` removed from this environment.
//
// Not "no such resource": the name is in the file, usually a few lines away, so
// that message sends a reader hunting for a typo that does not exist.
//
// It names three things, and each is needed: the target, so the reader knows
// which name; the environment, because the same configuration is correct
// elsewhere; and the origin of the filter, because finding the `only:` three
// resources away is the part that costs time.
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
// Stage 5 resolved the resource's own `provider:` and the one it inherited from a
// module call; what is left is the default, which only the compiler knows. Filled
// here rather than at dispatch, because a resource whose instance is decided at
// dispatch time is one whose state cannot say which account it is in — and a
// destroy has nothing but state.
func providerInstanceFor(inst modules.Instance, table providers.Table, ds *diag.Diagnostics) string {
	named := inst.ProviderInstance
	if named == "" {
		// The table's own default: the entry marked `default: true`, or the first
		// declared. Taken from the table rather than from Options so that the
		// instance a resource defaults to and the instance the registry built are
		// decided by one thing — a disagreement there creates a resource in one
		// account and then fails to find it in the other.
		return table.DefaultName()
	}
	if _, exists := table[named]; !exists {
		// Reported here, where the `provider:` key is, rather than at dispatch.
		// The executor's own guard says "no provider instance offers this type",
		// which is true and useless: the reader's mistake is a name, and the names
		// available are in a file they can read.
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary: inst.Address.String() + " names provider instance " +
				strconv.Quote(named) + ", which is not declared",
			Detail: "`provider:` selects one entry of the project's `providers:` list." +
				declaredInstancesDetail(table),
			Action: "Correct the name, or add a `providers:` entry with `name: " + named + "`.",
			Origin: inst.Decl.Origin,
		})
	}
	return named
}

// lifecycleFor settles a resource's lifecycle flags against its instance's
// `defaults:`.
//
// Written beats defaulted, which is why LifecycleDecl tracks whether each key was
// written at all: `prevent_destroy: false` on a resource under an instance
// defaulting it to true must win, and a bare bool cannot tell that from silence.
// Getting it backwards refuses a destroy the user explicitly allowed, which is
// the direction a user cannot work around.
//
// A non-boolean default was already refused when the instance was prepared, so
// AsBool failing here means the key is absent, and absent is the same as unset.
func lifecycleFor(decl config.LifecycleDecl, inst providers.Instance) resource.Lifecycle {
	out := resource.Lifecycle{
		PreventDestroy:      decl.PreventDestroy,
		PreventReplace:      decl.PreventReplace,
		CreateBeforeDestroy: decl.CreateBeforeDestroy,
		Retain:              decl.Retain,
		// Carried as written; stage 7 canonicalises it against the schema, where
		// the attribute names are known. Deliberately not settable from an
		// instance's `defaults:`: which attributes a resource lets drift is a
		// property of that resource and its pipeline, not of the account it lives
		// in.
		IgnoreChanges: decl.IgnoreChanges,
	}
	if !decl.PreventDestroySet {
		if b, ok := inst.Defaults["prevent_destroy"].AsBool(); ok {
			out.PreventDestroy = b
		}
	}
	if !decl.PreventReplaceSet {
		if b, ok := inst.Defaults["prevent_replace"].AsBool(); ok {
			out.PreventReplace = b
		}
	}
	if !decl.CreateBeforeDestroySet {
		if b, ok := inst.Defaults["create_before_destroy"].AsBool(); ok {
			out.CreateBeforeDestroy = b
		}
	}
	if !decl.RetainSet {
		if b, ok := inst.Defaults["retain"].AsBool(); ok {
			out.Retain = b
		}
	}
	return out
}

// declaredInstancesDetail lists what the user could have meant.
func declaredInstancesDetail(table providers.Table) string {
	names := table.Names()
	if len(names) == 0 {
		return "\nThis project declares no provider instances."
	}
	return "\nDeclared instances:\n  " + strings.Join(names, "\n  ")
}

// callInstances renders a for_each module call's instances for a diagnostic that
// has to say which ones exist.
//
// The call's own name rather than any address it produced: the resources inside
// are what the plan lists, but `store["orders"]` is what the user writes, and a
// diagnostic telling them to write something has to show that.
func callInstances(name string, keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, name+"[\""+k+"\"]")
	}
	return out
}

// instanceKeys renders a for_each resource's instances for a diagnostic that
// has to say which ones exist.
func instanceKeys(instances []address.Address) []string {
	out := make([]string, 0, len(instances))
	for _, a := range instances {
		out = append(out, a.String())
	}
	return out
}
