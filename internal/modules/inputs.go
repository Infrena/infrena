package modules

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

// Scope is the name environment inside one module instantiation: the variables
// visible to it, and what each bare name in ${name.attr} binds to.
//
// Every resource instantiated at one level shares one Scope, by pointer. The
// bindings are a property of the level, not of each resource.
//
// It satisfies expressions.Scope, so it is also what attribute evaluation runs
// against. One scope type serves both, which is what stops expansion and
// evaluation disagreeing about what a name means.
type Scope struct {
	// Module is the instantiation path to this level, outermost first, empty
	// at the root.
	Module []string
	// Vars is what ${var.name} resolves to here: the module's own inputs plus the
	// three process variables, or the project's whole variable scope at the
	// root.
	Vars variables.Scope
	// Secrets resolves ${secret.NAME}, and is nil when the caller supplies no
	// secrets — which reports "not set" rather than an empty string.
	//
	// Injected rather than read here, so that compilation stays a pure
	// function of what it is given: the CLI passes the environment, a test
	// passes a map.
	//
	// Unlike Vars and dirVars it crosses the module boundary. A module's
	// inputs stop at its edge because they are the caller's configuration. A
	// secret is not configuration but ambient credential material belonging
	// to the run, and a module that needs one would otherwise have to take it
	// as an input — sending the secret's value through a module call, where
	// it is far easier to log or record by accident.
	Secrets func(name string) (value.Value, bool)
	// SecretsErr reports that the secret source failed — an unopenable vault,
	// say — as opposed to one name being absent. See
	// expressions.SecretSourceError for why the two must not be conflated.
	SecretsErr func() error
	// Templates resolves ${template.NAME} and ${file.NAME}. dir is the
	// resources directory the referencing resource lives in, so a scoped
	// templates/ can win over the project-wide one.
	Templates func(dir, name string) (content string, origin value.Origin, ok bool)
	// dir is which resources directory this scope is bound to, set by In. It is
	// what makes a scoped template reachable at all: without it every lookup
	// would ask the project-wide question.
	dir string
	// dirVars holds the per-directory variable scopes from
	// resources/<dir>/vars/**, keyed by config.ResourceDecl.Dir. Set on the
	// root scope only: a module sees its own inputs and the process variables
	// and nothing else, so a directory's variables stop at the module
	// boundary exactly as the project's do.
	//
	// Read through In, never directly. Nil for a project with no scoped
	// variables.
	dirVars map[string]variables.Scope
	// names is what a bare name in ${name.attr} binds to at this level:
	// either a plain resource or a module call, each addressed once expansion
	// is done. Filled while expanding, by bind, and read through Lookup,
	// Names and Qualify.
	//
	// Shared by every scope In returns for this level; see In.
	names map[string]Binding
	// skipped records names declared at this level but excluded from this
	// environment, against the origin of the key that excluded them.
	//
	// Separate from names, deliberately: a skipped name must not resolve to an
	// address, or a reference to it would produce an edge to a resource that
	// is not being created. But it must not simply be absent either, or a
	// reference reports "no such resource" and sends a reader hunting for a
	// typo in a name that is right there in the file. This table is what lets
	// the diagnostic say "skipped" instead.
	//
	// Shared by In for the same reason names is.
	skipped map[string]value.Origin
}

// Skipped reports whether name was excluded from this environment at this
// level, and where. Consulted before reporting an unbound name.
func (s *Scope) Skipped(name string) (value.Origin, bool) {
	o, ok := s.skipped[name]
	return o, ok
}

// markSkipped records a name as excluded. See the skipped field.
func (s *Scope) markSkipped(name string, origin value.Origin) {
	if s.skipped == nil {
		s.skipped = map[string]value.Origin{}
	}
	s.skipped[name] = origin
}

