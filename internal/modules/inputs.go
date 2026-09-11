package modules

import (
	"sort"
	"strconv"
	"strings"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/expressions"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// Scope is the name environment inside one module instantiation: the variables
// visible to it, and (Task 7) what each bare name in ${name.attr} binds to.
//
// Every resource instantiated at one level shares ONE Scope, by pointer. The
// bindings are a property of the level, not of each resource, and copying them
// per resource would allocate a map whose entries could never differ.
//
// It satisfies expressions.Scope, so it is what compiler stage 6 evaluates a
// resource's attributes against. One scope type serves both stages, which is
// what stops the two from disagreeing about what a name means.
type Scope struct {
	// Module is the instantiation path to this level, outermost first, empty
	// at the root.
	Module []string
	// Vars is what ${name} resolves to here: the module's own inputs plus the
	// three process variables, or the project's whole variable scope at the
	// root.
	Vars variables.Scope
	// names is what a bare name in ${name.attr} binds to at this level:
	// either a plain resource or a module call, each addressed once
	// expansion is done. Filled only while expanding, by bind (outputs.go),
	// and read through Lookup, Names and Qualify — never touched outside this
	// package.
	names map[string]Binding
}

// Variable satisfies half of expressions.Scope.
func (s *Scope) Variable(name string) (value.Value, bool) { return s.Vars.Variable(name) }

// Attribute satisfies the other half. At compile time no resource has been
// created, so every reference to one reports unavailable — which is what turns
// it into an unknown carrying its expression, and simultaneously what makes the
// dependency edge discoverable.
//
// Module outputs never reach here: Task 7's Qualify folds them into the
// expression tree before it is evaluated, precisely so this method has one
// answer rather than two.
func (s *Scope) Attribute(value.Reference) (value.Value, bool) { return value.Value{}, false }

// moduleScope builds the scope inside one instantiation: its resolved inputs,
// plus the process variables carried across from the caller.
//
// caller is the scope the module call is written in. lv is the module's own
// level, whose Inputs say what it accepts. supplied is the call's attributes,
// already evaluated in the caller's scope.
func (w *walker) moduleScope(
	r *config.ResourceDecl, lv level, caller *Scope,
	supplied map[string]value.Value, module []string,
) *Scope {
	inner := &Scope{Module: module}

	// The three facts about the invocation cross every module boundary, each
	// copied as-is, keeping the provenance the compiler stamped. A module that
	// rendered ${environment} as having come from its own inputs would claim an
	// origin that does not exist.
	for _, name := range variables.ProcessVariables {
		if v, ok := caller.Variable(name); ok {
			inner.Vars.Override(name, v)
		}
	}

	moduleName := strings.TrimPrefix(r.Type, TypePrefix)
	schemas, schemaDiags := variables.Schemas(lv.Inputs, "input")
	w.ds.Extend(schemaDiags)

	// An attribute the module does not declare. Reported before the declared
	// ones are resolved, so a typo is not reported a second time as a missing
	// required input.
	for _, name := range sortedAttributeNames(r.Attributes) {
		if _, declared := schemas[name]; declared {
			continue
		}
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module " + strconv.Quote(moduleName) + " has no input " + strconv.Quote(name),
			Detail:   "Inputs it declares:\n  " + strings.Join(inputNames(lv.Inputs), "\n  "),
			Action: "Correct the name, or declare " + strconv.Quote(name) +
				" under that module's `inputs:`.",
			Origin: r.Attributes[name].Origin,
		})
	}

	// lv.Inputs is sorted by name (stage 2), so diagnostics about several bad
	// inputs come out in a stable order with no sort here.
	for _, d := range lv.Inputs {
		s := schemas[d.Name]
		v, ok := supplied[d.Name]
		if !ok {
			if !s.HasDefault {
				w.ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary: "module " + strconv.Quote(r.Name) + " requires input " +
						strconv.Quote(d.Name),
					Detail: strconv.Quote(d.Name) + " is declared at " + s.Origin.String() +
						" with no `default:`, so every instantiation must supply it.",
					Action: "Add `" + d.Name + ":` to this module call, or give the input a `default:`.",
					Origin: r.Origin,
				})
				// Reported AND resolved to a poison value of the DECLARED kind
				// (§7.4), so one missing input is not reported again at every
				// use site. The kind mirrors
				// internal/variables/resolve.go:327, which stamps an unset
				// variable's declared kind the same way (Amendment 2).
				inner.Vars.Override(d.Name, value.Unknown(s.Kind, value.SourceModule).
					WithScope(value.ScopeModuleDefault).
					WithOrigin(s.Origin))
				continue
			}
			// THE rung. Ruling 2: the module's own declared default is the only
			// thing that fills ScopeModuleDefault, and it is stamped here
			// rather than in variables.Schemas because a declaration is not a
			// resolution — only the stage that decides which level won may say
			// which level won.
			inner.Vars.Override(d.Name, s.Default.
				WithSource(value.SourceDefault).
				WithScope(value.ScopeModuleDefault))
			continue
		}

		// Coerce, then judge, then store — the same order and for the same
		// reason as variables.Resolve: compareBounds reads both sides in the
		// declared kind, so an uncoerced value is not merely mistyped, it is
		// silently unbounded.
		coerced, coerceDiags := s.Coerce(v)
		if coerceDiags.HasErrors() {
			w.ds.Extend(coerceDiags)
			continue
		}
		w.ds.Extend(s.Validate(coerced))
		// Stored exactly as evaluation produced it. Amendment 8b: stage 5 does
		// not re-stamp Scope. A literal arrives as stage 2 left it and a
		// ${count} from --var keeps ScopeCLIOverride, because Evaluate returns
		// the variable's own Value — a module boundary is a point where it is
		// tempting to re-derive provenance, and the rule is that provenance is
		// recorded where a value ENTERS, not where it is passed along.
		inner.Vars.Override(d.Name, coerced)
	}

	return inner
}

