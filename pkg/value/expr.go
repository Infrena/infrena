package value

import (
	"strconv"
	"strings"
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

// Reference names another resource's attribute.
type Reference struct {
	Resource  string
	Attribute string
}

// String renders a reference in the form used in configuration.
func (r Reference) String() string {
	if r.Attribute == "" {
		return r.Resource
	}
	return r.Resource + "." + r.Attribute
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
	case OpVarRef, OpResourceRef:
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
