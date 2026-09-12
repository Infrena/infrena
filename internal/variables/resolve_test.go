package variables

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/environments"
	"github.com/infrata/infrata/pkg/value"
)

func envOverride(name string, n int64) config.OverrideDecl {
	return config.OverrideDecl{Name: name, Value: value.Int(n, value.SourceEnvironment)}
}

// ladderInputs builds a chain and inputs where EVERY rung sets `replicas`, and
// the values descend as precedence ascends.
func ladderInputs() (decls []config.VariableDecl, chain environments.Chain, files map[string]value.Value, cli map[string]string) {
	d := config.VariableDecl{
		Name:       "replicas",
		Type:       value.KindInt,
		Default:    value.Int(50, value.SourceExplicit),
		HasDefault: true,
		Origin:     value.Origin{File: "variables.yml", Line: 2, Column: 3},
	}
	envDecls := []config.EnvironmentDecl{
		{Name: "base", Overrides: []config.OverrideDecl{envOverride("replicas", 30)}},
		{Name: "production", Extends: "base", Overrides: []config.OverrideDecl{envOverride("replicas", 20)}},
	}
	chain, _ = environments.Resolve(envDecls, "production")
	return []config.VariableDecl{d},
		chain,
		map[string]value.Value{"replicas": value.Int(40, value.SourceVariable)},
		map[string]string{"replicas": "10"}
}

func TestResolveWalksTheWholePrecedenceLadder(t *testing.T) {
	decls, chain, files, cli := ladderInputs()

	// Each step removes the winning rung, so the expected answer walks DOWN the
	// ladder one rung at a time. An implementation that always returns the last
	// entry it saw, or the largest, fails at the first step it does not happen
	// to match.
	for _, tc := range []struct {
		name  string
		files map[string]value.Value
		cli   map[string]string
		chain environments.Chain
		want  int64
		scope value.Scope
	}{
		{"--var wins", files, cli, chain, 10, value.ScopeCLIOverride},
		{"environment wins", files, nil, chain, 20, value.ScopeEnvironmentVar},
		{"inherited environment wins", files, nil, trimLast(chain), 30, value.ScopeEnvironmentInherit},
		{"variables.yml wins", files, nil, bare(chain), 40, value.ScopeBaseConfig},
		{"declared default is the floor", nil, nil, bare(chain), 50, value.ScopeBaseConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, ds := Resolve(decls, tc.chain, tc.files, nil, tc.cli)
			if ds.HasErrors() {
				t.Fatalf("unexpected diagnostics: %+v", ds)
			}
			got, ok := scope.Variable("replicas")
			if !ok {
				t.Fatal("replicas did not resolve at all")
			}
			if n, _ := got.AsInt(); n != tc.want {
				t.Errorf("replicas = %d, want %d", n, tc.want)
			}
			if got.Scope != tc.scope {
				t.Errorf("Scope = %v, want %v", got.Scope, tc.scope)
			}
		})
	}
}

// trimLast drops the selected layer, leaving the inherited one as the top of
// the environment part of the ladder. It re-stamps nothing: the remaining
// layer keeps the ScopeEnvironmentInherit stage 3 gave it, which is what the
// test is checking travels through unchanged.
func trimLast(c environments.Chain) environments.Chain {
	out := c
	out.Layers = append([]environments.Layer(nil), c.Layers[:len(c.Layers)-1]...)
	return out
}

// bare keeps the chain selected but removes every layer, so the environment
// rungs contribute nothing.
func bare(c environments.Chain) environments.Chain {
	return environments.Chain{Name: c.Name, Selected: true}
}

