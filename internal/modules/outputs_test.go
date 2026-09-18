package modules

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/expressions"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/value"
)

// evalAt parses, qualifies and evaluates src in the named resource's scope —
// the exact sequence compiler stage 6 performs.
func evalAt(t *testing.T, exp *Expansion, resource, src string) (value.Value, diag.Diagnostics) {
	t.Helper()
	scope := scopeOf(t, exp, resource)
	origin := value.Origin{File: "infra.yml", Line: 1, Column: 1}

	e, ds := expressions.Parse(src, origin)
	if ds.HasErrors() {
		t.Fatalf("fixture expression %q does not parse: %+v", src, ds)
	}
	v, evalDiags := expressions.Evaluate(scope.Qualify(e), scope)
	ds.Extend(evalDiags)
	return v, ds
}

func TestBareNameInsideAModuleResolvesToTheQualifiedAddress(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./net\nresources:\n  netA:\n    type: module.net\n",
		"net/module.yml": `
resources:
  db:
    type: test.thing
  web:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${db.endpoint}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	// THE Ruling 1 landmine. A bare `db` inside a module must not resolve to a
	// root `db`, nor to another module's.
	if got := v.Expr.Ref.Target.String(); got != "module.netA.db" {
		t.Errorf("reference target = %q, want %q — a bare name inside a module names THAT "+
			"module's resource", got, "module.netA.db")
	}
}

// TestQualifyPreservesAPathIntoAResourceAttribute guards the BindsResource
// branch, which used to rebuild Ref from Target and Attribute alone — dropping
// Path. ${server.tags.owner} inside a module would qualify the target
// correctly and then silently return the whole `tags` map instead of stepping
// into it.
func TestQualifyPreservesAPathIntoAResourceAttribute(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./net\nresources:\n  netA:\n    type: module.net\n",
		"net/module.yml": `
resources:
  server:
    type: test.thing
  web:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${server.tags.owner}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got := v.Expr.Ref.Target.String(); got != "module.netA.server" {
		t.Fatalf("reference target = %q, want %q", got, "module.netA.server")
	}
	if len(v.Expr.Ref.Path) != 1 || v.Expr.Ref.Path[0].Kind != value.StepKey || v.Expr.Ref.Path[0].Key != "owner" {
		t.Errorf("Qualify lost the path: Ref.Path = %#v, want a single key step \"owner\" — "+
			"${server.tags.owner} would silently resolve to the whole tags map", v.Expr.Ref.Path)
	}
}

func TestBareNameResolvesToAModuleCallsOutput(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
`,
		"db/module.yml": `
resources:
  server:
    type: test.thing
outputs:
  connection_string:
    value: ${server.endpoint}
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${database.connection_string}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if v.Known {
		t.Fatal("an output reading a resource attribute is unknown at plan time (Ruling 5)")
	}
	// The edge must land on the real resource inside the module, because that
	// is what has to exist before the value becomes knowable.
	if got := v.Expr.Ref.Target.String(); got != "module.database.server" {
		t.Errorf("reference target = %q, want %q", got, "module.database.server")
	}
}

