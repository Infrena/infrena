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
// expression. The apply-time scope resolves both. One evaluator serves both so
// the two phases cannot disagree.
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

	case value.OpTemplateRef:
		return evaluateTemplate(e, scope, ds)

	case value.OpFileRef:
		return evaluateFile(e, scope, ds)

	case value.OpSecretRef:
		return evaluateSecret(e, scope, ds)

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
		// The ref rendered without the path, so a diagnostic names the thing
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
			// A composite cannot be interpolated into a string. Refused here
			// rather than silently emitting "": a fabricated empty string is a
			// plan that lies.
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
		// known: a secret in any part makes the whole result sensitive. The
		// caller must not receive a value fabricated from the parts that did
		// resolve.
		return unknownResidual(e, evaluated, value.KindString, sensitive)
	}
	return value.String(b.String(), value.SourceComputed).
		WithSensitive(sensitive).
		WithOrigin(e.Origin)
}

// Sensitivity in this file is a top-level union on purpose: evaluateConcat and
// evaluateCall accumulate `sensitive` from each argument's own Sensitive flag
// rather than from the recursive value.HasSensitive. Making it "consistent" would
// quietly break redaction, because:
//
//   - Concatenation produces a string, and a composite argument cannot be
//     interpolated into one at all. Every argument that contributes is a scalar,
//     where the top-level flag is the only flag there is.
//
//   - A call's result is classified by the function itself, in funcs.go, using
//     the recursive anySensitive. What this adds on top can never be narrower.
//
//   - merge() is the exception and the reason this must stay top-level: it
//     returns a map and classifies per leaf. Widening to HasSensitive would mark
//     the whole merged map sensitive whenever any leaf anywhere in any argument
//     was, redacting every key to protect one.
//
// evaluateCall applies a built-in, deferring the call when an argument is not yet
// known.
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
		// The residual, not the source expression. A call cannot run with an
		// unknown argument, but its arguments can individually be resolved or
		// not; deferring the source would leave a resolved argument as a bare
		// reference, so the config hash would see only its name and not the
		// value two runs actually differed by.
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
		// Reached only when default() chose a fallback that is itself unknown.
		// The whole source expression is kept rather than a residual, because an
		// unknown must carry the expression that reproduces it in full:
		// re-evaluating just the fallback would skip the primary-vs-fallback
		// choice once the primary resolves.
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

