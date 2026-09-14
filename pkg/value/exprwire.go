package value

import (
	"fmt"

	"github.com/infrata/infrata/pkg/address"
)

// The on-disk form of an expression.
//
// AN UNKNOWN VALUE CARRIES THE EXPRESSION THAT WILL PRODUCE IT, and until this existed
// that expression did not survive being written down. The consequence was specific and
// silent: a saved plan containing `network: ${net.id}` decoded to an unknown with no
// expression, which is indistinguishable from a computed attribute, so the executor
// dropped it — the apply reported success and left the attribute unset.
//
// A MIRROR STRUCT rather than tags on Expr, following wireValue: the on-disk language is
// a versioned contract with its own migration path, and keeping it separate from the Go
// field names is what stops a rename in one being a format break in the other.

// exprOpWireNames is the frozen on-disk spelling of every ExprOp.
//
// BY NAME, never by number, for the reason OpKind and Kind are: ExprOp is an iota, so a
// constant inserted into that list would silently reinterpret every expression ever
// written. Entries here change only alongside a format version bump.
var exprOpWireNames = map[ExprOp]string{
	OpLiteral:     "literal",
	OpVarRef:      "var_ref",
	OpResourceRef: "resource_ref",
	OpConcat:      "concat",
	OpCall:        "call",
}

type wireExpr struct {
	Op       string         `json:"op"`
	Literal  *Value         `json:"literal,omitempty"`
	Ref      *wireReference `json:"ref,omitempty"`
	Args     []*wireExpr    `json:"args,omitempty"`
	Function string         `json:"function,omitempty"`
	Origin   *Origin        `json:"origin,omitempty"`
}

type wireReference struct {
	Module    []string `json:"module,omitempty"`
	Name      string   `json:"name"`
	Attribute string   `json:"attribute,omitempty"`
}

func exprOpToWireName(op ExprOp) (string, error) {
	name, ok := exprOpWireNames[op]
	if !ok {
		return "", fmt.Errorf("cannot encode expression operation %d", op)
	}
	return name, nil
}

func exprOpFromWireName(s string) (ExprOp, error) {
	for op, name := range exprOpWireNames {
		if name == s {
			return op, nil
		}
	}
	return 0, fmt.Errorf("unknown expression operation %q", s)
}

// toWireExpr converts an expression to its on-disk form.
func toWireExpr(e *Expr) (*wireExpr, error) {
	if e == nil {
		return nil, nil
	}
	op, err := exprOpToWireName(e.Op)
	if err != nil {
		return nil, err
	}
	w := &wireExpr{Op: op, Function: e.Function}

	// Only the field the operation actually uses is written. An OpConcat carrying a
	// zero Literal would otherwise serialise one, and a zero Value has KindInvalid,
	// which marshalling refuses outright.
	switch e.Op {
	case OpLiteral:
		lit := e.Literal
		w.Literal = &lit
	case OpVarRef, OpResourceRef:
		w.Ref = &wireReference{
			Module:    e.Ref.Target.Module,
			Name:      e.Ref.Target.Name,
			Attribute: e.Ref.Attribute,
		}
	}

	for _, arg := range e.Args {
		converted, err := toWireExpr(arg)
		if err != nil {
			return nil, err
		}
		w.Args = append(w.Args, converted)
	}

	if e.Origin.File != "" || e.Origin.Line != 0 || e.Origin.Column != 0 || e.Origin.Module != nil {
		o := e.Origin
		w.Origin = &o
	}
	return w, nil
}

// fromWireExpr converts an expression back from its on-disk form.
func fromWireExpr(w *wireExpr) (*Expr, error) {
	if w == nil {
		return nil, nil
	}
	op, err := exprOpFromWireName(w.Op)
	if err != nil {
		return nil, err
	}
	e := &Expr{Op: op, Function: w.Function}
	if w.Literal != nil {
		e.Literal = *w.Literal
	}
	if w.Ref != nil {
		e.Ref = Reference{
			Target:    address.Address{Module: w.Ref.Module, Name: w.Ref.Name},
			Attribute: w.Ref.Attribute,
		}
	}
	for _, arg := range w.Args {
		converted, err := fromWireExpr(arg)
		if err != nil {
			return nil, err
		}
		e.Args = append(e.Args, converted)
	}
	if w.Origin != nil {
		e.Origin = *w.Origin
	}
	return e, nil
}
