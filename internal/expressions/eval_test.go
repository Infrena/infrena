package expressions

import (
	"strings"
	"testing"

	"infra/internal/diag"
	"infra/pkg/value"
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
	got, _ := evalSrc(t, "${project}", compileScope())
	if s, _ := got.AsString(); s != "myapp" {
		t.Errorf("= %q, want \"myapp\"", s)
	}
	if got.Source != value.SourceVariable {
		t.Errorf("Source = %v, want SourceVariable — provenance survives evaluation", got.Source)
	}
}

func TestEvaluateConcat(t *testing.T) {
	got, _ := evalSrc(t, "${project}-${region}", compileScope())
	if s, _ := got.AsString(); s != "myapp-us-east-1" {
		t.Errorf("= %q", s)
	}
}

func TestEvaluateCall(t *testing.T) {
	got, _ := evalSrc(t, "${upper(project)}", compileScope())
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
	got, _ := evalSrc(t, "${project}-${database.endpoint}", compileScope())
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

	got, _ := evalSrc(t, "prefix-${secret}", scope)
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

	got, _ := evalSrc(t, "${secret}-${database.endpoint}", scope)
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
	_, d := evalSrc(t, "${nonexistent}", compileScope())
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
	_, d := evalSrc(t, "${lower(project, region)}", compileScope())
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

	got, d := evalSrc(t, "prefix-${tags}", scope)
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
