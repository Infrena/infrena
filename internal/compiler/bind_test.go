package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/config"
	"infra/internal/environments"
	"infra/internal/variables"
	"infra/pkg/address"
	"infra/pkg/value"
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
	scope, ds := variables.Resolve(nil, chain, nil, opts.Vars)
	if ds.HasErrors() {
		t.Fatalf("fixture scope: %+v", ds)
	}
	seedProcessVariables(&scope, opts)
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
    type: test.network
    cidr: 10.0.0.0/16
`)
	cfg, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
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
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	cfg, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
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
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    depends_on: [network]
`)
	cfg, _ := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
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
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
    depends_on: [network]
`)
	cfg, _ := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
	if got := cfg.Resources["database"].DependsOn; len(got) != 1 {
		t.Errorf("DependsOn = %v, want one edge", got)
	}
}

func TestBindReferenceToUnknownResourceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    network: ${nonexistent.id}
`)
	_, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
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
    type: test.database
    engine: postgres
    depends_on: [nonexistent]
`)
	if _, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("depends_on naming an undeclared resource must be an error")
	}
}

func TestBindSelfReferenceIsAnError(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: ${database.engine}
`)
	if _, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"}); !ds.HasErrors() {
		t.Error("a resource referring to itself is a cycle of one and must be rejected")
	}
}

func TestBindCarriesLifecycleAndOrigin(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    lifecycle:
      prevent_destroy: true
`)
	cfg, _ := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
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
    type: test.network
    cidr: ${cidr_block}
`)
	opts := Options{Environment: "dev", Vars: map[string]string{"cidr_block": "10.9.0.0/16"}}
	cfg, ds := bindReferences(p, scopeFor(t, opts), opts)
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
    type: test.network
    cidr: ${missing_one.id}
  b:
    type: test.network
    cidr: ${missing_two.id}
`)
	_, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — one bad reference must not mask the next", len(ds))
	}
}

func TestBindPropagatesSensitivityIntoUnknowns(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    password: ${secret_value}-${network.id}
`)
	opts := Options{
		Environment: "dev",
		Vars:        map[string]string{"secret_value": "hunter2"},
	}
	cfg, ds := bindReferences(p, scopeFor(t, opts), opts)
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
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${db.id}
`)
	cfg, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
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

// HasExpressions is set whenever any leaf of a list or map contains "${", but
// that leaf was never parsed as an expression — it is still raw, unevaluated
// text. bindAttribute must report this rather than pass the composite through
// unchanged, which would let unparsed "${...}" text reach the plan looking
// like a literal value.
func TestBindRejectsInterpolationInsideAList(t *testing.T) {
	p := decl(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    tags:
      - "${network.id}"
`)
	_, ds := bindReferences(p, scopeFor(t, Options{Environment: "dev"}), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("an expression nested inside a list is not supported and must be reported, not silently dropped or passed through unparsed")
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
    type: test.network
    cidr: 10.0.0.0/16
  yankee:
    type: test.network
    cidr: 10.1.0.0/16
  xray:
    type: test.network
    cidr: 10.2.0.0/16
  whiskey:
    type: test.network
    cidr: 10.3.0.0/16
  app:
    type: test.network
    cidr: 10.9.0.0/16
    depends_on: [zulu, yankee, xray, whiskey]
`
	want := []string{"whiskey", "xray", "yankee", "zulu"}
	for i := 0; i < 20; i++ {
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
