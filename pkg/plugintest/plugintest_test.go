package plugintest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/plugintest"
	"github.com/infrata/infrata/pkg/provider"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/schema"
	"github.com/infrata/infrata/pkg/value"
)

// package plugintest_test, not plugintest: an EXTERNAL test package, importing only
// what an outside plugin author can import.
//
// That is the whole point of this package's existence. The host is internal/, so a
// plugin in its own module cannot reach it, and the authoring guide recommended
// testing against it anyway — a recommendation nobody could follow.
//
// WHAT THIS FILE CANNOT PROVE, stated plainly: Go's internal/ rule is per MODULE, so an
// internal import added here would compile fine and this file would not notice. It
// demonstrates the intended shape and pins the behaviour; the only real check that an
// outside module can use this package is an outside module doing so.
//
// DISCHARGED 2026-09-13. infrata-provider-fake — a separate module — imports this
// package in internal/fake/protocol_test.go and its suite passes (that repository's
// 0fd8bcf). Recorded here rather than left open, so nobody re-derives the analysis:
// what remains unprovable from inside this module is only that it STAYS true, which is
// that suite's job to keep failing if it stops.

// demo is the smallest plugin that does anything, written the way a third party would.
type demo struct{}

func (demo) Name() string { return "demo" }

func (demo) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:        "demo.widget",
		Description: "A widget.",
		Attributes: map[string]schema.Attribute{
			"name":   {Kind: value.KindString, Required: true},
			"size":   {Kind: value.KindInt, Default: int64(3)},
			"secret": {Kind: value.KindString, Sensitive: true},
			"id":     {Kind: value.KindString, Computed: true},
		},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}}
}

func (demo) New(provider.Config) (provider.Provider, error) { return demoProvider{}, nil }

type demoProvider struct{}

func (demoProvider) Name() string { return "demo" }
func (d demoProvider) Definitions() []*schema.ResourceDefinition {
	return demo{}.Definitions()
}
func (demoProvider) ClassifyError(error) provider.Retryability { return provider.NotSafeToRetry }

func (demoProvider) Read(_ context.Context, current *resource.ResourceState) (*resource.ResourceState, error) {
	// Deliberately sloppy in the two ways a real plugin is: the secret is returned
	// unflagged, and no bookkeeping is carried forward. The host is supposed to make
	// both harmless, and this is how a plugin author confirms it.
	return &resource.ResourceState{
		Type:       current.Type,
		ProviderID: current.ProviderID,
		Attributes: map[string]value.Value{
			"name":   value.String("widget", value.SourceProvider),
			"secret": value.String("hunter2", value.SourceProvider),
		},
	}, nil
}

func (demoProvider) Create(context.Context, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (demoProvider) Update(context.Context, *resource.ResourceState, *resource.DesiredResource) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}
func (demoProvider) Delete(context.Context, *resource.ResourceState) error { return nil }
func (demoProvider) Discover(context.Context, provider.DiscoverRequest) ([]provider.DiscoveredResource, error) {
	return nil, provider.ErrNotImplemented
}
func (demoProvider) Import(context.Context, string, string) (*resource.ResourceState, error) {
	return nil, provider.ErrNotImplemented
}

func open(t *testing.T) *plugintest.Host {
	t.Helper()
	host, err := plugintest.Open(context.Background(), demo{}, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

// TestOpenValidatesSchemas is the cheapest test a plugin author can write, and the one
// worth writing first: it proves the plugin is loadable at all.
func TestOpenValidatesSchemas(t *testing.T) {
	host := open(t)
	defs := host.Definitions()
	if len(defs) != 1 || defs[0].Type != "demo.widget" {
		t.Fatalf("Definitions() = %+v", defs)
	}
	// The default made the round trip as an INTEGER. A schema crosses the protocol as
	// JSON, where a naive encoding turns int64 into float64 — and the compiler then
	// reports the plugin's own schema as a provider bug. This assertion is why the
	// harness is worth using rather than calling Definitions() directly.
	attr, ok := defs[0].Attribute("size")
	if !ok {
		t.Fatal("no size attribute")
	}
	v, ok := schema.DatumValue(attr.Default, attr.Kind)
	if !ok {
		t.Fatalf("the default did not survive the protocol: %#v (%T)", attr.Default, attr.Default)
	}
	if n, _ := v.AsInt(); n != 3 {
		t.Errorf("default = %v, want 3", v)
	}
}

// TestASchemaOutsideItsOwnPrefixIsRefused. A plugin serves `<name>.*` and nothing
// else, and Open is where an author finds that out.
func TestASchemaOutsideItsOwnPrefixIsRefused(t *testing.T) {
	_, err := plugintest.Open(context.Background(), wrongPrefix{}, t.TempDir())
	if err == nil {
		t.Fatal("a type outside the plugin's own prefix must be refused")
	}
	if !strings.Contains(err.Error(), "aws.instance") {
		t.Errorf("the error does not name the offending type: %v", err)
	}
}

type wrongPrefix struct{ demo }

func (wrongPrefix) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:         "aws.instance",
		Attributes:   map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}}
}

// TestTheHostsRulesApplyThroughTheHarness — what an author is really checking. The
// plugin above is sloppy on purpose; none of it reaches the engine.
func TestTheHostsRulesApplyThroughTheHarness(t *testing.T) {
	host := open(t)
	prov, err := host.Configure(provider.Config{Instance: "main"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	got, err := prov.Read(context.Background(), &resource.ResourceState{
		Type:       "demo.widget",
		ProviderID: "w-1",
		Provider:   "main",
		Lifecycle:  resource.Lifecycle{PreventDestroy: true},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.Lifecycle.PreventDestroy {
		t.Error("the host did not re-attach prevent_destroy, so a plugin could lose the guard")
	}
	if !got.Attributes["secret"].Sensitive {
		t.Error("a schema-sensitive value came back unflagged, so it would print in clear")
	}
	if got.Attributes["name"].Source != value.SourceProvider {
		t.Errorf("Source = %s, want provider", got.Attributes["name"].Source)
	}
}