func TestResolveRecordsSourceSeparatelyFromScope(t *testing.T) {
	decls, chain, _, _ := ladderInputs()

	fromFile, ds := Resolve(decls, bare(chain), map[string]value.Value{"replicas": value.Int(20, value.SourceVariable)}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("file: %+v", ds)
	}
	fromFlag, ds := Resolve(decls, bare(chain), nil, nil, map[string]string{"replicas": "20"})
	if ds.HasErrors() {
		t.Fatalf("flag: %+v", ds)
	}

	a, _ := fromFile.Variable("replicas")
	b, _ := fromFlag.Variable("replicas")

	if !a.Equal(b) {
		t.Error("the same datum from two scopes must be Equal — value.Equal ignores provenance, and acceptance invariant 2 (no-op plan) depends on it")
	}
	if a.Source != value.SourceVariable || b.Source != value.SourceVariable {
		t.Errorf("Source = %v and %v, want SourceVariable for both: Source says WHAT KIND of thing a value is, and both of these are variables", a.Source, b.Source)
	}
	if a.Scope != value.ScopeBaseConfig || b.Scope != value.ScopeCLIOverride {
		t.Errorf("Scope = %v and %v, want ScopeBaseConfig and ScopeCLIOverride: Scope says WHICH RUNG won, and a plan that cannot tell a file from a flag cannot explain itself", a.Scope, b.Scope)
	}
}

func TestResolveLetsAnExplicitEntryBeatItsOwnDeclaredDefault(t *testing.T) {
	// PLAN.md §7: "Explicit user configuration always overrides an implicit
	// default." Both sit at ScopeBaseConfig; Source is what separates them.
	decls, _, _, _ := ladderInputs()
	scope, ds := Resolve(decls, environments.Chain{}, map[string]value.Value{"replicas": value.Int(40, value.SourceVariable)}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got, _ := scope.Variable("replicas")
	if n, _ := got.AsInt(); n != 40 {
		t.Errorf("replicas = %d, want the explicit 40 rather than the declared default 50", n)
	}
	if got.Source != value.SourceVariable {
		t.Errorf("Source = %v, want SourceVariable: it is no longer a default", got.Source)
	}
}

func TestResolveStampsTheDeclaredDefaultWhenItWins(t *testing.T) {
	// Stage 2 leaves Default's provenance unset on purpose; stage 4 is the
	// only place that says which rung won.
	decls, _, _, _ := ladderInputs()
	scope, _ := Resolve(decls, environments.Chain{}, nil, nil, nil)
	got, _ := scope.Variable("replicas")
	if got.Source != value.SourceDefault || got.Scope != value.ScopeBaseConfig {
		t.Errorf("Source/Scope = %v/%v, want SourceDefault/ScopeBaseConfig", got.Source, got.Scope)
	}
}

func TestResolveReportsAnUnsetVariableWhenAnEnvironmentIsSelected(t *testing.T) {
	decls := []config.VariableDecl{{
		Name:   "domain",
		Type:   value.KindString,
		Origin: value.Origin{File: "variables.yml", Line: 4, Column: 3},
	}}
	chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "production"}}, "production")

	_, ds := Resolve(decls, chain, nil, nil, nil)
	if !ds.HasErrors() {
		t.Fatal("`infra plan production` has consulted everything that could set `domain`; nothing did, so this is a definite error")
	}
	var sb strings.Builder
	ds.Render(&sb)
	for _, want := range []string{"domain", "production", "--var"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("the diagnostic must name the variable, the environment, and a way to set it; %q missing:\n%s", want, sb.String())
		}
	}
}

