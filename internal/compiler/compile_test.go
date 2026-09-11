package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/value"
)

func loadFiles(t *testing.T, body string) []config.File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files
}

// loadFilesWithVariablesYml is loadFiles plus a variables.yml, for the tests
// that exercise Options.FileVars against config.ProjectDecl.VariableValues —
// the one rung of the precedence chain the two share.
func loadFilesWithVariablesYml(t *testing.T, infraBody, variablesBody string) []config.File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(infraBody), 0o644); err != nil {
		t.Fatalf("write infra.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "variables.yml"), []byte(variablesBody), 0o644); err != nil {
		t.Fatalf("write variables.yml: %v", err)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files
}

func TestCompileRunsTheFullPipelineOnAValidProject(t *testing.T) {
	files := loadFiles(t, `
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
	resolved, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if resolved.Project != "myapp" || resolved.Environment != "dev" {
		t.Errorf("Project/Environment = %q/%q, want myapp/dev", resolved.Project, resolved.Environment)
	}

	db, ok := resolved.Get(address.Address{Name: "database"})
	if !ok {
		t.Fatal("database resource missing from the resolved config")
	}
	size, ok := db.Attrs["size"]
	if !ok || size.Source != value.SourceDefault {
		t.Fatalf("stage 7's default for size was not filled in: %+v", size)
	}
	if n, _ := size.AsInt(); n != 10 {
		t.Errorf("size = %d, want the dev default of 10", n)
	}
}

func TestCompileStopsAfterDecodeErrors(t *testing.T) {
	// A duplicate definition leaves the decoder unable to build a complete
	// config. If later stages ran anyway, this lone database — which has no
	// network anywhere in the file — would also trip stage 8's missing-
	// requirement check, burying the real problem: the duplicate itself.
	files := loadFiles(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
  database:
    type: test.database
    engine: postgres
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a duplicate resource definition must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "defined more than once") {
		t.Errorf("expected the duplicate-definition diagnostic:\n%s", out.String())
	}
	if strings.Contains(out.String(), "network") {
		t.Errorf("stage 8 must not run against a config the decoder could not build; got noise:\n%s", out.String())
	}
}

func TestCompileStopsAfterSchemaErrorsBeforeGraphValidation(t *testing.T) {
	// bogus's unresolved type keeps stage 7 from finishing cleanly. guarded's
	// contradictory lifecycle would be caught by stage 8 — if stage 8 ran. It
	// must not: a resource whose type never resolved is exactly the config
	// later stages cannot build on.
	files := loadFiles(t, `
project: myapp
resources:
  bogus:
    type: not.a.real.type
  guarded:
    type: test.network
    cidr: 10.0.0.0/16
    lifecycle:
      prevent_destroy: true
      retain: true
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("an unregistered type must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "unknown resource type") {
		t.Errorf("expected stage 7's unknown-type diagnostic:\n%s", out.String())
	}
	if strings.Contains(out.String(), "prevent_destroy") {
		t.Errorf("stage 8 must not run once stage 7 has errors; got its lifecycle diagnostic anyway:\n%s", out.String())
	}
}

func TestCompileAccumulatesDiagnosticsWithinAStage(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  a:
    type: nope.one
  b:
    type: nope.two
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — one bad type must not mask the other", len(ds))
	}
}

func TestCompileAlwaysDefinesEnvironmentRegionAndAccount(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${environment}/${region}/${account}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{
		Environment: "production",
		Region:      "us-east-1",
		Account:     "123456789012",
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got, _ := cfg.Resources["network"].Attrs["cidr"].AsString()
	if got != "production/us-east-1/123456789012" {
		t.Errorf("cidr = %q, want %q — configuration must be able to name its own environment, region and account", got, "production/us-east-1/123456789012")
	}
}

func TestCompileLeavesRegionUndefinedWhenNoneWasSupplied(t *testing.T) {
	// The other direction of the predicate. Injecting an empty string instead
	// would interpolate silently into a resource name; an undefined variable
	// is a diagnostic the user can act on.
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${region}
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("${region} with no region supplied must be reported as undefined, not resolved to an empty string")
	}
}

// TestCompileLeavesAccountUndefinedWhenNoneWasSupplied is Region's twin.
// seedProcessVariables guards `account` with the identical `if opts.Account
// != ""` shape it guards `region` with, and nothing else in the suite
// exercises that specific guard — Region's own test does not, by
// construction, say anything about Account.
func TestCompileLeavesAccountUndefinedWhenNoneWasSupplied(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${account}
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("${account} with no account supplied must be reported as undefined, not resolved to an empty string")
	}
}

