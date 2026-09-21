package providers

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/registry"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
	testprovider "github.com/infrena/infrena/providers/test"
)

// Prepare is stage 4.5: resolve the declarations, then construct each instance from
// what was resolved.

func pluginRegistry(t *testing.T, dir string) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.RegisterPlugin(testprovider.NewPlugin(dir)); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	return reg
}

func rendered(t *testing.T, body string, reg *registry.Registry, scope variables.Scope) (Table, string) {
	t.Helper()
	table, ds := Prepare(context.Background(), project(t, body), scope, reg)
	var sb strings.Builder
	ds.Render(&sb)
	return table, sb.String()
}

// The factory split's payoff in one assertion: `cloud: ${var.account_file}` chooses
// the file the provider opens.
//
// Asserted by reading the constructed provider's path, because that is the only thing
// that distinguishes "resolved" from "resolved and then ignored".
func TestPrepareGivesThePluginTheRESOLVEDConfiguration(t *testing.T) {
	dir := t.TempDir()
	reg := pluginRegistry(t, dir)
	_, out := rendered(t, `
project: p
providers:
  - plugin: fake
    cloud: "${var.account_file}"
`, reg, scopeWith(map[string]string{"account_file": "prod-cloud.json"}))
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}

	p, ok := reg.ProviderFor("fake.network", "fake")
	if !ok {
		t.Fatal("no instance was constructed")
	}
	got := p.(*testprovider.Provider).CloudPath()
	if want := filepath.Join(dir, "prod-cloud.json"); got != want {
		t.Errorf("the provider opens %q, want %q — the resolved value reached nothing", got, want)
	}
}

// The plugin says what is wrong; Prepare says where. Neither alone is enough to fix
// it.
func TestAPluginsRefusalIsReportedAgainstTheDeclarationThatCausedIt(t *testing.T) {
	dir := t.TempDir()
	_, out := rendered(t, `
project: p
providers:
  - plugin: fake
    name: acct2
    clowd: other.json
`, pluginRegistry(t, dir), variables.Scope{})
	if out == "" {
		t.Fatal("a plugin refusing its configuration must be reported")
	}
	for _, want := range []string{"acct2", "clowd", "infrena.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// `plugin: awz` is a typo with no instance behind it, and the alternative to reporting
// it here is a mid-apply failure about a type.
func TestAnUndeclaredPluginIsReported(t *testing.T) {
	_, out := rendered(t, `
project: p
providers:
  - plugin: awz
`, pluginRegistry(t, t.TempDir()), variables.Scope{})
	if out == "" {
		t.Fatal("a `plugin:` naming nothing registered must be reported")
	}
	if !strings.Contains(out, "awz") || !strings.Contains(out, "fake") {
		t.Errorf("the diagnostic needs the name written and the names available:\n%s", out)
	}
}

// Every project written before `providers:` existed must keep working.
func TestAProjectWithNoProvidersBlockGetsOneImplicitInstance(t *testing.T) {
	reg := pluginRegistry(t, t.TempDir())
	table, out := rendered(t, "project: p\n", reg, variables.Scope{})
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if got := table.DefaultName(); got != "fake" {
		t.Errorf("DefaultName = %q, want the sole plugin's name", got)
	}
	// Constructed, not merely named: a table entry nothing built dispatches nowhere.
	if _, ok := reg.ProviderFor("fake.network", "fake"); !ok {
		t.Error("the implicit instance was named but never constructed")
	}
}

// A caller that handed over its own provider object — every test that builds a stub —
// keeps it. Rebuilding from configuration would throw that object away and replace it
// with a different one, which would make a stub silently inert.
func TestAnAlreadyRegisteredInstanceIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	reg := registry.New()
	mine := testprovider.New(filepath.Join(dir, "mine.json"))
	if err := reg.Register("fake", mine); err != nil {
		t.Fatalf("Register: %v", err)
	}

	table, out := rendered(t, "project: p\n", reg, variables.Scope{})
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if got := table.DefaultName(); got != "fake" {
		t.Errorf("DefaultName = %q, want the sole registered instance", got)
	}
	got, _ := reg.ProviderFor("fake.network", "fake")
	if got != mine {
		t.Error("the caller's own provider object was replaced")
	}
}

// permissivePlugin accepts any configuration at all, which is what makes it the right
// instrument here: with it, the only thing standing between an unresolved value and a
// constructed provider is Prepare's own halt.
//
// A plugin that validates its configuration — the fake provider does — hides that halt
// by refusing the bad value itself, and a test written against it passes with the halt
// deleted.
type permissivePlugin struct{ built int }

func (pl *permissivePlugin) Name() string { return "fake" }
func (pl *permissivePlugin) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:         "fake.network",
		Attributes:   map[string]schema.Attribute{"cidr": {Kind: value.KindString, Required: true}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}}
}

func (pl *permissivePlugin) New(provider.Config) (provider.Provider, error) {
	pl.built++
	return testprovider.New("/dev/null"), nil
}

// The halt matters more than the diagnostic: an instance built from a value that did
// not resolve is one a resource is dispatched to, and a resource created in a place
// nobody named is not a problem anyone gets to read about. Fail-closed is the engine's
// guarantee here, not each plugin's — a plugin that reads a missing value as "use the
// default" must not be able to turn a broken interpolation into a silently different
// account.
func TestAnUnresolvableInstanceConfigurationConstructsNothing(t *testing.T) {
	reg := registry.New()
	pl := &permissivePlugin{}
	if err := reg.RegisterPlugin(pl); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	_, out := rendered(t, `
project: p
providers:
  - plugin: fake
    cloud: "${var.nosuchvariable}"
`, reg, variables.Scope{})
	if out == "" {
		t.Fatal("an unresolvable instance configuration must be reported")
	}
	if pl.built != 0 {
		t.Error("the plugin was asked to build an instance from configuration that did not " +
			"resolve; nothing but the plugin's own goodwill then decides where resources land")
	}
	if _, ok := reg.ProviderFor("fake.network", "fake"); ok {
		t.Error("an instance was constructed from configuration that did not resolve")
	}
}

// A declared instance must never be dropped in silence.
//
// Keeping an instance a caller already supplied is deliberate — see
// TestAnAlreadyRegisteredInstanceIsLeftAlone — but it is only safe while the
// existing instance stands in for the same thing. An entry carrying
// configuration is not that: its `profile:`, `cloud:` or `assume_role_arn` is
// what decides which account the run touches, so keeping some other instance
// under that name quietly points every provider call at the wrong account, and
// a plan then reports every live resource as deleted.
func TestRegisterRefusesToDiscardADeclaredInstancesConfiguration(t *testing.T) {
	dir := t.TempDir()
	reg := registry.New()
	if err := reg.Register("fake", testprovider.New(filepath.Join(dir, "mine.json"))); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ds := Register(Table{"fake": Instance{
		Name:   "fake",
		Plugin: "fake",
		Config: map[string]value.Value{"cloud": value.String("other.json", value.SourceExplicit)},
	}}, reg)

	if !ds.HasErrors() {
		t.Fatal("a declared instance's configuration was discarded without a word")
	}
	var buf bytes.Buffer
	ds.Render(&buf)
	if !strings.Contains(buf.String(), "fake") {
		t.Errorf("the diagnostic does not name the instance: %s", buf.String())
	}
}