func TestResolveLeavesAnUnsetVariableUnknownWhenNoEnvironmentIsSelected(t *testing.T) {
	// `infra validate` has no environment argument, so it cannot know what
	// production sets. An unknown carries the declared Kind, so kind checks
	// downstream still work; an error here would fail configuration that plans
	// perfectly well, and a validate command users learn to ignore is worse
	// than no validate command.
	decls := []config.VariableDecl{{Name: "domain", Type: value.KindString}}

	scope, ds := Resolve(decls, environments.Chain{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("validate must not fail on a variable only an environment sets: %+v", ds)
	}
	got, ok := scope.Variable("domain")
	if !ok {
		t.Fatal("the variable must still be DEFINED, or stage 6 reports `undefined variable` instead")
	}
	if got.Known {
		t.Error("it must be unknown: there is no value, and inventing an empty string would be a confident wrong answer")
	}
	if got.Kind != value.KindString {
		t.Errorf("Kind = %v, want KindString: an unknown still carries its declared type", got.Kind)
	}
	if got.Scope != value.ScopeUnset {
		t.Errorf("Scope = %v, want ScopeUnset: no rung supplied it", got.Scope)
	}
}

func TestResolveValidatesTheWinningValueAgainstItsSchema(t *testing.T) {
	decls := []config.VariableDecl{{
		Name: "replicas", Type: value.KindInt,
		Min: value.Int(1, value.SourceExplicit), HasMin: true,
		Max: value.Int(100, value.SourceExplicit), HasMax: true,
		Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
	}}
	chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "dev"}}, "dev")

	if _, ds := Resolve(decls, chain, nil, nil, map[string]string{"replicas": "500"}); !ds.HasErrors() {
		t.Error("--var replicas=500 violates max: 100 and must be reported")
	}
	if _, ds := Resolve(decls, chain, nil, nil, map[string]string{"replicas": "50"}); ds.HasErrors() {
		t.Errorf("--var replicas=50 is inside the declared range and must be accepted: %+v", ds)
	}
}

func TestResolveValidatesAnEnvironmentOverrideToo(t *testing.T) {
	// The check is on the WINNING value, whichever rung it came from — not on
	// --var alone.
	decls := []config.VariableDecl{{
		Name: "replicas", Type: value.KindInt,
		Max: value.Int(100, value.SourceExplicit), HasMax: true,
		Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
	}}
	chain, _ := environments.Resolve([]config.EnvironmentDecl{
		{Name: "production", Overrides: []config.OverrideDecl{envOverride("replicas", 500)}},
	}, "production")

	if _, ds := Resolve(decls, chain, nil, nil, nil); !ds.HasErrors() {
		t.Error("an environment override outside the declared range must be reported")
	}
}

func TestResolveDoesNotDoubleReportADefaultsOwnBoundViolation(t *testing.T) {
	// Schemas validates a declared default once, at declaration time — its
	// own doc comment says so: "checked against its own constraints here,
	// once, rather than every time the default wins." checkAgainstSchemas
	// used to re-validate the SAME winning value whenever that default won
	// rung 1, reporting the identical diagnostic a second time. Counting
	// matters here, not just presence: a test that only asked ds.HasErrors()
	// could not see the duplicate.
	decls := []config.VariableDecl{{
		Name: "replicas", Type: value.KindInt,
		Default: value.Int(500, value.SourceExplicit), HasDefault: true,
		Max: value.Int(100, value.SourceExplicit), HasMax: true,
		Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
	}}
	_, ds := Resolve(decls, environments.Chain{}, nil, nil, nil)
	if len(ds) != 1 {
		t.Fatalf("len(ds) = %d, want exactly 1 — the same violation reported twice is not two problems: %+v", len(ds), ds)
	}
	if ds[0].Summary != `variable "replicas" must be at most 100` {
		t.Errorf("Summary = %q, want the bound-violation message naming the default's own value", ds[0].Summary)
	}

	// The genuinely-doubly-bad case: two DIFFERENT variables each with a bad
	// default must still produce two diagnostics, one per variable — the fix
	// must not suppress a second, distinct problem along with the duplicate.
	two := []config.VariableDecl{
		{
			Name: "replicas", Type: value.KindInt,
			Default: value.Int(500, value.SourceExplicit), HasDefault: true,
			Max: value.Int(100, value.SourceExplicit), HasMax: true,
			Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
		},
		{
			Name: "workers", Type: value.KindInt,
			Default: value.Int(-1, value.SourceExplicit), HasDefault: true,
			Min: value.Int(0, value.SourceExplicit), HasMin: true,
			Origin: value.Origin{File: "variables.yml", Line: 5, Column: 3},
		},
	}
	_, ds = Resolve(two, environments.Chain{}, nil, nil, nil)
	if len(ds) != 2 {
		t.Fatalf("len(ds) = %d, want exactly 2 (one per bad variable, still no duplicates): %+v", len(ds), ds)
	}
	var sb strings.Builder
	ds.Render(&sb)
	for _, want := range []string{`"replicas" must be at most 100`, `"workers" must be at least 0`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("missing %q in:\n%s", want, sb.String())
		}
	}
}