// In narrows s to the resources directory dir, which is what a resource
// declared in resources/<dir>/ evaluates its attributes against: the same
// level, with that directory's own variables. dir is config.ResourceDecl.Dir;
// an empty one, or one with no vars/ of its own, gets s unchanged.
//
// The returned scope shares s's binding table rather than copying it, which is
// the whole reason this is a narrowing rather than a separate scope per
// directory. Bindings are level-scoped, not directory-scoped: a resource in
// resources/app/ referring to one in resources/db/ is an ordinary sibling
// reference, because a resource's address does not depend on which directory
// declared it. Give each directory its own table and that reference stops
// resolving, reported as "no such resource".
//
// Sharing a map only works if the map exists, which is why both places that
// build a level's Scope allocate names eagerly. A narrowing taken before the
// first bind — evaluateCall does exactly that, at a level whose resources are
// all module calls — would otherwise capture nil, and every later binding would
// land on the level's map where the narrowed copy could not see it.
func (s *Scope) In(dir string) *Scope {
	// A directory with no vars/ of its own still narrows: vars are not the
	// only thing a directory can scope, since resources/<dir>/templates/ is
	// looked up by this same dir. Returning s early here would leave dir empty
	// and fall back to the project-wide templates.
	//
	// An empty dir is still s: there is nothing to narrow to.
	if dir == "" {
		return s
	}
	// The directory's own vars if it has any, and the caller's otherwise —
	// never the zero scope, which would wipe every variable for a directory
	// that has templates but no vars/ of its own.
	vars := s.Vars
	if scoped, ok := s.dirVars[dir]; ok {
		vars = scoped
	}
	return &Scope{
		Module: s.Module, Vars: vars, Secrets: s.Secrets, SecretsErr: s.SecretsErr,
		Templates: s.Templates, dir: dir,
		dirVars: s.dirVars, names: s.names, skipped: s.skipped,
	}
}

// Variable satisfies half of expressions.Scope.
func (s *Scope) Variable(name string) (value.Value, bool) { return s.Vars.Variable(name) }

// Template satisfies expressions.TemplateScope.
//
// A module gets the caller's templates but not the caller's directory: a
// module's own resources are not in the caller's resources directory, so a
// scoped template there is not theirs to read. The project-wide templates/ is
// shared, which is what makes a template usable from inside a module at all.
func (s *Scope) Template(name string) (string, value.Origin, bool) {
	if s.Templates == nil {
		return "", value.Origin{}, false
	}
	return s.Templates(s.dir, name)
}

// SecretSourceError satisfies expressions.SecretSourceError.
func (s *Scope) SecretSourceError() error {
	if s.SecretsErr == nil {
		return nil
	}
	return s.SecretsErr()
}

// Secret satisfies expressions.SecretScope. A nil Secrets reports every secret
// unset, which is the right answer for a caller that supplies none: the
// alternative is an empty string standing in for a credential.
func (s *Scope) Secret(name string) (value.Value, bool) {
	if s.Secrets == nil {
		return value.Value{}, false
	}
	return s.Secrets(name)
}

// Attribute satisfies the other half of expressions.Scope. At compile time no
// resource has been created, so every reference to one reports unavailable,
// which is what turns it into an unknown carrying its expression and what makes
// the dependency edge discoverable.
//
// Module outputs never reach here: Qualify folds them into the expression tree
// before it is evaluated, so this method has one answer rather than two.
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
	inner := &Scope{Module: module, Secrets: caller.Secrets, SecretsErr: caller.SecretsErr,
		Templates: caller.Templates, names: map[string]Binding{}, skipped: map[string]value.Origin{}}

	// The process variables cross every module boundary, copied as-is so they
	// keep the provenance the compiler stamped. A module that rendered
	// ${var.environment} as having come from its own inputs would claim an
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

	// lv.Inputs is already sorted by name, so diagnostics about several bad
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
				// Reported and also resolved to a poison value of the
				// declared kind, so one missing input is not reported again
				// at every use site. internal/variables stamps an unset
				// variable's declared kind the same way.
				inner.Vars.Override(d.Name, value.Unknown(s.Kind, value.SourceModule).
					WithScope(value.ScopeModuleDefault).
					WithOrigin(s.Origin))
				continue
			}
			// The module's own declared default is the only thing that fills
			// ScopeModuleDefault, and it is stamped here rather than in
			// variables.Schemas because a declaration is not a resolution:
			// only the stage that decides which level won may say so.
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
		// Stored exactly as evaluation produced it: expansion does not
		// re-stamp Scope. A ${var.count} from --var keeps ScopeCLIOverride,
		// because provenance is recorded where a value enters, not where it is
		// passed along.
		inner.Vars.Override(d.Name, coerced)
	}

	return inner
}

