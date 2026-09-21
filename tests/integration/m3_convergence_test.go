package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers convergence — desired == actual proposes zero operations —
// for configurations where one resource references another's computed attribute.
//
// It lives at the integration level on purpose. The failure only appears once a
// REAL provider schema (one with a computed attribute, such as fake.network's
// provider-assigned "id") meets a real reference across a real apply. Nothing
// below the CLI puts those three together, so nothing below the CLI can catch a
// plan that proposes the same phantom update after every apply, forever.

// referencingProject is the smallest configuration that exhibits it: a
// database whose network attribute is a reference to a network's computed id.
const referencingProject = `
project: conv
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`

// writeProject rewrites an existing project's infrena.yml. project() makes a
// fresh temporary directory each time, which is no use for the change-then-plan
// sequences below: they need the state and fake cloud the previous apply left
// behind.
func writeProject(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("rewrite infrena.yml: %v", err)
	}
}

// applied runs an apply and fails the test unless every operation succeeded.
// It asserts on the apply's own report rather than on its exit code, which
// distinguishes "there were changes" from "there were none" and is therefore
// not a success signal.
func applied(t *testing.T, dir string) result {
	t.Helper()
	res := run(t, dir, "apply", "dev", "--auto-approve")
	if !strings.Contains(res.combined(), "Apply complete") {
		t.Fatalf("apply did not complete (exit %d):\n%s", res.ExitCode, res.combined())
	}
	if strings.Contains(res.combined(), "failed") && !strings.Contains(res.combined(), "0 failed") {
		t.Fatalf("apply reported failures (exit %d):\n%s", res.ExitCode, res.combined())
	}
	return res
}

// TestApplyThenPlanConvergesAcrossAReference pins that a reference to a computed
// attribute is finished after apply. Left bound to a compile-time unknown, it
// proposes `network: "net-1" -> (known after apply)` after every apply, forever.
//
// It applies and re-plans three times rather than once: resolving the reference
// against a stale snapshot can converge on the second plan and diverge again on
// the third, and a single round trip would call that fixed.
func TestApplyThenPlanConvergesAcrossAReference(t *testing.T) {
	dir := project(t, referencingProject)

	applied(t, dir)

	for round := 1; round <= 3; round++ {
		res := run(t, dir, "plan", "dev")
		if res.ExitCode != 0 {
			t.Fatalf("round %d: plan exit code %d, want 0 — desired == actual must propose nothing:\n%s",
				round, res.ExitCode, res.combined())
		}
		requireContains(t, res.Stdout, "No changes")
		if strings.Contains(res.Stdout, "known after apply") {
			t.Fatalf("round %d: plan still defers an attribute that state already records:\n%s", round, res.Stdout)
		}

		// The second and later applies must be no-ops that exit 0. An apply
		// that keeps "converging" the same resource forever exits 2 here.
		again := run(t, dir, "apply", "dev", "--auto-approve")
		if again.ExitCode != 0 {
			t.Fatalf("round %d: re-apply exit code %d, want 0 (nothing to do):\n%s",
				round, again.ExitCode, again.combined())
		}
		if strings.Contains(again.combined(), "1 applied") {
			t.Fatalf("round %d: re-apply applied something on an unchanged configuration:\n%s", round, again.combined())
		}
	}
}

// TestApplyThenPlanConvergesAcrossAChainOfReferences is the same property one
// hop further out: application interpolates the database's computed endpoint,
// and the database references the network's computed id. Resolving only a
// resource's direct dependencies converges the first hop and leaves this one
// proposing an update forever.
func TestApplyThenPlanConvergesAcrossAChainOfReferences(t *testing.T) {
	dir := project(t, `
project: conv
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
  application:
    type: fake.application
    image: web:1
    database_url: postgres://${database.endpoint}/app
`)

	applied(t, dir)

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("plan exit code %d, want 0 — a re-plan after apply must propose no changes, here across a reference chain:\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "No changes")
}

// TestPlanOnUnappliedConfigurationStillDefersItsReferences is the constraint the
// convergence tests must not trade away: finishing references at plan time must
// never make a plan claim to know something it cannot. With nothing applied the
// network does not exist, so its id and the database attribute referring to it
// must both still render "(known after apply)".
//
// It also applies afterwards, because resolving a not-yet-created dependency's
// staged unknown as though it were an answer prints correctly here while
// stripping the expression apply needs — and the database is then created with
// no network at all.
func TestPlanOnUnappliedConfigurationStillDefersItsReferences(t *testing.T) {
	dir := project(t, referencingProject)

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2 (there are changes):\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "2 to create")
	requireContains(t, res.Stdout, "id: (known after apply)")
	requireContains(t, res.Stdout, "network: (known after apply)")

	out := applied(t, dir).combined()
	// The network's real, provider-assigned id must have reached the database.
	requireContains(t, out, `network: "net-1"`)

	after := run(t, dir, "plan", "dev")
	if after.ExitCode != 0 {
		t.Fatalf("plan after apply: exit code %d, want 0:\n%s", after.ExitCode, after.combined())
	}
}