func TestResolveKeepsAnUndeclaredCLIVariable(t *testing.T) {
	// Rung 6 branches on whether the name is declared: a declared name goes
	// through Schema.ParseText, an undeclared one is stored as plain text at
	// ScopeCLIOverride. Every other --var test in this file supplies a
	// declared name, so this is the only test that reaches the undeclared
	// half of that branch.
	//
	// "az", not "region": "region" is one of the three process-reserved
	// names (see reservedNameDiag) and --var refuses it outright since the
	// M4 final-review fix wave — a genuinely undeclared, non-reserved name
	// is what this test means to exercise.
	scope, ds := Resolve(nil, environments.Chain{}, nil, nil, map[string]string{"az": "us-east-1"})
	if ds.HasErrors() {
		t.Fatalf("an undeclared --var is not an error: %+v", ds)
	}
	got, ok := scope.Variable("az")
	if !ok {
		t.Fatal("az must resolve")
	}
	if s, _ := got.AsString(); s != "us-east-1" {
		t.Errorf("az = %q, want us-east-1: --var carries text, and no type is guessed for an undeclared name", s)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("Scope = %v, want ScopeCLIOverride", got.Scope)
	}
	// Amendment 6 (contract.md): SuppliedBy must be stamped even though its
	// value ("--var") happens to equal Scope.String()'s ScopeCLIOverride
	// fallback — a rendering-only assertion cannot tell "stamped as --var"
	// apart from "never stamped, fell back to --var", so this checks the
	// field directly rather than through annotation().
	if got.SuppliedBy != "--var" {
		t.Errorf("SuppliedBy = %q, want %q", got.SuppliedBy, "--var")
	}
}

func TestOverrideSetsAVariableOnAZeroValueScope(t *testing.T) {
	// The nil-map branch: a bare `var s Scope` (or Scope{}) has a nil vars
	// map, and Override must lazily allocate it rather than panic on the
	// write. Task 7 is Override's only CALLER today, but Scope is exported
	// and this codebase has already shipped one bug from treating "no caller
	// yet" as "unreachable" (Task 4).
	var s Scope
	s.Override("environment", value.String("production", value.SourceExplicit))

	got, ok := s.Variable("environment")
	if !ok {
		t.Fatal("environment must resolve after Override on a zero-value Scope")
	}
	if str, _ := got.AsString(); str != "production" {
		t.Errorf("environment = %q, want production", str)
	}
}

