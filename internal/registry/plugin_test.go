package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// The factory split (PLAN.md §12.1): a plugin's SCHEMAS register without any
// configuration; the provider OBJECT is built later, from configuration the compiler
// could only resolve once it had those schemas.

// stubPlugin records what it was constructed with, which is the only thing these
// tests are actually about.
type stubPlugin struct {
	name string
	defs []*schema.ResourceDefinition
	err  error

	gotInstance string
	gotConfig   map[string]value.Value
	calls       int
}

func (p *stubPlugin) Name() string                              { return p.name }
func (p *stubPlugin) Definitions() []*schema.ResourceDefinition { return p.defs }
func (p *stubPlugin) New(instance string, config map[string]value.Value) (provider.Provider, error) {
	p.calls++
	p.gotInstance = instance
	p.gotConfig = config
	if p.err != nil {
		return nil, p.err
	}
	return stubProvider{name: p.name, defs: p.defs}, nil
}

func plugin() *stubPlugin {
	return &stubPlugin{name: "test", defs: []*schema.ResourceDefinition{def("test.network")}}
}

// TestAPluginsSchemasAreAvailableBeforeAnyInstanceExists is the whole point. If this
// needed an instance, the cycle §12.1 describes would be unbroken: no compile without
// schemas, no schemas without configuration, no configuration without a compile.
func TestAPluginsSchemasAreAvailableBeforeAnyInstanceExists(t *testing.T) {
	r := New()
	if err := r.RegisterPlugin(plugin()); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if _, ok := r.Definition("test.network"); !ok {
		t.Error("the schema is not available from a registry holding only the plugin")
	}
	// And the other half: nothing can be dispatched to yet. A registry that
	// answered here would be one that invented a provider from no configuration.
	if _, ok := r.ProviderFor("test.network", "test"); ok {
		t.Error("a provider object exists before any instance was configured")
	}
	if names := r.InstanceNames(); len(names) != 0 {
		t.Errorf("InstanceNames = %v, want none", names)
	}
}

// TestRegisterInstancePassesResolvedConfigurationToThePlugin — the value the split
// was built to deliver. Before it, resolved configuration existed and reached nothing.
func TestRegisterInstancePassesResolvedConfigurationToThePlugin(t *testing.T) {
	r := New()
	p := plugin()
	if err := r.RegisterPlugin(p); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	cfg := map[string]value.Value{"cloud": value.String("/tmp/one.json", value.SourceVariable)}
	if err := r.RegisterInstance("acct2", "test", cfg); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}

	if p.gotInstance != "acct2" {
		t.Errorf("the plugin was told instance %q, want acct2 — a plugin whose configuration "+
			"is optional cannot otherwise keep two instances apart", p.gotInstance)
	}
	if got, _ := p.gotConfig["cloud"].AsString(); got != "/tmp/one.json" {
		t.Errorf("the plugin received cloud = %v, want the resolved value", p.gotConfig["cloud"])
	}
	if _, ok := r.ProviderFor("test.network", "acct2"); !ok {
		t.Error("the constructed instance does not serve the plugin's types")
	}
}

// TestTwoInstancesOfOnePluginAreConstructedSeparately. Each gets its own New call
// with its own configuration, which is what makes them two accounts rather than two
// names for one.
func TestTwoInstancesOfOnePluginAreConstructedSeparately(t *testing.T) {
	r := New()
	p := plugin()
	if err := r.RegisterPlugin(p); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	for _, name := range []string{"main", "acct2"} {
		if err := r.RegisterInstance(name, "test", nil); err != nil {
			t.Fatalf("RegisterInstance %s: %v", name, err)
		}
	}
	if p.calls != 2 {
		t.Errorf("the plugin was constructed %d times, want one per instance", p.calls)
	}
	if got := r.InstanceNames(); strings.Join(got, ",") != "acct2,main" {
		t.Errorf("InstanceNames = %v", got)
	}
}

// TestAPluginErrorIsReturnedNotSwallowed. A plugin refusing its configuration — an
// unreadable path, a missing credential, a key it does not accept — is a problem the
// user can fix, and the only way they hear about it is this return.
func TestAPluginErrorIsReturnedNotSwallowed(t *testing.T) {
	r := New()
	p := plugin()
	p.err = errors.New("unknown configuration \"clowd\"")
	if err := r.RegisterPlugin(p); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	err := r.RegisterInstance("main", "test", nil)
	if err == nil {
		t.Fatal("a plugin that refused its configuration registered anyway")
	}
	if !strings.Contains(err.Error(), "clowd") {
		t.Errorf("the plugin's own message was replaced: %v", err)
	}
	// And nothing was registered, so a caller that ignored the error does not get a
	// half-built instance that looks fine.
	if r.HasInstance("main") {
		t.Error("the instance was registered despite the plugin's refusal")
	}
}

// TestAConstructedProviderRegistersItsPluginByNameOnly.
//
// Registering an already-built provider is how every test that needs a stub does it,
// and how internal/cli's state-only commands used to do everything. Such a plugin can
// be COUNTED — which is what decides the implicit instance's name — but cannot build
// a further instance, and saying so beats a nil dereference.
func TestAConstructedProviderRegistersItsPluginByNameOnly(t *testing.T) {
	r := New()
	if err := r.Register("test", stubProvider{name: "test", defs: []*schema.ResourceDefinition{def("test.network")}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := r.PluginNames(); strings.Join(got, ",") != "test" {
		t.Errorf("PluginNames = %v, want the constructed provider's plugin listed", got)
	}
	// Not a FACTORY, though, which is the distinction the implicit-instance rule
	// turns on.
	if got := r.Factories(); len(got) != 0 {
		t.Errorf("Factories = %v, want none: a constructed provider carries no factory", got)
	}
	err := r.RegisterInstance("other", "test", nil)
	if err == nil {
		t.Fatal("a plugin known by name only built a second instance")
	}
	if !strings.Contains(err.Error(), "constructed provider") {
		t.Errorf("the error does not say why it cannot: %v", err)
	}
}

// TestRegisteringOnePluginTwiceIsRefused. Two calls means two objects believing they
// own one name, and the second silently winning is how a test double ends up serving
// production types.
func TestRegisteringOnePluginTwiceIsRefused(t *testing.T) {
	r := New()
	if err := r.RegisterPlugin(plugin()); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	if err := r.RegisterPlugin(plugin()); err == nil {
		t.Fatal("registering the same plugin twice must be refused")
	}
}

// TestRegisterPluginValidatesItsDefinitions, with the identical checks Register runs
// — they share checkDefinitions precisely so a schema refused through one door is not
// accepted through the other.
func TestRegisterPluginValidatesItsDefinitions(t *testing.T) {
	r := New()
	err := r.RegisterPlugin(&stubPlugin{name: "rogue", defs: []*schema.ResourceDefinition{
		{Type: "module.app", Attributes: map[string]schema.Attribute{"name": {Kind: value.KindString}}},
	}})
	if err == nil {
		t.Fatal("a plugin claiming the `module.` namespace must be refused")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("unexpected error: %v", err)
	}
	// And nothing was written, so the registry is not left half-populated.
	if _, ok := r.Definition("module.app"); ok {
		t.Error("the refused definition was registered anyway")
	}
}
