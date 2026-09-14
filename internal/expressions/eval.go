package expressions

import (
	"strconv"
	"strings"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// Scope resolves the names an expression refers to.
//
// The compile-time scope resolves variables and reports resource attributes as
// unavailable, which is what turns a reference into an unknown carrying its
// expression. The apply-time scope in M3 resolves both. One evaluator serves
// both so the two phases cannot disagree.
type Scope interface {
	Variable(name string) (value.Value, bool)
	Attribute(ref value.Reference) (value.Value, bool)
}

// Evaluate reduces an expression as far as the scope allows.
//
// A reference the scope cannot resolve yields an unknown value carrying the
// expression, not an error: it will become knowable once its dependency
// exists. An undefined variable IS an error, because no later phase can supply
// it.
func Evaluate(e *value.Expr, scope Scope) (value.Value, diag.Diagnostics) {
	var ds diag.Diagnostics
	if e == nil {
		return value.Unknown(value.KindString, value.SourceComputed), ds
	}
	return evaluate(e, scope, &ds), ds
}

func evaluate(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	switch e.Op {
	case value.OpLiteral:
		return e.Literal.WithOrigin(e.Origin)

	case value.OpVarRef:
		name := e.Ref.VarName()
		v, ok := scope.Variable(name)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "undefined variable " + strconv.Quote(name),
				Detail:   "No variable of that name is in scope.",
				Action:   "Define it in variables.yml, or pass --var " + name + "=value.",
				Origin:   e.Origin,
			})
			return unknownFrom(e, value.KindString, false)
		}
		v = v.WithOrigin(e.Origin)
		if len(e.Ref.Path) == 0 {
			return v
		}
		// The ref rendered WITHOUT the path, so a diagnostic names the thing
		// being stepped into rather than echoing the whole failing expression.
		base := "var." + e.Ref.VarName()
		out, ok := applyPath(v, e.Ref.Path, base, e.Origin, ds)
		if !ok {
			return unknownFrom(e, value.KindString, false)
		}
		return out

	case value.OpResourceRef:
		v, ok := scope.Attribute(e.Ref)
		if !ok {
			// Not an error: the dependency simply does not exist yet.
			return unknownFrom(e, value.KindString, false)
		}
		v = v.WithOrigin(e.Origin)
		if len(e.Ref.Path) == 0 {
			return v
		}
		base := e.Ref.Target.String() + "." + e.Ref.Attribute
		out, ok := applyPath(v, e.Ref.Path, base, e.Origin, ds)
		if !ok {
			return unknownFrom(e, value.KindString, false)
		}
		return out

	case value.OpConcat:
		return evaluateConcat(e, scope, ds)

	case value.OpCall:
		return evaluateCall(e, scope, ds)

	default:
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "unsupported expression",
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, false)
	}
}

func evaluateConcat(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	var b strings.Builder
	sensitive := false
	known := true
	evaluated := make([]value.Value, len(e.Args))

	for i, arg := range e.Args {
		v := evaluate(arg, scope, ds)
		evaluated[i] = v
		sensitive = sensitive || v.Sensitive
		if !v.Known {
			known = false
			continue
		}
		s, ok := stringify(v)
		if !ok {
			// A composite (list or map) cannot be interpolated into a string.
			// Schema binding (Task 7) will eventually catch this before
			// evaluation runs, but it does not exist yet, so the evaluator
			// itself must refuse it here rather than silently emitting "" —
			// a fabricated empty string is a plan that lies.
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "cannot interpolate a " + v.Kind.String() + " value into a string",
				Detail:   "Interpolation accepts only string, integer, float and boolean values.",
				Origin:   arg.Origin,
			})
			known = false
			// Mark it unknown so the residual keeps the source arg rather than
			// folding in a value the diagnostic just rejected.
			evaluated[i] = value.Unknown(v.Kind, value.SourceComputed)
			continue
		}
		b.WriteString(s)
	}

	if !known {
		// Unknownness is contagious, and the result is classified before it is
		// known: a secret in any part makes the whole result sensitive. A
		// composite argument is treated the same way — its error is already
		// recorded above, and the caller must not receive a value fabricated
		// from the parts that did resolve.
		return unknownResidual(e, evaluated, value.KindString, sensitive)
	}
	return value.String(b.String(), value.SourceComputed).
		WithSensitive(sensitive).
		WithOrigin(e.Origin)
}

func evaluateCall(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	fn, _, ok := Lookup(e.Function)
	if !ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "undefined function " + strconv.Quote(e.Function),
			Detail:   "Available functions:\n  " + strings.Join(Names(), "\n  "),
			Action:   "Use one of the available functions. The set is fixed by the configuration language.",
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, false)
	}

	args := make([]value.Value, 0, len(e.Args))
	sensitive := false
	anyUnknown := false
	for _, arg := range e.Args {
		v := evaluate(arg, scope, ds)
		sensitive = sensitive || v.Sensitive
		anyUnknown = anyUnknown || !v.Known
		args = append(args, v)
	}

	// `default` is the one function that is meaningful with an unknown
	// argument — supplying a fallback is its entire purpose.
	if anyUnknown && e.Function != "default" {
		// A call is all-or-nothing as a RESULT — it cannot run with an
		// unknown argument — but its individual arguments can still be
		// individually resolved or not, exactly like OpConcat's. Deferring
		// the source expression instead of the residual left the same
		// ConfigHash blindness Task 3 closed for concatenation open here: a
		// resolved argument stayed an OpVarRef/OpResourceRef, so hashExpr
		// could only see its ref NAME, never the value two --var runs
		// actually differed by.
		return unknownResidual(e, args, value.KindString, sensitive)
	}

	out, err := fn(args)
	if err != nil {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "cannot evaluate " + strconv.Quote(e.Function) + ": " + err.Error(),
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, sensitive)
	}
	if !out.Known {
		// default's fallback can itself be unknown (e.g. another unresolved
		// resource attribute). fn returned that argument's own value as-is,
		// whose Expr points only to the fallback sub-expression — not to
		// this call. An unknown must carry the expression that will
		// reproduce it in full: re-evaluating just the fallback would skip
		// the primary-vs-fallback logic entirely once the primary resolves.
		//
		// This is reached only when defaultFunc chose the fallback AND the
		// fallback is itself unknown — which happens whether the primary was
		// unknown outright, or known but blank (defaultFunc treats "" the
		// same as unknown). Either way the primary's resolved contribution,
		// when it has one, is always the same blank string: folding it into
		// a literal would not let ConfigHash distinguish two configurations
		// that a non-blank primary couldn't already distinguish by taking
		// the fully-resolved path below instead. So this deferral, alone
		// among evaluateCall's, keeps the whole source expression rather
		// than a residual.
		return unknownFrom(e, out.Kind, out.Sensitive || sensitive)
	}
	return out.WithSensitive(out.Sensitive || sensitive).WithOrigin(e.Origin)
}