func TestOverrideReplacesAVariableOnAnAlreadyPopulatedScope(t *testing.T) {
	// The non-nil branch: Override on a Scope Resolve already built must
	// replace an existing entry (or add a new one) without disturbing the
	// rest — it is not a second precedence ladder, it is authoritative.
	decls := []config.VariableDecl{{
		Name: "environment", Type: value.KindString,
		Default: value.String("dev", value.SourceExplicit), HasDefault: true,
	}}
	scope, ds := Resolve(decls, environments.Chain{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("fixture: %+v", ds)
	}
	before, _ := scope.Variable("environment")
	if s, _ := before.AsString(); s != "dev" {
		t.Fatalf("fixture: environment = %q, want dev before Override", s)
	}

	scope.Override("environment", value.String("production", value.SourceExplicit))
	after, ok := scope.Variable("environment")
	if !ok {
		t.Fatal("environment must still resolve after Override")
	}
	if s, _ := after.AsString(); s != "production" {
		t.Errorf("environment = %q, want production to have replaced the resolved dev", s)
	}
}

func TestScopeVariableReportsFalseForAnUnresolvedName(t *testing.T) {
	// The other half of Variable's two-result return, exercised nowhere else
	// in this file: every other test in this package resolves the name it
	// then looks up.
	scope, _ := Resolve(nil, environments.Chain{}, nil, nil, nil)
	if _, ok := scope.Variable("never_declared"); ok {
		t.Error("a name nothing set must report ok=false, not a zero Value mistaken for a real one")
	}
}

func TestScopeNamesListsResolvedVariablesSorted(t *testing.T) {
	// Names is stage 6's source for "did you mean" suggestions on an
	// undefined-variable diagnostic; a fixture in reverse-alphabetical
	// declaration order is what distinguishes "sorts" from "happens to be in
	// order" or "returns map iteration order unchanged".
	decls := []config.VariableDecl{
		{Name: "zulu", Type: value.KindString, Default: value.String("z", value.SourceExplicit), HasDefault: true},
		{Name: "alpha", Type: value.KindString, Default: value.String("a", value.SourceExplicit), HasDefault: true},
		{Name: "mike", Type: value.KindString, Default: value.String("m", value.SourceExplicit), HasDefault: true},
	}
	scope, ds := Resolve(decls, environments.Chain{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got := strings.Join(scope.Names(), ",")
	want := "alpha,mike,zulu"
	if got != want {
		t.Errorf("Names() = %q, want %q", got, want)
	}
}

func TestResolveKeepsAnUndeclaredVariable(t *testing.T) {
	// A variable need not be declared at all: PLAN.md §9 says schemas are
	// OPTIONAL. An undeclared name is untyped and unconstrained.
	scope, ds := Resolve(nil, environments.Chain{}, map[string]value.Value{
		"domain": value.String("example.com", value.SourceVariable),
	}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("an undeclared variable is not an error: %+v", ds)
	}
	if v, ok := scope.Variable("domain"); !ok {
		t.Fatal("domain must resolve")
	} else if s, _ := v.AsString(); s != "example.com" {
		t.Errorf("domain = %q, want example.com", s)
	}
}

func floatSchemaDecls(min, max float64) []config.VariableDecl {
	return []config.VariableDecl{{
		Name: "ratio", Type: value.KindFloat,
		Min: value.Float(min, value.SourceExplicit), HasMin: true,
		Max: value.Float(max, value.SourceExplicit), HasMax: true,
		Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
	}}
}

func TestResolveCoercesAYamlIntegerToADeclaredFloat(t *testing.T) {
	// YAML tags `ratio: 1` as !!int whatever `type: float` says. The user has
	// written the only spelling available to them.
	chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "dev"}}, "dev")
	scope, ds := Resolve(floatSchemaDecls(0.5, 10), chain,
		map[string]value.Value{"ratio": value.Int(1, value.SourceVariable)}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("`ratio: 1` under `type: float` must be accepted: %+v", ds)
	}
	got, _ := scope.Variable("ratio")
	if got.Kind != value.KindFloat {
		t.Errorf("Kind = %v, want KindFloat — the stored value must be the declared kind, or stage 7's kind check rejects it later", got.Kind)
	}
	if f, ok := got.AsFloat(); !ok || f != 1 {
		t.Errorf("value = %#v, want 1.0", got)
	}
}

func TestResolveCoercesBeforeCheckingBounds(t *testing.T) {
	// THE case that makes ordering load-bearing. compareBounds reads both
	// sides in the declared kind, so an uncoerced KindInt value against a
	// KindFloat bound is unreadable and compares as 0 — no complaint. Bound
	// first, coerce second, and the value is not mistyped, it is SILENTLY
	// UNBOUNDED. This test fails with no diagnostic at all before the change,
	// which is the failure mode worth pinning.
	chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "dev"}}, "dev")
	_, ds := Resolve(floatSchemaDecls(2, 10), chain,
		map[string]value.Value{"ratio": value.Int(1, value.SourceVariable)}, nil, nil)
	if !ds.HasErrors() {
		t.Fatal("1 is below `min: 2` and must be reported; an uncoerced value skips the bound check entirely rather than failing it")
	}
	// HasErrors() alone does not discriminate this ordering: coercing AFTER
	// Validate still produces an error, just the WRONG one — Validate's own
	// kind-mismatch check fires first on the still-uncoerced KindInt value
	// (against the declared KindFloat) and returns before ever reaching the
	// bound check, so a test that stops at HasErrors() cannot tell "caught
	// the bound violation" from "caught a kind mismatch that masks it". The
	// message must name the actual bound.
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "must be at least 2") {
		t.Errorf("the diagnostic must be the BOUND violation, not a kind mismatch masking it:\n%s", sb.String())
	}
}

