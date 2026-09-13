package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/providers"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
	testprovider "github.com/infrata/infrata/providers/test"
)

// `discover` and `import` ask EVERY available plugin, because their scope is not set by
// configuration — they report what exists, including in accounts nothing mentions.
//
// Reported by infrata-provider-fake's e2e suite: with the builtin `test` plus a real
// infrata-plugin-fake on --plugin-dir, `discover` printed "Nothing found." at exit 0
// while the fake cloud held resources. discoveryRegistry called providers.Implicit,
// whose job is to pick the ONE instance a resource that names none belongs to — so it
// correctly returns nothing when more than one plugin is available, and registered no
// instances at all.
//
// The failure was silent, which is what makes it worth a regression test rather than a
// fix: an empty survey at exit 0 reads as "there is nothing there".

// second is a second plugin, so the registry has more than one factory. One is all the
// real builtin set has, and the bug only appears at two.
type second struct{}

func (second) Name() string { return "second" }
func (second) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:         "second.thing",
		Attributes:   map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true, Import: true},
	}}
}
func (second) New(provider.Config) (provider.Provider, error) { return secondProvider{}, nil }

type secondProvider struct{}

func (secondProvider) Name() string { return "second" }
func (s secondProvider) Definitions() []*schema.ResourceDefinition {
	return second{}.Definitions()
}
func (secondProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }
func (secondProvider) Read(context.Context, *resource.ResourceState) (*resource.ResourceState, error) {
	return nil, nil
}
func (secondProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (secondProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (secondProvider) Delete(context.Context, *resource.ResourceState) error { return nil }
func (secondProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, nil
}
func (secondProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

// withTwoPlugins substitutes a two-plugin builtin set for one test.
func withTwoPlugins(t *testing.T) {
	t.Helper()
	previous := builtinsFor
	builtinsFor = func(dir string) map[string]provider.Plugin {
		return map[string]provider.Plugin{
			"test":   testprovider.NewPlugin(dir),
			"second": second{},
		}
	}
	t.Cleanup(func() { builtinsFor = previous })
}

// TestDiscoveryRegistersAnInstancePerPluginNotNone is the regression.
func TestDiscoveryRegistersAnInstancePerPluginNotNone(t *testing.T) {
	withTwoPlugins(t)
	dir := projectDir(t, "project: p\nresources: {}\n")

	reg, table, ds, closePlugins := discoveryRegistry(&GlobalOptions{Dir: dir})
	defer closePlugins()
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", renderToString(ds))
	}

	if len(table) != 2 {
		t.Fatalf("discovery has %d instances, want one per plugin: %v", len(table), table.Names())
	}
	// And they are REGISTERED, not merely tabulated — discovery walks the registry.
	names := strings.Join(reg.InstanceNames(), ",")
	if names != "second,test" {
		t.Errorf("registered instances = %q, want both plugins", names)
	}
	// Each serves its own plugin's types, which is what makes the survey complete.
	if _, ok := reg.ProviderFor("test.network", "test"); !ok {
		t.Error("the test instance does not serve test.network")
	}
	if _, ok := reg.ProviderFor("second.thing", "second"); !ok {
		t.Error("the second instance does not serve second.thing")
	}
}

// TestOnePluginStillGetsItsInstance is the boundary: the single-plugin case is every
// project today, and it must not regress while fixing the two-plugin one.
func TestOnePluginStillGetsItsInstance(t *testing.T) {
	dir := projectDir(t, "project: p\nresources: {}\n")

	_, table, ds, closePlugins := discoveryRegistry(&GlobalOptions{Dir: dir})
	defer closePlugins()
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", renderToString(ds))
	}
	if len(table) != 1 || table.Names()[0] != "test" {
		t.Errorf("discovery has %v, want the one builtin", table.Names())
	}
}

// TestBindingStillRefusesToGuessBetweenTwoPlugins is the other half, and the reason the
// two functions stayed separate. A RESOURCE naming no provider, with two plugins
// available, must STILL be refused rather than sent to an account nobody chose — which
// is the failure Implicit exists to prevent, and which the discovery fix must not undo.
func TestBindingStillRefusesToGuessBetweenTwoPlugins(t *testing.T) {
	withTwoPlugins(t)
	dir := projectDir(t, "project: p\nresources: {}\n")

	reg, closePlugins := buildRegistry(&GlobalOptions{Dir: dir})
	defer closePlugins()
	for _, name := range []string{"test", "second"} {
		if err := reg.EnsurePlugin(context.Background(), name); err != nil {
			t.Fatalf("EnsurePlugin(%s): %v", name, err)
		}
	}
	if table := providers.Implicit(reg); len(table) != 0 {
		t.Errorf("Implicit returned %v with two plugins available; it must refuse to guess "+
			"which account an unnamed resource belongs to", table.Names())
	}
	// While discovery, asking the other question, gets both.
	if table := providers.EveryPlugin(reg); len(table) != 2 {
		t.Errorf("EveryPlugin returned %v, want both plugins", table.Names())
	}
}