// evaluateCall resolves a module call's attributes in the caller's scope.
//
// Expansion does this itself rather than leaving it to ordinary attribute
// binding, because it cannot build the module's scope without these values and
// after expansion the call resource is gone.
//
// exprs is the call's attributes, already parsed once by parseCall: the
// ordering pass reads the references and this evaluates the trees, and parsing
// twice would report every syntax error twice. caller.Qualify resolves a bare
// name against the caller's own bindings first, so a sibling module call's
// output is reachable here — which is the entire reason orderCalls exists.
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
			// A composite input: parseCall holds one expression tree per
			// attribute and a composite has none of its own, so its leaves
			// are walked here instead. Skipping it would make a caller's
			// interpolated map vanish and let the module's own default win.
			if attr.Value.Kind == value.KindList || attr.Value.Kind == value.KindMap {
				out[name] = expressions.WalkLeaves(attr.Value, func(src string, origin value.Origin) value.Value {
					leaf, parseDiags := expressions.Parse(src, origin)
					w.ds.Extend(parseDiags)
					if leaf == nil {
						return value.Unknown(value.KindString, value.SourceModule).WithOrigin(origin)
					}
					qualified := caller.Qualify(leaf)
					if refuseWholeResourceInput(qualified, origin, w.ds) {
						return value.Unknown(value.KindString, value.SourceModule).WithOrigin(origin)
					}
					v, evalDiags := expressions.Evaluate(qualified, caller)
					w.ds.Extend(evalDiags)
					return v
				})
				continue
			}
			// Otherwise parseCall already reported the syntax error.
			continue
		}
		qualified := caller.Qualify(e)
		if refuseWholeResourceInput(qualified, attr.Origin, w.ds) {
			// Refused, not evaluated. Leaving the key unset matches the
			// syntax-error branch above: compilation halts after this stage
			// whenever a diagnostic is an error, so there is nothing for a
			// missing entry to stand in for.
			continue
		}
		v, evalDiags := expressions.Evaluate(qualified, caller)
		w.ds.Extend(evalDiags)
		out[name] = v
	}
	return out
}

// refuseWholeResourceInput reports and returns true when e carries a bare
// whole-resource reference such as `${net}`, whose attribute is empty until
// something projects it from a declaration.
//
// A resource attribute projects one from its own `References` declaration. A
// module input has no such declaration: it declares a type, not a relationship
// to a resource, so there is nothing to project against and refusal is the only
// honest outcome.
//
// It must be refused before expressions.Evaluate sees the reference, because
// Evaluate has no diagnostic for an unresolved reference — it returns an
// unknown carrying the expression as-is — so the reference would silently
// become the module's input, unset, forever.
func refuseWholeResourceInput(e *value.Expr, origin value.Origin, ds *diag.Diagnostics) bool {
	refused := false
	for _, ref := range e.References() {
		if ref.Attribute != "" {
			continue
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "${" + ref.Target.String() + "} passes a resource to a module input",
			Detail: "A module input declares a type, not a relationship, so there is nothing to " +
				"say which of " + strconv.Quote(ref.Target.String()) + "'s attributes is meant. " +
				"The provider declares that for a resource attribute; a module input has no provider.",
			Action: "Name the attribute you mean, as ${" + ref.Target.String() + ".<attribute>}.",
			Origin: origin,
		})
		refused = true
	}
	return refused
}

// sortedAttributeNames visits attributes in a stable order. Go's map iteration
// is randomised, and diagnostic order within one resource must not change
// between runs of the same configuration.
func sortedAttributeNames(attrs map[string]config.AttributeDecl) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// inputNames lists a module's declared inputs for a diagnostic. The decls are
// already sorted by name, and reading the slice rather than a map is what keeps
// the message identical between runs.
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