// residual builds the expression left to evaluate once everything resolvable has
// been resolved. evaluated[i] is the result of evaluating e.Args[i]; the two
// slices are parallel.
//
// A deferred expression is re-evaluated later, by the executor, once the resource
// it references exists. Keeping the source expression instead would force that
// later evaluation to carry the earlier one's whole scope, and would leave the
// deferred expression silent about the values it was built from — so the config
// hash, which folds an unresolved expression by op, function, ref name and arity,
// could not tell two configurations apart that differ only in one of them.
//
// OpConcat and OpCall are the two ops whose arguments can individually be
// resolved or not, so they are the two that fold. A bare reference either
// resolved or did not, so every other deferral keeps its source expression, which
// is already correct because nothing in it resolved.
//
// Per argument: a resolved one folds to a literal, and an unresolved one keeps
// evaluated[i].Expr rather than the raw source arg. Calls nest inside
// concatenations and inside other calls, so an unresolved argument may itself be
// a partially-resolved call whose own residual already folded what it could;
// reusing the source there would discard that inner fold. For a genuinely
// unresolved leaf reference the two are the same node, so this is a
// generalisation rather than a special case.
//
// The source expression is never modified: it belongs to the caller's AST, is
// shared by every value that references it, and is what a diagnostic renders.
func residual(e *value.Expr, evaluated []value.Value) *value.Expr {
	if (e.Op != value.OpConcat && e.Op != value.OpCall) || len(evaluated) != len(e.Args) {
		return e
	}

	out := &value.Expr{Op: e.Op, Function: e.Function, Ref: e.Ref, Origin: e.Origin}
	out.Args = make([]*value.Expr, len(e.Args))
	for i, arg := range e.Args {
		v := evaluated[i]
		if !v.Known {
			// Keep whatever residual the sub-evaluation produced, so a partial
			// fold inside a nested call is not thrown away. Falls back to the
			// source arg only when the sub-value carries no Expr of its own.
			if v.Expr != nil {
				out.Args[i] = v.Expr
			} else {
				out.Args[i] = arg
			}
			continue
		}
		// Fold the value in, sensitivity and all: a folded literal that lost its
		// Sensitive flag would launder a secret into a plan artifact.
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

// SecretScope supplies secrets by name. It is an optional interface on Scope:
// most scopes have no business holding secrets, and requiring the method would
// make every one of them declare that it does not.
//
// Deliberately separate from Variable rather than seeding secrets into the
// variable scope. A variable can be listed, defaulted, overridden per environment
// and printed in a diagnostic suggesting near-misses; a secret must do none of
// those, and sharing the namespace would mean every one of those features had to
// learn an exception.
type SecretScope interface {
	Secret(name string) (value.Value, bool)
}

// SecretSourceError is an optional companion to SecretScope, reporting that the
// source itself failed rather than that one name was absent.
//
// The two are indistinguishable from Secret's answer and completely different to
// act on: a vault that will not open reports every name as missing, so without
// this the user is told to set a variable when what actually happened is that
// their passphrase is wrong.
type SecretSourceError interface {
	SecretSourceError() error
}

// evaluateSecret resolves ${secret.NAME}.
//
// A missing secret is an error, never an empty string: something downstream
// authenticates with it, so an empty one does not fail here — it fails at a
// provider, as a permission error, at the far end of a plan somebody approved.
//
// The result is marked sensitive whatever the schema says. Schema-declared
// sensitivity describes the attribute; this describes the value, and a secret
// written into an attribute nobody marked is exactly the case where redaction
// would otherwise be missing.
func evaluateSecret(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	name := e.Ref.VarName()
	ss, ok := scope.(SecretScope)
	if !ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "secrets are not available here",
			Detail:   "${secret." + name + "} was written somewhere this engine does not supply secrets to.",
			Origin:   e.Origin,
		})
		return unknownFrom(e, value.KindString, true)
	}
	v, found := ss.Secret(name)
	if !found {
		// The source failing is a different fact from this name being absent,
		// and it is checked first because it explains every absence at once.
		if se, ok := scope.(SecretSourceError); ok {
			if err := se.SecretSourceError(); err != nil {
				ds.Add(diag.Diagnostic{
					Severity: diag.SeverityError,
					Summary:  "the secrets for this project could not be read",
					Detail:   err.Error() + "\n${secret." + name + "} needed them.",
					Origin:   e.Origin,
				})
				return unknownFrom(e, value.KindString, true)
			}
		}
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "secret " + strconv.Quote(name) + " is not set",
			Detail: "${secret." + name + "} is answered by the environment variable " + name +
				" first, then by this project's vault — secrets/<environment>.yml, then " +
				"secrets.yml. None of them supplied it, or what they supplied was empty. " +
				"Empty is refused rather than read as an empty string, because an empty " +
				"credential does not fail here — it fails at the provider, after a plan " +
				"somebody has already approved.",
			Action: "Set " + name + " in the environment this runs in — in CI that is a secret " +
				"the pipeline injects — or put it in a vault with `infrena vault edit secrets.yml`.",
			Origin: e.Origin,
		})
		return unknownFrom(e, value.KindString, true)
	}
	// Sensitive whatever the schema says, and origin from the use site so a
	// diagnostic points at the configuration rather than at the environment.
	return v.WithSensitive(true).WithOrigin(e.Origin)
}

// TemplateScope supplies the contents of a file under templates/. Optional, for
// the reason SecretScope is: most scopes have no templates to offer.
type TemplateScope interface {
	// Template returns a file's contents, and where it was found so that a
	// diagnostic about the file's content points at the file rather than at the
	// line of YAML that referenced it.
	Template(name string) (content string, origin value.Origin, ok bool)
}

// maxTemplateDepth bounds how far templates may nest. A template may reference
// another, which is useful and is also a loop waiting to happen; the limit is
// generous enough that no honest document reaches it and small enough that a
// cycle fails with a message rather than growing a string until the process
// dies.
const maxTemplateDepth = 8

// nested is a Scope wearing a depth counter, used only while expanding a
// template's contents. The counter lives on the scope rather than in evaluate's
// signature, so that templates do not appear in every signature in this package
// to be read by one caller.
type nested struct {
	Scope
	depth int
}

func (n nested) Template(name string) (string, value.Origin, bool) {
	ts, ok := n.Scope.(TemplateScope)
	if !ok {
		return "", value.Origin{}, false
	}
	return ts.Template(name)
}

func (n nested) Secret(name string) (value.Value, bool) {
	ss, ok := n.Scope.(SecretScope)
	if !ok {
		return value.Value{}, false
	}
	return ss.Secret(name)
}

func (n nested) SecretSourceError() error {
	se, ok := n.Scope.(SecretSourceError)
	if !ok {
		return nil
	}
	return se.SecretSourceError()
}

