package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/environments"
	"github.com/infrena/infrena/internal/modules"
	"github.com/infrena/infrena/internal/modules/source"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

// scopeFor builds the variable scope bindReferences now requires, through the
// real resolvers rather than a hand-made map — a test that constructs its own
// scope stops testing the thing that produces one.
func scopeFor(t *testing.T, opts Options) variables.Scope {
	t.Helper()
	chain, ds := environments.Resolve(nil, opts.Environment)
	if ds.HasErrors() {
		t.Fatalf("fixture chain: %+v", ds)
	}
	scope, ds, _ := variables.Resolve(nil, chain, nil, nil, opts.Vars)
	if ds.HasErrors() {
		t.Fatalf("fixture scope: %+v", ds)
	}
	seedProcessVariables(&scope, "fixture", opts)
	return scope
}

func decl(t *testing.T, body string) *config.ProjectDecl {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ds := config.Decode(files)
	if ds.HasErrors() {
		t.Fatalf("Decode: %+v", ds)
	}
	return p
}

func TestBindLiteralAttributesPassThrough(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	r := cfg.Resources["network"]
	if s, _ := r.Attrs["cidr"].AsString(); s != "10.0.0.0/16" {
		t.Errorf("cidr = %q", s)
	}
	if r.Attrs["cidr"].Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit", r.Attrs["cidr"].Source)
	}
}

func TestBindResourceReferenceBecomesUnknownWithAnEdge(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	db := cfg.Resources["database"]
	if db.Attrs["network"].Known {
		t.Error("a reference to a not-yet-created resource must be unknown")
	}
	if db.Attrs["network"].Expr == nil {
		t.Error("the unknown must carry its expression")
	}
	if len(db.DependsOn) != 1 || db.DependsOn[0].Name != "network" {
		t.Errorf("DependsOn = %v, want one edge to network", db.DependsOn)
	}
}

func TestBindExplicitDependsOnBecomesAnEdge(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    depends_on: [network]
`)
	cfg, _ := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	db := cfg.Resources["database"]
	if len(db.DependsOn) != 1 || db.DependsOn[0].Name != "network" {
		t.Errorf("DependsOn = %v", db.DependsOn)
	}
}

func TestBindDeduplicatesEdges(t *testing.T) {
	// A reference and an explicit depends_on naming the same target is one edge.
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
    depends_on: [network]
`)
	cfg, _ := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if got := cfg.Resources["database"].DependsOn; len(got) != 1 {
		t.Errorf("DependsOn = %v, want one edge", got)
	}
}

func TestBindReferenceToUnknownResourceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: fake.database
    engine: postgres
    network: ${nonexistent.id}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if !ds.HasErrors() {
		t.Fatal("a reference to a resource nobody declared can never become knowable and must be an error")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "nonexistent") {
		t.Errorf("diagnostic must name the missing resource:\n%s", out.String())
	}
}

func TestBindDependsOnUnknownResourceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: fake.database
    engine: postgres
    depends_on: [nonexistent]
`)
	if _, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable()); !ds.HasErrors() {
		t.Error("depends_on naming an undeclared resource must be an error")
	}
}

func TestBindSelfReferenceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: fake.database
    engine: ${database.engine}
`)
	if _, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable()); !ds.HasErrors() {
		t.Error("a resource referring to itself is a cycle of one and must be rejected")
	}
}

func TestBindCarriesLifecycleAndOrigin(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: fake.database
    engine: postgres
    lifecycle:
      prevent_destroy: true
`)
	cfg, _ := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	db := cfg.Resources["database"]
	if !db.Lifecycle.PreventDestroy {
		t.Error("lifecycle must survive binding")
	}
	if db.Origin.Line == 0 {
		t.Error("origin must survive binding, or diagnostics downstream lose their location")
	}
}

func TestBindResolvesCliVariables(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: ${var.cidr_block}
`)
	opts := Options{Environment: "dev", Vars: map[string]string{"cidr_block": "10.9.0.0/16"}}
	cfg, ds := bindReferences(rootOnly(t, p, opts), opts, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s, _ := cfg.Resources["network"].Attrs["cidr"].AsString(); s != "10.9.0.0/16" {
		t.Errorf("cidr = %q, want the --var value", s)
	}
}

