package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/diag"
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
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    size: ${var.size}
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
    type: fake.network
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
		"m/module.yml": "resources:\n  db:\n    type: fake.network\n    cidr: 10.0.0.0/16\n  user:\n    type: fake.database\n    engine: postgres\n    network: ${db.id}\n",
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
// check was ruled in for. Before it, ${store.endpoint} on a fake.network
// validated clean, produced a clean plan, and failed halfway through apply
// after real infrastructure existed — with a message naming the symptom.
func TestAReferenceToAnAttributeThatDoesNotExistIsRefused(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"infra.yml": `
project: demo
environments: {dev: {}}
resources:
  store:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    password: ${store.endpoint}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	got := rendered(ds)
	if !ds.HasErrors() {
		t.Fatal("a reference to an attribute that does not exist must be refused at compile time")
	}
	for _, want := range []string{`fake.network has no attribute "endpoint"`, "cidr", "id"} {
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
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
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
    type: fake.database
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
    type: fake.database
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

// TestAWholeResourceReferenceAsAModuleInputDoesNotEscapeAsAnEmptyAttribute is
// the module-path sibling to internal/compiler's own
// TestNoEmptyAttributeReferenceEscapesStageSix: that one only exercises a
// root-level direct reference. A whole-resource reference passed THROUGH a
// module input takes a different evaluation path (modules.evaluateCall,
// stage 5), which stage 6's projectRefs never sees — fix round 1's Critical.
func TestAWholeResourceReferenceAsAModuleInputDoesNotEscapeAsAnEmptyAttribute(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"modules/net-user/module.yml": `
inputs:
  vpc:
    type: string
resources:
  sub:
    type: fake.subnet
    vpc_id: ${var.vpc}
    cidr: 10.0.0.0/24
`,
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./modules/net-user
resources:
  net:
    type: fake.vpc
  user:
    type: module.net_user
    vpc: ${net}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a whole-resource reference passed as a module input must be refused before it " +
			"can reach the executor with an empty attribute")
	}
	if !strings.Contains(rendered(ds), "module input") {
		t.Errorf("the diagnostic must say why:\n%s", rendered(ds))
	}
}

// TestAWholeResourceReferenceToAModuleCallIsRefusedNotMisprojected is I2: a
// module call is not a resource with schema'd attributes, so projectRefs must
// not treat it like one. Before the fix, `vpc_id: ${prod}` against a
// consuming attribute that DOES declare References — fake.subnet's vpc_id —
// let Qualify decline to fold (there is no output named ""), then let
// projectRefs write "id" onto the still-empty attribute anyway, producing a
// self-contradicting diagnostic: `module call "prod" has no output "id"`,
// naming an attribute the user never wrote.
func TestAWholeResourceReferenceToAModuleCallIsRefusedNotMisprojected(t *testing.T) {
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
  sub:
    type: fake.subnet
    vpc_id: ${prod}
    cidr: 10.0.0.0/24
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a bare reference to a module call must be refused, not silently projected")
	}
	got := rendered(ds)
	if strings.Contains(got, `has no output "id"`) {
		t.Errorf("must not name an attribute (\"id\") the user never wrote — that is projectRefs "+
			"treating a module call as though it were a fake.vpc:\n%s", got)
	}
	if !strings.Contains(got, "module") || !strings.Contains(got, "output") {
		t.Errorf("the diagnostic must say a module exposes outputs and point at naming one, per the "+
			"design spec's §8 table:\n%s", got)
	}
	if !strings.Contains(got, "endpoint") {
		t.Errorf("the diagnostic must list the outputs the module DOES declare:\n%s", got)
	}
}

// TestAWholeResourceReferenceToAModuleCallWithNoConsumingDeclarationIsOneDiagnostic
// is I3's module-call variant: `cidr: ${prod}`, where cidr declares no
// References at all. Before the fix this produced BOTH "passes a resource to
// an attribute" (wrong — prod is a module, not a resource) AND a second,
// unrelated `module call "prod" has no output ""`, itself naming an
// attribute nobody wrote (an empty string) and inviting the equally wrong
// fix "add \"\" to the module's outputs:".
func TestAWholeResourceReferenceToAModuleCallWithNoConsumingDeclarationIsOneDiagnostic(t *testing.T) {
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
  sub:
    type: fake.subnet
    vpc_id: ${prod.endpoint}
    cidr: ${prod}
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a bare reference to a module call must be refused even where the consuming " +
			"attribute declares no reference at all")
	}
	got := rendered(ds)
	if strings.Contains(got, "passes a resource") {
		t.Errorf("must not call a module call a \"resource\":\n%s", got)
	}
	if strings.Contains(got, `has no output ""`) || strings.Contains(got, `add "" to`) {
		t.Errorf("must not report on an empty-string output name — that is the same still-empty "+
			"attribute reaching a second check:\n%s", got)
	}
	if !strings.Contains(got, "module") || !strings.Contains(got, "output") {
		t.Errorf("the diagnostic must say a module exposes outputs and point at naming one:\n%s", got)
	}
}

// TestAWholeResourceReferenceAsAModuleOutputDoesNotEscapeAsAnEmptyAttribute is
// the C1 regression: a module publishing `outputs: {whole: {value: ${net}}}`,
// where net is a resource INSIDE the module, used to compile clean. Qualify's
// BindsModule arm folds the still-empty-attribute reference into an OpLiteral
// at the caller, and pkg/value/expr.go's References() does not descend into
// OpLiteral.Literal.Expr — so projectRefs, canonicaliseRefs and the whole
// per-reference walk in bind.go never saw it, and a consuming attribute like
// vpc_id ended up unknown with an empty-attribute expression that stays
// deferred forever. This is the module-output sibling to
// TestAWholeResourceReferenceAsAModuleInputDoesNotEscapeAsAnEmptyAttribute
// above, and to internal/compiler's own
// TestNoEmptyAttributeReferenceEscapesStageSix (bind_test.go), which is
// root-only and module-free and so never exercised this path.
func TestAWholeResourceReferenceAsAModuleOutputDoesNotEscapeAsAnEmptyAttribute(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"modules/net-maker/module.yml": `
resources:
  net:
    type: fake.vpc
outputs:
  whole:
    value: ${net}
`,
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./modules/net-maker
resources:
  m:
    type: module.net_maker
  sub:
    type: fake.subnet
    vpc_id: ${m.whole}
    cidr: 10.0.0.0/24
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a whole-resource reference published as a module output must be refused before " +
			"it can reach a consumer with an empty attribute")
	}
	if !strings.Contains(rendered(ds), "module output") {
		t.Errorf("the diagnostic must say why:\n%s", rendered(ds))
	}
}

// TestAWholeResourceReferenceAsAModuleOutputIsRefusedInsideAFunctionCall is
// the nested case C1 also names: ${lower(m.whole)} at the caller cannot save
// a bare ${net} published as the module's own output — the reference must be
// refused where the module PUBLISHES it, not left to whatever expression the
// caller happens to wrap it in.
func TestAWholeResourceReferenceAsAModuleOutputIsRefusedInsideAFunctionCall(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"modules/net-maker/module.yml": `
resources:
  net:
    type: fake.vpc
outputs:
  whole:
    value: ${lower(net)}
`,
		"infra.yml": `
project: demo
environments: {dev: {}}
modules:
  - ./modules/net-maker
resources:
  m:
    type: module.net_maker
  sub:
    type: fake.subnet
    vpc_id: ${m.whole}
    cidr: 10.0.0.0/24
`,
	})

	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if !ds.HasErrors() {
		t.Fatal("a whole-resource reference nested inside a function call in a module output must " +
			"still be refused")
	}
	if !strings.Contains(rendered(ds), "module output") {
		t.Errorf("the diagnostic must say why:\n%s", rendered(ds))
	}
}
