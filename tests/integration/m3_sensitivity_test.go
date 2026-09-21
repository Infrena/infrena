package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers PROPAGATED sensitivity: a value that is secret not because a
// schema says so, but because a secret flowed into it through a reference.
//
// Every test here seeds state by RUNNING APPLY, for the same reason the
// lifecycle tests do. Hand-building a ResourceState with Sensitive: true proves
// only that Format redacts a marked value; it says nothing about whether the
// mark survives crossing the provider boundary, which is where it gets lost.
//
// The literal secret is asserted absent from whole command output, not just from
// the line it is expected on. A redaction test that checks the expected line and
// not the rest measures only the leak it already knows about.

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
	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", environment+".json"))
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

// TestApplySummaryRedactsAPropagatedSecret. The apply summary renders from
// STATE, so it leaks even when the plan preview is correct — the propagated
// flag has to survive into the state file, not merely into the plan.
func TestApplySummaryRedactsAPropagatedSecret(t *testing.T) {
	dir := project(t, secretProject)

	plan := run(t, dir, "plan", "dev")
	requireNoSecret(t, "plan", plan.combined())
	requireContains(t, plan.Stdout, "database_url: <sensitive>")

	res := applied(t, dir)
	requireNoSecret(t, "the apply summary", res.combined())
	requireContains(t, res.Stdout, "database_url: <sensitive>")
	// Schema-declared sensitivity must still work: a change that traded one for
	// the other would pass a test looking only at the propagated half.
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

// TestDestroyPlanRedactsAPropagatedSecret. A destroy renders every attribute of
// everything it is about to delete, from the provider OBSERVATION rather than
// from state — so carrying the flag in state alone still prints the secret in
// clear, from the command most likely to be run with someone watching.
func TestDestroyPlanRedactsAPropagatedSecret(t *testing.T) {
	dir := project(t, secretProject)
	applied(t, dir)

	res := runStdin(t, dir, "no\n", "destroy", "dev")
	requireNoSecret(t, "the destroy plan", res.combined())
	requireContains(t, res.Stdout, "database_url: <sensitive>")
	requireContains(t, res.Stdout, "password: <sensitive>")
}

// TestRefreshDoesNotEraseAPropagatedSensitivity. `infrena refresh` persists
// whatever Provider.Read returns, and a provider re-derives only SCHEMA
// sensitivity — so unless the engine carries the propagated flag onto the
// observation, a refresh ERASES it and the next apply summary leaks.
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
