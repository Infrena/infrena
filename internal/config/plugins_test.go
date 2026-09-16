package config

import (
	"strings"
	"testing"
)

// PLAN.md §31.1's `plugins:` block: a version constraint per plugin.

func TestAPluginConstraintIsDecoded(t *testing.T) {
	decl, out := providersIn(t, `
project: p
plugins:
  aws: ">= 0.3.0, < 0.4.0"
  hetzner: "1.2.0"
`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if len(decl.Plugins) != 2 {
		t.Fatalf("decoded %d constraints, want 2: %+v", len(decl.Plugins), decl.Plugins)
	}
	// As WRITTEN, so a diagnostic quotes what the user will go and edit.
	if got := decl.Plugins["aws"].Constraint.String(); got != ">= 0.3.0, < 0.4.0" {
		t.Errorf("aws = %q", got)
	}
	if decl.Plugins["aws"].Origin.File == "" {
		t.Error("no origin, so a diagnostic cannot point at the line")
	}
}

// TestAProjectWithNoPluginsBlockIsUnconstrained — every project written before the key
// existed, which is all of them.
func TestAProjectWithNoPluginsBlockIsUnconstrained(t *testing.T) {
	decl, _ := providersIn(t, "project: p\n")
	if len(decl.Plugins) != 0 {
		t.Errorf("want no constraints, got %+v", decl.Plugins)
	}
}

// TestPluginsMustBeAMappingNotAList is the shape mistake `providers:` invites, since
// that one IS a list. The two differ for a reason worth stating in the error.
func TestPluginsMustBeAMappingNotAList(t *testing.T) {
	_, out := providersIn(t, `
project: p
plugins:
  - aws: ">= 0.3.0"
`)
	if out == "" {
		t.Fatal("a list must be refused")
	}
	if !strings.Contains(out, "mapping") {
		t.Errorf("the diagnostic does not say what shape is wanted:\n%s", out)
	}
}

func TestAMalformedPluginConstraintIsRefused(t *testing.T) {
	_, out := providersIn(t, `
project: p
plugins:
  aws: "newest please"
`)
	if out == "" {
		t.Fatal("`aws: \"newest please\"` must be refused")
	}
	for _, want := range []string{"aws", "MAJOR.MINOR.PATCH"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// TestOnePluginConstrainedTwiceIsAnError. YAML is happy with the repeated key and takes
// the last silently, so which constraint won would depend on document order.
func TestOnePluginConstrainedTwiceIsAnError(t *testing.T) {
	_, out := providersIn(t, `
project: p
plugins:
  aws: ">= 0.3.0"
  aws: "< 0.2.0"
`)
	if out == "" {
		t.Fatal("constraining one plugin twice must be an error")
	}
	if !strings.Contains(out, "twice") {
		t.Errorf("the diagnostic does not say it is a duplicate:\n%s", out)
	}
	if got := distinctLines(out); len(got) < 2 {
		t.Errorf("the diagnostic names %v, want both lines:\n%s", got, out)
	}
}

// PLAN.md §31.3's additive mapping form: a project may also say where a plugin
// comes from.

// The scalar form is what every existing project writes and must behave
// EXACTLY as it does today. The mapping form is additive (section 58).
func TestPluginsAcceptsBothTheScalarAndMappingForms(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
plugins:
  aws: ">= 0.3.0, < 0.4.0"
  hetzner:
    version: ">= 1.2"
    source: github.com/someone/infrena-provider-hetzner
`,
	})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}

	if got := p.Plugins["aws"]; got.Source != "" {
		t.Errorf("scalar form gained a source: %q", got.Source)
	}
	if got := p.Plugins["aws"].Constraint.String(); got != ">= 0.3.0, < 0.4.0" {
		t.Errorf("scalar constraint = %q", got)
	}
	hetzner := p.Plugins["hetzner"]
	if hetzner.Source != "github.com/someone/infrena-provider-hetzner" {
		t.Errorf("source = %q", hetzner.Source)
	}
	if got := hetzner.Constraint.String(); got != ">= 1.2" {
		t.Errorf("mapping constraint = %q", got)
	}
	if hetzner.SourceOrigin.Line == 0 {
		t.Error("source carries no origin, so a diagnostic cannot point at it")
	}
}

// Fails closed on unknown keys, like every other block in this language.
func TestAnUnknownKeyInThePluginMappingIsAnError(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "plugins:\n  aws:\n    versoin: \">= 1\"\n",
	})
	if !ds.HasErrors() {
		t.Fatal("an unknown key was accepted")
	}
}

// A source the project names must be WELL FORMED at decode time, so the error
// points at the line in infra.yml rather than surfacing later from an install.
func TestAMalformedProjectSourceIsRefusedAtItsLine(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "plugins:\n  aws:\n    source: not-a-source\n",
	})
	if !ds.HasErrors() {
		t.Fatal("a malformed source was accepted")
	}
}
