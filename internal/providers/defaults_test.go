package providers

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/value"
)

// The `defaults:` block, and the rule that governs it: fail closed.
//
// A key nothing accepts applies to nothing, in every environment, forever, and there
// is no output in which its absence is visible. A plan looks right, an apply succeeds,
// and the value is simply never there.

func defaultsDiags(t *testing.T, body string) string {
	t.Helper()
	_, out := rendered(t, body, pluginRegistry(t, t.TempDir()), variables.Scope{})
	return out
}

// `tag:` for `tags:`.
func TestAKeyNoResourceTypeAcceptsIsRefused(t *testing.T) {
	out := defaultsDiags(t, `
project: p
providers:
  - plugin: fake
    defaults:
      tag:
        team: payments
`)
	if out == "" {
		t.Fatal("a `defaults:` key no resource type declares must be refused")
	}
	if !strings.Contains(out, "tag") {
		t.Errorf("the diagnostic does not name the key:\n%s", out)
	}
	// And it lists what exists, because the reader's next move is to pick the right
	// name and nothing else in the output tells them what the names are.
	if !strings.Contains(out, "tags") {
		t.Errorf("the diagnostic does not list the attribute they meant:\n%s", out)
	}
}

// The boundary, and the half that keeps the rule above from making `defaults:`
// unusable.
//
// `tags` is declared by fake.database and by neither of its siblings. A check demanding
// every type accept a key would refuse the single most obvious thing anyone would
// write here.
func TestAKeyOnlySomeResourceTypesDeclareIsAccepted(t *testing.T) {
	out := defaultsDiags(t, `
project: p
providers:
  - plugin: fake
    defaults:
      tags:
        team: payments
`)
	if out != "" {
		t.Errorf("`tags` is declared by one of the plugin's three types, which is the "+
			"ordinary case:\n%s", out)
	}
}

// The engine owns prevent_destroy and retain, so they belong to no plugin's schema
// and must still be accepted here.
func TestALifecycleOptionIsAcceptedThoughNoSchemaDeclaresIt(t *testing.T) {
	out := defaultsDiags(t, `
project: p
providers:
  - plugin: fake
    defaults:
      prevent_destroy: true
      retain: false
`)
	if out != "" {
		t.Errorf("the lifecycle options must be accepted in `defaults:`:\n%s", out)
	}
}

// `prevent_destroy: yes-please` is a string, and silently treating a non-empty string
// as true is how a destroy gets refused for a reason nobody wrote down.
func TestANonBooleanLifecycleOptionIsRefused(t *testing.T) {
	out := defaultsDiags(t, `
project: p
providers:
  - plugin: fake
    defaults:
      prevent_destroy: "sometimes"
`)
	if out == "" {
		t.Fatal("a non-boolean lifecycle default must be refused")
	}
	for _, want := range []string{"prevent_destroy", "boolean"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// A computed attribute is reported by the
// provider after the resource exists, so nothing can supply it in advance — emitting it
// into a resource would produce "is computed and cannot be set" at every use site.
func TestDefaultingAComputedAttributeIsRefused(t *testing.T) {
	out := defaultsDiags(t, `
project: p
providers:
  - plugin: fake
    defaults:
      endpoint: db.example.com
`)
	if out == "" {
		t.Fatal("defaulting a computed attribute must be refused")
	}
	for _, want := range []string{"endpoint", "computed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// `size` is an integer everywhere it is declared.
func TestADefaultOfTheWrongKindIsRefused(t *testing.T) {
	out := defaultsDiags(t, `
project: p
providers:
  - plugin: fake
    defaults:
      size: enormous
`)
	if out == "" {
		t.Fatal("`size: enormous` against an integer attribute must be refused")
	}
	for _, want := range []string{"size", "integer"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// The reason the block is resolved after stage 4 at all: per-environment tags.
func TestADefaultMayInterpolateAVariable(t *testing.T) {
	reg := pluginRegistry(t, t.TempDir())
	table, out := rendered(t, `
project: p
providers:
  - plugin: fake
    defaults:
      tags:
        environment: ${var.environment}
`, reg, scopeWith(map[string]string{"environment": "production"}))
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	tags, ok := table["fake"].Defaults["tags"]
	if !ok {
		t.Fatal("the instance has no `tags` default")
	}
	m, ok := tags.Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags Raw is %T", tags.Raw)
	}
	if got, _ := m["environment"].AsString(); got != "production" {
		t.Errorf("environment = %v, want the variable resolved", m["environment"])
	}
}

// Same reasoning as a `defaults:` key nothing declares: `plugins: {awz: ">= 1"}`
// constrains nothing, in every environment, forever, with no output in which its absence
// is visible. The user believes they have pinned a version and they have not.
func TestAConstraintOnAPluginThisProjectDoesNotUseIsRefused(t *testing.T) {
	out := defaultsDiags(t, `
project: p
plugins:
  awz: ">= 1.0"
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if out == "" {
		t.Fatal("a constraint on a plugin nothing uses must be refused")
	}
	if !strings.Contains(out, "awz") {
		t.Errorf("the diagnostic does not name the key:\n%s", out)
	}
	// And it lists what the project does use, because the next move is to pick the
	// right name.
	if !strings.Contains(out, "fake") {
		t.Errorf("the diagnostic does not list the plugins in use:\n%s", out)
	}
}

// The boundary, and the half that keeps the rule above from making `plugins:`
// unusable.
//
// Both sources count as "uses it": a `providers:` entry naming the plugin, and a
// resource type prefixed with it.
func TestAConstraintOnAPluginThisProjectDoesUseIsAccepted(t *testing.T) {
	for _, body := range []string{
		// Named by a resource type's prefix only.
		`
project: p
plugins:
  fake: ">= 0.0.0"
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`,
		// Named by a `providers:` entry only.
		`
project: p
plugins:
  fake: ">= 0.0.0"
providers:
  - plugin: fake
`,
	} {
		if out := defaultsDiags(t, body); out != "" {
			t.Errorf("a constraint on a plugin the project uses must be accepted:\n%s", out)
		}
	}
}
