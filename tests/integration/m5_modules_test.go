package integration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/modules/source"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// lineContaining returns the single output line containing needle. Asserting
// on a whole command's output cannot tell "the cycle diagnostic shows the
// cycle" from "the word appears twice, once in the chain and once in a file
// path". Narrowing to one line can.
func lineContaining(t *testing.T, out, needle string) string {
	t.Helper()
	var found []string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, needle) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line containing %q, found %d:\n%s", needle, len(found), out)
	}
	return found[0]
}

// firstDiagnosticLine returns the "Error: <summary>" line a rendered
// diagnostic opens with. Two failures that must be distinct (contract Ruling
// 6) are compared through this rather than through a hard-coded wording, so
// the assertion survives Tasks 4-7 choosing their own words.
func firstDiagnosticLine(t *testing.T, out string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "Error: ") {
			return l
		}
	}
	t.Fatalf("no diagnostic in:\n%s", out)
	return ""
}

// modHeader renders the plan header for a resource inside a module instance,
// built from pkg/address rather than spelled literally.
//
// The canonical form is module-prefixed at every level —
// module.prod.database, module.prod.module.network.vpc — and Amendment 12a
// settled that it stays so. The reason is worth knowing before anyone
// "simplifies" it: an instance both CONTAINS resources and EXPOSES outputs, so
// `prod.endpoint` (an output reference) and a flat `prod.database` (a contained
// resource's address) would be the same shape with two grammars.
// `state show prod.database` could not be told from an output reference. The
// redundant `module.` is what keeps addresses and references visually apart.
//
// Deriving every header from the same function the renderer uses means these
// tests assert that the plan agrees with pkg/address rather than re-spelling
// the format twelve times. The format itself is pinned ONCE, literally, by
// TestTheCanonicalAddressFormIsWhatThePlanPrints below. Without that one
// literal this helper would be tautological — a renderer that stopped using
// Address.String() would satisfy every call of it.
func modHeader(typ string, path []string, name string) string {
	return typ + "." + address.Address{Module: path, Name: name}.String()
}

// TestTheCanonicalAddressFormIsWhatThePlanPrints is the single literal
// assertion the helper above rests on, and the only place in this file that
// spells the canonical form out. internal/state/golden_test.go:119 pins the
// same form on the state side; this is its plan-output counterpart.
func TestTheCanonicalAddressFormIsWhatThePlanPrints(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.Stdout, "fake.database.module.primary.store") {
		t.Errorf("the plan does not print the canonical module-prefixed address produced by "+
			"address.Address.String(). A flat `primary.store` would be indistinguishable from a "+
			"reference to an OUTPUT named `store` on the instance (Amendment 12a):\n%s", r.Stdout)
	}
	if got := modHeader("fake.database", []string{"primary"}, "store"); !strings.Contains(r.Stdout, got) {
		t.Errorf("the plan header disagrees with address.Address.String(): want a line containing %q\n%s",
			got, r.Stdout)
	}
}

// dbModule is one module source, instantiated by most fixtures below. `size`
// carries a declared default of 7, which is neither of the fake provider's own
// defaults (10 outside production, 100 in it) — so a plan showing 7 proves the
// MODULE default won, and a plan showing 10 proves it did not.
const dbModule = `
inputs:
  network:
    type: string
  size:
    type: integer
    default: 7
resources:
  store:
    type: fake.database
    engine: postgres
    network: ${var.network}
    size: ${var.size}
outputs:
  endpoint:
    value: ${store.endpoint}
`

