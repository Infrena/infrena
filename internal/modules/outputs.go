package modules

import (
	"sort"
	"strconv"
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
	// Kind says whether this name is a resource or a module call.
	Kind BindingKind
	// Address is the qualified resource, when Kind is BindsResource.
	Address address.Address
	// Addresses is everything the call produced, when Kind is BindsModule.
	// Nothing is addressed with the call's own name after expansion, so this is
	// how an edge naming it is resolved.
	Addresses []address.Address
	// Instances is every address a `for_each` resource produced, when Kind is
	// BindsResource and the resource declared one. Empty for a resource
	// declared once, which is what distinguishes the two at a reference site:
	// `${subnet.id}` is an attribute of one resource, and an error naming the
	// instances when there are several.
	Instances []address.Address
	// Outputs are the call's collected outputs keyed by name, when Kind is
	// BindsModule. A value here is routinely unknown.
	//
	// Empty when the call declared `for_each`: a keyed call has one set of
	// outputs per instance and no single answer to `${store.endpoint}`, which
	// is why Keys exists to make the reference an error naming the instances
	// rather than a silent pick.
	Outputs map[string]value.Value
	// Keys are the instances a `for_each` module call produced, in sorted
	// order, when Kind is BindsModule. Empty for a call made once, which is
	// what tells the two apart at a reference site, exactly as Instances does
	// for a resource.
	Keys []string
	// KeyedOutputs are one instance's outputs, by key, for a `for_each` call.
	KeyedOutputs map[string]map[string]value.Value
	// KeyedAddresses are one instance's resources, by key. A reference naming
	// an instance depends on that instance alone; depending on everything the
	// call produced would make `${store["orders"].endpoint}` wait for every
	// other instance too, which is slower and can invent a cycle the
	// configuration does not contain.
	KeyedAddresses map[string][]address.Address
}

// Lookup reports what a bare name binds to at this level.
func (s *Scope) Lookup(name string) (Binding, bool) {
	b, ok := s.names[name]
	return b, ok
}

