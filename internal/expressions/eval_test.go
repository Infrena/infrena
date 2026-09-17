package expressions

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// testScope resolves variables but not resource attributes, which is exactly
// the compile-time scope.
type testScope struct {
	vars  map[string]value.Value
	attrs map[string]value.Value
}

func (s testScope) Variable(name string) (value.Value, bool) {
	v, ok := s.vars[name]
	return v, ok
}

func (s testScope) Attribute(ref value.Reference) (value.Value, bool) {
	v, ok := s.attrs[ref.String()]
	return v, ok
}

func compileScope() testScope {
	return testScope{vars: map[string]value.Value{
		"project": value.String("myapp", value.SourceVariable),
		"region":  value.String("us-east-1", value.SourceVariable),
	}}
}

func evalSrc(t *testing.T, src string, scope Scope) (value.Value, diag.Diagnostics) {
	t.Helper()
	e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 1})
	if ds.HasErrors() {
		t.Fatalf("Parse(%q): %+v", src, ds)
	}
	return Evaluate(e, scope)
}

func TestEvaluateLiteral(t *testing.T) {
	got, d := evalSrc(t, "postgres", compileScope())
	if d.HasErrors() {
		t.Fatal("literal evaluation should not error")
	}
	if s, _ := got.AsString(); s != "postgres" {
		t.Errorf("= %q, want \"postgres\"", s)
	}
}

func TestEvaluateVariable(t *testing.T) {
	got, _ := evalSrc(t, "${var.project}", compileScope())
	if s, _ := got.AsString(); s != "myapp" {
		t.Errorf("= %q, want \"myapp\"", s)
	}
	if got.Source != value.SourceVariable {
		t.Errorf("Source = %v, want SourceVariable — provenance survives evaluation", got.Source)
	}
}

func TestEvaluateConcat(t *testing.T) {
	got, _ := evalSrc(t, "${var.project}-${var.region}", compileScope())
	if s, _ := got.AsString(); s != "myapp-us-east-1" {
		t.Errorf("= %q", s)
	}
}

func TestEvaluateCall(t *testing.T) {
	got, _ := evalSrc(t, "${upper(var.project)}", compileScope())
	if s, _ := got.AsString(); s != "MYAPP" {
		t.Errorf("= %q, want \"MYAPP\"", s)
	}
}

func TestUnresolvableAttributeBecomesUnknown(t *testing.T) {
	got, d := evalSrc(t, "${database.endpoint}", compileScope())
	if d.HasErrors() {
		t.Fatal("an attribute the compile scope cannot resolve is unknown, not an error")
	}
	if got.Known {
		t.Error("must be unknown")
	}
	if got.Expr == nil {
		t.Fatal("an unknown value must carry the expression that will produce it")
	}
	if got.Source != value.SourceComputed {
		t.Errorf("Source = %v, want SourceComputed", got.Source)
	}
}

func TestUnknownIsContagiousThroughConcat(t *testing.T) {
	got, _ := evalSrc(t, "${var.project}-${database.endpoint}", compileScope())
	if got.Known {
		t.Error("a concat with an unknown argument must be unknown")
	}
	if got.Expr == nil {
		t.Error("the result must carry its expression for the executor to finish")
	}
}

func TestUnknownIsContagiousThroughCall(t *testing.T) {
	got, d := evalSrc(t, "${upper(database.endpoint)}", compileScope())
	if d.HasErrors() {
		t.Fatal("an unknown argument is not an error")
	}
	if got.Known {
		t.Error("a call with an unknown argument must be unknown")
	}
	if got.Kind != value.KindString {
		t.Errorf("Kind = %v, want KindString — an unknown still knows its type", got.Kind)
	}
}

func TestSensitivityUnionsThroughConcat(t *testing.T) {
	scope := compileScope()
	scope.vars["secret"] = value.String("hunter2", value.SourceVariable).WithSensitive(true)

	got, _ := evalSrc(t, "prefix-${var.secret}", scope)
	if !got.Sensitive {
		t.Error("a concat containing a secret must be sensitive")
	}
	if s, _ := got.AsString(); strings.Contains(s, "hunter2") && !got.Sensitive {
		t.Error("secret leaked unclassified")
	}
}

func TestSensitivitySurvivesIntoAnUnknown(t *testing.T) {
	scope := compileScope()
	scope.vars["secret"] = value.String("hunter2", value.SourceVariable).WithSensitive(true)

	got, _ := evalSrc(t, "${var.secret}-${database.endpoint}", scope)
	if got.Known {
		t.Fatal("should be unknown")
	}
	if !got.Sensitive {
		t.Error("an unknown built from a secret must still be sensitive — it is classified before it is known")
	}
}