// TestCompileWillNotLetAVarFlagRedefineTheEnvironment used to assert that
// `--var environment=staging` was silently discarded and "production" won
// with NO diagnostic. That was M4 final review's MAJOR 3: the flag vanished
// with no error, and rendered as `[environment, from --var]` — read by a
// user as confirmation the flag WAS honoured. seedProcessVariables staying
// authoritative was and remains correct; silence about a discarded flag was
// not. Updated 2026-09-11 to assert the refusal instead of the silence.
func TestCompileWillNotLetAVarFlagRedefineTheEnvironment(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${environment}
`)
	_, ds := Compile(files, testRegistry(t), Options{
		Environment: "production",
		Vars:        map[string]string{"environment": "staging"},
	})
	if !ds.HasErrors() {
		t.Fatal("--var environment=staging must be refused, not silently discarded — " +
			"the environment argument decides which state file is written, and a flag " +
			"that cannot change that must say so rather than vanish")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "environment") {
		t.Errorf("the diagnostic must name \"environment\" as the refused variable:\n%s", sb.String())
	}
}

// TestCompileAllowsEnvironmentDeclaredAsAVariable is a regression test.
// "environment" is unconditionally seeded by seedProcessVariables, which
// runs strictly AFTER variables.Resolve returns — so before this fix,
// declaring "environment" under `variables:` made Resolve's own unset check
// see nothing had supplied it yet and report `variable "environment" is not
// set`, even though it always would, three lines later. A project has no
// reason to know that its own environment/region/account is process-
// reserved rather than an ordinary identifier it may want typed.
func TestCompileAllowsEnvironmentDeclaredAsAVariable(t *testing.T) {
	files := loadFiles(t, `
project: myapp
variables:
  environment:
    type: string
resources:
  network:
    type: test.network
    cidr: ${environment}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "production"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got, _ := cfg.Resources["network"].Attrs["cidr"].AsString(); got != "production" {
		t.Errorf("cidr = %q, want production — the process-supplied environment must still win even when the project also declares \"environment\" as its own variable", got)
	}
}

// TestSeededEnvironmentDoesNotCreditDashDashVar reproduces M4 final review's
// MAJOR 2: seedProcessVariables stamped ScopeCLIOverride but never
// SuppliedBy, so value.ScopeLabel (and, before this fix wave, the bare
// Scope.String() the renderer used directly) fell back to the scope's
// generic label — which is "--var" — and a BARE `infra plan dev`, with no
// flags whatsoever, rendered `cidr: "dev" [environment, from --var]`.
// seedProcessVariables' own doc comment argues at length that a --var cannot
// set "environment"; the plan asserted the opposite of the code's own
// contract.
func TestSeededEnvironmentDoesNotCreditDashDashVar(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: ${environment}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got := cfg.Resources["network"].Attrs["cidr"]
	if got.SuppliedBy == "--var" {
		t.Error("a bare `infra plan dev` with no --var must not credit --var for the seeded environment")
	}
	if annotated := value.Annotate(got, value.FormatOptions{Unknown: "(unknown)", QuoteStrings: true}); strings.Contains(annotated, "from --var") {
		t.Errorf("the rendered annotation must not name --var when none was passed: %s", annotated)
	}
}

func TestCompileResolvesVariablesThroughTheEnvironmentChain(t *testing.T) {
	files := loadFiles(t, `
project: myapp
variables:
  replicas:
    type: integer
    default: 50
    min: 1
    max: 100
environments:
  base:
    replicas: 30
  production:
    extends: base
    replicas: 20
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
    size: ${replicas}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "production"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	size := cfg.Resources["database"].Attrs["size"]
	if n, _ := size.AsInt(); n != 20 {
		t.Errorf("size = %d, want 20 — production's own value beats the one it inherits and the declared default", n)
	}
	if size.Scope != value.ScopeEnvironmentVar {
		t.Errorf("Scope = %v, want ScopeEnvironmentVar", size.Scope)
	}
}

func TestCompileReportsAnUnknownEnvironment(t *testing.T) {
	files := loadFiles(t, `
project: myapp
environments:
  production: {}
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "prod"})
	if !ds.HasErrors() {
		t.Fatal("`infra plan prod` against a project declaring only `production` must be reported by stage 3")
	}
}

func TestCompileStopsAfterEnvironmentErrors(t *testing.T) {
	// A failed chain does NOT by itself make stage 4 report every
	// environment-scoped variable as unset: a Chain that failed to resolve
	// carries Selected: false, and stage 4 already treats an unselected
	// chain as "nothing to check" (see Compile's doc comment) — `domain`
	// here resolves to an unknown, not an error. This fixture pins that
	// specifically: it has one declared variable and no default, the exact
	// shape that WOULD add a second diagnostic if stage 4's unset check
	// misfired here.
	files := loadFiles(t, `
project: myapp
variables:
  domain:
    type: string
environments:
  a:
    extends: b
  b:
    extends: a
resources:
  network:
    type: test.network
    cidr: ${domain}
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "a"})
	if len(ds) != 1 {
		t.Fatalf("want exactly the cycle diagnostic, got %d:\n%+v", len(ds), ds)
	}
}

// TestCompileStopsAfterEnvironmentErrorsSuppressesChainIndependentVariableErrors
// is what the halt after stage 3 actually guards, proven by a fixture the
// sibling test above cannot exercise. A malformed variable DECLARATION
// (variables.Schemas validating `default: not-a-number` against `type:
// integer`) is chain-independent — Schemas runs before the chain is ever
// consulted — so it fires whether or not the chain resolves. Without the
// halt, this fixture's cycle diagnostic and the bad-default diagnostic both
// reach the caller; with it, only the cycle does, because a report about a
// declaration failure "while resolving environment a" is judged less useful
// than letting the user fix the chain first and re-run (see Compile's doc
// comment on the tension with §7.4).
func TestCompileStopsAfterEnvironmentErrorsSuppressesChainIndependentVariableErrors(t *testing.T) {
	files := loadFiles(t, `
project: myapp
variables:
  port:
    type: integer
    default: not-a-number
environments:
  a:
    extends: b
  b:
    extends: a
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "a"})
	if len(ds) != 1 {
		t.Fatalf("want exactly the cycle diagnostic — the halt after stage 3 exists to suppress stage 4's chain-independent declaration errors until the chain itself is fixed, got %d:\n%+v", len(ds), ds)
	}
}

// TestCompileStopsAfterVariableErrors is the stage-4-only sibling of
// TestCompileStopsAfterEnvironmentErrors. That test's cycle makes
// environments.Resolve fail, which returns a Chain with Selected false —
// stage 4 already treats an unselected chain as "nothing to check" (an
// unset variable becomes an unknown, not an error), so it produces no
// diagnostics of its own either way and does not by itself prove the halt
// after stage 4 does anything. This fixture resolves its environment
// chain cleanly and fails only because a declared variable has no default
// and nothing sets it: without the halt, stage 6 additionally reports
// `undefined variable "domain"` at ${domain}'s use site — the same
// problem told twice, exactly what the halt exists to prevent.
func TestCompileStopsAfterVariableErrors(t *testing.T) {
	files := loadFiles(t, `
project: myapp
variables:
  domain:
    type: string
resources:
  network:
    type: test.network
    cidr: ${domain}
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if len(ds) != 1 {
		t.Fatalf("want exactly the unset-variable diagnostic, got %d:\n%+v", len(ds), ds)
	}
}

// TestCompileFileVarWinsOverVariablesYml exercises fileVars' merge order.
// internal/cli does not populate Options.FileVars yet — --var-file still
// errors upstream in root.go, so no end-to-end path reaches this today — but
// fileVars is reachable directly through Compile, and its merge order (the
// file named on the command line beats variables.yml) is exactly the
// distinction Options.FileVars and ProjectDecl.VariableValues exist to
// preserve as two separate fields rather than one.
func TestCompileFileVarWinsOverVariablesYml(t *testing.T) {
	files := loadFilesWithVariablesYml(t, `
project: myapp
variables:
  name:
    type: string
    default: from-schema-default
resources:
  network:
    type: test.network
    cidr: ${name}
`, "name: from-variables-yml\n")

	cfg, ds := Compile(files, testRegistry(t), Options{
		Environment: "dev",
		FileVars:    map[string]value.Value{"name": value.String("from-var-file", value.SourceVariable)},
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got, _ := cfg.Resources["network"].Attrs["cidr"].AsString(); got != "from-var-file" {
		t.Errorf("cidr = %q, want from-var-file — --var-file is more specific than variables.yml and must win", got)
	}
}

// TestCompileVariablesYmlWinsWhenNoFileVarIsSupplied is the other direction:
// absent Options.FileVars, variables.yml still beats the declared default.
func TestCompileVariablesYmlWinsWhenNoFileVarIsSupplied(t *testing.T) {
	files := loadFilesWithVariablesYml(t, `
project: myapp
variables:
  name:
    type: string
    default: from-schema-default
resources:
  network:
    type: test.network
    cidr: ${name}
`, "name: from-variables-yml\n")

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got, _ := cfg.Resources["network"].Attrs["cidr"].AsString(); got != "from-variables-yml" {
		t.Errorf("cidr = %q, want from-variables-yml", got)
	}
}

func TestCompileReportsStage8Diagnostics(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in the project must be caught before any provider call")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "network") {
		t.Errorf("expected stage 8's missing-requirement diagnostic:\n%s", out.String())
	}
}
