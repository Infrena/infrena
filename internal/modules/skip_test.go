package modules

import (
	"maps"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/value"
)

// PLAN.md §6.2, stage 5: resolve `skip`/`only` and MARK. The dropping is Task 7's.

// declIn writes a one-file project and runs stages 1 and 2 over it, failing if
// either rejects the fixture — see `fixture`, whose guard this reuses.
func declIn(t *testing.T, body string, extra map[string]string) (*config.ProjectDecl, string) {
	t.Helper()
	files := map[string]string{"infra.yml": body}
	maps.Copy(files, extra)
	return fixture(t, files)
}

func stringVal(s string) value.Value { return value.String(s, value.SourceVariable) }

func listOf(items ...string) value.Value {
	vs := make([]value.Value, len(items))
	for i, s := range items {
		vs[i] = stringVal(s)
	}
	return value.List(vs, value.SourceVariable)
}

func expandIn(t *testing.T, env Env, body string, extra map[string]string) (*Expansion, string) {
	t.Helper()
	decl, dir := declIn(t, body, extra)
	exp, ds := Expand(decl, variables.Scope{}, nil, env, dir, paths{})
	var sb strings.Builder
	ds.Render(&sb)
	return exp, sb.String()
}

func addressesOf(exp *Expansion) []string {
	var out []string
	for _, i := range exp.Instances {
		if i.Skipped {
			continue
		}
		out = append(out, i.Address.String())
	}
	return out
}

const twoEnvs = `
project: p
environments:
  dev: {}
  production: {}
`

// TestOnlyKeepsAResourceInItsNamedEnvironments, and drops it elsewhere. BOTH
// directions in one test, because a filter checked in one environment cannot
// tell "kept correctly" from "kept everywhere".
func TestOnlyKeepsAResourceInItsNamedEnvironments(t *testing.T) {
	body := twoEnvs + `
resources:
  always:
    type: test.network
    cidr: 10.0.0.0/16
  prod_only:
    type: test.network
    cidr: 10.1.0.0/16
    only: production
`
	prod, out := expandIn(t, Env{Name: "production", Declared: []string{"dev", "production"}}, body, nil)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if got := strings.Join(addressesOf(prod), ","); got != "always,prod_only" {
		t.Errorf("production has %v, want both resources", addressesOf(prod))
	}

	dev, out := expandIn(t, Env{Name: "dev", Declared: []string{"dev", "production"}}, body, nil)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if got := strings.Join(addressesOf(dev), ","); got != "always" {
		t.Errorf("dev has %v, want only the unfiltered resource", addressesOf(dev))
	}
}

// TestSkipRemovesAResourceFromItsNamedEnvironments — the other key, both
// directions again.
func TestSkipRemovesAResourceFromItsNamedEnvironments(t *testing.T) {
	body := twoEnvs + `
resources:
  always:
    type: test.network
    cidr: 10.0.0.0/16
  not_in_dev:
    type: test.network
    cidr: 10.1.0.0/16
    skip: [dev]
`
	dev, _ := expandIn(t, Env{Name: "dev", Declared: []string{"dev", "production"}}, body, nil)
	if got := strings.Join(addressesOf(dev), ","); got != "always" {
		t.Errorf("dev has %v, want the skipped resource gone", addressesOf(dev))
	}
	prod, _ := expandIn(t, Env{Name: "production", Declared: []string{"dev", "production"}}, body, nil)
	if got := strings.Join(addressesOf(prod), ","); got != "always,not_in_dev" {
		t.Errorf("production has %v, want both", addressesOf(prod))
	}
}

// TestASkippedResourceIsMarkedNotDropped is the rule that governs this
// milestone. A resource that vanished here makes Task 6 impossible, and the
// failure lands on a user as "no such resource" for a name in front of them.
func TestASkippedResourceIsMarkedNotDropped(t *testing.T) {
	exp, _ := expandIn(t, Env{Name: "dev", Declared: []string{"dev", "production"}}, twoEnvs+`
resources:
  gone:
    type: test.network
    cidr: 10.0.0.0/16
    only: production
`, nil)

	var found *Instance
	for i := range exp.Instances {
		if exp.Instances[i].Address.Name == "gone" {
			found = &exp.Instances[i]
		}
	}
	if found == nil {
		t.Fatal("the skipped resource is absent from the expansion; stage 6 cannot then " +
			"distinguish it from a name that was never declared")
	}
	if !found.Skipped {
		t.Error("the instance is present but not marked skipped")
	}
	// The origin travels, so stage 6 can point at the line that excluded it.
	if found.SkipOrigin.File == "" {
		t.Error("no SkipOrigin, so a diagnostic cannot say which key excluded it")
	}
}

// TestAnUnknownEnvironmentNameIsAnError. `skip: [prod]` against an environment
// called `production` would otherwise match nothing, in every environment,
// forever, with nothing to notice.
func TestAnUnknownEnvironmentNameIsAnError(t *testing.T) {
	_, out := expandIn(t, Env{Name: "dev", Declared: []string{"dev", "production"}}, twoEnvs+`
resources:
  a:
    type: test.network
    cidr: 10.0.0.0/16
    skip: [prod]
`, nil)
	if out == "" {
		t.Fatal("`skip: [prod]` against a `production` environment must be refused")
	}
	if !strings.Contains(out, "prod") {
		t.Errorf("the diagnostic does not name the bad entry:\n%s", out)
	}
	// The declared names, so the user can see what they meant.
	if !strings.Contains(out, "production") {
		t.Errorf("the diagnostic does not list the declared environments:\n%s", out)
	}
}

