package compiler

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

// `project` as a fourth process variable (PLAN.md §6.3).
//
// A resource name or a tag almost always wants the project name, and threading
// it through as an ordinary variable makes every project declare the same line
// in its own variables.yml.

// TestProjectIsAProcessVariable — it resolves, alongside the other three, in the
// shape §6.3's own examples use.
func TestProjectIsAProcessVariable(t *testing.T) {
	files := loadFiles(t, `
project: MainApp
environments:
  dev: {}
resources:
  network:
    type: test.network
    cidr: ${project}-${environment}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}

	got := cfg.Resources["network"].Attrs["cidr"]
	if s, _ := got.AsString(); s != "MainApp-dev" {
		t.Errorf("cidr = %v, want %q", got, "MainApp-dev")
	}
	// No scope assertion here, deliberately. A CONCATENATION carries neither
	// operand's scope — it resolves to SourceComputed at ScopeUnset, and that is
	// pre-existing and true of `${environment}-x` just the same. The renderer
	// omits the annotation entirely rather than printing "from unset", so
	// nothing wrong reaches a user; it just means provenance cannot be asserted
	// through a concatenated value. TestProjectIsAuthoritative below asserts it
	// on a bare interpolation, which is where the rung is observable.
}

// TestProjectIsAuthoritative — the project name outranks anything in a file.
//
// The other three process variables are stamped ScopeCLIOverride precisely so
// nothing in a file can quietly disagree with the invocation. A `project` on a
// lower rung could be overridden by an environment, and a resource name would
// then claim one project while state recorded another.
func TestProjectIsAuthoritative(t *testing.T) {
	files := loadFiles(t, `
project: MainApp
environments:
  dev:
    project: NotThis
resources:
  network:
    type: test.network
    cidr: ${project}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	got := cfg.Resources["network"].Attrs["cidr"]
	if s, _ := got.AsString(); s != "MainApp" {
		t.Errorf("cidr = %v; an environment override beat the project name", got)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("${project} resolved at scope %v, want the invocation rung", got.Scope)
	}
}

// TestProjectIsStampedWithWhereItCameFrom. value.ScopeLabel falls back to
// Scope.String() — "--var" — when SuppliedBy is unset, so an unstamped project
// variable renders in the plan as though the user typed it on the command line.
// M4 shipped exactly that bug for the other three.
func TestProjectIsStampedWithWhereItCameFrom(t *testing.T) {
	files := loadFiles(t, `
project: MainApp
environments:
  dev: {}
resources:
  network:
    type: test.network
    cidr: ${project}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", rendered(ds))
	}
	label := value.ScopeLabel(cfg.Resources["network"].Attrs["cidr"])
	if label == "--var" || label == "" {
		t.Errorf("${project} renders as %q — it did not come from the command line and the plan "+
			"must not say it did", label)
	}
	if !strings.Contains(label, "project") {
		t.Errorf("${project} renders as %q, which does not say where the value came from", label)
	}
}

// TestProjectCrossesAModuleBoundary. A module sees its own inputs and the
// process variables and nothing else (PLAN.md §11.3).
//
// Asserted from INSIDE a module deliberately: internal/modules carries the list
// of process variables separately from where compiler seeds them, so seeding a
// fourth without adding it to the list leaves it working at the root and
// undefined one level down.
func TestProjectCrossesAModuleBoundary(t *testing.T) {
	files, dir := moduleFixture(t, map[string]string{
		"modules/namer/module.yml": `
resources:
  net:
    type: test.network
    cidr: ${project}-inner
`,
		"infra.yml": `
project: MainApp
environments:
  dev: {}
modules:
  - ./modules/namer
resources:
  stack:
    type: module.namer
`,
	})

	cfg, ds := Compile(files, testRegistry(t), Options{Environment: "dev", Dir: dir})
	if ds.HasErrors() {
		t.Fatalf("${project} is not visible inside a module:\n%s", rendered(ds))
	}
	inner, ok := cfg.Resources["module.stack.net"]
	if !ok {
		t.Fatalf("no module.stack.net: %v", sortedTargetsOf(cfg))
	}
	if s, _ := inner.Attrs["cidr"].AsString(); s != "MainApp-inner" {
		t.Errorf("inner cidr = %v, want the project name carried across the boundary", inner.Attrs["cidr"])
	}
}

// TestAProjectOverrideOnTheCommandLineIsRefused. `project` joins the reserved
// names: the project name is declared in configuration and recorded in state,
// so a --var that changed it would make a resource claim one project while its
// state recorded another.
//
// Refused rather than silently overwritten, which is the bug M4 fixed for
// `environment`: Resolve applied the entry like any other variable and
// seedProcessVariables overwrote it moments later with no diagnostic at all.
func TestAProjectOverrideOnTheCommandLineIsRefused(t *testing.T) {
	files := loadFiles(t, `
project: MainApp
environments:
  dev: {}
resources:
  network:
    type: test.network
    cidr: ${project}
`)
	cfg, ds := Compile(files, testRegistry(t), Options{
		Environment: "dev",
		Vars:        map[string]string{"project": "SomethingElse"},
	})
	if !ds.HasErrors() {
		t.Fatal("--var project=... must be refused, not silently ignored")
	}
	out := rendered(ds)
	if !strings.Contains(out, "project") {
		t.Errorf("the diagnostic does not name the variable:\n%s", out)
	}
	// And the value must not have been taken. Asserting only that a diagnostic
	// appeared would pass against an implementation that reported the problem
	// and used the value anyway.
	if r, ok := cfg.Resources["network"]; ok {
		if s, _ := r.Attrs["cidr"].AsString(); s == "SomethingElse" {
			t.Error("the refused --var was applied anyway")
		}
	}
}
