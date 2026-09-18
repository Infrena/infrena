package integration

import (
	"encoding/json"
	"path/filepath"
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

// TestForEachOnAModuleCall expands the call once per entry, with every resource
// inside landing under the keyed call.
//
// It was SILENTLY IGNORED until 2026-09-18: stage 5 honours for_each in its
// resource loop and the module-call loop never read it, so a call carrying one
// expanded exactly once, produced a clean plan and said nothing. Ask for two
// databases, get one.
func TestForEachOnAModuleCall(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  store:
    type: module.db
    for_each: {orders: postgres, billing: mysql}
    cidr: 10.0.0.0/16
    engine: ${each.value}
`)
	writeIn(t, dir, "modules/db/module.yml", keyedModule)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan: %d\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	for _, want := range []string{
		`module.store["orders"].db`,
		`module.store["billing"].db`,
		`module.store["orders"].net`,
		`module.store["billing"].net`,
		// each.value reached the module's inputs, which is the whole reason the
		// call's inputs are evaluated once per entry rather than once.
		`"postgres"`,
		`"mysql"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan never mentions %q:\n%s", want, out)
		}
	}
}

// TestAModuleInstanceOutputIsReadByKey, which also proves the dependency edge
// lands on that instance rather than on the whole call.
func TestAModuleInstanceOutputIsReadByKey(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  store:
    type: module.db
    for_each: {orders: postgres, billing: mysql}
    cidr: 10.0.0.0/16
    engine: ${each.value}
  web:
    type: fake.application
    image: nginx
    database_url: ${store["orders"].endpoint}
`)
	writeIn(t, dir, "modules/db/module.yml", keyedModule)

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	state := readFileString(t, filepath.Join(dir, ".infra", "state", "dev.json"))

	// The endpoint orders' database actually got, read back out of state, so
	// this cannot pass by both instances happening to look alike.
	var orders struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				// `any`, not string: state records a value's raw form with its
				// own type, so `size` is a number and would fail to decode
				// into a string field — taking the whole test down over an
				// attribute it never looks at.
				Raw any `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(state), &orders); err != nil {
		t.Fatal(err)
	}
	want, _ := orders.Resources[`module.store["orders"].db`].Attributes["endpoint"].Raw.(string)
	if want == "" {
		t.Fatalf("orders' database has no endpoint in state:\n%s", state)
	}
	got, _ := orders.Resources["web"].Attributes["database_url"].Raw.(string)
	if got != want {
		t.Errorf("database_url = %q, want orders' endpoint %q — the key selected the wrong instance", got, want)
	}
}

// TestRemovingOneModuleEntryAffectsOnlyThatInstance. Identity is the key at the
// module level for the same reason it is at the resource level.
func TestRemovingOneModuleEntryAffectsOnlyThatInstance(t *testing.T) {
	const both = `
project: myapp
resources:
  store:
    type: module.db
    for_each: {orders: postgres, billing: mysql}
    cidr: 10.0.0.0/16
    engine: ${each.value}
`
	dir := project(t, both)
	writeIn(t, dir, "modules/db/module.yml", keyedModule)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	writeIn(t, dir, "infra.yml", strings.Replace(both,
		"{orders: postgres, billing: mysql}", "{orders: postgres}", 1))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan: %d\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	if !strings.Contains(out, "2 to destroy") {
		t.Errorf("want exactly the two resources of one instance destroyed:\n%s", out)
	}
	if !strings.Contains(out, "0 to create") || !strings.Contains(out, "0 to replace") {
		t.Errorf("removing one entry disturbed the other instance:\n%s", out)
	}
	// The surviving instance must not be in the CHANGES. It still appears in
	// the refresh lines above them, which is correct — everything managed is
	// read — so the check is on the destroy markers rather than on the name.
	if strings.Contains(out, `- fake.database.module.store["orders"].db`) {
		t.Errorf("the surviving instance is being destroyed:\n%s", out)
	}
}

// TestReferencingAWholeKeyedCallNamesTheInstances.
//
// The generic "no such output" message contradicted itself here — "has no
// output endpoint. Outputs it declares: endpoint" — because a keyed call has no
// single set of outputs to answer from, not because the output is missing.
func TestReferencingAWholeKeyedCallNamesTheInstances(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  store:
    type: module.db
    for_each: {orders: postgres, billing: mysql}
    cidr: 10.0.0.0/16
    engine: ${each.value}
  web:
    type: fake.application
    image: nginx
    database_url: ${store.endpoint}
`)
	writeIn(t, dir, "modules/db/module.yml", keyedModule)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("a reference to the whole keyed call was accepted")
	}
	out := r.combined()
	if strings.Contains(out, "has no output") {
		t.Errorf("the output exists; the message must not deny it:\n%s", out)
	}
	for _, want := range []string{"for_each", `store["orders"]`, `store["billing"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic never mentions %q:\n%s", want, out)
		}
	}
}

const keyedModule = `
inputs:
  cidr:
    type: string
  engine:
    type: string
resources:
  net:
    type: fake.network
    cidr: ${var.cidr}
  db:
    type: fake.database
    engine: ${var.engine}
    network: ${net}
outputs:
  endpoint:
    value: ${db.endpoint}
`

// TestAnInstanceAttributeCanBeReferenced — ${subnet["alpha"].id}, naming the
// instance AND the attribute.
//
// This was refused as a malformed reference until 2026-09-18. A guard written
// before `for_each` existed rejected any bracket in the first segment, and it
// survived `for_each` landing, so the key-parsing code beneath it could never
// run: only the whole-resource form ${subnet["alpha"]} worked, and writing the
// attribute out — the form anybody reaches for first — was an error telling
// them to remove the bracket.
//
// The parser's own unit tests passed the whole time, because they call the
// reference parser with segments already split and never traverse the guard.
// This test goes through the binary, which is the path a user takes.
func TestAnInstanceAttributeCanBeReferenced(t *testing.T) {
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
    network: ${subnet["beta"].id}
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}

	var st struct {
		Resources map[string]struct {
			Attributes map[string]struct {
				Raw any `json:"raw"`
			} `json:"attributes"`
		} `json:"resources"`
	}
	raw := readFileString(t, filepath.Join(dir, ".infra", "state", "dev.json"))
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	// The id beta actually got, so this cannot pass by the two instances
	// happening to look alike.
	want, _ := st.Resources[`subnet["beta"]`].Attributes["id"].Raw.(string)
	if want == "" {
		t.Fatalf(`subnet["beta"] has no id in state:\n%s`, raw)
	}
	got, _ := st.Resources["db"].Attributes["network"].Raw.(string)
	if got != want {
		t.Errorf("network = %q, want beta's id %q — the key selected the wrong instance", got, want)
	}
}

// TestANumericIndexIsStillRefused, and says why rather than saying "remove the
// bracket".
//
// Removing the guard above must not make ${subnet[0].id} mean something: an
// instance's identity is its key, and selecting one by position is the whole
// mistake `for_each` exists to prevent.
func TestANumericIndexIsStillRefused(t *testing.T) {
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
    network: ${subnet[0].id}
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("an instance was selected by position")
	}
	if !strings.Contains(r.combined(), "named, not numbered") {
		t.Errorf("the refusal does not explain itself:\n%s", r.combined())
	}
}
