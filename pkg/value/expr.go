package value

import (
	"strconv"
	"strings"

	"github.com/infrena/infrena/pkg/address"
)

// ExprOp is the kind of an expression node.
type ExprOp uint8

const (
	// OpLiteral is a constant embedded in an expression.
	OpLiteral ExprOp = iota
	// OpVarRef references a variable. Variables arrive in M4; the node exists
	// now so the AST does not change shape then.
	OpVarRef
	// OpResourceRef references another resource's attribute.
	OpResourceRef
	// OpConcat joins its arguments into one string.
	OpConcat
	// OpCall applies one of the built-in pure functions.
	OpCall
)

// StepKind distinguishes the two ways a path moves into a composite.
type StepKind uint8

const (
	// StepKey reads a key from a map.
	StepKey StepKind = iota
	// StepIndex reads an entry from a list.
	StepIndex
)

// Step is one move along a path into a composite value.
//
// The two kinds are kept apart at PARSE time rather than resolved against the
// value's kind at evaluation time, because a map may have a numeric-looking
// key: `${var.ports.0}` would otherwise mean key "0" or index 0 depending on
// what `ports` turned out to be, which makes a reference's meaning depend on
// the type of the thing it names. `.0` is always a key and `[0]` is always an
// index, decided with no value in hand.
type Step struct {
	Kind  StepKind
	Key   string // StepKey only
	Index int    // StepIndex only
}

// String renders a step as it was written, including its leading delimiter, so
// that joining a path's steps reproduces the source.
func (s Step) String() string {
	if s.Kind == StepIndex {
		return "[" + strconv.Itoa(s.Index) + "]"
	}
	return "." + s.Key
}

// Reference names another resource's attribute — or, under OpVarRef, a
// variable by name.
//
// Target is an address.Address rather than a bare name because a reference and
// an address name the same thing and must agree about what that thing is. With
// a bare name, ${db.id} written inside a module and ${db.id} written at the
// root are indistinguishable, so once stage 5 re-roots a module's resources to
// module.net.db a lookup keyed on "db" misses the module's — or, when two
// modules each declare a `db`, matches the WRONG one and returns its
// attributes as the answer. That is a silently wrong plan, not an error.
//
// A reference parsed out of configuration is SCOPE-RELATIVE: its module path is
// empty, meaning "in whichever scope this expression was written". At the root
// that is already absolute, which is why LocalRef is the constructor for both.
//
// Stage 6 fills the path, via the scope stage 5 recorded for each instantiated
// resource: bindAttribute parses, qualifies, then evaluates. Carrying the field
// is only half of it — a module path that nothing ever fills leaves every
// reference scope-relative and two modules' same-named resources resolving to
// one, with a field present that makes it look handled.
//
// Under OpVarRef only Target.Name is meaningful. A variable has no module path:
// it is resolved in the scope the expression was written in, by stage 4, before
// any of this.
type Reference struct {
	Target    address.Address
	Attribute string
	// Path is the steps taken into Attribute's value — or, under OpVarRef,
	// into the variable's own value, where Attribute is empty.
	//
	// It is part of String(), and therefore part of References()' dedup key.
	// Without that, ${vpc.tags.Name} and ${vpc.tags.Env} render identically,
	// the second is dropped as a duplicate, and its expression resolves to the
	// first one's value.
	Path []Step
}

// LocalRef builds a reference as written, with no module path — which is what
// the parser produces and what a root-scoped reference is.
func LocalRef(resource, attribute string) Reference {
	return Reference{Target: address.Address{Name: resource}, Attribute: attribute}
}

// VarRef builds the reference an OpVarRef node carries.
func VarRef(name string) Reference {
	return Reference{Target: address.Address{Name: name}}
}

// VarName returns the variable named by a reference under OpVarRef.
func (r Reference) VarName() string { return r.Target.Name }