func TestBindReportsEveryProblemAtOnce(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  a:
    type: fake.network
    cidr: ${missing_one.id}
  b:
    type: fake.network
    cidr: ${missing_two.id}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — one bad reference must not mask the next", len(ds))
	}
}

func TestBindPropagatesSensitivityIntoUnknowns(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    password: ${var.secret_value}-${network.id}
`)
	opts := Options{
		Environment: "dev",
		Vars:        map[string]string{"secret_value": "hunter2"},
	}
	cfg, ds := bindReferences(rootOnly(t, p, opts), opts, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	// The variable is not marked sensitive here (M4 adds secret vars), so this
	// asserts the mechanism rather than the classification: the unknown must
	// carry its expression so the executor can finish it.
	pw := cfg.Resources["database"].Attrs["password"]
	if pw.Known {
		t.Error("an attribute mixing a variable with a resource reference must be unknown")
	}
	if pw.Expr == nil {
		t.Error("and must carry its expression")
	}
}

// The self-reference check compares the full reference name against the
// declaring resource's name with ==, not a prefix or suffix test, so a
// resource whose name happens to be a substring of another's must not be
// falsely rejected as a self-reference.
func TestBindDoesNotFalselyFlagAPrefixNameAsSelfReference(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  db:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${db.id}
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("a reference to a different resource whose name is a prefix of the referrer's own name must not be rejected: %+v", ds)
	}
	if cfg.Resources["database"].Attrs["network"].Known {
		t.Error("network should be unknown pending db's creation")
	}
	if len(cfg.Resources["database"].DependsOn) != 1 || cfg.Resources["database"].DependsOn[0].Name != "db" {
		t.Errorf("DependsOn = %v, want one edge to db", cfg.Resources["database"].DependsOn)
	}
}

// TestBindRejectsInterpolationInsideAList was here until M10 implemented
// PLAN.md §10.1. It asserted that an expression nested inside a list was
// REFUSED, which was correct for the code that existed: the leaf was never
// parsed, so passing the composite through would have put raw "${...}" text into
// a plan looking like a literal.
//
// Its replacement asserts the opposite, and the direction that matters is the
// one it kept from the original: the leaf must be RESOLVED, never passed through
// as text.
func TestBindResolvesInterpolationInsideAList(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.0.0.0/16
  database:
    type: fake.database
    engine: postgres
    tags:
      - "${network.id}"
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		var sb strings.Builder
		ds.Render(&sb)
		t.Fatalf("an interpolation inside a list is resolved, not refused:\n%s", sb.String())
	}

	tags := cfg.Resources["database"].Attrs["tags"]
	items, ok := tags.Raw.([]value.Value)
	if !ok || len(items) != 1 {
		t.Fatalf("tags did not survive as a one-element list: %#v", tags.Raw)
	}
	// Raw text reaching the plan is the failure the original refusal existed to
	// prevent, and it stays prevented: the leaf is an UNKNOWN carrying its
	// expression, because network.id does not exist until apply.
	if s, _ := items[0].AsString(); s == "${network.id}" {
		t.Error("the leaf reached the plan as unparsed text")
	}
	if items[0].Known {
		t.Error("a reference to a not-yet-created resource must be unknown")
	}
	// And the EDGE was recorded from inside the list. Without it the plan reads
	// correctly and the apply fails waiting for a value nothing produced — the
	// defect M5's integration suite found twice.
	var deps []string
	for _, d := range cfg.Resources["database"].DependsOn {
		deps = append(deps, d.String())
	}
	if strings.Join(deps, ",") != "network" {
		t.Errorf("database depends on %v, want [network] — the edge inside the list went missing", deps)
	}
}

// recordEdge must not let which origin survives depend on the order edges are
// recorded in — Go map iteration over a resource's attributes is randomised,
// so if the last-visited attribute's origin always won, the origin behind a
// dependency edge would change from run to run of the very same
// configuration.
func TestRecordEdgeKeepsEarliestOrigin(t *testing.T) {
	edges := map[string]value.Origin{}
	recordEdge(edges, "network", value.Origin{File: "infra.yml", Line: 10})
	recordEdge(edges, "network", value.Origin{File: "infra.yml", Line: 3})
	recordEdge(edges, "network", value.Origin{File: "infra.yml", Line: 20})
	if got := edges["network"].Line; got != 3 {
		t.Errorf("edges[\"network\"].Line = %d, want 3 (the earliest of the origins recorded)", got)
	}
}

// TestBindSortsDependsOnEveryTime pins bind.go's address.Sort in
// sortedAddresses. DependsOn is built by ranging a map, so the order is
// randomised per run: one compile that happens to come out sorted proves
// nothing, and Go's small-map range is skewed toward insertion order, so the
// accidental-pass rate is high. Hence the loop, and hence four dependencies
// rather than two.
//
// Seeded through Compile rather than by constructing a ResolvedResource,
// because the property under test is that PRODUCTION establishes the ordering —
// a hand-built fixture would assert only that the test author sorted a slice.
func TestBindSortsDependsOnEveryTime(t *testing.T) {
	body := `
project: myapp
resources:
  zulu:
    type: fake.network
    cidr: 10.0.0.0/16
  yankee:
    type: fake.network
    cidr: 10.1.0.0/16
  xray:
    type: fake.network
    cidr: 10.2.0.0/16
  whiskey:
    type: fake.network
    cidr: 10.3.0.0/16
  app:
    type: fake.network
    cidr: 10.9.0.0/16
    depends_on: [zulu, yankee, xray, whiskey]
`
	want := []string{"whiskey", "xray", "yankee", "zulu"}
	for i := range 20 {
		resolved, ds := Compile(loadFiles(t, body), testRegistry(t), Options{Environment: "dev"})
		if ds.HasErrors() {
			t.Fatalf("unexpected diagnostics: %+v", ds)
		}
		app, ok := resolved.Get(address.Address{Name: "app"})
		if !ok {
			t.Fatal("app resource missing from the resolved config")
		}
		got := make([]string, 0, len(app.DependsOn))
		for _, d := range app.DependsOn {
			got = append(got, d.Name)
		}
		if len(got) != len(want) {
			t.Fatalf("DependsOn = %v, want %v", got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: DependsOn = %v, want it sorted as %v", i, got, want)
			}
		}
	}
}

func TestAWholeResourceReferenceIsProjectedToTheDeclaredAttribute(t *testing.T) {
	// fake.subnet's vpc_id declares References{fake.vpc, "id"}, so ${net}
	// must become ${net.id} before anything downstream sees it.
	p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.subnet
    vpc_id: ${net}
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	refs := cfg.Resources["sub"].Attrs["vpc_id"].Expr.References()
	if len(refs) != 1 {
		t.Fatalf("References() = %v, want exactly one", refs)
	}
	got := refs[0]
	if got.Attribute != "id" {
		t.Errorf("Attribute = %q, want %q — the projection reads the consuming attribute's declaration", got.Attribute, "id")
	}
	if got.Target.Name != "net" {
		t.Errorf("Target.Name = %q, want %q", got.Target.Name, "net")
	}
}

func TestAWholeResourceReferenceWithNoDeclarationIsAnError(t *testing.T) {
	// fake.subnet's `cidr` declares no References, so ${net} there cannot be
	// projected. The engine must NOT guess.
	p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.subnet
    cidr: ${net}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if !ds.HasErrors() {
		t.Fatal("passing a resource to an attribute that declares no reference must be an error, never a guess")
	}
	// I3: before this reference was tracked as "handled" once projectRefs had
	// reported on it, the still-empty attribute flowed on into the
	// attribute-axis check below and drew a second, unrelated "fake.vpc has
	// no attribute \"\"" — spec §2.2's promise that a missing declaration
	// "costs exactly nothing" was not met. Before this branch, the same
	// configuration was a single parse error.
	if len(ds) != 1 {
		t.Errorf("got %d diagnostics, want exactly 1: %s", len(ds), rendered(ds))
	}
	if !strings.Contains(ds[0].Detail, "${net.") {
		t.Errorf("Detail = %q, want it to show naming an attribute explicitly as the fix", ds[0].Detail)
	}
}

func TestNoEmptyAttributeReferenceEscapesStageSix(t *testing.T) {
	// The invariant Task 3's parser comment relies on. An empty attribute
	// downstream means ResourceScope misses, the value stays deferred forever,
	// and the resource is created with the attribute silently unset.
	p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.subnet
    vpc_id: ${net}
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	for _, rc := range cfg.Resources {
		for name, v := range rc.Attrs {
			for _, ref := range v.Expr.References() {
				if ref.Attribute == "" {
					t.Errorf("%s.%s kept an empty-attribute reference past stage 6", rc.Address, name)
				}
			}
		}
	}
}

// rootOnly wraps a project as stage 5 would when it contains no modules: every
// resource an instance at the root, each sharing the root scope.
//
// It exists so the tests written before M5 keep testing what they were written
// to test. A configuration with no `modules:` block is exactly this, and
// Expand produces it — going through Expand here instead would make every one
// of these unit tests an integration test of stage 5.
func rootOnly(t *testing.T, p *config.ProjectDecl, opts Options) *modules.Expansion {
	t.Helper()
	exp, ds := modules.Expand(p, scopeFor(t, opts), nil, modules.Env{Name: "dev"}, t.TempDir(), noRemotes{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("rootOnly: expanding a module-free project must not fail: %+v", ds)
	}
	return exp
}

// noRemotes is a Resolver that must never be called. Every fixture in this file
// is module-free, so a call means the test grew a module without meaning to.
type noRemotes struct{}

func (noRemotes) Resolve(s source.Source, _ string) (source.Resolution, diag.Diagnostics) {
	panic("compiler bind tests are module-free; nothing should resolve a source: " + s.String())
}

// TestAnUnregisteredConsumingTypeSkipsProjectionSilently pins fix round 1's
// Important, tightened by fix round 2's I3: consuming == nil in projectRefs
// was true both when the attribute declares no reference AND when the
// consuming resource's own type is unregistered (a nil def yields a nil
// attribute either way), so an unregistered type reaching a whole-resource
// reference got the "declares no reference" diagnostic AND then a second,
// unrelated one from the reference target's own attribute-axis check
// ("fake.vpc has no attribute \"\"") when the misprojected empty attribute
// reached it.
//
// An unregistered type must report NOTHING from this stage — not even the
// symptom-y "has no attribute \"\"" fix round 1 left standing. It has no
// schema to check anything against, and stage 7 (bindSchemas) already reports
// the unregistered type itself; that is the diagnostic that should stand
// alone, which is why bindReferences on its own must come back clean here.
// TestAnUnregisteredConsumingTypeStillFailsCompileOverall, below, is what
// proves the full pipeline still refuses the configuration.
func TestAnUnregisteredConsumingTypeSkipsProjectionSilently(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: bogus.thing
    vpc_id: ${net}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("stage 6 has no schema for an unregistered type and nothing useful to say about a "+
			"reference into one of its attributes — it must say nothing and let stage 7's own "+
			"\"unknown resource type\" diagnostic stand alone: %+v", ds)
	}
}

// TestAnUnregisteredConsumingTypeStillFailsCompileOverall is the full-Compile
// half of the test above: stage 6 staying silent must not mean the
// configuration compiles clean. The type is `fake.doesnotexist` rather than a
// wholly unknown plugin prefix, deliberately — an unrecognised PLUGIN fails
// earlier still, at stage 4.5 (providers.Prepare), before stage 6 or stage 7
// ever run; a type the loaded `fake` plugin itself does not define is what
// actually reaches stage 7 (bindSchemas), which reports the unregistered
// type on its own. That must be the ONLY diagnostic — not piled on top of a
// stage 6 symptom about the reference's still-empty attribute.
func TestAnUnregisteredConsumingTypeStillFailsCompileOverall(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"infra.yml": `
project: demo
environments: {dev: {}}
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.doesnotexist
    vpc_id: ${net}
`,
	})
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("an unregistered resource type must still fail the compile overall")
	}
	got := rendered(ds)
	if !strings.Contains(got, `unknown resource type "fake.doesnotexist"`) {
		t.Errorf("want stage 7's own diagnostic about the unregistered type:\n%s", got)
	}
	if strings.Contains(got, "has no attribute") {
		t.Errorf("must not also show the symptom of the reference's still-empty attribute:\n%s", got)
	}
}

func TestPassingTheWrongResourceTypeIsACompileError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  db:
    type: fake.database
  sub:
    type: fake.subnet
    vpc_id: ${db}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if !ds.HasErrors() {
		t.Fatal("vpc_id refers to fake.vpc; passing a fake.database must fail at compile time, not at the API")
	}
	if !strings.Contains(ds[0].Summary, "fake.vpc") || !strings.Contains(ds[0].Summary, "fake.database") {
		t.Errorf("Summary = %q, want both type names", ds[0].Summary)
	}
}

func TestNamingAnAttributeDoesNotEscapeTheTypeCheck(t *testing.T) {
	// The projection is sugar; the type check is not. Writing the attribute
	// out avoids the projection and must NOT avoid the check.
	p := decl(t, `
project: myapp
resources:
  db:
    type: fake.database
  sub:
    type: fake.subnet
    vpc_id: ${db.id}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if !ds.HasErrors() {
		t.Fatal("${db.id} into an attribute that refers to fake.vpc must fail too")
	}
}