// evaluateCall resolves a module call's attributes IN THE CALLER'S SCOPE.
//
// Stage 5 does this rather than leaving it to stage 6's bindAttribute, because
// stage 5 runs first and cannot build the module's scope without these values;
// after expansion the call resource is gone, so stage 6 never sees it.
//
// exprs is the call's attributes, already parsed once by parseCall
// (outputs.go) — Ruling 6: the ordering pass reads the references and this
// evaluates the trees, and parsing twice would report every syntax error
// twice. caller.Qualify resolves a bare name against the caller's own
// bindings before evaluation, exactly as stage 6's bindAttribute does — Task 5
// left this uncalled because no module output existed yet for a reference to
// resolve to; now a sibling module call's OUTPUT is reachable here, which is
// the entire reason orderCalls exists.
func (w *walker) evaluateCall(
	r *config.ResourceDecl, caller *Scope, exprs map[string]*value.Expr,
) map[string]value.Value {
	out := make(map[string]value.Value, len(r.Attributes))
	for _, name := range sortedAttributeNames(r.Attributes) {
		attr := r.Attributes[name]
		if !attr.HasExpressions {
			out[name] = attr.Value
			continue
		}
		e, ok := exprs[name]
		if !ok {
			// parseCall already reported the syntax error or the
			// composite-carrying-interpolation refusal; nothing to evaluate.
			continue
		}
		v, evalDiags := expressions.Evaluate(caller.Qualify(e), caller)
		w.ds.Extend(evalDiags)
		out[name] = v
	}
	return out
}

// sortedAttributeNames visits attributes in a stable order. Go's map iteration
// is randomised, and diagnostic order within one resource must not change
// between runs of the same configuration (invariant 6).
func sortedAttributeNames(attrs map[string]config.AttributeDecl) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// inputNames lists a module's declared inputs for a diagnostic. lv.Inputs is
// already sorted by name (stage 2), so there is nothing to sort — reading the
// slice rather than a map is what keeps the message identical between runs.
func inputNames(decls []config.VariableDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Name)
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}
