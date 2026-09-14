package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M10 through the built binary: a map whose values interpolate (PLAN.md §10.1),
// `merge()` with an inline literal (§10.2, §10.3), and the governing rule —
// sensitivity per leaf — at the only place a user meets it.

const interpolatedTags = `
project: MainApp
environments:
  dev: {}
  production: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
    tags:
      environment: ${var.environment}
      project: ${var.project}
      team: payments
`

// TestAMapWhoseValuesInterpolateDiffersPerEnvironment — §10.1's whole purpose.
//
// Both environments are asserted, because one cannot tell "resolved" from
// "resolved correctly": a build that substituted a constant would satisfy either
// on its own.
func TestAMapWhoseValuesInterpolateDiffersPerEnvironment(t *testing.T) {
	dir := project(t, interpolatedTags)

	got := map[string]string{}
	for _, env := range []string{"dev", "production"} {
		p := run(t, dir, "plan", env)
		if p.ExitCode != 2 {
			t.Fatalf("plan %s exit = %d:\n%s", env, p.ExitCode, p.combined())
		}
		got[env] = lineContaining(t, p.Stdout, "tags:")
	}

	for env, line := range got {
		if !strings.Contains(line, `environment: "`+env+`"`) {
			t.Errorf("%s: tags line does not carry its own environment: %s", env, line)
		}
		if !strings.Contains(line, `project: "MainApp"`) {
			t.Errorf("%s: ${var.project} did not resolve: %s", env, line)
		}
		// A leaf with no interpolation survives untouched, alongside ones that
		// resolved.
		if !strings.Contains(line, `team: "payments"`) {
			t.Errorf("%s: the literal leaf was lost: %s", env, line)
		}
		// And nothing reached the plan as raw text, which is the failure the old
		// refusal existed to prevent.
		if strings.Contains(line, "${") {
			t.Errorf("%s: an interpolation reached the plan unresolved: %s", env, line)
		}
	}
	if got["dev"] == got["production"] {
		t.Errorf("both environments produced the same tags, so nothing environment-dependent "+
			"was resolved: %s", got["dev"])
	}
}

// TestMergeCombinesAVariableMapWithAnInlineLiteral, in the spelling §10.3
// documents — quoted, because YAML rejects it otherwise.
func TestMergeCombinesAVariableMapWithAnInlineLiteral(t *testing.T) {
	dir := projectWithFiles(t, `
project: MainApp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
    tags: "${merge(base_tags, {team: payments, project: billing})}"
`, map[string]string{
		"variables.yml": "base_tags:\n  owner: platform\n  team: unassigned\n",
	})

	p := run(t, dir, "plan", "dev")
	if p.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", p.ExitCode, p.combined())
	}
	line := lineContaining(t, p.Stdout, "tags:")

	// The union: a key only the variable has survives.
	if !strings.Contains(line, `owner: "platform"`) {
		t.Errorf("a key only the variable map holds was lost: %s", line)
	}
	// And the LITERAL wins the shared key. Asserting only the union would pass
	// against a merge that folded the wrong way.
	if !strings.Contains(line, `team: "payments"`) {
		t.Errorf("team is not the literal's value, so the later argument did not win: %s", line)
	}
	if strings.Contains(line, "unassigned") {
		t.Errorf("the variable's value for a shared key survived: %s", line)
	}
	if !strings.Contains(line, `project: "billing"`) {
		t.Errorf("a key only the literal holds was lost: %s", line)
	}
}

// TestASecretInsideAMapIsRedactedAndNeverExported is this milestone's governing
// rule at the only place a user meets it.
//
// It has to go through a real APPLY. At compile time a reference to a sensitive
// attribute is a plain unknown — sensitivity arrives with the provider's own
// value — which is why the compiler-level version of this test was removed in
// Task 2 rather than weakened.
func TestASecretInsideAMapIsRedactedAndNeverExported(t *testing.T) {
	const secret = "hunter2-correct-horse-battery"
	dir := project(t, `
project: MainApp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: fake.database
    engine: postgres
    network: ${net.id}
    password: `+secret+`
  replica:
    type: fake.database
    engine: postgres
    network: ${net.id}
    tags:
      team: payments
      inherited: ${primary.password}
`)

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d:\n%s", a.ExitCode, a.combined())
	}

	// The plan after apply reads the REAL values back out of state, which is
	// where the secret actually is.
	p := run(t, dir, "plan", "dev")
	if strings.Contains(p.combined(), secret) {
		t.Fatalf("the secret is in the plan in clear:\n%s", p.combined())
	}
	// And export, which §28 says carries the same sensitivity rule.
	e := run(t, dir, "export", "dev")
	if e.ExitCode != 0 {
		t.Fatalf("export exit = %d:\n%s", e.ExitCode, e.combined())
	}
	if strings.Contains(e.combined(), secret) {
		t.Fatalf("the secret is in the export:\n%s", e.combined())
	}
	// The OTHER half: the non-secret leaf must still be visible, or the whole map
	// was redacted and a reader has lost information they need.
	if !strings.Contains(e.combined(), "payments") {
		t.Errorf("the whole tags map was redacted, hiding a key the reader needs:\n%s", e.combined())
	}
}

// TestAnEdgeInsideAMapOrdersTheApply. A dependency recorded from inside a map
// must reach the EXECUTOR, not only the plan: a missing edge here is a clean plan
// and an apply that fails partway, which is the defect M5's integration suite
// found twice.
func TestAnEdgeInsideAMapOrdersTheApply(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: 10.0.0.0/16
    tags:
      network_id: ${net.id}
`)

	a := run(t, dir, "apply", "dev", "--auto-approve")
	if a.ExitCode != 2 {
		t.Fatalf("apply exit = %d — an edge from inside a map did not order the executor:\n%s",
			a.ExitCode, a.combined())
	}
	// Converged, which is what proves the unknown inside the map was resolved
	// rather than left as text.
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Errorf("does not converge; re-plan exit = %d:\n%s", again.ExitCode, again.combined())
	}
	// And the resolved value is the network's real ID, not the expression.
	line := lineContaining(t, run(t, dir, "export", "dev").Stdout, "network_id:")
	if strings.Contains(line, "${") {
		t.Errorf("the leaf stayed unresolved through an apply: %s", line)
	}
}

// TestAnUnquotedLiteralSaysToQuoteIt — §10.3. YAML rejects the shape before this
// project's code sees it, with a message that says nothing about quoting.
func TestAnUnquotedLiteralSaysToQuoteIt(t *testing.T) {
	dir := project(t, `
project: MainApp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
    tags: ${merge(a, {b: c})}
`)
	r := run(t, dir, "validate")
	if r.ExitCode == 0 {
		t.Fatalf("YAML rejects this shape:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "quotes") {
		t.Errorf("the error does not mention quoting, which is the only thing that fixes it:\n%s",
			r.combined())
	}
}

// TestTheShopExampleUsesAnInterpolatedTagMap keeps the worked example current
// with the language. An example nobody runs rots, and this one is the first thing
// a reader copies.
func TestTheShopExampleUsesAnInterpolatedTagMap(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("..", "..", "examples", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(src, "resources", "app", "app.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "tags:") {
		t.Error("the example does not demonstrate an interpolated tag map, which is M10's " +
			"whole user-visible feature")
	}
}