func TestAnAttributeWithNoDeclarationIsNotTypeChecked(t *testing.T) {
	// Coverage buys checking; absence costs nothing. `cidr` declares no
	// reference, so anything may be interpolated into it, exactly as today.
	p := decl(t, `
project: myapp
resources:
  db:
    type: fake.database
  sub:
    type: fake.subnet
    cidr: ${db.engine}
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("an attribute with no declared reference must not be type checked: %+v", ds)
	}
	if cfg.Resources == nil {
		t.Fatal("expected a resolved config")
	}
}

func TestAPathIntoADeclaredMapIsKeyCheckedAtCompileTime(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.subnet
    cidr: ${net.meta.nmae}
`)
	_, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if !ds.HasErrors() {
		t.Fatal("a typo in a declared map key must fail at compile time, not halfway through apply")
	}
	if !strings.Contains(ds[0].Detail, "name") {
		t.Errorf("Detail = %q, want it to list the keys that exist", ds[0].Detail)
	}
}

func TestAPathIntoAnOpenMapIsStillUnchecked(t *testing.T) {
	// fake.vpc's tags is an open map, exactly as AWS tags are and always will
	// be. Declaring Fields for them would be a lie, so nil must stay a
	// first-class answer rather than a gap.
	p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.subnet
    cidr: ${net.tags.anything}
`)
	cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
	if ds.HasErrors() {
		t.Fatalf("an attribute with no declared Fields must accept any key, as today: %+v", ds)
	}
	if cfg.Resources == nil {
		t.Fatal("expected a resolved config")
	}
}

// TestAWholeResourceReferenceProjectsThroughAnAliasedAttribute. The consuming
// attribute is written as one of its ALIASES, which is how a user of a real plugin
// writes it: §14.1 shipped aliases so `cidr` stands for `CidrBlock`, and the AWS
// plugin's naming design rests on them.
//
// The lookup used to be exact against the user's spelling, so `vpc: ${net}` reported
// "declares no reference" about an attribute that plainly declares one — while the
// canonical `vpc_id: ${net}` worked. Reported by the AWS plugin's author against
// v0.6.1, on aws.subnet's VpcId.
func TestAWholeResourceReferenceProjectsThroughAnAliasedAttribute(t *testing.T) {
	for _, spelling := range []string{"vpc_id", "vpc"} {
		t.Run(spelling, func(t *testing.T) {
			p := decl(t, `
project: myapp
resources:
  net:
    type: fake.vpc
  sub:
    type: fake.subnet
    `+spelling+`: ${net}
`)
			cfg, ds := bindReferences(rootOnly(t, p, Options{Environment: "dev"}), Options{Environment: "dev"}, testRegistry(t), testTable())
			if ds.HasErrors() {
				t.Fatalf("unexpected diagnostics: %+v", ds)
			}
			// Keyed by the spelling as written: attribute keys are canonicalised
			// later than this stage, which is itself why the projection's own
			// lookup had to canonicalise rather than assume.
			refs := cfg.Resources["sub"].Attrs[spelling].Expr.References()
			if len(refs) != 1 {
				t.Fatalf("References() = %v, want exactly one", refs)
			}
			if got := refs[0].Attribute; got != "id" {
				t.Errorf("projected to %q, want %q", got, "id")
			}
		})
	}
}
