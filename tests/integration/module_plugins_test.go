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

// TestAModuleOnlyProjectLoadsItsPlugins.
//
// Which plugins to load is derived from the resource types configuration
// declares, and `module.<name>` is not one of them — a module call is not a
// provider resource. Module files are not read until stage 5, by which time the
// registry was already built, so a provider used ONLY inside a module was never
// loaded and the plan failed naming a type inside the module. The module got
// blamed for the plugin's absence.
//
// Stage 5.5 loads what expansion revealed. This project declares no
// `providers:` block on purpose: needing one was the workaround, and a block a
// reader gains nothing from is exactly what makes `providers:` optional in the
// first place.
func TestAModuleOnlyProjectLoadsItsPlugins(t *testing.T) {
	dir := project(t, moduleOnlyProject)
	writeIn(t, dir, "modules/db/module.yml", dbModuleOnly)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("a project whose resources are all inside modules did not plan: %d\n%s",
			r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "module.primary.net") {
		t.Errorf("the module's resource is not in the plan:\n%s", r.combined())
	}
}

// TestAPluginThatIsGenuinelyMissingStillSaysSo, and says it about the plugin
// rather than about the type.
//
// Stage 5.5 loading late must not turn a real absence into a worse message: the
// diagnostic still has to name the plugin, report that nothing loaded, and not
// offer a list of known types that cannot contain the answer.
func TestAPluginThatIsGenuinelyMissingStillSaysSo(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  primary:
    type: module.db
    cidr: 10.0.0.0/16
`)
	writeIn(t, dir, "modules/db/module.yml", `
inputs:
  cidr:
    type: string
resources:
  net:
    type: nosuchplugin.network
    cidr: ${var.cidr}
`)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatalf("a type from a plugin nobody has produced a plan\n%s", r.combined())
	}
	out := r.combined()
	if !strings.Contains(out, "nosuchplugin") {
		t.Errorf("the diagnostic does not name the plugin:\n%s", out)
	}
}

// TestAProvidersBlockIsEnoughForAModuleOnlyProject. No longer the required
// workaround, and it must still work: naming a plugin explicitly is the older
// and more explicit way to say the same thing, and plenty of projects have the
// block anyway for a region or an account.
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