// TestAModuleIsInstantiatedUnderModuleQualifiedAddresses is M5's headline: two
// instantiations of one module source become two distinct resources, addressed
// by module path, and nothing downstream sees a module (contract Ruling 7).
//
// It also pins contract Ruling 3 in both directions. `primary` passes size
// explicitly; `secondary` does not and takes the module's declared default.
// Those are two different rungs of PLAN.md §7's chain and must not render the
// same: an explicit value the caller wrote is base configuration at the
// caller's level, and only the module's own `default:` fills ScopeModuleDefault
// — the rung M4 declared and deliberately left empty.
func TestAModuleIsInstantiatedUnderModuleQualifiedAddresses(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
    size: 20
  secondary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 (changes present)\n%s", r.ExitCode, r.combined())
	}

	// Three resources, not two: one module source instantiated twice is two
	// resources. An implementation that keyed instantiations by source rather
	// than by name would produce two.
	requireContains(t, r.Stdout, "3 to create")

	primary := attrLine(t, r.Stdout, modHeader("fake.database", []string{"primary"}, "store"), "size")
	if !strings.HasPrefix(primary, "size: 20") {
		t.Errorf("primary size line = %q, want the caller's explicit 20", primary)
	}
	if strings.Contains(primary, "module default") {
		t.Errorf("primary size line = %q — a value the CALLER wrote is base configuration at the caller's "+
			"level, not a module default; putting it on that rung would place an explicit value below an "+
			"environment override and invert `explicit config always wins`", primary)
	}

	secondary := attrLine(t, r.Stdout, modHeader("fake.database", []string{"secondary"}, "store"), "size")
	if !strings.HasPrefix(secondary, "size: 7 [") {
		t.Errorf("secondary size line = %q, want `size: 7 [...]` — the module's declared default, not the "+
			"provider's 10", secondary)
	}
	if !strings.Contains(secondary, "from module default") {
		t.Errorf("secondary size line = %q does not name the module-defaults rung; PLAN.md §7 declares it "+
			"and M4 left it empty for this milestone", secondary)
	}

	// Absence. A flat address set means nothing renders the module's own
	// internal name, and neither instantiation's value may appear on the
	// other's line.
	if n := strings.Count(r.Stdout, "fake.database."+address.Address{Name: "store"}.String()); n != 0 {
		t.Errorf("an unqualified `fake.database.store` appears %d times — after stage 5 every address "+
			"carries its module path:\n%s", n, r.Stdout)
	}
	for _, text := range []string{"size: 20", "size: 7"} {
		if n := strings.Count(r.Stdout, text); n != 1 {
			t.Errorf("%q appears %d times, want 1 — one instantiation's input leaked into the other:\n%s",
				text, n, r.Stdout)
		}
	}
	if strings.Contains(r.Stdout, "size: 10") {
		t.Errorf("the provider default reached a resource whose size was supplied:\n%s", r.Stdout)
	}
}

// TestAModuleInstantiatesAModule is the case a one-level fixture cannot see.
// Spec §7.2 BOUNDS recursion at 32 rather than forbidding it, so a module file
// carries its own `modules:` block and an address grows one segment per level:
// module.platform.module.storage.store. Every part of the engine that handles a
// module path — Address.String, the sort that makes invariant 6 hold, the
// module-qualified header the renderer prints, Origin.Module in a diagnostic —
// is a loop over that path, and a loop is exactly what a single-element fixture
// cannot distinguish from a hard-coded first element.
//
// The inner module's output travels out through the outer module's output,
// which is the second thing one level cannot test: an output whose value reads
// another module's output rather than a resource's attribute.
func TestAModuleInstantiatesAModule(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/platform
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  platform:
    type: module.platform
    network: ${net.id}
  app:
    type: fake.application
    image: nginx:1.27
    database_url: ${platform.dsn}
`, map[string]string{
		"modules/platform/module.yml": `
inputs:
  network:
    type: string
modules:
  - ../db
resources:
  storage:
    type: module.db
    network: ${var.network}
    size: 30
outputs:
  dsn:
    value: ${storage.endpoint}
`,
		"modules/db/module.yml": dbModule,
	})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	// Two segments, outermost first, and the module path is a path rather than
	// a single name.
	line := attrLine(t, r.Stdout, modHeader("fake.database", []string{"platform", "storage"}, "store"), "size")
	if !strings.HasPrefix(line, "size: 30") {
		t.Errorf("nested store size line = %q, want the 30 the outer module passed in", line)
	}

	// An output that reads another module's output, two levels up.
	dsn := attrLine(t, r.Stdout, "fake.application.app", "database_url")
	if dsn != "database_url: (known after apply)" {
		t.Errorf("database_url = %q, want `database_url: (known after apply)` — the inner module's "+
			"computed endpoint, republished by the outer module's output", dsn)
	}

	// Absence: neither a one-level address nor the inner module's own name
	// alone may appear.
	for _, wrong := range []string{
		modHeader("fake.database", []string{"storage"}, "store"),
		modHeader("fake.database", []string{"platform"}, "store"),
	} {
		if strings.Contains(r.Stdout, wrong) {
			t.Errorf("%s appears in the plan; a nested instantiation carries BOTH levels of its path:\n%s",
				wrong, r.Stdout)
		}
	}
}

// TestTwoModulesEachDeclaringADbAreDifferentResources is Ruling 1's landmine
// through the binary. Task 1's TestAttributeRefusesAnUnqualifiedReference
// guards the same property at the unit level — it fails if Qualify is skipped,
// or if someone patches around a missing Qualify with a bare-name fallback in
// ResourceScope.Attribute. This is the other end: the user-visible consequence,
// which is that two modules each declaring a `db` get two databases with their
// own engines rather than one shared or one shadowing the other.
//
// The engines differ per instantiation, so a reference resolved by bare name
// shows up as the wrong engine on one side rather than as an error.
func TestTwoModulesEachDeclaringADbAreDifferentResources(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/app
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  left:
    type: module.app
    network: ${net.id}
    engine: postgres
  right:
    type: module.app
    network: ${net.id}
    engine: mysql
`, map[string]string{"modules/app/module.yml": `
inputs:
  network:
    type: string
  engine:
    type: string
resources:
  db:
    type: fake.database
    engine: ${var.engine}
    network: ${var.network}
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	// Three resources: the network and one database per instantiation. Two
	// modules collapsing into one would show 2.
	requireContains(t, r.Stdout, "3 to create")

	if line := attrLine(t, r.Stdout, modHeader("fake.database", []string{"left"}, "db"), "engine"); line != `engine: "postgres"` {
		t.Errorf("left db engine line = %q, want `engine: \"postgres\"`", line)
	}
	if line := attrLine(t, r.Stdout, modHeader("fake.database", []string{"right"}, "db"), "engine"); line != `engine: "mysql"` {
		t.Errorf("right db engine line = %q, want `engine: \"mysql\"`", line)
	}

	// Absence: each engine appears exactly once. One module shadowing the
	// other shows the same engine twice, which is a wrong plan rather than an
	// invalid one and is what makes this worth asserting both ways.
	for _, engine := range []string{`"postgres"`, `"mysql"`} {
		if n := strings.Count(r.Stdout, engine); n != 1 {
			t.Errorf("%s appears %d times, want 1 — one instantiation's `db` resolved to the other's:\n%s",
				engine, n, r.Stdout)
		}
	}
}

