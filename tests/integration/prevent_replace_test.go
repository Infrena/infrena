package integration

import (
	"strings"
	"testing"
)

const replaceableDB = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    lifecycle:
      prevent_replace: true
`

// TestPreventReplaceRefusesAForcedReplacement.
//
// prevent_destroy guards a resource LEAVING configuration. This guards one that
// STAYS in configuration while an attribute the provider marks ForceNew changes
// underneath it — the more insidious of the two, because the configuration
// still names the resource, the diff reads as an edit, and the data is gone all
// the same.
//
// It also fills a hole left on purpose: the environment-wide prevent_destroy
// deliberately does not cover replacements, since refusing every replacement
// would make any immutable attribute unchangeable across a whole environment.
// This is the per-resource answer for the few resources it matters for.
func TestPreventReplaceRefusesAForcedReplacement(t *testing.T) {
	dir := project(t, replaceableDB)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	// engine is ForceNew on fake.database, so changing it forces a replacement.
	writeIn(t, dir, "infrena.yml", strings.Replace(replaceableDB, "engine: postgres", "engine: mysql", 1))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1 — the replacement must be refused\n%s", r.ExitCode, r.combined())
	}
	for _, want := range []string{"prevent_replace", "engine"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the refusal never mentions %q, so the reader cannot tell what forced it:\n%s",
				want, r.combined())
		}
	}
}

// TestPreventReplaceLeavesOrdinaryUpdatesAlone is the half that stops this
// being a resource nobody can change. `size` is not ForceNew, so editing it is
// an update and must go through untouched — a guard that refused every change
// would pass the test above and be useless.
func TestPreventReplaceLeavesOrdinaryUpdatesAlone(t *testing.T) {
	dir := project(t, replaceableDB)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	writeIn(t, dir, "infrena.yml", strings.Replace(replaceableDB,
		"    engine: postgres", "    engine: postgres\n    size: 50", 1))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 — an ordinary update is not a replacement\n%s",
			r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "1 to update") {
		t.Errorf("the update was not planned:\n%s", r.combined())
	}
}

// TestPreventReplaceIsNotPreventDestroy. The two guard different mistakes and
// neither implies the other, so a resource that sets only prevent_replace must
// still be destroyable by removing it from configuration.
func TestPreventReplaceIsNotPreventDestroy(t *testing.T) {
	dir := project(t, replaceableDB)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	writeIn(t, dir, "infrena.yml", `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 — prevent_replace must not block a destroy\n%s",
			r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "1 to destroy") {
		t.Errorf("removing it from configuration did not propose a destroy:\n%s", r.combined())
	}
}
