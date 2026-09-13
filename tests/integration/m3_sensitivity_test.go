package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers spec §36 for PROPAGATED sensitivity: a value that is secret
// not because a schema says so, but because a secret flowed into it through a
// reference.
//
// Every test here seeds state by RUNNING APPLY, for the same reason the
// lifecycle tests do (see m3_lifecycle_test.go's header). A fixture that
// hand-builds a ResourceState with Sensitive: true proves that Format redacts
// a marked value — which was never in doubt — and says nothing about whether a
// marked value ever reaches it. It did not: the flag was lost crossing the
// provider boundary, and the apply summary printed the secret in clear.
//
// The literal secret is asserted absent from whole command output, not just
// from the one line it was known to appear on. A redaction test that checks
// the expected line and not the rest measures the leak it already knows about.

const secretProject = `
project: leak
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    password: hunter2
  app:
    type: fake.application
    image: nginx
    database_url: ${db.password}
`

const theSecret = "hunter2"

// stateAttrSensitive reports the sensitive flag the state FILE records for one
// attribute, plus the raw value beside it. Reading the file rather than an
// in-process type is the point: this is the record every later command renders
// from.
func stateAttrSensitive(t *testing.T, dir, environment, name, attr string) (sensitive bool, raw any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", environment+".json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var doc struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				Sensitive bool `json:"sensitive"`
				Raw       any  `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	r, ok := doc.Resources[name]
	if !ok {
		t.Fatalf("resource %q not found in state", name)
	}
	a, ok := r.Attributes[attr]
	if !ok {
		t.Fatalf("attribute %q not found on %q", attr, name)
	}
	return a.Sensitive, a.Raw
}

func requireNoSecret(t *testing.T, what, output string) {
	t.Helper()
	if strings.Contains(output, theSecret) {
		t.Fatalf("%s printed the secret in clear:\n%s", what, output)
	}
}

// TestApplySummaryRedactsAPropagatedSecret is the regression test for the
// reported leak. The plan preview was already correct; the summary rendered
// from state, and state no longer knew.
func TestApplySummaryRedactsAPropagatedSecret(t *testing.T) {
	dir := project(t, secretProject)

	plan := run(t, dir, "plan", "dev")
	requireNoSecret(t, "plan", plan.combined())
	requireContains(t, plan.Stdout, "database_url: <sensitive>")

	res := applied(t, dir)
	requireNoSecret(t, "the apply summary", res.combined())
	requireContains(t, res.Stdout, "database_url: <sensitive>")
	// Schema-declared sensitivity must still work; it is the half that was
	// never broken, and a fix that traded one for the other would pass a test
	// that only looked at the propagated one.
	requireContains(t, res.Stdout, "password: <sensitive>")

	// The flag survives into state, so everything that renders from state
	// later redacts too.
	sensitive, raw := stateAttrSensitive(t, dir, "dev", "app", "database_url")
	if !sensitive {
		t.Fatalf("state records app.database_url as not sensitive (raw %v) — "+
			"propagated sensitivity was lost crossing the provider boundary", raw)
	}
	if sensitive, _ := stateAttrSensitive(t, dir, "dev", "db", "password"); !sensitive {
		t.Fatalf("state records db.password as not sensitive")
	}

	// `state show` is one of those later readers.
	show := run(t, dir, "state", "show", "dev", "app")
	requireNoSecret(t, "state show", show.combined())
	requireContains(t, show.Stdout, "database_url <sensitive>")
}

// TestDestroyPlanRedactsAPropagatedSecret covers the surface the original
// report did not name. A destroy renders every attribute of everything it is
// about to delete, and it renders them from the provider OBSERVATION rather
// than from state — so fixing state alone would still have printed the secret
// in clear, from the one command most likely to be run with someone watching.
func TestDestroyPlanRedactsAPropagatedSecret(t *testing.T) {
	dir := project(t, secretProject)
	applied(t, dir)

	res := runStdin(t, dir, "no\n", "destroy", "dev")
	requireNoSecret(t, "the destroy plan", res.combined())
	requireContains(t, res.Stdout, "database_url: <sensitive>")
	requireContains(t, res.Stdout, "password: <sensitive>")
}

// TestRefreshDoesNotEraseAPropagatedSensitivity is this bug's version of the
// lifecycle refresh hole. `infra refresh` persists whatever Provider.Read
// returns, and a provider re-derives only SCHEMA sensitivity — so without the
// engine carrying the propagated flag onto the observation, a refresh would
// not leave the flag stale, it would erase it, and the next apply summary
// would leak again.
func TestRefreshDoesNotEraseAPropagatedSensitivity(t *testing.T) {
	dir := project(t, secretProject)
	applied(t, dir)

	if res := run(t, dir, "refresh", "dev"); res.ExitCode != 0 {
		t.Fatalf("refresh exit code %d, want 0:\n%s", res.ExitCode, res.combined())
	}
	requireNoSecret(t, "refresh", run(t, dir, "refresh", "dev").combined())

	if sensitive, raw := stateAttrSensitive(t, dir, "dev", "app", "database_url"); !sensitive {
		t.Fatalf("refresh erased the propagated sensitivity from state (raw %v)", raw)
	}
	show := run(t, dir, "state", "show", "dev", "app")
	requireNoSecret(t, "state show after refresh", show.combined())
}

// TestApplyDoesNotClassifyValuesThatAreNotSensitive pins the other direction.
// Redacting everything would satisfy every test above and destroy the product:
// a plan whose values are all <sensitive> tells a user nothing, and a marker
// that appears everywhere is one nobody reads.
func TestApplyDoesNotClassifyValuesThatAreNotSensitive(t *testing.T) {
	dir := project(t, secretProject)
	res := applied(t, dir)

	// Plain values, including one on the SAME resource as the propagated
	// secret and one computed by the provider after the secret passed through
	// it, still render in clear.
	for _, want := range []string{`image: "nginx"`, `cidr: "10.0.0.0/16"`, `engine: "postgres"`} {
		requireContains(t, res.Stdout, want)
	}
	if !strings.Contains(res.Stdout, `url: "https://`) {
		t.Errorf("a provider-computed url was redacted:\n%s", res.Stdout)
	}

	for _, attr := range []struct{ resource, name string }{
		{"app", "image"}, {"app", "url"}, {"app", "replicas"},
		{"db", "engine"}, {"db", "endpoint"}, {"net", "cidr"},
	} {
		if sensitive, _ := stateAttrSensitive(t, dir, "dev", attr.resource, attr.name); sensitive {
			t.Errorf("state classifies %s.%s as sensitive; nothing made it so", attr.resource, attr.name)
		}
	}
}