func TestResolveRejectsALossyCoercion(t *testing.T) {
	intDecls := []config.VariableDecl{{
		Name: "replicas", Type: value.KindInt,
		Origin: value.Origin{File: "variables.yml", Line: 2, Column: 3},
	}}
	chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "dev"}}, "dev")

	// 1.5 cannot become an integer without changing what the user wrote.
	_, ds := Resolve(intDecls, chain,
		map[string]value.Value{"replicas": value.Float(1.5, value.SourceVariable)}, nil, nil)
	if !ds.HasErrors() {
		t.Error("`replicas: 1.5` under `type: integer` must stay an error — rounding would silently change the value")
	}

	// And an integer too large to survive a float64 keeps its error too.
	const tooBig = int64(1)<<53 + 1
	_, ds = Resolve(floatSchemaDecls(0, 1e18), chain,
		map[string]value.Value{"ratio": value.Int(tooBig, value.SourceVariable)}, nil, nil)
	if !ds.HasErrors() {
		t.Errorf("%d cannot be stored as a float64 without changing it, so it must be reported rather than coerced", tooBig)
	}
}

func TestResolveLeavesANonNumericMismatchToValidate(t *testing.T) {
	// Coercion must not swallow a type error. A string where a float is
	// declared is not a lossy conversion, it is the wrong kind, and Validate
	// owns that message.
	chain, _ := environments.Resolve([]config.EnvironmentDecl{{Name: "dev"}}, "dev")
	_, ds := Resolve(floatSchemaDecls(0, 10), chain,
		map[string]value.Value{"ratio": value.String("half", value.SourceVariable)}, nil, nil)
	if !ds.HasErrors() {
		t.Fatal("a string supplied for a float variable is still an error")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "must be a float") {
		t.Errorf("the message must be Validate's kind-mismatch one, not a coercion failure:\n%s", sb.String())
	}
}