// TestAModuleOutputReachesTheCallerAndMayBeUnknown is contract Ruling 5, end
// to end, plus Ruling 4's resolution of `${name.attr}` and Ruling 7's promise
// that stage 8 sees a module's resources.
//
// `${database.endpoint}` names a MODULE, not a resource, and its output reads
// a computed attribute of a resource that does not exist yet. It must stay
// unknown all the way to the page: never an empty string, never a coercion
// failure. And `fake.application` declares a Requirement for a fake.database
// (providers/test/definitions.go) which only the module supplies — so stage 8
// passing is itself proof that flattening happened before validation.
func TestAModuleOutputReachesTheCallerAndMayBeUnknown(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: module.db
    network: ${net.id}
  app:
    type: fake.application
    image: nginx:1.27
    database_url: ${database.endpoint}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	line := attrLine(t, r.Stdout, "fake.application.app", "database_url")
	if line != "database_url: (known after apply)" {
		t.Errorf("database_url = %q, want `database_url: (known after apply)` — a module output reading a "+
			"computed attribute is unknown at plan time, and an empty string or a coercion error here is "+
			"the failure contract Ruling 5 names", line)
	}

	// Stage 8's requirement check saw the module's database. The real
	// wording (internal/compiler/validate.go) is `"<addr>" is missing
	// required database`; if stage 8 could not see the module's fake.database,
	// that is what `app` would fail with.
	if strings.Contains(r.combined(), "is missing required database") {
		t.Errorf("stage 8 did not see the module's resource:\n%s", r.combined())
	}

	// And the whole thing converges: apply, then re-plan clean.
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes: a fresh apply always has changes)\n%s", a.ExitCode, a.combined())
	}
	again := run(t, dir, "plan", "dev")
	if again.ExitCode != 0 {
		t.Fatalf("re-plan exit = %d, want 0 (acceptance invariant 2)\n%s", again.ExitCode, again.combined())
	}
	requireContains(t, again.Stdout, "No changes.")
}