// TestAProjectWithNoEnvironmentsChecksNothing — the concession
// environments.Resolve has made since M2, applied here too. With nothing
// declared there is nothing for a name to be wrong against.
func TestAProjectWithNoEnvironmentsChecksNothing(t *testing.T) {
	_, out := expandIn(t, Env{Name: "dev"}, `
project: p
resources:
  a:
    type: test.network
    cidr: 10.0.0.0/16
    skip: [anything]
`, nil)
	if out != "" {
		t.Errorf("a project declaring no environments must not have its filters checked:\n%s", out)
	}
}

// TestSkipMayBeAnExpression, resolving to a list and to a scalar. This is what
// §6.2 exists to allow, not a generalisation of it.
func TestSkipMayBeAnExpression(t *testing.T) {
	scope := variables.Scope{}
	scope.Override("targets", listOf("production"))
	scope.Override("one", stringVal("production"))

	for _, attr := range []string{"only: ${targets}", "only: ${one}"} {
		decl, dir := declIn(t, twoEnvs+`
resources:
  a:
    type: test.network
    cidr: 10.0.0.0/16
    `+attr+"\n", nil)
		exp, ds := Expand(decl, scope, nil, Env{Name: "dev", Declared: []string{"dev", "production"}}, dir, paths{})
		if ds.HasErrors() {
			var sb strings.Builder
			ds.Render(&sb)
			t.Fatalf("%s: %s", attr, sb.String())
		}
		if got := addressesOf(exp); len(got) != 0 {
			t.Errorf("%s: dev kept %v, want the resource excluded", attr, got)
		}
	}
}

// TestAFilterThatDependsOnAResourceIsAnError.
//
// The input matters, and the first version of this test used
// `only: ${nosuchvariable}` — which errors during EVALUATION, from the
// expression package, so the unknown-value guard it meant to exercise never ran
// and deleting that guard changed nothing.
//
// A reference to a resource ATTRIBUTE is the case that reaches it: at compile
// time no resource exists, so it evaluates to an unknown rather than to an
// error. The filter decides whether a resource exists at all, so it has to be
// known before anything is planned — and quietly matching nothing would include
// the resource in every environment, which is the direction that DEPLOYS things.
func TestAFilterThatDependsOnAResourceIsAnError(t *testing.T) {
	_, out := expandIn(t, Env{Name: "dev", Declared: []string{"dev", "production"}}, twoEnvs+`
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  a:
    type: test.network
    cidr: 10.1.0.0/16
    only: ${net.id}
`, nil)
	if out == "" {
		t.Fatal("an `only` that depends on a resource attribute must be reported, not treated " +
			"as an empty list")
	}
	if !strings.Contains(out, "only") {
		t.Errorf("the diagnostic does not name the key:\n%s", out)
	}
}

// TestAnUndefinedVariableInAFilterIsAnError — the neighbouring case, reported by
// the expression package rather than by the guard above. Kept because it is a
// real user mistake, and separate because they are different code paths.
func TestAnUndefinedVariableInAFilterIsAnError(t *testing.T) {
	_, out := expandIn(t, Env{Name: "dev", Declared: []string{"dev", "production"}}, twoEnvs+`
resources:
  a:
    type: test.network
    cidr: 10.0.0.0/16
    only: ${nosuchvariable}
`, nil)
	if out == "" {
		t.Fatal("an `only` naming an undefined variable must be reported")
	}
}

// TestACompositeModuleInputIsEvaluatedNotDropped — PLAN.md §10.1 at a call site.
//
// A module call's attributes are parsed into one expression tree EACH, and a
// composite has none of its own, so its leaves must be walked separately. When
// the composite walk first landed this branch silently dropped the attribute and
// the module's own default won — a comment promised the leaves were evaluated
// elsewhere and nothing did it. The shop example caught it; this pins it.
func TestACompositeModuleInputIsEvaluatedNotDropped(t *testing.T) {
	scope := variables.Scope{}
	scope.Override("who", stringVal("platform"))

	decl, dir := declIn(t, twoEnvs+`
modules:
  - ./modules/tagged
resources:
  stack:
    type: module.tagged
    tags:
      owner: ${who}
      team: storefront
`, map[string]string{
		"modules/tagged/module.yml": `
inputs:
  tags:
    type: map
    default: {}
resources:
  db:
    type: test.database
    engine: postgres
    tags: ${tags}
`,
	})

	exp, ds := Expand(decl, scope, nil, Env{Name: "dev", Declared: []string{"dev", "production"}}, dir, paths{})
	if ds.HasErrors() {
		var sb strings.Builder
		ds.Render(&sb)
		t.Fatalf("unexpected diagnostics:\n%s", sb.String())
	}

	var inner *Instance
	for i := range exp.Instances {
		if exp.Instances[i].Address.String() == "module.stack.db" {
			inner = &exp.Instances[i]
		}
	}
	if inner == nil {
		t.Fatal("no module.stack.db")
	}
	tags, ok := inner.Scope.Vars.Variable("tags")
	if !ok {
		t.Fatal("the module did not receive a `tags` input at all")
	}
	m, ok := tags.Raw.(map[string]value.Value)
	if !ok || len(m) != 2 {
		t.Fatalf("the caller's map did not arrive; the module's default won instead: %#v", tags.Raw)
	}
	// The interpolated leaf RESOLVED, in the caller's scope.
	if got, _ := m["owner"].AsString(); got != "platform" {
		t.Errorf("owner = %v, want the caller's variable resolved", m["owner"])
	}
	if got, _ := m["team"].AsString(); got != "storefront" {
		t.Errorf("team = %v, want the literal leaf", m["team"])
	}
}