func TestResolveCoercionKeepsProvenanceAndSensitivity(t *testing.T) {
	// M3 lost a Critical to an engine that recorded what a provider returned
	// and dropped what it knew. A rebuilt value that loses Scope would make a
	// plan name the wrong rung; one that loses Sensitive prints a secret.
	chain, _ := environments.Resolve([]config.EnvironmentDecl{
		{Name: "production", Overrides: []config.OverrideDecl{{
			Name: "ratio",
			Value: value.Int(1, value.SourceEnvironment).
				WithSensitive(true).
				WithOrigin(value.Origin{File: "environments/production.yml", Line: 4, Column: 3}),
		}}},
	}, "production")

	scope, ds := Resolve(floatSchemaDecls(0, 10), chain, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got, _ := scope.Variable("ratio")
	if got.Kind != value.KindFloat {
		t.Fatalf("Kind = %v, want KindFloat", got.Kind)
	}
	if got.Source != value.SourceEnvironment || got.Scope != value.ScopeEnvironmentVar {
		t.Errorf("Source/Scope = %v/%v, want SourceEnvironment/ScopeEnvironmentVar — coercion changes the datum's type, never where it came from", got.Source, got.Scope)
	}
	if !got.Sensitive {
		t.Error("a coerced value must stay sensitive: rebuilding it through a constructor and forgetting this is how a secret reaches a plan in clear")
	}
	if got.Origin.Line != 4 {
		t.Errorf("Origin = %+v, want environments/production.yml:4:3 — a diagnostic about this value must still point at the line the user wrote", got.Origin)
	}
}

// chainWith builds a one-environment Chain (no `extends`) whose named
// environment sets exactly the given overrides, each as a plain string at
// SourceEnvironment — environments.Resolve is what stamps their Scope
// (ScopeEnvironmentVar for a chain with a single layer), not this helper.
func chainWith(t *testing.T, envName string, overrides map[string]string) environments.Chain {
	t.Helper()
	var ov []config.OverrideDecl
	for name, val := range overrides {
		ov = append(ov, config.OverrideDecl{Name: name, Value: value.String(val, value.SourceEnvironment)})
	}
	chain, ds := environments.Resolve([]config.EnvironmentDecl{{Name: envName, Overrides: ov}}, envName)
	if ds.HasErrors() {
		t.Fatalf("building chain: %+v", ds)
	}
	return chain
}

// emptyChain is environments.Resolve's answer for no environment argument at
// all (Selected: false, no layers) — the same Chain `infra validate` compiles
// against.
func emptyChain(t *testing.T) environments.Chain {
	t.Helper()
	chain, ds := environments.Resolve(nil, "")
	if ds.HasErrors() {
		t.Fatalf("building empty chain: %+v", ds)
	}
	return chain
}

func TestFileEntryAtCLIScopeOutranksEnvironmentConfiguration(t *testing.T) {
	// A --var-file entry arrives inside the same `files` map as a
	// variables.yml entry and is told apart ONLY by its Scope. An
	// implementation that assigns one scope to the whole map cannot express
	// this, and --var-file would be silently ignored for every name the
	// environment also sets.
	//
	// "az", not "region": "region" is process-reserved (reservedNameDiag)
	// and a --var-file naming it is refused since the M4 final-review fix
	// wave; this test means to exercise an ordinary name's precedence.
	chain := chainWith(t, "production", map[string]string{"az": "us-east-1"}) // ScopeEnvironmentVar
	files := map[string]value.Value{
		"az": value.String("eu-west-1", value.SourceVariable).WithScope(value.ScopeCLIOverride),
	}

	scope, ds := Resolve(nil, chain, files, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	got, ok := scope.Variable("az")
	if !ok {
		t.Fatal("az is not in scope")
	}
	if s, _ := got.AsString(); s != "eu-west-1" {
		t.Errorf("az = %q, want eu-west-1 — a --var-file outranks environment configuration", s)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("Scope = %v, want ScopeCLIOverride — the winning level must be recorded", got.Scope)
	}
}

func TestFileEntryAtBaseScopeLosesToEnvironmentConfiguration(t *testing.T) {
	// The mirror image, and the reason the first test is not satisfied by
	// "the files map always wins": variables.yml sits BELOW the environment.
	chain := chainWith(t, "production", map[string]string{"region": "us-east-1"})
	files := map[string]value.Value{
		"region": value.String("eu-west-1", value.SourceVariable).WithScope(value.ScopeBaseConfig),
	}

	scope, _ := Resolve(nil, chain, files, nil, nil)
	got, _ := scope.Variable("region")
	if s, _ := got.AsString(); s != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", s)
	}
	if got.Scope != value.ScopeEnvironmentVar {
		t.Errorf("Scope = %v, want ScopeEnvironmentVar", got.Scope)
	}
}

func TestCLIVarOutranksAFileEntryAtTheSameScope(t *testing.T) {
	// PLAN.md §8: "CLI values override variable files." Both sit at
	// ScopeCLIOverride, so the tie is broken by application order, not by
	// comparing Scope.
	//
	// "az", not "region": see TestFileEntryAtCLIScopeOutranksEnvironmentConfiguration.
	files := map[string]value.Value{
		"az": value.String("eu-west-1", value.SourceVariable).WithScope(value.ScopeCLIOverride),
	}
	scope, _ := Resolve(nil, emptyChain(t), files, nil, map[string]string{"az": "ap-south-1"})
	got, _ := scope.Variable("az")
	if s, _ := got.AsString(); s != "ap-south-1" {
		t.Errorf("az = %q, want ap-south-1", s)
	}
}

// TestVarFileBoundViolationNamesTheFileNotDashDashVar reproduces M4 final
// review's MAJOR 1, at the level the bug actually lived: Resolve, not
// Schema.Validate in isolation. `size` is declared `max: 500`; the violating
// value arrives exactly as config.DecodeVariableFile stamps a --var-file
// entry — ScopeCLIOverride, SuppliedBy the path as typed — never as a bare
// --var. Before the fix, boundDiag built its "supplied by" clause from
// v.Scope.String() alone, which is "--var" for EVERY ScopeCLIOverride value
// regardless of which flag actually supplied it, so this diagnostic named a
// flag the user never passed.
func TestVarFileBoundViolationNamesTheFileNotDashDashVar(t *testing.T) {
	decls := []config.VariableDecl{{
		Name: "size", Type: value.KindInt,
		Max: value.Int(500, value.SourceExplicit), HasMax: true,
		Origin: value.Origin{File: "infra.yml", Line: 7, Column: 10},
	}}
	chain := chainWith(t, "prod", nil)
	files := map[string]value.Value{
		"size": value.Int(9999, value.SourceVariable).
			WithScope(value.ScopeCLIOverride).
			WithOrigin(value.Origin{File: "conf/big.yml", Line: 1, Column: 1}).
			WithSuppliedBy("conf/big.yml"),
	}

	_, ds := Resolve(decls, chain, files, nil, nil)
	if !ds.HasErrors() {
		t.Fatal("size=9999 violates max:500 and must be reported")
	}
	var sb strings.Builder
	ds.Render(&sb)
	out := sb.String()
	if !strings.Contains(out, "supplied by conf/big.yml") {
		t.Errorf("the diagnostic must name the --var-file path that actually supplied the value:\n%s", out)
	}
	if strings.Contains(out, "supplied by --var ") {
		t.Errorf("the diagnostic must not blame --var for a --var-file value:\n%s", out)
	}
}

// TestResolveRefusesDashDashVarNamingEnvironment reproduces M4 final review's
// MAJOR 3: before this fix, a --var naming one of the process-reserved names
// was silently applied here and then silently overwritten moments later by
// compiler.seedProcessVariables (its only caller), with no diagnostic either
// way — `infra plan dev --var environment=production` planned "dev" and said
// nothing about the flag it ignored. Resolve must refuse the flag outright,
// the same way destroy and refresh already refuse --var/--var-file entirely
// (internal/cli/varopts.go's rejectVariableFlags) for the parallel reason
// that the flag cannot change the outcome.
func TestResolveRefusesDashDashVarNamingEnvironment(t *testing.T) {
	chain := chainWith(t, "dev", nil)
	scope, ds := Resolve(nil, chain, nil, nil, map[string]string{"environment": "production"})
	if !ds.HasErrors() {
		t.Fatal("--var environment=... must be refused, not silently applied")
	}
	// It must not have been applied even transiently — the only writer of
	// "environment" is meant to be compiler.seedProcessVariables, later.
	if v, ok := scope.Variable("environment"); ok {
		t.Errorf("environment must not be set by Resolve itself, got %+v", v)
	}
}

// TestResolveRefusesVarFileNamingReservedNames is
// TestResolveRefusesDashDashVarNamingEnvironment's --var-file twin, and
// covers region/account too — nothing else in this file exercises a
// --var-file entry naming any of the three reserved names.
func TestResolveRefusesVarFileNamingReservedNames(t *testing.T) {
	for _, name := range []string{"environment", "region", "account"} {
		files := map[string]value.Value{
			name: value.String("nope", value.SourceVariable).
				WithScope(value.ScopeCLIOverride).
				WithOrigin(value.Origin{File: "conf/vars.yml", Line: 1, Column: 1}).
				WithSuppliedBy("conf/vars.yml"),
		}
		_, ds := Resolve(nil, chainWith(t, "dev", nil), files, nil, nil)
		if !ds.HasErrors() {
			t.Errorf("--var-file setting %q must be refused, not silently applied", name)
		}
	}
}

func TestProcessVariablesMatchesWhatOverrideDocuments(t *testing.T) {
	want := []string{"account", "environment", "region"}
	if len(ProcessVariables) != len(want) {
		t.Fatalf("ProcessVariables = %v, want %v", ProcessVariables, want)
	}
	for i := range want {
		if ProcessVariables[i] != want[i] {
			t.Fatalf("ProcessVariables = %v, want %v (sorted, so consumers need no sort)",
				ProcessVariables, want)
		}
	}
}