// Names lists every bound name, sorted, for diagnostics that say what is in
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
// this level. It runs between expressions.Parse and expressions.Evaluate.
//
// A name bound to a resource has its reference target replaced by the
// resource's qualified address. Without this, `${db.id}` written inside two
// different modules parses to the same reference and resolves to whichever `db`
// the engine looked at first.
//
// A name bound to a module call has its whole node replaced by an OpLiteral
// holding the output's value. Folding here rather than resolving at evaluation
// time is what puts the dependency edge on the real resource inside the module,
// which has to exist before the value becomes knowable. Resolving it in
// Scope.Attribute instead cannot work: the evaluator passes Attribute the
// parsed, bare reference, so the deferred expression would keep a target naming
// nothing.
//
// It emits no diagnostics. A reference it cannot resolve — a name bound to
// nothing, or a module output that does not exist — is left exactly as it
// stands for the general attribute-existence check to report, which Qualify
// supplies with candidates through Names and OutputNames.
//
// A new tree is returned and e is never modified: the source AST is shared by
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
		// Bound to nothing. Left as it stands, to be reported later against
		// Names as the candidate list, so the wording lives in one place.
		return e
	}

	switch b.Kind {
	case BindsResource:
		out := *e
		target := b.Address
		// A for_each resource is addressed by key, and the key travelled on
		// the reference's own target. Carried across here so the reference
		// names one instance; a key naming no instance, or a missing key
		// where instances exist, is left to be reported later against the
		// candidate list.
		if len(b.Instances) > 0 && e.Ref.Target.Key != "" {
			for _, in := range b.Instances {
				if in.Key == e.Ref.Target.Key {
					target = in
					break
				}
			}
		}
		out.Ref = value.Reference{Target: target, Attribute: e.Ref.Attribute, Path: e.Ref.Path}
		return &out

	case BindsModule:
		outputs := b.Outputs
		if len(b.Keys) > 0 {
			// A keyed call. A reference naming no instance is left alone
			// for the same reason the resource arm leaves one alone: the
			// later check names the instances that exist, which beats
			// picking one or calling the name undeclared.
			if e.Ref.Target.Key == "" {
				return e
			}
			byKey, ok := b.KeyedOutputs[e.Ref.Target.Key]
			if !ok {
				return e
			}
			outputs = byKey
		}
		v, declared := outputs[e.Ref.Attribute]
		if !declared {
			// The module arm of attribute-existence. Left unresolved for
			// the same check that catches `${store.endpoint}` on a provider
			// resource with no `endpoint` — one mechanism, rather than two
			// messages for one mistake.
			return e
		}
		// One branch for known and unknown alike. Splicing v.Expr for an
		// unknown would lose the Kind, because evaluation re-derives an
		// unresolved reference as KindString, and the type check downstream
		// then reports a mismatch instead of reaching value.Coerce's unknown
		// branch. An OpLiteral loses nothing: evaluation returns e.Literal
		// verbatim, so Known, Kind, Sensitive, the value's own Expr and the
		// dependency edge all survive.
		//
		// Sensitivity rides along deliberately: dropping it would launder a
		// secret into a plan artifact.
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
// It is the module arm of one general attribute-existence check: a provider
// resource answers from its schema, a module instance answers from here, and
// the user reads the same sentence either way. A module-only diagnostic here
// instead would reimplement for modules what the compiler already does for
// everything.
func (s *Scope) OutputNames(name string) ([]string, bool) {
	b, ok := s.names[name]
	if !ok || b.Kind != BindsModule {
		return nil, false
	}
	if len(b.Keys) > 0 {
		// Every instance of one call exposes the same outputs, because they
		// come from one module file, so the first is the whole answer. That
		// keeps "which outputs exist" a question about the module rather than
		// about which instance you happened to name.
		return sortedValueKeys(b.KeyedOutputs[b.Keys[0]]), true
	}
	return sortedValueKeys(b.Outputs), true
}

// collectOutputs evaluates a module's `outputs:` block in the module's own
// scope. An output is an expression over the module's resources, so at plan
// time it is routinely unknown — not an error, and never an empty string.
func (w *walker) collectOutputs(lv level, scope *Scope) map[string]value.Value {
	out := make(map[string]value.Value, len(lv.Outputs))
	// lv.Outputs is already sorted by name, so several bad outputs report in
	// a stable order with no sort here.
	for _, o := range lv.Outputs {
		out[o.Name] = w.collectOutput(o, scope)
	}
	return out
}

func (w *walker) collectOutput(o config.OutputDecl, scope *Scope) value.Value {
	if !o.HasExpressions {
		// No bare-reference check here: internal/config refuses a bare
		// one-dot output at decode, where the line number is, and two
		// implementations of one heuristic would drift. A literal output is
		// legal: `outputs: {region: {value: us-east-1}}`.
		return o.Value.WithSource(value.SourceModule)
	}

	src, ok := o.Value.AsString()
	if !ok {
		// A composite output is walked leaf by leaf, through the same walk a
		// resource attribute uses, so the two cannot come to disagree about
		// the same YAML.
		return expressions.WalkLeaves(o.Value, func(leafSrc string, leafOrigin value.Origin) value.Value {
			return w.evaluateOutputExpression(leafSrc, leafOrigin, scope)
		}).WithSource(value.SourceModule)
	}

	v := w.evaluateOutputExpression(src, o.Origin, scope)

	// SourceModule says what kind of thing this is at the call site: a value
	// that came out of a module.
	//
	// Redundant today, because the only reader of an Outputs entry is
	// Qualify's BindsModule fold, which re-stamps SourceModule anyway. Kept
	// because Outputs is a public field of Binding, and a future direct reader
	// must not see the source of whatever expression the output happened to
	// evaluate standing in for a module boundary.
	return v.WithSource(value.SourceModule)
}

// evaluateOutputExpression resolves one interpolated string in an output's
// value.
//
// Extracted so a leaf inside a composite output goes through the identical path
// a bare output does. Two copies would mean an output inside a map eventually
// resolving differently from the same expression outside one.
func (w *walker) evaluateOutputExpression(src string, origin value.Origin, scope *Scope) value.Value {
	e, parseDiags := expressions.Parse(src, origin)
	w.ds.Extend(parseDiags)
	if parseDiags.HasErrors() {
		return value.Unknown(value.KindString, value.SourceModule).WithOrigin(origin)
	}
	qualified := scope.Qualify(e)
	if refuseWholeResourceOutput(qualified, origin, w.ds) {
		// Refused, not evaluated, as in evaluateCall. Leaving the value an
		// unknown with no reference matches the parse-error branch above:
		// once reported, there is nothing for a bad output to stand in for.
		return value.Unknown(value.KindString, value.SourceModule).WithOrigin(origin)
	}
	v, evalDiags := expressions.Evaluate(qualified, scope)
	w.ds.Extend(evalDiags)
	return v
}

// refuseWholeResourceOutput reports and returns true when e carries a
// whole-resource reference — a bare `${net}` published as a module output,
// whose attribute is empty because nothing projects one here.
//
// The output-side twin of refuseWholeResourceInput, for the same reason: a
// module output publishes a value, not a schema'd attribute, so there is no
// consuming declaration to project against — not here, and not at the call site
// that eventually reads the output, because by then the module has been
// expanded and the resource behind ${net} no longer has a name.
//
// It must be refused before Qualify's BindsModule fold splices the reference
// into an OpLiteral: value.Expr.References does not descend into
// OpLiteral.Literal.Expr, so a reference that reached the fold would be
// invisible to every downstream walk, compiling clean and leaving the attribute
// that consumed it unset forever.
func refuseWholeResourceOutput(e *value.Expr, origin value.Origin, ds *diag.Diagnostics) bool {
	refused := false
	for _, ref := range e.References() {
		if ref.Attribute != "" {
			continue
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref.Target.String() + "} publishes a resource as a module output",
			Detail: "A module output publishes a value, not a resource, so there is nothing to " +
				"say which of " + strconv.Quote(ref.Target.String()) + "'s attributes is meant. " +
				"Whatever eventually reads this output has no provider declaration to project " +
				"against, because by then the module has been expanded and " +
				strconv.Quote(ref.Target.String()) + " no longer has a name.",
			Action: "Name the attribute you mean, as ${" + ref.Target.String() + ".<attribute>}.",
			Origin: origin,
		})
		refused = true
	}
	return refused
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

// parseCall parses each of a module call's attributes once. The ordering pass
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
			// A composite call input has no single expression tree, and this
			// map holds one tree per attribute. Its leaves are evaluated in
			// evaluateCall instead, so nothing is refused and nothing is
			// parsed twice.
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
// sibling's attributes read has already been expanded. An `application` call
// taking ${database.connection_string} sorts before `database` by name, so name
// order would resolve its attribute against a call that does not exist yet.
//
// graph.Layers sorts each layer by ID, so the result is identical on every run
// and degenerates to name order when no call references another.
func (w *walker) orderCalls(
	lv level, exprs map[string]map[string]*value.Expr, module []string,
) []*config.ResourceDecl {
	byName := map[string]*config.ResourceDecl{}
	g := graph.New[callNode]()
	// Every node is added before any edge: graph.Edge panics on an endpoint
	// it has not seen.
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