// Ruling 5's explicit requirement: never an empty string.
func TestUnknownModuleOutputNeverRendersAsAnEmptyString(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
`,
		"db/module.yml": `
resources:
  server:
    type: test.thing
outputs:
  connection_string:
    value: ${server.endpoint}
`,
	})

	exp, _ := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	v, ds := evalAt(t, exp, "web", "postgres://${database.connection_string}/app")
	if ds.HasErrors() {
		t.Fatalf("an unknown output must not be a coercion failure: %+v", ds)
	}
	if v.Known {
		t.Fatal("unknownness is contagious: a string built from an unknown output is unknown")
	}
	rendered := value.Format(v, value.ProseFormatOptions)
	if rendered == "" || rendered == "postgres:///app" {
		t.Fatalf("rendered as %q; an unknown output must render as the renderer's unknown "+
			"text, never as an empty string spliced into a plan", rendered)
	}
}

func TestKnownModuleOutputFoldsToALiteralFromTheModule(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
    region: eu-west-1
`,
		"db/module.yml": `
inputs:
  region:
    type: string
resources:
  server:
    type: test.thing
outputs:
  home:
    value: ${var.region}
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${database.home}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := v.AsString(); s != "eu-west-1" {
		t.Errorf("output = %v, want eu-west-1", v.Raw)
	}
	if v.Source != value.SourceModule {
		t.Errorf("source = %v, want SourceModule — at the call site the honest answer to "+
			"\"what kind of thing is this\" is that it came out of a module", v.Source)
	}
}

// Amendment 11: Qualify reports nothing and supplies candidates. These pin the
// DATA the general check builds its message from; Task 8 asserts the message.
func TestQualifyLeavesAnUnresolvableReferenceForTheGeneralCheck(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  web:
    type: test.thing
  database:
    type: module.db
`,
		// Three outputs, declared so that NO natural order produces the sorted
		// one: `zone` is first in the file and last alphabetically, `host` is
		// last in the file and in the middle. A single-output fixture — or one
		// already in order — passes against an implementation that never sorts,
		// and it looks tidier, which is why it has to be said rather than left
		// to care.
		"db/module.yml": `
resources:
  server:
    type: test.thing
outputs:
  zone:
    value: a
  alpha:
    value: b
  host:
    value: fixed
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	scope := scopeOf(t, exp, "web")

	// A name bound to nothing: the node comes back untouched, so stage 6 sees a
	// reference it can report on with the candidate list below.
	e, _ := expressions.Parse("${databse.host}", value.Origin{File: "infra.yml", Line: 1})
	if got := scope.Qualify(e); got != e {
		t.Errorf("Qualify rewrote an unresolvable reference (%v); it must leave it exactly as "+
			"it stands so ONE check reports it", got)
	}

	names := scope.Names()
	if strings.Join(names, ",") != "database,web" {
		t.Errorf("Names() = %v, want [database web] sorted — this is the candidate list the "+
			"general check prints, and Ruling 4 requires it to include BOTH a mistyped "+
			"resource and a mistyped module call", names)
	}

	// A module call whose output does not exist: same treatment, different
	// candidate source.
	e2, _ := expressions.Parse("${database.hsot}", value.Origin{File: "infra.yml", Line: 2})
	if got := scope.Qualify(e2); got != e2 {
		t.Errorf("Qualify rewrote a reference to a nonexistent output (%v)", got)
	}
	outs, ok := scope.OutputNames("database")
	if !ok {
		t.Fatal("OutputNames must answer for a module call — it is the module arm of the " +
			"attribute-existence check (Amendment 13c)")
	}
	if strings.Join(outs, ",") != "alpha,host,zone" {
		t.Errorf("OutputNames(database) = %v, want [alpha host zone] SORTED. It is read from a "+
			"map, so without the sort this list reorders between identical runs — and it "+
			"feeds a diagnostic telling the user what they could have written instead, "+
			"which is a user-visible defect no other test would notice", outs)
	}
	if _, ok := scope.OutputNames("web"); ok {
		t.Error("OutputNames must report false for a plain resource; its attributes come from " +
			"its schema, which is the other arm of the same check")
	}
}

// PLAN.md §11's flagship example. `application` sorts BEFORE `database`, so name
// order expands it first and its attribute reads an output that does not exist
// yet. The fixture's names contradict the required order on purpose.
func TestSiblingCallsAreExpandedInDependencyOrderNotNameOrder(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./app
  - ./db
resources:
  application:
    type: module.app
    database_url: ${database.connection_string}
  database:
    type: module.db
`,
		"app/module.yml": `
inputs:
  database_url:
    type: string
resources:
  server:
    type: test.thing
`,
		"db/module.yml": `
resources:
  pg:
    type: test.thing
outputs:
  connection_string:
    value: ${pg.endpoint}
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("PLAN.md §11's own example must compile: %+v", ds)
	}

	v, ok := scopeOf(t, exp, "server").Variable("database_url")
	if !ok {
		t.Fatal("database_url is not in the application module's scope")
	}
	if v.Known {
		t.Fatal("it reads an output that reads a resource attribute; it is unknown at plan time")
	}
	if got := v.Expr.Ref.Target.String(); got != "module.database.pg" {
		t.Errorf("database_url defers to %q, want %q — name order would have expanded "+
			"`application` first and resolved this against nothing", got, "module.database.pg")
	}
}

func TestSiblingCallsReferencingEachOtherAreRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  a:
    type: module.m
    text: ${b.out}
  b:
    type: module.m
    text: ${a.out}
`,
		"m/module.yml": `
inputs:
  text:
    type: string
resources:
  thing:
    type: test.thing
outputs:
  out:
    value: ${var.text}
`,
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if !hasFragment(ds, "module calls reference each other in a cycle") {
		t.Errorf("two siblings each reading the other's output have no valid order; got %+v", ds)
	}
	if !hasFragment(ds, "a -> b -> a") {
		t.Errorf("the diagnostic must show the cycle, not merely assert one (§7.4); got %+v", ds)
	}
	for _, s := range summaries(ds) {
		if strings.Contains(s, "never terminates") {
			t.Fatalf("reported as a SOURCE cycle (%q); these siblings terminate fine and simply "+
				"have no valid order — two different failures", s)
		}
	}
}

