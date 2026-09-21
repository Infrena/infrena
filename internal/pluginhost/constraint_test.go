package pluginhost

import (
	"context"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/semver"
	"github.com/infrena/infrena/pkg/value"
)

// A project may constrain a plugin's version, and the loader is where that is enforced:
// four separate paths load plugins, and a constraint checked in three of them is a
// constraint nobody can rely on.

// versioned is a plugin reporting whatever version a test asks for. An empty version
// means it does not implement Version() at all, which is the case with its own message.
type versioned struct{ version string }

func (p versioned) Name() string { return "demo" }
func (p versioned) Definitions() []*schema.ResourceDefinition {
	return []*schema.ResourceDefinition{{
		Type:         "demo.widget",
		Attributes:   map[string]schema.Attribute{"name": {Kind: value.KindString, Required: true}},
		Capabilities: schema.Capabilities{Create: true, Read: true, Delete: true},
	}}
}
func (p versioned) New(provider.Config) (provider.Provider, error) {
	return &badProvider{p: &badPlugin{}}, nil
}

// reports implements the optional Version() the SDK looks for.
type reports struct {
	versioned
	v string
}

func (p reports) Version() string { return p.v }

func loaderFor(t *testing.T, plugin provider.Plugin, constraint string) *Loader {
	t.Helper()
	l := &Loader{Dir: t.TempDir(), Builtin: map[string]provider.Plugin{"demo": plugin}}
	if constraint != "" {
		c, err := semver.ParseConstraint(constraint)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", constraint, err)
		}
		l.Constraints = map[string]semver.Constraint{"demo": c}
	}
	t.Cleanup(l.Close)
	return l
}

func TestAVersionInsideTheConstraintLoads(t *testing.T) {
	l := loaderFor(t, reports{v: "0.3.5"}, ">= 0.3.0, < 0.4.0")
	if _, err := l.Load(context.Background(), "demo"); err != nil {
		t.Fatalf("0.3.5 satisfies >= 0.3.0, < 0.4.0: %v", err)
	}
}

// The refusal names both numbers: a reader needs what they have as well as what is
// wanted.
func TestAVersionOutsideTheConstraintIsRefused(t *testing.T) {
	l := loaderFor(t, reports{v: "0.2.0"}, ">= 0.3.0")
	_, err := l.Load(context.Background(), "demo")
	if err == nil {
		t.Fatal("0.2.0 does not satisfy >= 0.3.0 and must be refused")
	}
	for _, want := range []string{"0.2.0", ">= 0.3.0", "demo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// The SDK answers "0.0.0" for a plugin that does not implement Version(), so it fails
// any constraint above 0.0.0 — which is correct, and unhelpful said as "0.0.0 does not
// satisfy >= 0.3.0": that sends an author looking for a version they never set.
//
// Note the asymmetry with infrena's own `infrena:` floor, which exempts a 0.0.0 build.
// There the unversioned binary is the user's development build and a complaint is not
// actionable. Here it is a third-party plugin they installed, and it is.
func TestAPluginReportingNoVersionGetsItsOwnMessage(t *testing.T) {
	l := loaderFor(t, versioned{}, ">= 0.3.0")
	_, err := l.Load(context.Background(), "demo")
	if err == nil {
		t.Fatal("a plugin that reports no version cannot satisfy a constraint")
	}
	if !strings.Contains(err.Error(), "does not report a version") {
		t.Errorf("the error reads as a version mismatch rather than a missing version: %v", err)
	}
	if !strings.Contains(err.Error(), "Version()") {
		t.Errorf("the error does not say how an author fixes it: %v", err)
	}
}

// `plugins:` is optional, and a project that states nothing must behave exactly as it
// did before the field existed.
func TestAnUnconstrainedPluginLoadsWhateverIsThere(t *testing.T) {
	for _, p := range []provider.Plugin{versioned{}, reports{v: "0.1.0"}, reports{v: "99.0.0"}} {
		l := loaderFor(t, p, "")
		if _, err := l.Load(context.Background(), "demo"); err != nil {
			t.Errorf("an unconstrained plugin must load: %v", err)
		}
	}
}

// The command is going to fail; leaving a child process behind to be reaped at exit is
// how a refusal becomes a hang on a plugin that ignores stdin closing. Observable here
// as the loader holding nothing to close afterwards.
func TestARefusedPluginIsShutDownNotLeftRunning(t *testing.T) {
	l := loaderFor(t, reports{v: "0.2.0"}, ">= 0.3.0")
	if _, err := l.Load(context.Background(), "demo"); err == nil {
		t.Fatal("expected a refusal")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.loaded) != 0 {
		t.Errorf("a refused plugin was kept as loaded: %v", l.loaded)
	}
}

// A project naming one bad plugin on twenty resources should say so once.
func TestTheRefusalIsCachedSoAProjectHearsItOnce(t *testing.T) {
	plugin := &countingPlugin{v: "0.2.0"}
	l := &Loader{
		Dir:     t.TempDir(),
		Builtin: map[string]provider.Plugin{"demo": plugin},
	}
	c, err := semver.ParseConstraint(">= 0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	l.Constraints = map[string]semver.Constraint{"demo": c}
	t.Cleanup(l.Close)

	for range 3 {
		if _, err := l.Load(context.Background(), "demo"); err == nil {
			t.Fatal("expected a refusal")
		}
	}
	if plugin.definitionCalls == 0 {
		t.Fatal("the plugin was never consulted at all")
	}
	// Loaded once, refused once, remembered: the plugin is not started again per
	// caller.
	if plugin.starts != 1 {
		t.Errorf("the plugin was started %d times; a cached failure means once", plugin.starts)
	}
}

type countingPlugin struct {
	reports
	starts          int
	definitionCalls int
}

func (p *countingPlugin) Definitions() []*schema.ResourceDefinition {
	p.definitionCalls++
	if p.definitionCalls == 1 {
		p.starts++
	}
	return p.reports.Definitions()
}