// TestPlanShowsAResolvedReferenceForANewlyAddedResource covers adding a resource
// to a configuration that has already been applied. Its dependency exists and is
// unchanged, so the plan can and must show the real value it will be created
// with rather than deferring it.
//
// Asserted separately from convergence because it fails as an under-reported
// plan rather than a broken apply: the executor finishes the reference either
// way.
func TestPlanShowsAResolvedReferenceForANewlyAddedResource(t *testing.T) {
	dir := project(t, `
project: conv
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	applied(t, dir)

	writeProject(t, dir, referencingProject)

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2 (the database is new):\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "1 to create")
	requireContains(t, res.Stdout, `network: "net-1"`)
	if strings.Contains(res.Stdout, "network: (known after apply)") {
		t.Errorf("plan deferred a reference to a network that already exists and is not changing:\n%s", res.Stdout)
	}
	// The database's own computed attribute is still genuinely unknown.
	requireContains(t, res.Stdout, "endpoint: (known after apply)")

	applied(t, dir)
	after := run(t, dir, "plan", "dev")
	if after.ExitCode != 0 {
		t.Fatalf("plan after adding a resource: exit code %d, want 0:\n%s", after.ExitCode, after.combined())
	}
}

// TestPlanDefersAReferenceWhoseDependencyIsBeingReplaced covers why the
// planner resolves a reference against what the plan says its dependency WILL
// be, not against what state records it was.
//
// fake.network's cidr is ForceNew, so changing it replaces the network and its
// id will not survive. A planner that answered ${network.id} from the recorded
// state would report the database unchanged, then leave it pointing at a
// network that no longer exists — and the plan after that apply would propose
// the update all over again.
func TestPlanDefersAReferenceWhoseDependencyIsBeingReplaced(t *testing.T) {
	dir := project(t, referencingProject)
	applied(t, dir)

	// Same project, different cidr: a replacement of the network.
	writeProject(t, dir, `
project: conv
resources:
  network:
    type: fake.network
    cidr: 10.1.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`)

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("plan exit code %d, want 2 (the network is being replaced):\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "1 to replace")
	requireContains(t, res.Stdout, "network: \"net-1\" -> (known after apply)")

	applied(t, dir)

	after := run(t, dir, "plan", "dev")
	if after.ExitCode != 0 {
		t.Fatalf("plan after replacing a referenced resource: exit code %d, want 0:\n%s", after.ExitCode, after.combined())
	}
	requireContains(t, after.Stdout, "No changes")
}

// TestPlanAcrossAReferenceIsDeterministic. Reference resolution walks maps of
// resources and maps of attributes, and Go randomises map iteration, so this is
// a property to measure repeatedly rather than assume — twelve runs, because a
// two-resource ordering bug passes a single run about half the time.
func TestPlanAcrossAReferenceIsDeterministic(t *testing.T) {
	dir := project(t, referencingProject)
	applied(t, dir)

	// Only the PLAN is required to be stable, and it begins at the "Plan for
	// project" line. Everything before it is progress, written as each provider
	// read COMPLETES; the refresh runs those reads concurrently, so their order
	// is not byte-stable and never claimed to be.
	planOf := func(stdout string) string {
		if i := strings.Index(stdout, "Plan for project"); i >= 0 {
			return stdout[i:]
		}
		return stdout
	}

	first := run(t, dir, "plan", "dev")
	for i := 2; i <= 12; i++ {
		next := run(t, dir, "plan", "dev")
		if next.ExitCode != first.ExitCode {
			t.Fatalf("run %d: exit code %d, first run %d", i, next.ExitCode, first.ExitCode)
		}
		if planOf(next.Stdout) != planOf(first.Stdout) {
			t.Fatalf("run %d differs from the first — two identical runs must produce identical output:\n--- first ---\n%s\n--- run %d ---\n%s",
				i, first.Stdout, i, next.Stdout)
		}
	}
}