// TestAModuleOutputBindsToTheModulesOwnResource. The root `store` is a decoy:
// a fake.network with the same LOGICAL NAME as the module's fake.database,
// referenced by nothing. If the module's output `${store.endpoint}` is left
// scope-relative it binds to that decoy, and `app` acquires a dependency on it.
//
// The decoy is then removed from configuration and the resulting destroy is
// inspected. renderDependentsWarning prints "This resource has N dependent
// resources" on a destroy, so a decoy with a dependent says so — and a decoy
// nothing legitimately references must have none.
func TestAModuleOutputBindsToTheModulesOwnResource(t *testing.T) {
	const withDecoy = `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  store:
    type: fake.network
    cidr: 10.1.0.0/16
  thedb:
    type: module.db
    network: ${net.id}
  app:
    type: fake.application
    image: nginx:1.27
    database_url: ${thedb.endpoint}
`
	dir := projectWithFiles(t, withDecoy, map[string]string{"modules/db/module.yml": dbModule})

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes: a fresh apply always has changes)\n%s", a.ExitCode, a.combined())
	}

	// Drop the decoy. Nothing references it, so this must be a lone destroy.
	writeIn(t, dir, "infra.yml", strings.Replace(withDecoy, `  store:
    type: fake.network
    cidr: 10.1.0.0/16
`, "", 1))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 — an exit of 1 here is the unqualified case: with the decoy "+
			"gone, a bare `store` matches nothing declared and stage 6 reports an undeclared "+
			"resource\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.Stdout, "1 to destroy")
	requireContains(t, r.Stdout, "fake.network.store")

	// The assertion. A dependents warning on the decoy means `app` depends on
	// it, which can only happen if the module output's ${store.endpoint}
	// resolved to the root `store` instead of the module's own.
	for line := range strings.SplitSeq(r.Stdout, "\n") {
		if strings.Contains(line, "dependent") {
			t.Errorf("the decoy has dependents (%q) — a module output's reference escaped its module and "+
				"bound to a root resource of the same name:\n%s", strings.TrimSpace(line), r.Stdout)
		}
	}

	// And the application itself is untouched: its database_url still comes
	// from the module, so removing the decoy changes nothing about it.
	if strings.Contains(r.Stdout, "fake.application.app") {
		t.Errorf("removing an unrelated resource changed the application:\n%s", r.Stdout)
	}
}

// TestAReferenceToAMistypedInstanceNamesTheRealOnes.
//
// Ruling 4 got NARROWER under Amendment 8: an instance IS a resource, so
// ${database.endpoint} is an ordinary resource reference whose target happens
// to expand, and there is no module-vs-resource ambiguity left at the caller.
// What survives is the plain undeclared-reference diagnostic — but it now has
// to list module instances among the known resources, because `database` IS
// one, and a user who mistypes an instance name and is shown a list without it
// will conclude the module never loaded.
func TestAReferenceToAMistypedInstanceNamesTheRealOnes(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: module.db
    network: ${net.id}
  app:
    type: fake.application
    image: nginx:1.27
    database_url: ${datbase.endpoint}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1 (configuration is not valid)\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, "datbase")  // the name as typed, not a normalised form
	requireContains(t, out, "database") // the instance it should have named, in the known list
	requireContains(t, out, "net")      // and the provider resource, so the list is not instances-only
}

// TestABareDottedOutputValueIsRefused. PLAN.md §11's own example writes
// `value: service.endpoint` with no ${}, and taking that literally would make
// every dotted scalar ambiguous: `value: production` is a string,
// `value: 1.2.3` is a version, `value: db.example.com` is a hostname, and
// nothing distinguishes any of them from a reference. ${...} is this
// language's only reference syntax. The guard exists so that a user following
// the old example gets an error naming both spellings rather than a module
// that silently publishes the nine characters "store.endpoint".
func TestABareDottedOutputValueIsRefused(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": `
inputs:
  network:
    type: string
  size:
    type: integer
    default: 7
resources:
  store:
    type: fake.database
    engine: postgres
    network: ${var.network}
    size: ${var.size}
outputs:
  endpoint:
    value: store.endpoint
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1 — a bare dotted output value is refused, not published as a "+
			"string\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, "${store.endpoint}") // the spelling the user wants
	// The implementation's second offered spelling is quoting the literal
	// (internal/config/module_file.go's bareReference), not the "$${"
	// dollar-brace escape used elsewhere in the language for a *value*
	// containing a literal "${" — an output's `value:` here is the scalar
	// "store.endpoint" itself, with no "${" in it to escape.
	requireContains(t, out, `"store.endpoint"`) // and the quoting alternative, for the case they meant the literal
}

