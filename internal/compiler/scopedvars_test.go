package compiler

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/value"
)

// Directory-scoped variables (PLAN.md §4.1, §7's directory rung).
//
// These go through Compile rather than through variables.Resolve directly, and
// deliberately so. Resolve answers "what beats what", which is half the
// feature; the other half is "who can see it", and that half lives in the seam
// between config.Load's walk, stage 4's per-directory runs, and stage 5's
// Scope.In. Every defect this feature could have is at one of those joins, so
// a test that stopped at the ladder would pin the half that was never in doubt.

// scopedFixture is the shared layout: a project-wide `size`, one directory that
// overrides it, and a second directory that must not see the override.
//
// `size` is DECLARED (so --var can type it) and `db_only` is not, which is
// legal — an undeclared variable is untyped, not an error. db_only could not be
// declared here even if it were useful to: declaring a variable with no
// `default:` makes it required of the PROJECT, and a directory supplying it
// does not satisfy that, so infra.yml would refuse to compile before any of
// these tests reached their assertion. Whether a directory OUGHT to be able to
// satisfy a project-wide declaration is an open question (docs/…/m7-layout.md,
// Task 3), not something these tests assume either way.
func scopedFixture(t *testing.T, extra map[string]string) ([]config.File, string) {
	t.Helper()
	files := map[string]string{
		"infra.yml": `
project: demo
variables:
  size:
    type: integer
environments:
  dev: {type: development}
  production:
    type: production
    size: 100
`,
		"variables.yml": "size: 10\n",
		"resources/db/vars/sizes.yml": `
size: 50
db_only: 7
`,
		"resources/net/net.yml": `
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
`,
		"resources/db/db.yml": `
resources:
  db:
    type: test.database
    engine: postgres
    size: ${size}
    network: ${net.id}
`,
		"resources/app/app.yml": `
resources:
  app:
    type: test.database
    engine: postgres
    size: ${size}
    network: ${net.id}
`,
	}
	for path, body := range extra {
		files[path] = body
	}
	return moduleFixture(t, files)
}

func sizeOf(t *testing.T, cfg ResolvedConfig, addr string) int64 {
	t.Helper()
	r, ok := cfg.Resources[addr]
	if !ok {
		t.Fatalf("no resource %q; got %v", addr, sortedTargetsOf(cfg))
	}
	n, ok := r.Attrs["size"].AsInt()
	if !ok {
		t.Fatalf("%s size = %v, which is not an integer", addr, r.Attrs["size"])
	}
	return n
}

// TestADirectoryScopedVariableBeatsTheProjectWideOne is §7's new rung: a
// directory saying something about its own resources is more specific than the
// project saying it about all of them.
func TestADirectoryScopedVariableBeatsTheProjectWideOne(t *testing.T) {
	files, dir := scopedFixture(t, nil)

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	if got := sizeOf(t, cfg, "db"); got != 50 {
		t.Errorf("db size = %d, want 50 from resources/db/vars/sizes.yml — variables.yml's 10 outranked a more specific file", got)
	}
}

