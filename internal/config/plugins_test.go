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