// Ruling 5's Coerce case, by Amendment 2's chain: an unknown INTEGER leaves
// module a as an output and enters module b's input, declared `type: float`.
//
// `count` is supplied from a root variable that is declared but unset rather
// than left unsupplied, so the fixture produces NO diagnostics: an unsupplied
// defaultless input is a diagnostic in its own right (Task 5), and a fixture
// carrying an unrelated error is one whose real assertion nobody can trust.
func TestUnknownIntegerOutputCoercesToAFloatInputAndStaysUnknown(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
variables:
  scale:
    type: integer
modules:
  - ./a
  - ./b
resources:
  ca:
    type: module.a
    count: ${var.scale}
  cb:
    type: module.b
    ratio: ${ca.n}
`,
		"a/module.yml": `
inputs:
  count:
    type: integer
resources:
  thing:
    type: test.thing
outputs:
  n:
    value: ${var.count}
`,
		"b/module.yml": `
inputs:
  ratio:
    type: float
resources:
  worker:
    type: test.thing
`,
	})

	// What `infra validate` hands stage 5: an environment-scoped variable with
	// no environment selected resolves to an unknown of its DECLARED kind
	// (internal/variables/resolve.go:327).
	var vars variables.Scope
	vars.Override("scale", value.Unknown(value.KindInt, value.SourceVariable))

	exp, ds := Expand(decl, vars, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("an unknown integer is exact to retype as a float — there is no datum to "+
			"round: %+v", ds)
	}

	got, ok := scopeOf(t, exp, "worker").Variable("ratio")
	if !ok {
		t.Fatal("ratio is not in module b's scope")
	}
	if got.Known {
		t.Fatal("it must still be unknown; Coerce retypes the claim, it does not invent a datum")
	}
	if got.Kind != value.KindFloat {
		t.Errorf("ratio kind = %v, want KindFloat — this is value.Coerce's unknown branch, "+
			"added in M4 as dead code with a comment saying it goes live in M5", got.Kind)
	}
	if got.Raw != nil {
		t.Error("Value's contract is that Raw is nil when Known is false")
	}
}

// The half of the chain the uniform OpLiteral fold buys. An Expr-splice would
// lose the Kind here and the test above would report a type mismatch instead of
// reaching Coerce.
func TestAnUnknownOutputKeepsItsKindAcrossTheModuleBoundary(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
variables:
  scale:
    type: integer
modules:
  - ./a
resources:
  web:
    type: test.thing
  ca:
    type: module.a
    count: ${var.scale}
`,
		"a/module.yml": `
inputs:
  count:
    type: integer
resources:
  thing:
    type: test.thing
outputs:
  n:
    value: ${var.count}
`,
	})

	var vars variables.Scope
	vars.Override("scale", value.Unknown(value.KindInt, value.SourceVariable))

	exp, ds := Expand(decl, vars, nil, Env{Name: "dev"}, dir, paths{}, nil, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	v, ds := evalAt(t, exp, "web", "${ca.n}")
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if v.Kind != value.KindInt {
		t.Errorf("kind = %v, want KindInt — an unknown output must cross the boundary carrying "+
			"the kind it had inside the module; the evaluator re-derives an unresolved "+
			"reference as KindString, which is why Qualify folds a VALUE and not an Expr", v.Kind)
	}
}
