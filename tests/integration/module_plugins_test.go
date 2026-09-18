package integration

import (
	"strings"
	"testing"
)

const moduleOnlyProject = `
project: myapp
resources:
  primary:
    type: module.db
    cidr: 10.0.0.0/16
`

const dbModuleOnly = `
inputs:
  cidr:
    type: string
resources:
  net:
    type: fake.network
    cidr: ${var.cidr}
`

// TestAModuleOnlyProjectSaysThePluginNeverLoaded.
//
// Which plugins to load comes from the resource types the ROOT declares, and
// `module.<name>` is not one of them — correctly, since a module call is not a
// provider resource. Module files are not read until stage 5, by which time the
// registry is built, so a provider used only inside a module is never found.
//
// What made it bad was the diagnostic rather than the gap. It named a type
// inside the module and offered "Known types:" followed by NOTHING, because
// nothing had loaded — advice to fix a spelling that was never wrong, and the
// real fact left unsaid. This asserts the message names the actual cause and
// the actual fix.
func TestAModuleOnlyProjectSaysThePluginNeverLoaded(t *testing.T) {
	dir := project(t, moduleOnlyProject)
	writeIn(t, dir, "modules/db/module.yml", dbModuleOnly)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatalf("a project with no plugin available produced a plan\n%s", r.combined())
	}
	out := r.combined()
	for _, want := range []string{
		"never loaded",
		"declared at the root",
		"providers:",
		"- plugin: fake",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic never mentions %q:\n%s", want, out)
		}
	}
	// The old message's whole problem, asserted directly: an empty list of types
	// presented as the answer.
	if strings.Contains(out, "Known types:") {
		t.Errorf("a list of known types was offered when no plugin had loaded:\n%s", out)
	}
}

// TestAProvidersBlockIsEnoughForAModuleOnlyProject — the documented fix has to
// work, or the diagnostic is advice that fails.
func TestAProvidersBlockIsEnoughForAModuleOnlyProject(t *testing.T) {
	dir := project(t, `
project: myapp
providers:
  - plugin: fake
resources:
  primary:
    type: module.db
    cidr: 10.0.0.0/16
`)
	writeIn(t, dir, "modules/db/module.yml", dbModuleOnly)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("the fix the diagnostic suggests did not work: %d\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "module.primary.net") {
		t.Errorf("the module's resource is not in the plan:\n%s", r.combined())
	}
}

// TestAnUnknownTypeFromALoadedPluginStillListsTypes. The two cases must not
// collapse into one message: a typo in a type a loaded plugin does not offer is
// exactly what the list of types answers, and narrowing the other branch must
// not take it away.
func TestAnUnknownTypeFromALoadedPluginStillListsTypes(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  oops:
    type: fake.netwrok
    cidr: 10.1.0.0/16
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatalf("a misspelled type produced a plan\n%s", r.combined())
	}
	out := r.combined()
	if !strings.Contains(out, "Known types:") || !strings.Contains(out, "fake.network") {
		t.Errorf("a typo was not answered with the types that exist:\n%s", out)
	}
	if strings.Contains(out, "never loaded") {
		t.Errorf("a loaded plugin was reported as never loaded:\n%s", out)
	}
	// Advice that cannot work: explaining the misspelling fails the same way.
	if strings.Contains(out, "explain fake.netwrok") {
		t.Errorf("the action suggests explaining the misspelled type:\n%s", out)
	}
}
