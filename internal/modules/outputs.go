package modules

import (
	"sort"
	"strings"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/internal/graph"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// BindingKind says which of the two things a bare name in ${name.attr} refers
// to. The parser has no scope and must not guess, so this is decided here,
// where both the resources and the module calls at a level are in hand.
type BindingKind uint8

const (
	// BindsNothing is the zero value: the name is not in scope.
	BindsNothing BindingKind = iota
	// BindsResource: ${name.attr} reads a resource's attribute.
	BindsResource
	// BindsModule: ${name.attr} reads a module call's output.
	BindsModule
)

// Binding is what one bare name refers to at one level.
type Binding struct {
	Kind BindingKind
	// Address is the qualified resource, when Kind is BindsResource.
	Address address.Address
	// Addresses is everything the call produced, when Kind is BindsModule.
	// Nothing is addressed with the call's own name after expansion, so this is
	// how an edge naming it is resolved.
	Addresses []address.Address
	// Outputs are the call's collected outputs keyed by name, when Kind is
	// BindsModule. A value here is routinely unknown (Ruling 5).
	Outputs map[string]value.Value
}

// Lookup reports what a bare name binds to at this level.
func (s *Scope) Lookup(name string) (Binding, bool) {
	b, ok := s.names[name]
	return b, ok
}

// Names lists every bound name, sorted, for diagnostics that say what IS in
// scope. Go's map iteration is randomised and such a list must not reorder
// itself between runs of the same configuration.
func (s *Scope) Names() []string {
	out := make([]string, 0, len(s.names))
	for name := range s.names {
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"(nothing)"}
	}
	return out
}

func (s *Scope) bind(name string, b Binding) {
	if s.names == nil {
		s.names = map[string]Binding{}
	}
	s.names[name] = b
}

// Qualify rewrites every resource reference in e against the names in scope at
// this level. Compiler stage 6 calls it between expressions.Parse and
// expressions.Evaluate (Amendment 7).
//
// A name bound to a RESOURCE has its reference target replaced by the
// resource's qualified address. Without this, `${db.id}` written inside two
// different modules parses to the same reference and resolves to whichever `db`
// the engine looked at first — Ruling 1's silently wrong plan, now with a
// module field that nothing fills so the bug looks handled.
//
// A name bound to a MODULE CALL has its whole node replaced by an OpLiteral
// holding the output's VALUE. Folding here rather than resolving at evaluation
// time is what puts the dependency edge on the real resource inside the module,
// which has to exist before the value becomes knowable. Resolving it in
// Scope.Attribute instead cannot work — the evaluator passes Attribute the
// parsed, BARE reference, so the deferred expression would keep a target naming
// nothing.
//
// IT EMITS NO DIAGNOSTICS, and returns one value — Amendment 7's own spelling,
// `e = inst.Scope.Qualify(e)`. A reference it cannot resolve (a name bound to
// nothing, or a module output that does not exist) is left EXACTLY as it stands
// for stage 6's general attribute-existence check to report (Amendment 11).
// Qualify supplies that check its candidates through Names() and OutputNames();
// it does not build a second message of its own.
//
// A NEW tree is returned and e is never modified: the source AST is shared by
// every value that references it, and internal/expressions' residual relies on
// that.
func (s *Scope) Qualify(e *value.Expr) *value.Expr {
	if e == nil {
		return nil
	}
	if e.Op != value.OpResourceRef {
		if len(e.Args) == 0 {
			return e
		}
		out := *e
		out.Args = make([]*value.Expr, len(e.Args))
		for i, a := range e.Args {
			out.Args[i] = s.Qualify(a)
		}
		return &out
	}

	b, ok := s.names[e.Ref.Target.Name]
	if !ok {
		// Bound to nothing. Left as it stands; stage 6 reports it with Names()
		// as the candidate list, so Ruling 4's "no resource or module named X"
		// wording comes from the one place that wording lives.
		return e
	}

	switch b.Kind {
	case BindsResource:
		out := *e
		out.Ref = value.Reference{Target: b.Address, Attribute: e.Ref.Attribute}
		return &out

	case BindsModule:
		v, declared := b.Outputs[e.Ref.Attribute]
		if !declared {
			// The module arm of attribute-existence. Left unresolved for the
			// same check that catches `${store.endpoint}` on a provider
			// resource with no `endpoint` — one mechanism, rather than two
			// messages for one mistake.
			return e
		}
		// ONE branch for known and unknown alike. Splicing v.Expr for an
		// unknown loses the Kind — eval.go re-derives an unresolved reference
		// as KindString — and Amendment 2's chain then reports a type mismatch
		// instead of reaching value.Coerce's unknown branch. An OpLiteral loses
		// nothing: evaluate()'s OpLiteral case returns e.Literal verbatim, so
		// Known, Kind, Sensitive and the value's own Expr all survive, and the
		// dependency edge survives with them.
		//
		// Sensitivity rides along deliberately. Dropping it would launder a
		// secret into a plan artifact — value.Expr.String redacts a sensitive
		// literal for exactly this reason, so folding is never a second
		// redaction path.
		return &value.Expr{
			Op:      value.OpLiteral,
			Literal: v.WithSource(value.SourceModule).WithOrigin(e.Origin),
			Origin:  e.Origin,
		}

	default:
		return e
	}
}

