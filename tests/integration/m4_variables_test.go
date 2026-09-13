package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// projectWithFiles writes infra.yml plus any extra files, keyed by path
// relative to the project directory.
func projectWithFiles(t *testing.T, body string, extra map[string]string) string {
	t.Helper()
	dir := project(t, body)
	for rel, content := range extra {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

func TestPlanResolvesVariablesFromFilesEnvironmentsAndFlags(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
variables:
  replicas:
    type: integer
    default: 50
    min: 1
    max: 100
environments:
  base:
    replicas: 30
  production:
    extends: base
    replicas: 20
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
    size: ${replicas}
`, map[string]string{
		"variables.yml": "replicas: 40\n",
	})

	// The environment beats variables.yml, which beats the declared default.
	// This is a fresh project, so the plan proposes creates (exit code 2 —
	// spec §16 — not a failure); an actual failure would be exit code 1.
	res := run(t, dir, "plan", "production")
	if res.ExitCode != 2 {
		t.Fatalf("plan failed:\n%s", res.combined())
	}
	requireContains(t, res.combined(), "20")

	// --var beats all of them. Asserted as "size: 10" rather than bare "10":
	// the fixture also sets cidr: 10.0.0.0/16, which renders verbatim on
	// every one of these plans regardless of what replicas resolves to, so a
	// bare "10" is satisfied by that unrelated line and would not catch
	// --var being silently dropped.
	res = run(t, dir, "plan", "production", "--var", "replicas=10")
	if res.ExitCode != 2 {
		t.Fatalf("plan failed:\n%s", res.combined())
	}
	requireContains(t, res.combined(), "size: 10")

	// And a value outside the declared range is refused before anything runs.
	res = run(t, dir, "plan", "production", "--var", "replicas=500")
	if res.ExitCode == 0 || res.ExitCode == 2 {
		t.Fatalf("replicas=500 violates max: 100 and must fail:\n%s", res.combined())
	}
	requireContains(t, res.combined(), "replicas")
}

func TestPlanNamesTheEnvironmentItIsPlanning(t *testing.T) {
	// fake.network has no `name` attribute (providers/test/definitions.go), so
	// the synthetic ${environment} is exercised through `cidr` instead — the
	// point under test is that the variable resolves, not which attribute
	// carries it.
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: net-${environment}
`)
	res := run(t, dir, "plan", "production")
	if res.ExitCode != 2 {
		t.Fatalf("plan failed:\n%s", res.combined())
	}
	requireContains(t, res.combined(), "net-production")
}