// TestAReferenceToAnUndeclaredModuleOutputIsReported. The module exists and
// the output does not. Unlike the general referenced-attribute gap, stage 5
// can see this one: it resolves ${module.output} against the module's declared
// outputs, so the lookup that fails is the lookup that reports.
//
// Asserted on `validate` rather than `plan` because it is a configuration
// error and must not require state or a refresh to surface.
func TestAReferenceToAnUndeclaredModuleOutputIsReported(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  thedb:
    type: module.db
    network: ${net.id}
  app:
    type: fake.application
    image: nginx:1.27
    database_url: ${thedb.vpc_id}
`, map[string]string{"modules/db/module.yml": dbModule})

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — a reference to an output the module does not declare is "+
			"an error, not an unknown: rendered as `(known after apply)` it promises a value that will "+
			"never arrive\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, "vpc_id")   // the output that does not exist
	requireContains(t, out, "thedb")    // the module it was asked of
	requireContains(t, out, "endpoint") // and the one it does declare, per PLAN.md §44
}

// TestAReferenceToANonexistentAttributeFailsAtValidate. The measured failure
// was not "a confusing message" — it was `infrena apply` CREATING REAL
// INFRASTRUCTURE and then failing partway through on a typo that `validate`
// had passed. So the assertions are: validate refuses it, and apply creates
// nothing.
//
// There is deliberately no assertion on `plan` output. A phantom attribute
// renders identically to a legitimate computed reference — `(known after
// apply)`, or `<sensitive>` when the attribute is sensitive, as `password` is
// — so there is nothing in a plan a user or a test could read to tell them
// apart. Asserting on plan text here would be asserting on a string that is
// correct in both worlds.
func TestAReferenceToANonexistentAttributeFailsAtValidate(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  store:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${store.id}
    password: ${store.endpoint}
`)

	v := run(t, dir, "validate", "dev")
	if v.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — fake.network has no `endpoint`, and validate is the "+
			"command whose entire job is to catch that before anything is created\n%s",
			v.ExitCode, v.combined())
	}
	out := v.combined()
	requireContains(t, out, "endpoint")
	requireContains(t, out, "fake.network") // the cause, which the apply-time message never names
	requireContains(t, out, "cidr")         // what was expected, per §44

	// The regression that matters. Before Amendment 11 this apply created
	// `store` for real and then failed on `db`.
	a := run(t, dir, "apply", "dev", "--auto-approve")
	if a.ExitCode != 1 {
		// Not merely "!= 0": a successful apply that found and applied
		// changes exits 2 (every other apply-based test in this file relies
		// on that), so checking only for 0 would let a wrongly-successful
		// apply through uncaught.
		t.Fatalf("apply exit = %d, want 1 (an error, not a success) — apply succeeded on configuration "+
			"validate rejects\n%s", a.ExitCode, a.combined())
	}
	st := run(t, dir, "state", "list", "dev")
	if strings.Contains(st.Stdout, "store") {
		t.Errorf("apply created `store` before failing on the typo — the failure this check exists to "+
			"prevent is real infrastructure existing after a run that should never have started:\n%s",
			st.Stdout)
	}

	// And the symptom message must not be what the user sees.
	if strings.Contains(a.combined(), "still unknown after its dependencies were applied") {
		t.Errorf("apply reported the symptom rather than being refused at compile time:\n%s", a.combined())
	}
}

// TestAModuleCycleShowsTheCycle is contract Ruling 6. The cycle runs through
// two module LEVELS — root instantiates ping, ping instantiates pong, pong
// instantiates ping — because modules nest (spec §7.2 bounds recursion rather
// than forbidding it) and a module naming itself directly is the easy half. A
// detector that only compares a module against its immediate parent passes the
// self-reference case and hangs here.
//
// Spec §7.4 requires the diagnostic to name the full cycle, not one
// participant — and "shows the cycle" is asserted by requiring the chain line
// to mention `ping` twice,
// because a chain that returns to where it started is what makes it a cycle.
// Narrowing to the one line containing the arrow is what stops the file path
// `modules/ping/module.yml`, which also says "ping", from satisfying it.
func TestAModuleCycleShowsTheCycle(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/ping
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  first:
    type: module.ping
`, map[string]string{
		"modules/ping/module.yml": "modules:\n  - ../pong\nresources:\n  next:\n    type: module.pong\n",
		"modules/pong/module.yml": "modules:\n  - ../ping\nresources:\n  back:\n    type: module.ping\n",
	})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	chain := lineContaining(t, r.combined(), "->")
	if !strings.Contains(chain, "pong") {
		t.Errorf("the cycle chain %q does not mention the module in the middle of it", chain)
	}
	if n := strings.Count(chain, "ping"); n < 2 {
		t.Errorf("the cycle chain %q names `ping` %d times; a cycle diagnostic must SHOW the cycle "+
			"returning to where it started, not merely assert one exists", chain, n)
	}
}

// TestExcessiveModuleNestingIsItsOwnDiagnostic is the rest of Ruling 6: depth
// 32 and a cycle are different failures and must not be collapsed. Nothing
// here hard-codes either wording — the assertion is that the two summaries
// DIFFER, which is the property that matters and the one a shared diagnostic
// would break.
func TestExcessiveModuleNestingIsItsOwnDiagnostic(t *testing.T) {
	const depth = 40 // comfortably past the bound of 32 (spec §7.2)
	files := map[string]string{}
	for i := range depth {
		files[fmt.Sprintf("modules/n%d/module.yml", i)] = fmt.Sprintf(
			"modules:\n  - ../n%d\nresources:\n  deeper:\n    type: module.n%d\n", i+1, i+1)
	}
	files[fmt.Sprintf("modules/n%d/module.yml", depth)] = "resources: {}\n"

	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/n0
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  top:
    type: module.n0
`, files)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("plan exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "32")

	// Build the cycle fixture again and compare summaries.
	cycleDir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/ping
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  first:
    type: module.ping
`, map[string]string{
		"modules/ping/module.yml": "modules:\n  - ../pong\nresources:\n  next:\n    type: module.pong\n",
		"modules/pong/module.yml": "modules:\n  - ../ping\nresources:\n  back:\n    type: module.ping\n",
	})
	cycle := run(t, cycleDir, "plan", "dev")

	depthSummary := firstDiagnosticLine(t, r.combined())
	cycleSummary := firstDiagnosticLine(t, cycle.combined())
	if depthSummary == cycleSummary {
		t.Errorf("nesting too deep and a module instantiating itself share one diagnostic (%q); one says "+
			"the nesting is too deep and the other says it never terminates, and a user acts on them "+
			"differently", depthSummary)
	}
}