// OutputNames reports the outputs a module instance exposes, sorted, and false
// when name is not a module instance at this level.
//
// This is the module arm of ONE attribute-existence check (Amendment 11).
// Nothing validates today that a referenced ATTRIBUTE exists on its target —
// bind.go:148 checks only that the resource is declared — so `${store.endpoint}`
// on a `fake.network` with no `endpoint` passes `validate`, produces a clean
// plan, and fails halfway through `apply` after creating real infrastructure.
// Task 8 closes that generally. A provider resource answers from its schema, a
// module instance answers from here, and the user reads the same sentence
// either way.
//
// A module-only diagnostic here instead would be a module-only implementation
// of a check the compiler performs generally — the same shape Amendment 8
// removed from the input-provenance code, where Amendment 5b's rung logic
// reimplemented for modules what bindAttribute already did for everything.
// Removing that in one amendment and reintroducing it in the next is not a
// trade worth making.
func (s *Scope) OutputNames(name string) ([]string, bool) {
	b, ok := s.names[name]
	if !ok || b.Kind != BindsModule {
		return nil, false
	}
	return sortedValueKeys(b.Outputs), true
}

// collectOutputs evaluates a module's `outputs:` block in the MODULE's own
// scope. An output is an expression over the module's resources, so at plan
// time it is routinely unknown (Ruling 5) — not an error, and never an empty
// string.
func (w *walker) collectOutputs(lv level, scope *Scope) map[string]value.Value {
	out := make(map[string]value.Value, len(lv.Outputs))
	// lv.Outputs is sorted by name (stage 2), so several bad outputs report in a
	// stable order with no sort here.
	for _, o := range lv.Outputs {
		out[o.Name] = w.collectOutput(o, scope)
	}
	return out
}

func (w *walker) collectOutput(o config.OutputDecl, scope *Scope) value.Value {
	if !o.HasExpressions {
		// No bare-reference check here. internal/config refuses a bare
		// one-dot output at DECODE, where the line number is, and two
		// implementations of one heuristic drift. See the note on
		// bareReference in internal/config/module_file.go.
		// A literal output is legal: `outputs: {region: {value: us-east-1}}`.
		return o.Value.WithSource(value.SourceModule)
	}

	src, ok := o.Value.AsString()
	if !ok {
		// A composite output is walked leaf by leaf (PLAN.md §10.1), through the
		// same walk compiler stage 6 uses. Three sites used to refuse this with
		// three copies of the refusal; sharing the walk is what stops a module
		// output and a resource attribute coming to disagree about the same YAML.
		return expressions.WalkLeaves(o.Value, func(leafSrc string, leafOrigin value.Origin) value.Value {
			return w.evaluateOutputExpression(leafSrc, leafOrigin, scope)
		}).WithSource(value.SourceModule)
	}

	v := w.evaluateOutputExpression(src, o.Origin, scope)

	// SourceModule says what KIND of thing this is at the call site: a value
	// that came out of a module.
	//
	// FINDING, recorded rather than fixed by adding a test: today every path
	// that reads an Outputs map entry is Qualify's BindsModule fold, which
	// unconditionally re-stamps SourceModule on the value it splices in — so
	// removing this WithSource call does not fail
	// TestKnownModuleOutputFoldsToALiteralFromTheModule or anything else in
	// this package; the sabotage in §7.6 step 7 does not reproduce. It is kept
	// anyway because Outputs is a public field of Binding (Task 8-10 read
	// OutputNames off it already) and a future direct reader of the map —
	// bypassing Qualify entirely — must not see the Source of whatever
	// expression the output happened to evaluate (an input's own
	// SourceVariable/SourceDefault) mislabeled as something other than what a
	// module boundary actually is.
	return v.WithSource(value.SourceModule)
}

