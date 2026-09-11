package environments

import (
	"strings"
	"testing"

	"infra/internal/config"
	"infra/pkg/value"
)

func override(name string, n int64) config.OverrideDecl {
	return config.OverrideDecl{
		Name:   name,
		Value:  value.Int(n, value.SourceEnvironment),
		Origin: value.Origin{File: "infra.yml", Line: 4, Column: 5},
	}
}

func env(name, extends string, overrides ...config.OverrideDecl) config.EnvironmentDecl {
	return config.EnvironmentDecl{
		Name:          name,
		Extends:       extends,
		ExtendsOrigin: value.Origin{File: "infra.yml", Line: 3, Column: 5},
		Overrides:     overrides,
		Origin:        value.Origin{File: "infra.yml", Line: 2, Column: 3},
	}
}

func TestResolveOrdersAncestorsFirst(t *testing.T) {
	decls := []config.EnvironmentDecl{
		env("base", "", override("replicas", 30)),
		env("middle", "base", override("replicas", 20)),
		env("leaf", "middle", override("replicas", 10)),
	}

	chain, ds := Resolve(decls, "leaf")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if !chain.Selected || chain.Name != "leaf" {
		t.Fatalf("Chain = {Name:%q Selected:%v}, want {leaf true}", chain.Name, chain.Selected)
	}

	var got []string
	for _, l := range chain.Layers {
		got = append(got, l.Name)
	}
	want := []string{"base", "middle", "leaf"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("layers = %v, want %v (ancestors first, so a single forward loop applies precedence)", got, want)
	}
}

func TestResolveMarksInheritedLayersDifferentlyFromTheSelectedOne(t *testing.T) {
	decls := []config.EnvironmentDecl{
		env("base", "", override("replicas", 30)),
		env("middle", "base", override("replicas", 20)),
		env("leaf", "middle", override("replicas", 10)),
	}
	chain, _ := Resolve(decls, "leaf")

	for i, l := range chain.Layers {
		want := value.ScopeEnvironmentInherit
		if i == len(chain.Layers)-1 {
			want = value.ScopeEnvironmentVar
		}
		if l.Scope != want {
			t.Errorf("layer %q scope = %v, want %v (PLAN.md §7 separates environment inheritance from environment variables; the named environment is the latter)", l.Name, l.Scope, want)
		}
	}
}

func TestResolveKeepsOverridesInStageTwosOrder(t *testing.T) {
	// Stage 2 sorted these and gave each its own Origin. Re-sorting here — or
	// routing them through a map and sorting on the way out — would be the
	// twelfth redundant sort in this codebase and would lose the origins.
	decls := []config.EnvironmentDecl{
		env("dev", "", override("alpha", 1), override("beta", 2)),
	}
	chain, _ := Resolve(decls, "dev")
	got := chain.Layers[0].Overrides
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("overrides = %+v, want alpha then beta, unchanged", got)
	}
	if got[0].Origin.Line == 0 {
		t.Error("each override must keep its own origin, or a diagnostic cannot point at the offending line")
	}
}

func TestResolveRejectsSelfExtends(t *testing.T) {
	chain, ds := Resolve([]config.EnvironmentDecl{env("dev", "dev")}, "dev")
	if !ds.HasErrors() {
		t.Fatal("`dev: {extends: dev}` is a one-node cycle and must be reported, not walked forever")
	}
	if chain.Selected || len(chain.Layers) != 0 {
		t.Errorf("a failed Resolve must return an EMPTY chain, not a partial one: %+v", chain)
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "dev -> dev") {
		t.Errorf("the diagnostic must render the full cycle including the wrap:\n%s", sb.String())
	}
}

func TestResolveRejectsATwoCycle(t *testing.T) {
	decls := []config.EnvironmentDecl{env("a", "b"), env("b", "a")}
	_, ds := Resolve(decls, "a")
	if !ds.HasErrors() {
		t.Fatal("a extends b extends a must be reported")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "a -> b -> a") {
		t.Errorf("the diagnostic must name every participant in order:\n%s", sb.String())
	}
}

func TestResolveRejectsExtendsOfAnUndeclaredEnvironment(t *testing.T) {
	decls := []config.EnvironmentDecl{env("dev", "shared"), env("production", "")}
	_, ds := Resolve(decls, "dev")
	if !ds.HasErrors() {
		t.Fatal("`extends: shared` with no `shared` declared must be reported")
	}
	var sb strings.Builder
	ds.Render(&sb)
	for _, want := range []string{"shared", "production", "infra.yml:3:5"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("the diagnostic must name the missing environment, list the ones that exist, and point at the `extends` line; %q missing:\n%s", want, sb.String())
		}
	}
}

func TestResolveRejectsAnUnknownEnvironmentName(t *testing.T) {
	_, ds := Resolve([]config.EnvironmentDecl{env("production", "")}, "prod")
	if !ds.HasErrors() {
		t.Fatal("`infra plan prod` against a project declaring only `production` must be reported")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "production") {
		t.Errorf("the diagnostic must list the environments that DO exist, so the typo is visible:\n%s", sb.String())
	}
}

func TestResolveAcceptsAnyNameWhenNoEnvironmentsAreDeclared(t *testing.T) {
	// M2 plans projects with no environments block at all, and the integration
	// suite depends on it. Adopting environments is what turns a name into a
	// checkable claim.
	chain, ds := Resolve(nil, "dev")
	if ds.HasErrors() {
		t.Fatalf("a project with no environments must still plan: %+v", ds)
	}
	if !chain.Selected || len(chain.Layers) != 1 || chain.Layers[0].Name != "dev" {
		t.Fatalf("chain = %+v, want one synthetic layer named dev", chain)
	}
}

func TestResolveWithNoEnvironmentNameSelectsNothing(t *testing.T) {
	// `infra validate` compiles with the environment left empty.
	chain, ds := Resolve([]config.EnvironmentDecl{env("production", "")}, "")
	if ds.HasErrors() {
		t.Fatalf("`infra validate` has no environment argument and must not fail here: %+v", ds)
	}
	if chain.Selected || len(chain.Layers) != 0 {
		t.Fatalf("chain = %+v, want an unselected, empty chain", chain)
	}
}
