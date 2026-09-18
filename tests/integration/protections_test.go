package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

const protectedProject = `
project: myapp
environments:
  dev: {}
  production:
    require_approval: true
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`

// TestRequireApprovalRefusesAutoApprove is §38's headline, and the reason the
// refusal has to exist at all: a protection a flag can switch off is not one,
// because the flag is a line in the pipeline that was going to run anyway.
//
// Exit 77 rather than 1: this is "changes need an approval this run cannot
// obtain", which is exactly what §37.1 reserves 77 for, and a pipeline that
// already branches on 77 needs no new number.
func TestRequireApprovalRefusesAutoApprove(t *testing.T) {
	dir := project(t, protectedProject)

	r := run(t, dir, "apply", "production", "--auto-approve")
	if r.ExitCode != 77 {
		t.Fatalf("apply exit = %d, want 77\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	for _, want := range []string{"require_approval", "--auto-approve is refused", "--plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal never mentions %q, so the reader cannot act on it:\n%s", want, out)
		}
	}

	// The same project's UNPROTECTED environment is untouched. Without this,
	// a refusal that fired for every environment would pass the test above.
	if d := run(t, dir, "apply", "dev", "--auto-approve"); d.ExitCode != 2 {
		t.Errorf("apply dev exit = %d, want 2 — dev declares no protection\n%s", d.ExitCode, d.combined())
	}
}

// TestASavedPlanIsTheApprovalOnAProtectedEnvironment is the escape the whole
// design rests on, and the one that makes CI possible.
//
// A saved plan is a STRONGER approval than typing "yes": it is applied without
// recompiling and is refused outright if state moved since it was made, so
// what runs is what a person read. That is why --auto-approve is accepted here
// and refused above — the flag is not the approval, the plan is.
func TestASavedPlanIsTheApprovalOnAProtectedEnvironment(t *testing.T) {
	dir := project(t, protectedProject)
	artifact := filepath.Join(t.TempDir(), "plan.json")

	if p := run(t, dir, "plan", "production", "--output", artifact); p.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", p.ExitCode, p.combined())
	}
	a := run(t, dir, "apply", "production", "--plan", artifact, "--auto-approve")
	if a.ExitCode != 2 {
		t.Fatalf("applying a reviewed plan to a protected environment exit = %d, want 2\n%s",
			a.ExitCode, a.combined())
	}
	requireContains(t, a.Stdout, "Apply complete:")
}

// TestNoChangesIsNotRefused. There is nothing to approve when the plan is
// empty, so a protected environment must not fail a no-op run — otherwise
// every pipeline that applies on a schedule turns red the moment it catches up
// with configuration.
func TestNoChangesIsNotRefused(t *testing.T) {
	dir := project(t, protectedProject)
	artifact := filepath.Join(t.TempDir(), "plan.json")
	run(t, dir, "plan", "production", "--output", artifact)
	if a := run(t, dir, "apply", "production", "--plan", artifact, "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("first apply: %d\n%s", a.ExitCode, a.combined())
	}

	second := run(t, dir, "apply", "production", "--auto-approve")
	if second.ExitCode != 0 {
		t.Errorf("a no-op apply on a protected environment exit = %d, want 0 — "+
			"there is nothing to approve\n%s", second.ExitCode, second.combined())
	}
}

// TestPreventDestroyRefusesAtPlanTime. Same timing as the resource-level
// sibling, and for the reason that one gives: the refusal must arrive before
// the approval, not after it.
func TestPreventDestroyRefusesAtPlanTime(t *testing.T) {
	dir := project(t, `
project: myapp
environments:
  production:
    prevent_destroy: true
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if a := run(t, dir, "apply", "production", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding apply: %d\n%s", a.ExitCode, a.combined())
	}

	// destroy, whose whole premise is an empty configuration, is refused.
	d := run(t, dir, "destroy", "production", "--auto-approve")
	if d.ExitCode == 0 {
		t.Fatalf("destroy succeeded against prevent_destroy:\n%s", d.combined())
	}
	if !strings.Contains(d.combined(), "prevent_destroy") {
		t.Errorf("the refusal does not name the setting that caused it:\n%s", d.combined())
	}

	// And the resource is still there afterwards, which is the property that
	// actually matters. A refusal that printed and destroyed anyway would pass
	// every assertion above.
	if s := run(t, dir, "state", "show", "production", "net"); s.ExitCode != 0 {
		t.Errorf("net is gone from state, so the refusal did not prevent anything:\n%s", s.combined())
	}
}

// TestPreventDestroyRefusesRemovalFromConfiguration is the apply-side half:
// deleting a resource from configuration is the ordinary way to destroy one.
func TestPreventDestroyRefusesRemovalFromConfiguration(t *testing.T) {
	dir := project(t, `
project: myapp
environments:
  production:
    prevent_destroy: true
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  extra:
    type: fake.network
    cidr: 10.1.0.0/16
`)
	if a := run(t, dir, "apply", "production", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding apply: %d\n%s", a.ExitCode, a.combined())
	}

	writeIn(t, dir, "infra.yml", `
project: myapp
environments:
  production:
    prevent_destroy: true
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "apply", "production", "--auto-approve")
	if r.ExitCode == 0 || r.ExitCode == 2 {
		t.Fatalf("removing a resource from configuration destroyed it despite prevent_destroy:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "extra") {
		t.Errorf("the refusal does not name the resource it saved:\n%s", r.combined())
	}
}

// TestProtectionsInheritThroughExtends. Inheriting is the safe direction: the
// mistake nobody can see is a child that silently lost its parent's guard.
func TestProtectionsInheritThroughExtends(t *testing.T) {
	dir := project(t, `
project: myapp
environments:
  base:
    require_approval: true
  prod-eu:
    extends: base
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "apply", "prod-eu", "--auto-approve")
	if r.ExitCode != 77 {
		t.Fatalf("apply prod-eu exit = %d, want 77 — it extends a protected environment\n%s",
			r.ExitCode, r.combined())
	}
	// The refusal must name WHERE the protection was declared. Sending someone
	// to prod-eu's file to remove a setting that lives in base's wastes the
	// only thing a diagnostic is for.
	if !strings.Contains(r.combined(), "base") {
		t.Errorf("the refusal does not say the protection came from \"base\":\n%s", r.combined())
	}
}

// TestRequireApprovalIsNotAVariable is the failure this key was most likely to
// have. Every unrecognised key in an `environments:` block becomes a VARIABLE
// override, so a `require_approval` that fell through to that default would
// declare a variable of that name and protect nothing at all — silently, and
// exactly as `type:` did for seven milestones before it was refused.
func TestRequireApprovalIsNotAVariable(t *testing.T) {
	dir := project(t, protectedProject)
	r := run(t, dir, "apply", "production", "--auto-approve")
	if r.ExitCode != 77 {
		t.Fatalf("exit = %d, want 77 — `require_approval` was read as a variable, not a protection\n%s",
			r.ExitCode, r.combined())
	}
}