// evaluateOutputExpression resolves ONE interpolated string in an output's value.
//
// Extracted so a leaf inside a composite output goes through the identical path
// a bare output does — the same qualification, the same diagnostics. Two copies
// would mean an output inside a map eventually resolving differently from the
// same expression outside one.
func (w *walker) evaluateOutputExpression(src string, origin value.Origin, scope *Scope) value.Value {
	e, parseDiags := expressions.Parse(src, origin)
	w.ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(value.KindString, value.SourceModule).WithOrigin(origin)
	}
	v, evalDiags := expressions.Evaluate(scope.Qualify(e), scope)
	w.ds.Extend(evalDiags)
	return v
}

// sortedValueKeys lists a value map's keys for a diagnostic.
func sortedValueKeys(m map[string]value.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}

// parseCall parses each of a module call's attributes ONCE. The ordering pass
// reads the references; the evaluation reads the trees. Parsing twice would
// report every syntax error twice.
func (w *walker) parseCall(r *config.ResourceDecl) map[string]*value.Expr {
	out := map[string]*value.Expr{}
	for _, name := range sortedAttributeNames(r.Attributes) {
		attr := r.Attributes[name]
		if !attr.HasExpressions {
			continue
		}
		src, ok := attr.Value.AsString()
		if !ok {
			// A composite call INPUT is not parsed into a single expression tree
			// here, because this map is keyed by attribute and holds one tree
			// each. The leaves are evaluated where the call's inputs are
			// evaluated instead — see evaluateCall — so nothing is refused and
			// nothing is parsed twice.
			continue
		}
		e, parseDiags := expressions.Parse(src, attr.Origin)
		w.ds.Extend(parseDiags)
		if parseDiags.HasErrors() {
			continue
		}
		out[name] = e
	}
	return out
}

// callNode is one sibling module call, for the ordering graph.
type callNode struct{ name string }

func (n callNode) ID() string { return n.name }

// orderCalls returns one level's module calls in an order where every call a
// sibling's attributes read has already been expanded.
//
// PLAN.md §11's own example needs this: an `application` call takes
// ${database.connection_string} and sorts BEFORE `database`, so name order would
// resolve its attribute against a call that does not exist yet.
//
// graph.Layers sorts each layer by ID, so the result is identical on every run
// and degenerates to name order when no call references another — invariant 6
// holds with no sort of our own.
func (w *walker) orderCalls(
	lv level, exprs map[string]map[string]*value.Expr, module []string,
) []*config.ResourceDecl {
	byName := map[string]*config.ResourceDecl{}
	g := graph.New[callNode]()
	// Every node is added BEFORE any edge: graph.Edge panics on an endpoint it
	// has not seen, and its doc comment says so.
	for _, r := range lv.Resources {
		if !strings.HasPrefix(r.Type, TypePrefix) {
			continue
		}
		byName[r.Name] = r
		g.Add(callNode{name: r.Name})
	}

	for name, r := range byName {
		for _, byAttr := range exprs[name] {
			for _, ref := range byAttr.References() {
				target := ref.Target.Name
				if target == r.Name {
					continue // self-reference; the cycle below reports it
				}
				if _, sibling := byName[target]; sibling {
					g.Edge(target, r.Name) // target must be expanded first
				}
			}
		}
	}

	layers, err := g.Layers()
	if err != nil {
		cycle := g.Cycle()
		full := append(append([]string(nil), cycle...), cycle[0])
		w.ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "module calls reference each other in a cycle",
			Detail: strings.Join(full, " -> ") + "\n\nEach call's attributes read an output of " +
				"the next, in " + where(module) + ", so there is no order in which they can be " +
				"expanded.\n\nThis is not a `modules:` cycle: these calls terminate, they simply " +
				"have no valid order.",
			Action: "Remove one of the references, or move the shared value into a variable both " +
				"can read.",
			Origin: byName[cycle[0]].Origin,
		})
		return nil
	}

	out := make([]*config.ResourceDecl, 0, len(byName))
	for _, layer := range layers {
		for _, n := range layer {
			out = append(out, byName[n.name])
		}
	}
	return out
}