// TestADirectoryScopedVariableIsNotVisibleElsewhere is the SCOPING half, and
// the one that makes the feature worth having.
//
// It asserts on a variable NO other file declares, so the only way `app` can
// resolve it is if the scoping leaked. Asserting merely that `app` kept the
// project-wide 10 would pass against an implementation that made every scoped
// variable global and simply lost the race — `db_only` cannot be explained
// that way.
func TestADirectoryScopedVariableIsNotVisibleElsewhere(t *testing.T) {
	files, dir := scopedFixture(t, map[string]string{
		"resources/app/app.yml": `
resources:
  app:
    type: test.database
    engine: postgres
    size: ${db_only}
    network: ${net.id}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("resources/app read `db_only`, which only resources/db/vars declares; a directory's variables must stop at that directory")
	}
	if out := rendered(ds); !strings.Contains(out, "db_only") {
		t.Errorf("the diagnostic must name the variable that could not be resolved; got:\n%s", out)
	}
}

// TestAnEnvironmentBeatsADirectoryScopedVariable is the two axes. A directory
// is how the project is ORGANISED; an environment is where it is DEPLOYED. If a
// directory outranked an environment, production could no longer tune a value a
// directory had set locally — and finding that out would need a production
// apply.
func TestAnEnvironmentBeatsADirectoryScopedVariable(t *testing.T) {
	files, dir := scopedFixture(t, nil)

	prod, ds := Compile(files, testRegistry(t), Options{Environment: "production", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	if got := sizeOf(t, prod, "db"); got != 100 {
		t.Errorf("db size in production = %d, want the environment's 100", got)
	}

	// The other half of the same assertion: without this, a build that ignored
	// directory variables entirely would pass the check above, because 100 is
	// also what it would produce.
	files, dir = scopedFixture(t, nil)
	dev, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	if got := sizeOf(t, dev, "db"); got != 50 {
		t.Errorf("db size in dev = %d, want the directory's 50 — the environment must win only where it says something", got)
	}
}

// TestDashDashVarBeatsADirectoryScopedVariable — the operator saying what they
// want right now is the one rung no file outranks, including a file that is
// more specific than every other file.
//
// production is selected deliberately: --var has to beat the environment AND
// the directory in the same run, which is the case a ladder that inserted the
// new rung in the wrong place would get wrong.
func TestDashDashVarBeatsADirectoryScopedVariable(t *testing.T) {
	files, dir := scopedFixture(t, nil)

	cfg, ds := Compile(files, testRegistry(t), Options{
		Environment: "production",
		Dir:         dir,
		Vars:        map[string]string{"size": "77"},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	if got := sizeOf(t, cfg, "db"); got != 77 {
		t.Errorf("db size = %d, want --var's 77", got)
	}
}

// TestThePlanNamesTheScopeAValueCameFrom is provenance. A new rung that renders
// as an existing one is a rung a user cannot tell apart — and `explain`, the
// plan's `[default]` marks and `infra plan`'s value annotations all read this
// one field.
func TestThePlanNamesTheScopeAValueCameFrom(t *testing.T) {
	files, dir := scopedFixture(t, nil)

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}

	db := cfg.Resources["db"].Attrs["size"]
	if db.Scope != value.ScopeScopedVars {
		t.Errorf("db size scope = %v, want ScopeScopedVars", db.Scope)
	}
	if got := value.ScopeLabel(db); got != "directory vars" {
		t.Errorf("db size renders as %q, want %q", got, "directory vars")
	}

	// The neighbouring value must NOT have moved onto the new rung. A rung
	// applied unconditionally would relabel every variable in the project and
	// still pass the two assertions above.
	app := cfg.Resources["app"].Attrs["size"]
	if app.Scope != value.ScopeBaseConfig {
		t.Errorf("app size scope = %v, want ScopeBaseConfig — it comes from variables.yml", app.Scope)
	}
}

// TestAModuleCallInADirectorySeesItsVariablesAndItsSiblings is the case that
// exercises Scope.In's SHARED binding table, and the only one that can.
//
// A plain resource's references are resolved against the un-narrowed level
// scope (see bindAttribute), so the test below cannot tell a shared table from
// a copied one. Nor can a call referring to a plain SIBLING: at the root level
// Qualify has no module path to prepend, so `${net.id}` resolves to the same
// address whether the binding was found or not.
//
// A module OUTPUT is the case where the table is load-bearing. `${stack.endpoint}`
// means nothing after expansion — `stack` is addressed as nothing at all — so it
// resolves ONLY through the binding the level recorded when it expanded that
// call. Hand the second call a copied table and that binding is not in it.
//
// It also pins the variable half in the same place: the call's `size` input
// must pick up resources/db/vars, because a call's inputs are evaluated in the
// CALLER's scope and the caller is a file in that directory.
func TestAModuleCallInADirectorySeesItsVariablesAndItsSiblings(t *testing.T) {
	files, dir := scopedFixture(t, map[string]string{
		"modules/sized/module.yml": `
inputs:
  size:
    type: integer
  network:
    type: string
resources:
  inner:
    type: test.database
    engine: postgres
    size: ${size}
    network: ${network}
outputs:
  endpoint:
    value: ${inner.endpoint}
  label:
    value: label-${size}
`,
		"modules/sink/module.yml": `
inputs:
  upstream:
    type: string
resources:
  probe:
    type: test.network
    cidr: ${upstream}
`,
		"resources/db/call.yml": `
resources:
  stack:
    type: module.sized
    size: ${size}
    network: ${net.id}
  sink:
    type: module.sink
    upstream: ${stack.label}
`,
	})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}

	if got := sizeOf(t, cfg, "module.stack.inner"); got != 50 {
		t.Errorf("the call's size input resolved to %d, want resources/db/vars' 50 — a call's inputs are evaluated in the caller's scope, and its caller is a file in that directory", got)
	}

	var deps []string
	for _, d := range cfg.Resources["module.stack.inner"].DependsOn {
		deps = append(deps, d.String())
	}
	if strings.Join(deps, ",") != "net" {
		t.Errorf("module.stack.inner depends on %v, want [net]", deps)
	}

	// The binding that only a shared table can supply. `stack` is addressed as
	// nothing once expanded, so `${stack.label}` resolves ONLY through the
	// binding the level recorded when it expanded that call — and Qualify folds
	// a KNOWN output into a literal right there. Hand the second call a copied
	// table and the fold does not happen: the value survives as an unknown
	// carrying its expression, which is indistinguishable from a legitimate
	// not-yet-created reference everywhere except here.
	cidr, ok := cfg.Resources["module.sink.probe"].Attrs["cidr"].AsString()
	if !ok || cidr != "label-50" {
		t.Errorf("module.sink.probe cidr = %v, want \"label-50\" — the narrowed scope lost the level's module bindings",
			cfg.Resources["module.sink.probe"].Attrs["cidr"])
	}
}

// TestAResourceCanReferToOneInAnotherDirectory pins the plain-resource half of
// the same rule: a directory scopes VARIABLES; it does not scope names.
//
// config.ResourceDecl.Dir is deliberately not part of a resource's identity, so
// `${net.id}` in resources/db/ naming a resource declared in resources/net/ is
// an ordinary sibling reference.
//
// This one holds for a reason the test above does not cover: bindAttribute
// resolves references against the un-narrowed scope on purpose. It would keep
// passing if In copied the binding table instead of sharing it — which is what
// the module-call test is for.
func TestAResourceCanReferToOneInAnotherDirectory(t *testing.T) {
	files, dir := scopedFixture(t, nil)

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("a reference across two resources directories must resolve:\n%s", rendered(ds))
	}
	var deps []string
	for _, d := range cfg.Resources["db"].DependsOn {
		deps = append(deps, d.String())
	}
	if strings.Join(deps, ",") != "net" {
		t.Errorf("db depends on %v, want [net] — the edge across directories went missing", deps)
	}
}
