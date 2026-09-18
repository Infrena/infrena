package integration

import (
	"strings"
	"testing"
)

const zonedSubnets = `
project: myapp
variables:
  zones:
    type: list
    default: [alpha, beta, gamma]
resources:
  subnet:
    type: fake.network
    for_each: ${var.zones}
    cidr: 10.1.0.0/16
`

// TestRemovingOneEntryAffectsOnlyThatResource is the entire reason for_each
// exists here and `count` does not.
//
// Terraform addresses count instances by ordinal, so removing the middle of
// three shifts every later one: the third becomes the second, and a plan
// proposes destroying and recreating resources that did not change. Identity
// by key makes that structurally impossible — this asserts exactly one destroy
// and no replacements.
func TestRemovingOneEntryAffectsOnlyThatResource(t *testing.T) {
	dir := project(t, zonedSubnets)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	writeIn(t, dir, "infra.yml", strings.Replace(zonedSubnets,
		"[alpha, beta, gamma]", "[alpha, gamma]", 1))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan: %d\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	if !strings.Contains(out, "1 to destroy") {
		t.Errorf("want exactly one destroy — the other two did not change:\n%s", out)
	}
	if !strings.Contains(out, "0 to replace") || !strings.Contains(out, "0 to create") {
		t.Errorf("removing one entry disturbed the others:\n%s", out)
	}
	if !strings.Contains(out, `subnet["beta"]`) {
		t.Errorf("the destroyed instance is not the one that was removed:\n%s", out)
	}
}

// TestEachKeyAndValue over a map, which is where the two differ.
func TestEachKeyAndValue(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    for_each: {orders: postgres, billing: mysql}
    engine: ${each.value}
    network: ${net}
    tags:
      service: ${each.key}
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan: %d\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	for _, want := range []string{`db["orders"]`, `db["billing"]`, `"postgres"`, `"mysql"`} {
		if !strings.Contains(out, want) {
			t.Errorf("plan never mentions %q:\n%s", want, out)
		}
	}
}

// TestAnInstanceCanBeReferencedByKey, which also proves the dependency edge
// lands on the right instance rather than on the set.
func TestAnInstanceCanBeReferencedByKey(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  subnet:
    type: fake.network
    for_each: [alpha, beta]
    cidr: 10.1.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${subnet["alpha"]}
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	if s := run(t, dir, "state", "show", "dev", `subnet["alpha"]`); s.ExitCode != 0 {
		t.Errorf("the instance is not addressable by key:\n%s", s.combined())
	}
}

// TestReferencingTheWholeSetNamesTheInstances. "Undeclared" would be true of
// the address and false of the resource, sending the reader to look for a typo
// in a name that is right there in the file.
func TestReferencingTheWholeSetNamesTheInstances(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  subnet:
    type: fake.network
    for_each: [alpha, beta]
    cidr: 10.1.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${subnet}
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("a reference to the whole set was accepted")
	}
	out := r.combined()
	if strings.Contains(out, "undeclared") {
		t.Errorf("the resource was reported as undeclared, which it is not:\n%s", out)
	}
	for _, want := range []string{"for_each", `subnet["alpha"]`, `subnet["beta"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic never mentions %q:\n%s", want, out)
		}
	}
}

// TestAnEmptyForEachMakesNothing. This is the "optional resource" case, and why
// there is no `count: enabled ? 1 : 0` idiom to learn.
func TestAnEmptyForEachMakesNothing(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  subnet:
    type: fake.network
    for_each: []
    cidr: 10.1.0.0/16
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 0 {
		t.Fatalf("plan exit = %d, want 0 — an empty for_each declares nothing\n%s",
			r.ExitCode, r.combined())
	}
}

// TestADuplicateKeyIsRefused rather than deduplicated: two entries with one key
// describe two resources sharing an identity, and the second would replace the
// first in state.
func TestADuplicateKeyIsRefused(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  subnet:
    type: fake.network
    for_each: [alpha, alpha]
    cidr: 10.1.0.0/16
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 || r.ExitCode == 0 {
		t.Fatalf("a duplicate key was accepted, exit = %d\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "twice") {
		t.Errorf("the refusal does not say what is wrong:\n%s", r.combined())
	}
}

// TestForEachKeysMustBeKnown. The set of resources to create cannot depend on
// something that does not exist yet, or a plan could not say what it would do.
func TestForEachKeysMustBeKnown(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  subnet:
    type: fake.network
    for_each: ${net.id}
    cidr: 10.1.0.0/16
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("for_each over an unknown value was accepted")
	}
	if !strings.Contains(r.combined(), "for_each") {
		t.Errorf("the diagnostic does not name the setting:\n%s", r.combined())
	}
}

// TestCountSuggestsForEach. `count` is not reserved — a provider may
// legitimately have an attribute of that name — so the hint lives where an
// unknown attribute is reported, which is only reached by a `count` the type
// genuinely has none of.
func TestCountSuggestsForEach(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  subnet:
    type: fake.network
    count: 3
    cidr: 10.1.0.0/16
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("count was silently accepted as an attribute")
	}
	if !strings.Contains(r.combined(), "for_each") {
		t.Errorf("a Terraform user is not pointed at the replacement:\n%s", r.combined())
	}
}

// TestForEachInsideAModule. A module's resources are expanded by the same walk
// as the root's, so this works without special handling — which is worth a test
// precisely because nothing in the implementation mentions modules, and a
// future change to either could break the combination silently.
//
// The address is the proof: module path, then name, then key.
func TestForEachInsideAModule(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    engines: [postgres, mysql]
`, map[string]string{"modules/db/module.yml": `
inputs:
  engines:
    type: list
resources:
  store:
    type: fake.database
    for_each: ${var.engines}
    engine: ${each.key}
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	for _, want := range []string{
		`module.primary.store["postgres"]`,
		`module.primary.store["mysql"]`,
	} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("plan does not contain %q:\n%s", want, r.combined())
		}
	}
}