// TestASensitiveAttributeInsideAModuleStaysRedacted. Exactly one redaction
// path exists, pkg/value.Format, and module expansion introduces a new way for
// a value to travel — through a caller's `inputs:` into a module's resource.
// A second path, or a value that escaped the first, is how a plaintext secret
// reached output in M2.
func TestASensitiveAttributeInsideAModuleStaysRedacted(t *testing.T) {
	const secret = "hunter2-do-not-print"
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
    secret: `+secret+`
`, map[string]string{"modules/db/module.yml": `
inputs:
  network:
    type: string
  secret:
    type: string
resources:
  store:
    type: fake.database
    engine: postgres
    network: ${var.network}
    password: ${var.secret}
`})

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), secret) {
		t.Errorf("the secret reached the command's output:\n%s", r.combined())
	}
	line := attrLine(t, r.Stdout, modHeader("fake.database", []string{"primary"}, "store"), "password")
	if !strings.HasPrefix(line, "password: <sensitive>") {
		t.Errorf("password rendered as %q, want a redacted value", line)
	}

	// Apply, then check it did not leak through state inspection either.
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes: a fresh apply always has changes)\n%s", a.ExitCode, a.combined())
	}
	show := run(t, dir, "state", "show", "dev", "module.primary.store")
	if show.ExitCode != 0 {
		t.Fatalf("state show exit = %d, want 0\n%s", show.ExitCode, show.combined())
	}
	if strings.Contains(show.combined(), secret) {
		t.Errorf("the secret reached `state show`:\n%s", show.combined())
	}
}

// TestPlanWithModulesIsDeterministic is acceptance invariant 6 under the one
// thing M5 adds that can break it: expansion order. Four instantiations of one
// module plus a network is five resources, and Go randomises map iteration per
// range — an expansion that leaves order to a map differs between runs with
// high probability over ten runs, and almost never over one.
func TestPlanWithModulesIsDeterministic(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  delta:
    type: module.db
    network: ${net.id}
  alpha:
    type: module.db
    network: ${net.id}
  charlie:
    type: module.db
    network: ${net.id}
  bravo:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	first := run(t, dir, "plan", "dev")
	if first.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", first.ExitCode, first.combined())
	}
	for i := 1; i < 10; i++ {
		again := run(t, dir, "plan", "dev")
		if again.Stdout != first.Stdout {
			t.Fatalf("run %d differs from run 0:\n--- run 0 ---\n%s\n--- run %d ---\n%s",
				i, first.Stdout, i, again.Stdout)
		}
	}

	// Sorted, not merely stable: a stable-but-unsorted order would pass the
	// loop above. The instantiations are declared delta, alpha, charlie,
	// bravo, so declaration order and sorted order differ.
	want := []string{
		modHeader("fake.database", []string{"alpha"}, "store"),
		modHeader("fake.database", []string{"bravo"}, "store"),
		modHeader("fake.database", []string{"charlie"}, "store"),
		modHeader("fake.database", []string{"delta"}, "store"),
	}
	at := 0
	for _, name := range want {
		i := strings.Index(first.Stdout[at:], name)
		if i < 0 {
			t.Fatalf("%s does not appear after the previous instantiation; operations are not sorted by "+
				"address:\n%s", name, first.Stdout)
		}
		at += i + len(name)
	}
}

// TestAMalformedModuleTypeIsReported covers Amendment 8c's first two guards.
// They are TASK 2's to implement (Amendment 13b, `checkResourceType`); this
// asserts the user-visible half.
//
// Stage 2 rather than stage 5, because stage 2 holds the line number — and
// because at stage 5 `HasPrefix` would select a malformed `module.` type and
// then report that some module does not exist, a diagnostic about a
// consequence, naming a module name the user never wrote.
//
// `module.` with nothing after it, and a name containing a further dot. The
// second matters more than it looks: a caller names a top-level loaded module,
// never a path into one, so `module.db.store` is a user reaching for a
// resource inside a module and must be told that rather than silently
// resolving to nothing.
func TestAMalformedModuleTypeIsReported(t *testing.T) {
	for _, tc := range []struct{ name, typ, want string }{
		{"empty", "module.", "module."},
		{"dotted", "module.db.store", "module.db.store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  bad:
    type: `+tc.typ+`
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

			r := run(t, dir, "validate", "dev")
			if r.ExitCode != 1 {
				t.Fatalf("validate exit = %d, want 1\n%s", r.ExitCode, r.combined())
			}
			requireContains(t, r.combined(), tc.want)
			requireContains(t, r.combined(), "bad") // the resource, per §44: where
		})
	}
}

// TestALocalOnlyProjectWritesNoLockFileOrModuleCache is Amendment 10's
// negative space, and it is the assertion most likely to be skipped.
//
// modules.lock records the commit a remote tag resolved to (10b), and
// .infra/modules/ caches fetched sources. A project whose every source is a
// filesystem path has nothing to pin and nothing to fetch, so writing either
// would be noise a user has to reason about and — for modules.lock, which is
// meant to be committed — noise in their diff. A lock file listing no remotes
// is worse than absent: it suggests pinning is happening.
func TestALocalOnlyProjectWritesNoLockFileOrModuleCache(t *testing.T) {
	dir := projectWithFiles(t, `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`, map[string]string{"modules/db/module.yml": dbModule})

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes: a fresh apply always has changes)\n%s", a.ExitCode, a.combined())
	}

	if _, err := os.Stat(filepath.Join(dir, "modules.lock")); !os.IsNotExist(err) {
		t.Errorf("modules.lock exists for a project with no remote sources (stat err = %v); a lock file "+
			"pinning nothing suggests pinning is happening", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".infra", "modules")); !os.IsNotExist(err) {
		t.Errorf(".infra/modules exists for a project that fetched nothing (stat err = %v)", err)
	}
}

// prepopulateCache creates a valid cache entry for a hash-pinned source inside
// dir's project cache, so that resolution skips the network (13.6).
//
// It goes through source.PrepopulateCache rather than building
// .infra/modules/<hash>/<ref>/ here. A copy of that layout in this package
// would keep passing after the layout changed and quietly stop testing
// anything, because a cache miss against example.invalid fails looking exactly
// like an ordinary network error.
//
// This is Author D's wrapper (their step 13.6a), reused verbatim so both
// suites share one copy rather than growing two.
func prepopulateCache(t *testing.T, dir, location, commit string, files map[string]string) {
	t.Helper()
	s, ds := source.Parse(location+":"+commit, value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("parsing %q: %+v", location+":"+commit, ds)
	}
	if err := source.PrepopulateCache(dir, s, commit, files); err != nil {
		t.Fatalf("prepopulating the cache: %v", err)
	}
}

// TestALockfileEntryThatDisagreesIsRefusedAndNotRewritten.
//
// READ THE NEXT PARAGRAPH BEFORE DELETING THIS TEST AS NONSENSICAL. For a
// hash-pinned source the scenario is degenerate on purpose: Commit always
// equals Ref, so a lock entry recording a different commit is one no correct
// run could ever have written. The test SUPPLIES that entry synthetically to a
// REAL mechanism — it exercises read-compare-refuse, and the
// byte-identical-afterward property, through the binary. That second half is
// the one no unit test reaches, because only the binary decides whether a
// command writes the file.
//
// A hash pin is used because it is the only git source reachable without a
// remote (Author D's 13.6: a valid cache entry skips the network). The
// alternative — a tag — would need a live server.
func TestALockfileEntryThatDisagreesIsRefusedAndNotRewritten(t *testing.T) {
	const (
		source = "https://example.invalid/repo"
		pinned = "1111111111111111111111111111111111111111" // what the source pins
		stale  = "2222222222222222222222222222222222222222" // what the lock claims
	)

	dir := projectWithFiles(t, `
project: myapp
modules:
  - `+source+`:`+pinned+`
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.repo
    network: ${net.id}
`, map[string]string{
		// modules.lock, hand-written: source and ref match, commit does not.
		// Shape from internal/modules/source's Lockfile/Record.
		"modules.lock": `{
  "version": 1,
  "modules": [
    {
      "source": "` + source + `",
      "ref": "` + pinned + `",
      "commit": "` + stale + `"
    }
  ]
}
`,
	})
	// Commit == the pin, forced by the helper. So the cache CANNOT be the
	// source of the disagreement, which is what leaves modules.lock as the
	// only place one can live — and is why Amendment 20c's "hash pins get
	// lock entries too" is what makes this test possible at all.
	prepopulateCache(t, dir, source, pinned, map[string]string{"module.yml": dbModule})

	before, err := os.ReadFile(filepath.Join(dir, "modules.lock"))
	if err != nil {
		t.Fatalf("reading the fixture lockfile: %v", err)
	}

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — a lockfile entry recording a different commit for the "+
			"same source and ref must be refused, not adopted\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	requireContains(t, out, source) // which module
	requireContains(t, out, stale)  // what the lock records
	requireContains(t, out, pinned) // what it resolves to now

	// THE ASSERTION THAT CARRIES THE PROPERTY. Everything above would also
	// pass against a command that reported the mismatch and then rewrote the
	// file — which is the failure being guarded, because it is silent and
	// leaves the user believing the lock enforced something.
	after, err := os.ReadFile(filepath.Join(dir, "modules.lock"))
	if err != nil {
		t.Fatalf("reading the lockfile after validate: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("validate rewrote modules.lock.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestEditingAPinIsNotALockfileConflict is the other half of the keying, and
// the half a user meets weekly. Bumping `:v1` to `:v2` — here, one hash to
// another — changes the REF, so it is a different key and a new entry rather
// than a conflict. Without this, the feature would refuse every deliberate
// upgrade and tell the user to delete a line they had just edited.
func TestEditingAPinIsNotALockfileConflict(t *testing.T) {
	const (
		source = "https://example.invalid/repo"
		oldPin = "1111111111111111111111111111111111111111"
		newPin = "3333333333333333333333333333333333333333"
	)

	dir := projectWithFiles(t, `
project: myapp
modules:
  - `+source+`:`+newPin+`
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.repo
    network: ${net.id}
`, map[string]string{
		"modules.lock": `{
  "version": 1,
  "modules": [
    {
      "source": "` + source + `",
      "ref": "` + oldPin + `",
      "commit": "` + oldPin + `"
    }
  ]
}
`,
	})
	prepopulateCache(t, dir, source, newPin, map[string]string{"module.yml": dbModule})

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 0 {
		t.Fatalf("validate exit = %d, want 0 — the ref changed, so this is a different lockfile key and "+
			"a new entry, not a conflict\n%s", r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), oldPin) {
		t.Errorf("the superseded pin was reported as a conflict:\n%s", r.combined())
	}
}

// TestMovingAResourceBetweenModulesDestroysAndRecreatesIt is spec §7.2's
// second cost, proved rather than asserted. The module instantiation is
// renamed from `old` to `new` and NOTHING else changes — same source, same
// inputs, same attributes — and the plan is a destroy and a create rather than
// a no-op. Documenting this in CLAUDE.md says it is true; this test is what
// makes the claim checkable, and the note is what puts it in front of the
// person about to approve it.
func TestMovingAResourceBetweenModulesDestroysAndRecreatesIt(t *testing.T) {
	const body = `
project: myapp
modules:
  - ./modules/db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  %s:
    type: module.db
    network: ${net.id}
    size: 30
`
	dir := projectWithFiles(t, fmt.Sprintf(body, "old"), map[string]string{"modules/db/module.yml": dbModule})

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes: a fresh apply always has changes)\n%s", a.ExitCode, a.combined())
	}
	if p := run(t, dir, "plan", "dev"); p.ExitCode != 0 {
		t.Fatalf("re-plan exit = %d, want 0 before the rename\n%s", p.ExitCode, p.combined())
	}

	writeIn(t, dir, "infra.yml", fmt.Sprintf(body, "new"))

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2 after the rename\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.Stdout, "1 to create")
	requireContains(t, r.Stdout, "1 to destroy")
	requireContains(t, r.Stdout, modHeader("fake.database", []string{"new"}, "store"))
	requireContains(t, r.Stdout, modHeader("fake.database", []string{"old"}, "store"))

	// The user is told what happened, on the destroy, before they approve it.
	note := lineContaining(t, r.Stdout, "destroyed and recreated")
	if !strings.Contains(note, address.Address{Module: []string{"new"}, Name: "store"}.String()) {
		t.Errorf("the note %q does not name the address the resource is reappearing at", note)
	}

	// And it is not shown on a plan where nothing moved.
	unchanged := projectWithFiles(t, fmt.Sprintf(body, "old"), map[string]string{"modules/db/module.yml": dbModule})
	if u := run(t, unchanged, "plan", "dev"); strings.Contains(u.Stdout, "destroyed and recreated") {
		t.Errorf("a first plan with nothing to destroy carries the move note:\n%s", u.Stdout)
	}
}
