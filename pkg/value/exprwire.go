package value

import (
	"fmt"

	"github.com/infrena/infrena/pkg/address"
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

// stepKindWireNames is the frozen on-disk spelling of every StepKind, for the
// same reason exprOpWireNames exists: StepKind is an iota, and a bare number
// on disk would silently reinterpret every path ever written the moment a
// third kind were inserted above it.
var stepKindWireNames = map[StepKind]string{
	StepKey:   "key",
	StepIndex: "index",
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
	Module    []string   `json:"module,omitempty"`
	Name      string     `json:"name"`
	Attribute string     `json:"attribute,omitempty"`
	Path      []wireStep `json:"path,omitempty"`
}

// wireStep is the on-disk form of a Step. Kind is spelled out ("key"/"index")
// rather than carrying StepKind's numeric value, matching how wireExpr spells
// out Op — see exprOpWireNames and stepKindWireNames.
type wireStep struct {
	Kind  string `json:"kind"`
	Key   string `json:"key,omitempty"`
	Index int    `json:"index,omitempty"`
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

func stepKindToWireName(k StepKind) (string, error) {
	name, ok := stepKindWireNames[k]
	if !ok {
		return "", fmt.Errorf("cannot encode step kind %d", k)
	}
	return name, nil
}

func stepKindFromWireName(s string) (StepKind, error) {
	for k, name := range stepKindWireNames {
		if name == s {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown step kind %q", s)
}

// toWireSteps converts a path to its on-disk form.
func toWireSteps(path []Step) ([]wireStep, error) {
	if path == nil {
		return nil, nil
	}
	out := make([]wireStep, len(path))
	for i, s := range path {
		kind, err := stepKindToWireName(s.Kind)
		if err != nil {
			return nil, err
		}
		out[i] = wireStep{Kind: kind, Key: s.Key, Index: s.Index}
	}
	return out, nil
}

// fromWireSteps converts a path back from its on-disk form.
func fromWireSteps(path []wireStep) ([]Step, error) {
	if path == nil {
		return nil, nil
	}
	out := make([]Step, len(path))
	for i, w := range path {
		kind, err := stepKindFromWireName(w.Kind)
		if err != nil {
			return nil, err
		}
		out[i] = Step{Kind: kind, Key: w.Key, Index: w.Index}
	}
	return out, nil
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
		path, err := toWireSteps(e.Ref.Path)
		if err != nil {
			return nil, err
		}
		w.Ref = &wireReference{
			Module:    e.Ref.Target.Module,
			Name:      e.Ref.Target.Name,
			Attribute: e.Ref.Attribute,
			Path:      path,
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
		path, err := fromWireSteps(w.Ref.Path)
		if err != nil {
			return nil, err
		}
		e.Ref = Reference{
			Target:    address.Address{Module: w.Ref.Module, Name: w.Ref.Name},
			Attribute: w.Ref.Attribute,
			Path:      path,
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
