package value

import "strings"

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

// String renders the expression back to something close to its source form.
// It is for diagnostics, not round-tripping.
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	switch e.Op {
	case OpLiteral:
		if s, ok := e.Literal.AsString(); ok {
			return s
		}
		return "<literal>"
	case OpVarRef, OpResourceRef:
		return "${" + e.Ref.String() + "}"
	case OpCall:
		parts := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			parts = append(parts, a.String())
		}
		return "${" + e.Function + "(" + strings.Join(parts, ", ") + ")}"
	case OpConcat:
		var b strings.Builder
		for _, a := range e.Args {
			b.WriteString(a.String())
		}
		return b.String()
	default:
		return "<expr>"
	}
}
