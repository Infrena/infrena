package expressions

import (
	"strconv"
	"strings"

	"infra/internal/diag"
	"infra/pkg/value"
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
		v, ok := scope.Variable(e.Ref.Resource)
		if !ok {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "undefined variable " + strconv.Quote(e.Ref.Resource),
				Detail:   "No variable of that name is in scope.",
				Action:   "Define it in variables.yml, or pass --var " + e.Ref.Resource + "=value.",
				Origin:   e.Origin,
			})
			return unknownFrom(e, value.KindString, false)
		}
		return v.WithOrigin(e.Origin)

	case value.OpResourceRef:
		v, ok := scope.Attribute(e.Ref)
		if !ok {
			// Not an error: the dependency simply does not exist yet.
			return unknownFrom(e, value.KindString, false)
		}
		return v.WithOrigin(e.Origin)

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

	for _, arg := range e.Args {
		v := evaluate(arg, scope, ds)
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
		return unknownFrom(e, value.KindString, sensitive)
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
		return unknownFrom(e, value.KindString, sensitive)
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
