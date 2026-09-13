package providers

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/registry"
	"github.com/infrata/infrata/internal/variables"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
	testprovider "github.com/infrata/infrata/providers/test"
)

// Prepare is stage 4.5: resolve the declarations, then CONSTRUCT each instance from
// what was resolved (PLAN.md §12.1).

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
	table, ds := Prepare(decls(t, body), scope, reg)
	var sb strings.Builder
	ds.Render(&sb)
	return table, sb.String()
}

// TestPrepareGivesThePluginTheRESOLVEDConfiguration is the factory split's payoff in
// one assertion: `cloud: ${account_file}` chooses the file the provider opens.
//
// Asserted by reading the constructed provider's path, because that is the only thing
// that distinguishes "resolved" from "resolved and then ignored" — which is exactly
// what the build before this did.
func TestPrepareGivesThePluginTheRESOLVEDConfiguration(t *testing.T) {
	dir := t.TempDir()
	reg := pluginRegistry(t, dir)
	_, out := rendered(t, `
project: p
providers:
  - plugin: test
    cloud: "${account_file}"
`, reg, scopeWith(map[string]string{"account_file": "prod-cloud.json"}))
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}

	p, ok := reg.ProviderFor("test.network", "test")
	if !ok {
		t.Fatal("no instance was constructed")
	}
	got := p.(*testprovider.Provider).CloudPath()
	if want := filepath.Join(dir, "prod-cloud.json"); got != want {
		t.Errorf("the provider opens %q, want %q — the resolved value reached nothing", got, want)
	}
}

// TestAPluginsRefusalIsReportedAgainstTheDeclarationThatCausedIt. The plugin says what
// is wrong; Prepare says where. Neither alone is enough to fix it.
func TestAPluginsRefusalIsReportedAgainstTheDeclarationThatCausedIt(t *testing.T) {
	dir := t.TempDir()
	_, out := rendered(t, `
project: p
providers:
  - plugin: test
    name: acct2
    clowd: other.json
`, pluginRegistry(t, dir), variables.Scope{})
	if out == "" {
		t.Fatal("a plugin refusing its configuration must be reported")
	}
	for _, want := range []string{"acct2", "clowd", "infra.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// TestAnUndeclaredPluginIsReported. `plugin: awz` is a typo with no instance behind it,
// and the alternative to reporting it here is a mid-apply failure about a type.
func TestAnUndeclaredPluginIsReported(t *testing.T) {
	_, out := rendered(t, `
project: p
providers:
  - plugin: awz
`, pluginRegistry(t, t.TempDir()), variables.Scope{})
	if out == "" {
		t.Fatal("a `plugin:` naming nothing registered must be reported")
	}
	if !strings.Contains(out, "awz") || !strings.Contains(out, "test") {
		t.Errorf("the diagnostic needs the name written and the names available:\n%s", out)
	}
}

// TestAProjectWithNoProvidersBlockGetsOneImplicitInstance — every project written
// before §12.1, which must all keep working.
func TestAProjectWithNoProvidersBlockGetsOneImplicitInstance(t *testing.T) {
	reg := pluginRegistry(t, t.TempDir())
	table, out := rendered(t, "project: p\n", reg, variables.Scope{})
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if got := table.DefaultName(); got != "test" {
		t.Errorf("DefaultName = %q, want the sole plugin's name", got)
	}
	// Constructed, not merely named: a table entry nothing built dispatches nowhere.
	if _, ok := reg.ProviderFor("test.network", "test"); !ok {
		t.Error("the implicit instance was named but never constructed")
	}
}

// TestAnAlreadyRegisteredInstanceIsLeftAlone.
//
// A caller that handed over its own provider object — every test that builds a stub —
// keeps it. Rebuilding from configuration would throw that object away and replace it
// with a different one, which would make a stub silently inert.
func TestAnAlreadyRegisteredInstanceIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	reg := registry.New()
	mine := testprovider.New(filepath.Join(dir, "mine.json"))
	if err := reg.Register("test", mine); err != nil {
		t.Fatalf("Register: %v", err)
	}

	table, out := rendered(t, "project: p\n", reg, variables.Scope{})
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if got := table.DefaultName(); got != "test" {
		t.Errorf("DefaultName = %q, want the sole registered instance", got)
	}
	got, _ := reg.ProviderFor("test.network", "test")
	if got != mine {
		t.Error("the caller's own provider object was replaced")
	}
}

// permissivePlugin accepts any configuration at all, which is what makes it the right
// instrument here: with it, the only thing standing between an unresolved value and a
// constructed provider is Prepare's own halt.
//
// A plugin that validates its configuration — the fake provider does — hides that halt
// by refusing the bad value itself. The first version of the test below used the fake
// provider and passed with the halt deleted, for exactly that reason.
type permissivePlugin struct{ built int }

func (pl *permissivePlugin) Name() string { return "test" }
func (pl *permissivePlugin) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:         "test.network",
		Attributes:   map[string]schema.Attribute{"cidr": {Kind: value.KindString, Required: true}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}}
}

func (pl *permissivePlugin) New(string, map[string]value.Value) (provider.Provider, error) {
	pl.built++
	return testprovider.New("/dev/null"), nil
}

// TestAnUnresolvableInstanceConfigurationConstructsNothing.
//
// The halt matters more than the diagnostic: an instance built from a value that did
// not resolve is one a resource is dispatched to, and a resource created in a place
// nobody named is not a problem anyone gets to read about. Fail-closed is the ENGINE's
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
  - plugin: test
    cloud: "${nosuchvariable}"
`, reg, variables.Scope{})
	if out == "" {
		t.Fatal("an unresolvable instance configuration must be reported")
	}
	if pl.built != 0 {
		t.Error("the plugin was asked to build an instance from configuration that did not " +
			"resolve; nothing but the plugin's own goodwill then decides where resources land")
	}
	if _, ok := reg.ProviderFor("test.network", "test"); ok {
		t.Error("an instance was constructed from configuration that did not resolve")
	}
}