// unknownFrom builds an unknown carrying the expression that will produce it.
func unknownFrom(e *value.Expr, kind value.Kind, sensitive bool) value.Value {
	v := value.Unknown(kind, value.SourceComputed).
		WithSensitive(sensitive).
		WithOrigin(e.Origin)
	v.Expr = e
	return v
}

// residual builds the expression that is left to evaluate once everything
// resolvable has been resolved.
//
// A deferred expression is re-evaluated later — by the executor, once the
// resource it references exists. Attaching the SOURCE expression makes that
// later evaluation redo work that already succeeded, which has two costs. It
// forces the later evaluator to carry the earlier one's whole scope, including
// variables that have nothing to do with the resource being waited on. And it
// makes the deferred expression silent about the values it was built from, so
// ConfigHash — which folds an unresolved expression by op, function, ref name
// and arity — cannot distinguish two configurations that differ only in one of
// them.
//
// OpConcat and OpCall are the two ops whose ARGUMENTS can be individually
// resolved or not, so they are the two that fold. A call's RESULT is still
// all-or-nothing — it cannot run with an unknown argument, so there is never
// a partial value to keep — but folding here is not about computing a partial
// result; it is about recording which of the call's inputs were already
// known, the same as OpConcat's partial string. A bare reference either
// resolved or did not, so it has nothing to fold. Every other deferral
// therefore keeps its source expression, which is already correct because
// nothing in it resolved.
//
// The rule for each argument position is: a residual's argument IS the
// sub-expression's own residual, whatever depth it came from. A resolved
// argument folds to a literal. An unresolved one keeps evaluated[i].Expr —
// not the raw source arg — because the parser lets OpCall nest inside
// OpConcat and inside another OpCall's arguments, so an unresolved argument
// may itself be a partially-resolved call whose own residual already folded
// what it could. Reusing the source arg there would silently discard that
// inner fold and put the unfolded source back in, the same blindness this
// function exists to close, one level out. For a genuinely unresolved leaf
// reference, evaluate() sets Expr to the source node itself
// (unknownFrom(e, ...) does exactly that), so evaluated[i].Expr equals arg in
// that case — this is a strict generalisation, not a special case for calls.
//
// evaluated[i] is the result of evaluating e.Args[i]; the two slices are
// parallel. The source expression is never modified: it belongs to the caller's
// AST, is shared by every value that references it, and is what a diagnostic
// renders.
func residual(e *value.Expr, evaluated []value.Value) *value.Expr {
	if (e.Op != value.OpConcat && e.Op != value.OpCall) || len(evaluated) != len(e.Args) {
		return e
	}

	out := &value.Expr{Op: e.Op, Function: e.Function, Ref: e.Ref, Origin: e.Origin}
	out.Args = make([]*value.Expr, len(e.Args))
	for i, arg := range e.Args {
		v := evaluated[i]
		if !v.Known {
			// Still unknown: keep whatever residual the sub-evaluation itself
			// produced, so a partial fold inside a nested call is not thrown
			// away. Falls back to the source arg only when the sub-value
			// carries no Expr of its own.
			if v.Expr != nil {
				out.Args[i] = v.Expr
			} else {
				out.Args[i] = arg
			}
			continue
		}
		// Resolved: fold the VALUE in, sensitivity and all. A folded literal
		// that lost its Sensitive flag would be a way to launder a secret into
		// a plan artifact.
		out.Args[i] = &value.Expr{Op: value.OpLiteral, Literal: v, Origin: arg.Origin}
	}
	return out
}

// unknownResidual defers the residual of a partially-resolved expression.
func unknownResidual(e *value.Expr, evaluated []value.Value, kind value.Kind, sensitive bool) value.Value {
	return unknownFrom(residual(e, evaluated), kind, sensitive)
}

// stringify renders a known scalar value for concatenation. It reports false
// for a composite (list or map), which has no string form.
func stringify(v value.Value) (string, bool) {
	switch v.Kind {
	case value.KindString:
		return v.AsString()
	case value.KindInt:
		n, ok := v.AsInt()
		return strconv.FormatInt(n, 10), ok
	case value.KindBool:
		b, ok := v.AsBool()
		return strconv.FormatBool(b), ok
	case value.KindFloat:
		f, ok := v.Raw.(float64)
		return strconv.FormatFloat(f, 'g', -1, 64), ok
	default:
		return "", false
	}
}
