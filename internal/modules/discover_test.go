package modules

import (
	"testing"

	"github.com/infrata/infrata/internal/variables"
)

func TestADirectoryHoldingAModuleFileIsLoadedWithoutAModulesEntry(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nresources:\n  prod:\n    type: module.networking\n",
		// No `modules:` entry at all.
		"modules/networking/module.yml": "resources:\n  vpc:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("a module.yml under the project root is loaded automatically: %+v", ds)
	}
	if len(exp.Instances) != 1 || exp.Instances[0].Decl.Name != "vpc" {
		t.Fatalf("instances = %+v, want the discovered module's one resource", exp.Instances)
	}
}

func TestDiscoveryNormalisesHyphensToUnderscores(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":                    "project: demo\nresources:\n  prod:\n    type: module.app_stack\n",
		"modules/app-stack/module.yml": "resources:\n  svc:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("`app-stack` must be loadable as `module.app_stack`: %+v", ds)
	}
}

// §8e's one exception to the collision rule.
func TestAnExplicitModulesEntryBeatsADiscoveredOneSilently(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - name: net
    source: ./vendor/net
resources:
  prod:
    type: module.net
`,
		"net/module.yml":        "resources:\n  discovered:\n    type: test.thing\n",
		"vendor/net/module.yml": "resources:\n  explicit:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("explicit beating implicit is not a collision (§7, §8e): %+v", ds)
	}
	// The assertion that matters: WHICH one won. A test that only counted
	// instances would pass with either.
	if len(exp.Instances) != 1 || exp.Instances[0].Decl.Name != "explicit" {
		t.Fatalf("instances = %+v, want the explicitly loaded module's resource — "+
			"§7: explicit config always wins over an implicit default", exp.Instances)
	}
}

func TestTwoDiscoveredDirectoriesWithOneNameAreRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":        "project: demo\nresources:\n  prod:\n    type: module.net\n",
		"a/net/module.yml": "resources:\n  one:\n    type: test.thing\n",
		"b/net/module.yml": "resources:\n  two:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, dir, paths{})
	if !hasFragment(ds, "two directories both define a module named \"net\"") {
		t.Errorf("two discovered modules deriving one name must be refused, never resolved "+
			"by order; got %+v", ds)
	}
	if !hasFragment(ds, "a/net") || !hasFragment(ds, "b/net") {
		t.Errorf("the diagnostic must name BOTH directories; got %+v", ds)
	}
}

func TestDiscoverySkipsDotDirectories(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":                            "project: demo\nresources:\n  prod:\n    type: module.real\n",
		"real/module.yml":                      "resources:\n  r:\n    type: test.thing\n",
		".infra/modules/abc123/net/module.yml": "resources:\n  cached:\n    type: test.thing\n",
		".git/hooks/net/module.yml":            "resources:\n  bogus:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	for _, inst := range exp.Instances {
		if inst.Decl.Name == "cached" || inst.Decl.Name == "bogus" {
			t.Fatalf("%q came from a skipped directory; the module cache holds every module "+
				"this project has ever fetched, under names taken from content hashes", inst.Decl.Name)
		}
	}
}

func TestDiscoveryDoesNotReachInsideAModule(t *testing.T) {
	// A module's dependencies come from its own `modules:` list. If discovery
	// applied at every level, a module would resolve a name against whatever
	// directories the CONSUMING project happens to contain, and would work in
	// one project and fail in the next.
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nresources:\n  prod:\n    type: module.outer\n",
		// `helper` is discoverable from the ROOT, and `outer` tries to use it
		// without loading it.
		"outer/module.yml":  "resources:\n  inner:\n    type: module.helper\n",
		"helper/module.yml": "resources:\n  h:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, dir, paths{})
	if !hasFragment(ds, "no module named \"helper\" is loaded") {
		t.Errorf("a module must declare its own dependencies; got %+v", ds)
	}
	if !hasFragment(ds, "module prod") {
		t.Errorf("the diagnostic must say which instantiation the problem is inside; got %+v", ds)
	}
}