// evaluateTemplate reads a file under templates/ and evaluates its contents in
// the same grammar and the same scope the configuration uses.
//
// One language, deliberately, rather than handing the file to a general template
// engine. Evaluating the contents through evaluate means a reference inside a
// template is the same reference it would be in YAML, with the same deferral, the
// same dependency edge and the same sensitivity — so the single redaction path
// holds, and a policy naming ${bucket.arn} renders after the bucket has one
// rather than before.
func evaluateTemplate(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	name := e.Ref.VarName()
	content, origin, ok := lookupTemplate(e, scope, name, ds)
	if !ok {
		return unknownFrom(e, value.KindString, false)
	}

	depth := 0
	if n, isNested := scope.(nested); isNested {
		depth = n.depth
	}
	if depth >= maxTemplateDepth {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "templates nest more than " + strconv.Itoa(maxTemplateDepth) + " deep at " + strconv.Quote(name),
			Detail: "A template may reference another, so this is most likely a cycle — a file that " +
				"reaches itself, directly or through others.",
			Action: "Break the cycle.",
			Origin: e.Origin,
		})
		return unknownFrom(e, value.KindString, false)
	}

	// The `{{ }}` pass first, when the call gave it arguments. It sees only
	// those arguments — never the project's scope — which is what stops a
	// template engine writing a secret into the text as a literal and losing its
	// sensitivity on the way. `${...}` is left alone here and handled below, so a
	// reference inside a template keeps its deferral and its edge.
	sensitiveArgs := false
	if len(e.Args) > 0 {
		arg := evaluate(e.Args[0], scope, ds)
		data, known, sensitive, argErr := templateArguments(arg)
		sensitiveArgs = sensitive
		if argErr != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "the argument to ${template." + name + "} is not usable",
				Detail:   argErr.Error(),
				Action:   "Pass a map, as ${template." + name + "({team: \"platform\"})}.",
				Origin:   e.Origin,
			})
			return unknownFrom(e, value.KindString, sensitive)
		}
		if !known {
			// A template cannot represent "not yet", so the whole render is
			// deferred rather than rendered around the gap. The executor
			// evaluates this again once the argument resolves.
			return unknownFrom(e, value.KindString, sensitive)
		}
		rendered, renderErr := renderTemplate(name, content, data)
		if renderErr != nil {
			ds.Add(diag.Diagnostic{
				Severity: diag.SeverityError,
				Summary:  "template " + strconv.Quote(name) + " could not be rendered",
				Detail:   renderErr.Error(),
				Action:   "Correct the {{ }} in " + origin.File + ". Its functions are: " + strings.Join(TemplateFuncNames(), ", ") + ".",
				Origin:   e.Origin,
			})
			return unknownFrom(e, value.KindString, sensitive)
		}
		content = rendered
	}

	// Parsed with the template's origin, so a malformed reference inside the
	// file reports the file and line it is on rather than the configuration
	// line that referenced it.
	inner, parseDiags := Parse(content, origin)
	ds.Extend(parseDiags)
	if parseDiags.HasErrors() || inner == nil {
		return unknownFrom(e, value.KindString, false)
	}
	out := evaluate(inner, nested{Scope: scope, depth: depth + 1}, ds)
	// A sensitive argument taints the whole rendered document, and it has to be
	// the whole thing: the template decides where the value lands, so there is no
	// leaf to mark. Coarse, and correct in the only direction that matters.
	if sensitiveArgs {
		out = out.WithSensitive(true)
	}
	return out
}

// evaluateFile reads a file under templates/ verbatim.
//
// Shell scripts, user-data and cloud-init contain ${...} of their own.
// Interpolating one would consume ${HOME} and ${AWS_REGION} out of a bootstrap
// script and substitute nothing, producing a script that runs and misbehaves
// rather than one that fails.
func evaluateFile(e *value.Expr, scope Scope, ds *diag.Diagnostics) value.Value {
	name := e.Ref.VarName()
	content, _, ok := lookupTemplate(e, scope, name, ds)
	if !ok {
		return unknownFrom(e, value.KindString, false)
	}
	return value.String(content, value.SourceExplicit).WithOrigin(e.Origin)
}

// lookupTemplate finds a file and reports the two ways it can be absent.
func lookupTemplate(e *value.Expr, scope Scope, name string, ds *diag.Diagnostics) (string, value.Origin, bool) {
	ts, ok := scope.(TemplateScope)
	if !ok {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "templates are not available here",
			Detail:   strconv.Quote(name) + " was referenced somewhere this engine does not supply templates to.",
			Origin:   e.Origin,
		})
		return "", value.Origin{}, false
	}
	content, origin, found := ts.Template(name)
	if !found {
		ds.Add(diag.Diagnostic{
			Severity: diag.SeverityError,
			Summary:  "no template named " + strconv.Quote(name),
			Detail: "Templates are read from templates/ beside the project, and from " +
				"resources/<directory>/templates/ for a resource in that directory, which wins.",
			Action: "Create templates/" + name + ", or correct the name.",
			Origin: e.Origin,
		})
		return "", value.Origin{}, false
	}
	return content, origin, true
}