func TestResolvableAttributeEvaluates(t *testing.T) {
	// The apply-time scope, where the dependency now exists.
	scope := compileScope()
	scope.attrs = map[string]value.Value{
		"database.endpoint": value.String("db-1.db.test", value.SourceProvider),
	}
	got, _ := evalSrc(t, "${database.endpoint}", scope)
	if !got.Known {
		t.Fatal("an attribute the scope can resolve must evaluate")
	}
	if s, _ := got.AsString(); s != "db-1.db.test" {
		t.Errorf("= %q", s)
	}
}

func TestUnknownVariableIsAnError(t *testing.T) {
	// A variable, unlike a resource attribute, cannot become known later.
	_, d := evalSrc(t, "${var.nonexistent}", compileScope())
	if !d.HasErrors() {
		t.Error("an undefined variable must be an error, not an unknown")
	}
}

func TestUnknownFunctionIsAnErrorNamingTheAlternatives(t *testing.T) {
	e, _ := Parse("${md5(project)}", value.Origin{})
	_, ds := Evaluate(e, compileScope())
	if !ds.HasErrors() {
		t.Fatal("calling an undefined function must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "lower") {
		t.Errorf("the diagnostic should list the available functions:\n%s", out.String())
	}
}

func TestFunctionErrorBecomesADiagnostic(t *testing.T) {
	_, d := evalSrc(t, "${lower(var.project, var.region)}", compileScope())
	if !d.HasErrors() {
		t.Error("wrong arity must surface as a diagnostic, not a panic or a silent result")
	}
}

func TestEvaluateNilIsEmptyNotPanic(t *testing.T) {
	got, ds := Evaluate(nil, compileScope())
	if ds.HasErrors() {
		t.Error("evaluating nil should not error")
	}
	if got.Known {
		t.Error("evaluating nil yields an unknown, not a known zero")
	}
}

// TestConcatOfAListIsAnErrorNotAnEmptyString guards against a defect in the
// literal brief: evaluateConcat's stringify helper, when given a composite
// (list or map), fell through to AsString on a non-string Raw and returned
// "" — a fabricated empty string silently spliced into the result, exactly
// what rule 1 (unknownness must not substitute a zero value) forbids. Schema
// binding (Task 7) is meant to reject composites inside interpolations before
// evaluation ever runs, but it does not exist yet, so nothing else stops a
// list-typed variable from reaching this path today.
func TestConcatOfAListIsAnErrorNotAnEmptyString(t *testing.T) {
	scope := compileScope()
	scope.vars["tags"] = value.List([]value.Value{
		value.String("a", value.SourceVariable),
		value.String("b", value.SourceVariable),
	}, value.SourceVariable)

	got, d := evalSrc(t, "prefix-${var.tags}", scope)
	if !d.HasErrors() {
		t.Fatal("interpolating a list must be a diagnostic error, not a silent empty string")
	}
	if got.Known {
		t.Fatal("a value produced alongside an error must not be treated as a real result")
	}
	if s, ok := got.AsString(); ok && s == "prefix-" {
		t.Error("the list must not have silently contributed an empty string to the concat")
	}
}

// TestDefaultWithUnknownFallbackCarriesTheWholeCallExpr guards against a
// second defect in the literal brief: evaluateCall returned fn's result
// as-is when unknown, without overriding its Expr. defaultFunc, when its
// primary argument is unknown (or blank) and its *fallback* argument is
// itself unknown, returns the fallback argument's own Value — whose Expr
// points only to that sub-expression, not to the default(...) call. Re-
// evaluating that narrower Expr once the fallback resolves would skip the
// primary-vs-fallback logic entirely, silently discarding default's
// semantics the moment the primary attribute also becomes known.
func TestDefaultWithUnknownFallbackCarriesTheWholeCallExpr(t *testing.T) {
	got, d := evalSrc(t, "${default(database.endpoint, backup.endpoint)}", compileScope())
	if d.HasErrors() {
		t.Fatal("two unresolvable attributes are unknown, not an error")
	}
	if got.Known {
		t.Fatal("should be unknown: both the primary and the fallback are unresolved")
	}
	if got.Expr == nil {
		t.Fatal("an unknown value must carry an expression")
	}
	if got.Expr.Op != value.OpCall || got.Expr.Function != "default" {
		t.Errorf("Expr = %s, want the whole default(...) call so re-evaluation redoes the fallback logic", got.Expr)
	}
}

func foldScope() testScope {
	return testScope{vars: map[string]value.Value{
		"prefix": value.String("acme", value.SourceVariable),
		"secret": value.String("hunter2", value.SourceVariable).WithSensitive(true),
	}}
}

// TestDeferredConcatFoldsResolvedParts pins that a deferred expression carries
// only what is still unknown. Before this, the whole source expression was
// deferred, so a variable resolved at compile time was left to be resolved
// again by whoever evaluated it later — and ConfigHash could not see its value.
func TestDeferredConcatFoldsResolvedParts(t *testing.T) {
	got, ds := evalSrc(t, "${var.prefix}-${network.id}", foldScope())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.Known {
		t.Fatal("a resource reference is unknown until apply, so the result must be unknown")
	}
	if got.Expr == nil {
		t.Fatal("a deferred value must carry the expression that will produce it")
	}
	if len(got.Expr.Args) != 3 {
		t.Fatalf("residual should keep all three positions, got %d", len(got.Expr.Args))
	}
	// The variable resolved, so it is now a literal carrying its VALUE.
	if op := got.Expr.Args[0].Op; op != value.OpLiteral {
		t.Errorf("arg[0] op = %v, want OpLiteral — the variable resolved at compile time", op)
	}
	if s, _ := got.Expr.Args[0].Literal.AsString(); s != "acme" {
		t.Errorf("arg[0] literal = %q, want %q — folding must carry the value, not just the kind", s, "acme")
	}
	// The resource reference did not, so it survives as a reference.
	if op := got.Expr.Args[2].Op; op != value.OpResourceRef {
		t.Errorf("arg[2] op = %v, want OpResourceRef — it is genuinely unknown until apply", op)
	}
}

// TestDeferredSensitivitySurvivesFolding pins that folding a resolved SENSITIVE
// part into a literal does not lose its mark. A folded literal that dropped its
// Sensitive flag would be a way to launder a secret into a plan artifact.
//
// Both assertions are UNCONDITIONAL on purpose. An earlier draft guarded the
// second with `if args[0].Op == OpLiteral`, which is false before the fix — so
// the check could not fail until after the change it exists to verify. That is
// the vacuous-assertion shape this project has shipped eight times; do not
// reintroduce it by making either line conditional.
func TestDeferredSensitivitySurvivesFolding(t *testing.T) {
	got, _ := evalSrc(t, "${var.secret}-${network.id}", foldScope())
	if !got.Sensitive {
		t.Error("a deferred value built from a sensitive part must itself be sensitive")
	}
	if got.Expr == nil || len(got.Expr.Args) == 0 {
		t.Fatal("no residual expression")
	}
	if got.Expr.Args[0].Op != value.OpLiteral {
		t.Fatalf("arg[0] op = %v, want OpLiteral", got.Expr.Args[0].Op)
	}
	if !got.Expr.Args[0].Literal.Sensitive {
		t.Error("the folded literal must stay marked sensitive; otherwise the residual launders the secret")
	}
}

// TestFullyUnresolvedConcatKeepsItsShape checks the fold does not disturb the
// case where nothing resolved. This one PASSES before the fix as well as after,
// and that is stated rather than dressed up: it is a guard against the fold
// damaging an expression it should leave alone, not evidence the fold works.
func TestFullyUnresolvedConcatKeepsItsShape(t *testing.T) {
	got, _ := evalSrc(t, "${a.id}-${b.id}", foldScope())
	if got.Expr == nil {
		t.Fatal("expected a deferred expression")
	}
	if s := got.Expr.String(); s != "${a.id}-${b.id}" {
		t.Errorf("residual = %q, want the source shape back", s)
	}
}

// TestFoldedSensitiveLiteralRedactsInStringRendering pins the fix for a leak
// residual introduced: folding a resolved SENSITIVE value into an OpLiteral
// inside a deferred expression made it reachable by Expr.String()/inner(),
// neither of which checked Literal.Sensitive before this fix. String() is not
// the sanctioned redaction path — value.Format is — so a second, unguarded
// rendering path is exactly how a secret leaked in M2.
//
// Measured before this fix: got.Expr.String() == "hunter2-${network.id}".
func TestFoldedSensitiveLiteralRedactsInStringRendering(t *testing.T) {
	got, _ := evalSrc(t, "${var.secret}-${network.id}", foldScope())
	if got.Expr == nil {
		t.Fatal("expected a deferred expression")
	}
	rendered := got.Expr.String()
	if strings.Contains(rendered, "hunter2") {
		t.Errorf("String() = %q leaks the sensitive value", rendered)
	}
	if !strings.Contains(rendered, value.Redacted) {
		t.Errorf("String() = %q, want it to contain %q", rendered, value.Redacted)
	}
}

// TestDeferredCallFoldsResolvedArgs pins that evaluateCall's non-default
// deferral folds its resolved arguments into literals too, the same as
// evaluateConcat already does. Before this fix, a call with a mix of
// resolved and unresolved arguments deferred the whole SOURCE expression, so
// the resolved argument stayed an OpVarRef — ref NAME only, no value — which
// is the exact ConfigHash blindness Task 3 exists to close, left open for
// calls.
func TestDeferredCallFoldsResolvedArgs(t *testing.T) {
	got, ds := evalSrc(t, `${replace(network.id, "old", var.prefix)}`, foldScope())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.Known {
		t.Fatal("network.id is unresolved, so the call result must be unknown")
	}
	if got.Expr == nil || len(got.Expr.Args) != 3 {
		t.Fatalf("residual should keep all three call arguments, got %v", got.Expr)
	}
	if op := got.Expr.Args[0].Op; op != value.OpResourceRef {
		t.Errorf("arg[0] op = %v, want OpResourceRef — network.id is genuinely unknown", op)
	}
	if op := got.Expr.Args[1].Op; op != value.OpLiteral {
		t.Errorf("arg[1] op = %v, want OpLiteral — \"old\" was always a literal", op)
	}
	// prefix resolved at compile time, so it must fold to a literal carrying
	// its VALUE, not survive as a reference ConfigHash can only see by name.
	if op := got.Expr.Args[2].Op; op != value.OpLiteral {
		t.Errorf("arg[2] op = %v, want OpLiteral — prefix resolved at compile time", op)
	}
	if s, _ := got.Expr.Args[2].Literal.AsString(); s != "acme" {
		t.Errorf("arg[2] literal = %q, want %q", s, "acme")
	}
}

// TestDefaultsUnknownFallbackDeferralStillCarriesTheWholeCall guards the one
// call shape that must NOT fold: default()'s own unknown-out deferral (the
// path TestDefaultWithUnknownFallbackCarriesTheWholeCallExpr already covers
// for the fully-unknown case). This pins it also holds when the PRIMARY
// resolved to a known, blank value — default() still routes to the fallback,
// the fallback is still unknown, and folding a literal "" in would not let
// ConfigHash distinguish anything a non-blank value couldn't already show by
// taking the fully-resolved path instead. residual leaves this call's source
// expression untouched.
func TestDefaultsUnknownFallbackDeferralStillCarriesTheWholeCall(t *testing.T) {
	scope := testScope{vars: map[string]value.Value{
		"blank": value.String("", value.SourceVariable),
	}}
	got, ds := evalSrc(t, "${default(var.blank, network.id)}", scope)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.Known {
		t.Fatal("the fallback is unresolved, so the result must be unknown")
	}
	if got.Expr == nil || got.Expr.Op != value.OpCall || got.Expr.Function != "default" {
		t.Fatalf("Expr = %v, want the whole default(...) call", got.Expr)
	}
	if op := got.Expr.Args[0].Op; op != value.OpVarRef {
		t.Errorf("arg[0] op = %v, want OpVarRef — the blank primary is not folded here", op)
	}
}

// TestMergeThroughTheEvaluatorStaysPerLeaf guards the one place where making
// this file's sensitivity union recursive would be a regression rather than a
// fix.
//
// merge() returns a map and classifies per leaf on purpose: a map holding one
// secret and four ordinary keys must redact the one, not all five. The
// evaluator then ORs its own argument union onto the result. That union is
// top-level, so a secret sitting INSIDE an argument map does not mark the
// whole result — it stays on the leaf, where value.Format redacts it.
//
// Widen the union to value.HasSensitive and this test fails, which is the
// point: the change looks like a consistency fix and costs every merged map
// its readability.
func TestMergeThroughTheEvaluatorStaysPerLeaf(t *testing.T) {
	scope := compileScope()
	scope.vars["creds"] = value.Map(map[string]value.Value{
		"password": value.String("hunter2", value.SourceVariable).WithSensitive(true),
	}, value.SourceVariable)
	scope.vars["labels"] = value.Map(map[string]value.Value{
		"team": value.String("platform", value.SourceVariable),
	}, value.SourceVariable)

	got, ds := evalSrc(t, "${merge(var.creds, var.labels)}", scope)
	if ds.HasErrors() {
		t.Fatalf("merge did not evaluate: %v", ds)
	}
	if got.Sensitive {
		t.Error("the whole merged map was classified; one secret leaf must not redact every key")
	}

	entries, ok := got.Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("merge returned %T, want a map", got.Raw)
	}
	if !entries["password"].Sensitive {
		t.Error("the secret leaf lost its flag, which prints it in clear")
	}
	if entries["team"].Sensitive {
		t.Error("an ordinary leaf was classified alongside the secret one")
	}
}