// InModule returns the reference as seen from inside a parent module
// instantiation: the primitive modules.Scope.Qualify applies, outermost last,
// to turn a scope-relative reference into an absolute one at stage 6.
//
// If Task 8's Qualify sets Target.Module from the scope's path directly rather
// than chaining this, DELETE this method in that task. An unused constructor on
// a type whose whole point is that its module path gets filled is the same
// misleading-by-presence problem the field itself would be.
//
// It delegates to address.InModule rather than appending to Target.Module,
// because that function copies the slice first: appending in place would let
// one instantiation's re-rooting alias into another's.
func (r Reference) InModule(module string) Reference {
	return Reference{Target: r.Target.InModule(module), Attribute: r.Attribute}
}

// String renders a reference in the form used in configuration.
func (r Reference) String() string {
	var b strings.Builder
	b.WriteString(r.Target.String())
	if r.Attribute != "" {
		b.WriteString(".")
		b.WriteString(r.Attribute)
	}
	for _, s := range r.Path {
		b.WriteString(s.String())
	}
	return b.String()
}

// Expr is a parsed expression.
//
// It lives in this package rather than its own because Value and Expr are
// mutually recursive: an unknown Value carries the Expr that will produce it,
// and an Expr carries literal Values.
type Expr struct {
	Op       ExprOp
	Literal  Value     // OpLiteral only
	Ref      Reference // OpVarRef and OpResourceRef only
	Args     []*Expr   // OpConcat and OpCall only
	Function string    // OpCall only
	Origin   Origin
}

// References returns every resource reference in the expression, in source
// order and deduplicated. Each one becomes a dependency edge, and a reference
// used twice is still one edge.
func (e *Expr) References() []Reference {
	if e == nil {
		return nil
	}
	var out []Reference
	seen := map[string]bool{}

	var walk func(*Expr)
	walk = func(n *Expr) {
		if n == nil {
			return
		}
		if n.Op == OpResourceRef && !seen[n.Ref.String()] {
			seen[n.Ref.String()] = true
			out = append(out, n.Ref)
		}
		for _, a := range n.Args {
			walk(a)
		}
	}
	walk(e)
	return out
}

// String renders the expression back toward its configuration source form, for
// diagnostics.
//
// Rendering splits in two because an expression reads differently depending on
// where it sits. At the top level a reference is written ${db.endpoint}, but as
// an argument inside a call it is written db.endpoint — bare. Rendering
// arguments through String() would wrap each of them in delimiters of their own
// and produce ${lower(${db.endpoint})}, which is not syntax a user could paste
// back into their configuration.
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	switch e.Op {
	case OpLiteral:
		// residual (internal/expressions) can fold a resolved SENSITIVE value
		// into a literal inside an otherwise-deferred expression. Rendering it
		// verbatim here would be a second redaction path alongside
		// value.Format's, and a second path is exactly how a secret leaked in
		// M2 — so this one defers to the same marker rather than growing its
		// own.
		if e.Literal.Sensitive {
			return Redacted
		}
		if s, ok := e.Literal.AsString(); ok {
			return s
		}
		return "<literal>"
	case OpConcat:
		var b strings.Builder
		for _, a := range e.Args {
			b.WriteString(a.String())
		}
		return b.String()
	default:
		return "${" + e.inner() + "}"
	}
}

// inner renders an expression as it appears inside ${...}, without the
// delimiters. A literal is re-quoted here because that is how it was written:
// replace(engine, "sql", "SQL") takes quoted arguments, and dropping the quotes
// would render something that no longer parses.
func (e *Expr) inner() string {
	if e == nil {
		return ""
	}
	switch e.Op {
	case OpLiteral:
		// Same redaction as String() above — a sensitive literal folded into a
		// call argument must not render its value just because it is nested
		// one level deeper.
		if e.Literal.Sensitive {
			return Redacted
		}
		if s, ok := e.Literal.AsString(); ok {
			return strconv.Quote(s)
		}
		return "<literal>"
	case OpVarRef:
		return "var." + e.Ref.String()
	case OpResourceRef:
		return e.Ref.String()
	case OpCall:
		parts := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			parts = append(parts, a.inner())
		}
		return e.Function + "(" + strings.Join(parts, ", ") + ")"
	case OpConcat:
		var b strings.Builder
		for _, a := range e.Args {
			b.WriteString(a.inner())
		}
		return b.String()
	default:
		return "<expr>"
	}
}
