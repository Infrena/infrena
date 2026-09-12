package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
)

// moduleFixture writes a project and returns the files and its directory.
func moduleFixture(t *testing.T, files map[string]string) ([]config.File, string) {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return loaded, dir
}

func rendered(ds diag.Diagnostics) string {
	var sb strings.Builder
	ds.Render(&sb)
	return sb.String()
}

const appStack = `
inputs:
  size:
    type: integer
    default: 10
resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    size: ${size}
    network: ${net.id}
outputs:
  endpoint:
    value: ${db.endpoint}
`

// TestCompileExpandsAModule is the milestone in one assertion: a module call
// becomes real resources under module-qualified addresses, the caller's input
// reaches them, and a reference written INSIDE the module resolves there.
func TestCompileExpandsAModule(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"modules/app-stack/module.yml": appStack,
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./modules/app-stack
resources:
  prod:
    type: module.app_stack
    size: 42
`,
	})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}

	// Nothing is addressed `prod`: a call is expanded away entirely (Ruling 7).
	if _, ok := cfg.Resources["prod"]; ok {
		t.Error("the call itself reached ResolvedConfig; stage 6 onward must not know modules exist")
	}
	db, ok := cfg.Resources["module.prod.db"]
	if !ok {
		t.Fatalf("no module.prod.db; got %v", sortedTargetsOf(cfg))
	}
	if n, _ := db.Attrs["size"].AsInt(); n != 42 {
		t.Errorf("size = %v, want the caller's 42 — the input did not reach the module", db.Attrs["size"])
	}
	// ${net.id} inside the module must resolve to the module's own net, which
	// is an edge, not to a root resource of the same name.
	var deps []string
	for _, d := range db.DependsOn {
		deps = append(deps, d.String())
	}
	if strings.Join(deps, ",") != "module.prod.net" {
		t.Errorf("db depends on %v, want [module.prod.net] — a reference inside a module must resolve inside it", deps)
	}
}

func sortedTargetsOf(cfg ResolvedConfig) []string {
	out := make([]string, 0, len(cfg.Resources))
	for k := range cfg.Resources {
		out = append(out, k)
	}
	return out
}

// TestCompileStopsAfterModuleErrors pins the halt, INCLUDING its cost: a
// module cycle suppresses stage 6 entirely, so the unrelated typo in this
// fixture is not reported until the cycle is fixed.
//
// That is deliberate. After a failed expansion the resource set stage 6 would
// walk is not the user's configuration, so every reference into the module
// that did not expand becomes "no such resource" — the real diagnostic buried
// under symptoms of itself.
func TestCompileStopsAfterModuleErrors(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"a/module.yml": "modules:\n  - ../b\nresources:\n  x:\n    type: module.b\n",
		"b/module.yml": "modules:\n  - ../a\nresources:\n  y:\n    type: module.a\n",
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./a
resources:
  top:
    type: module.a
  typo:
    type: test.network
    cidr: ${nosuchresource.id}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	got := rendered(ds)
	if !strings.Contains(got, "cycle") {
		t.Fatalf("the cycle must be reported:\n%s", got)
	}
	if strings.Contains(got, "nosuchresource") {
		t.Errorf("stage 6 ran after a module error; the halt is what keeps the real\n"+
			"diagnostic from being buried under symptoms of itself:\n%s", got)
	}
}

// TestTwoModulesEachDeclaringADbResolveSeparately is Ruling 1's whole purpose.
// Keyed on a bare name, both `db`s would be one resource and one module's
// reference would silently resolve to the other's — a wrong plan, not an error.
func TestTwoModulesEachDeclaringADbResolveSeparately(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"m/module.yml": "resources:\n  db:\n    type: test.network\n    cidr: 10.0.0.0/16\n  user:\n    type: test.database\n    engine: postgres\n    network: ${db.id}\n",
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./m
resources:
  one:
    type: module.m
  two:
    type: module.m
`,
	})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	for _, inst := range []string{"one", "two"} {
		user := cfg.Resources["module."+inst+".user"]
		if user == nil {
			t.Fatalf("no module.%s.user", inst)
		}
		var deps []string
		for _, d := range user.DependsOn {
			deps = append(deps, d.String())
		}
		want := "module." + inst + ".db"
		if strings.Join(deps, ",") != want {
			t.Errorf("module.%s.user depends on %v, want [%s] — each module's `db` is its own",
				inst, deps, want)
		}
	}
}

// TestAReferenceToAnAttributeThatDoesNotExistIsRefused closes the defect this
// check was ruled in for. Before it, ${store.endpoint} on a test.network
// validated clean, produced a clean plan, and failed halfway through apply
// after real infrastructure existed — with a message naming the symptom.
func TestAReferenceToAnAttributeThatDoesNotExistIsRefused(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"infra.yml": `
project: demo
environments: {dev: {}}
resources:
  store:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    password: ${store.endpoint}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	got := rendered(ds)
	if !ds.HasErrors() {
		t.Fatal("a reference to an attribute that does not exist must be refused at compile time")
	}
	for _, want := range []string{`test.network has no attribute "endpoint"`, "cidr", "id"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic does not mention %q — it must name the type and what it DOES offer:\n%s", want, got)
		}
	}
}

// TestAReferenceToAComputedAttributeIsStillFine is the boundary, and the half
// that keeps the check honest: every dependency edge in this codebase is a
// reference to a computed attribute, so an over-reaching check breaks the
// product entirely rather than subtly.
func TestAReferenceToAComputedAttributeIsStillFine(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"infra.yml": `
project: demo
environments: {dev: {}}
resources:
  store:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    network: ${store.id}
`,
	})

	if _, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir}); ds.HasErrors() {
		t.Fatalf("a reference to a computed attribute must remain legal:\n%s", rendered(ds))
	}
}

// TestAnUnboundNameNamesBothPossibilities pins Ruling 4's wording, which is
// implemented at exactly one site and asserted nowhere else. "no such resource"
// reads perfectly well and would satisfy every other test in this package.
func TestAnUnboundNameNamesBothPossibilities(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"infra.yml": `
project: demo
environments: {dev: {}}
resources:
  db:
    type: test.database
    engine: postgres
    network: ${nosuch.id}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	got := rendered(ds)
	if !strings.Contains(got, "or module") {
		t.Errorf("an unbound name must name BOTH possibilities: a user who mistyped a module "+
			"call and is told only about resources concludes the module never loaded:\n%s", got)
	}
}

// TestAMisspelledModuleOutputIsDistinguishedFromAnUnboundName. A call is
// expanded away and never appears in `declared`, so without the scope's second
// answer this would report `prod` as undeclared — naming a call the user can
// plainly see in their own file.
func TestAMisspelledModuleOutputIsDistinguishedFromAnUnboundName(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"modules/app-stack/module.yml": appStack,
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./modules/app-stack
resources:
  prod:
    type: module.app_stack
  db:
    type: test.database
    engine: postgres
    network: ${prod.nosuch}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	got := rendered(ds)
	if !strings.Contains(got, `module call "prod" has no output "nosuch"`) {
		t.Errorf("a bad output on a real call must say so, not report the call as undeclared:\n%s", got)
	}
	if !strings.Contains(got, "endpoint") {
		t.Errorf("the diagnostic must list the outputs the module DOES declare:\n%s", got)
	}
}
