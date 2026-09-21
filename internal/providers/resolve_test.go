package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/value"
)

// Resolving provider instances, after variables.

func writeFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "infrena.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decls(t *testing.T, body string) []config.ProviderDecl {
	t.Helper()
	return project(t, body).Providers
}

// project decodes a one-file fixture, failing if it does not decode: a fixture that
// cannot be read reaches no resolution at all, so the test would assert nothing.
func project(t *testing.T, body string) *config.ProjectDecl {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, body)
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	decl, ds := config.Decode(files)
	if ds.HasErrors() {
		t.Fatalf("the fixture does not decode, so no resolution is reached: %+v", ds)
	}
	return decl
}

func scopeWith(pairs map[string]string) variables.Scope {
	var s variables.Scope
	for k, v := range pairs {
		s.Override(k, value.String(v, value.SourceVariable))
	}
	return s
}

// The point of the whole rule. Two scopes, because one alone cannot tell "resolved"
// from "resolved correctly".
func TestAnInstancesConfigInterpolates(t *testing.T) {
	d := decls(t, "project: p\nproviders:\n  - plugin: fake\n    iam-role: ${var.aws_role}\n")

	for _, tc := range []struct{ role string }{{"arn:prod"}, {"arn:dev"}} {
		table, ds := Resolve(d, scopeWith(map[string]string{"aws_role": tc.role}))
		if ds.HasErrors() {
			t.Fatalf("%s: %+v", tc.role, ds)
		}
		got, _ := table["fake"].Config["iam-role"].AsString()
		if got != tc.role {
			t.Errorf("iam-role = %q, want %q", got, tc.role)
		}
	}
}

// Per leaf, through the shared interpolation walk rather than a second one.
func TestACompositeConfigValueInterpolatesPerLeaf(t *testing.T) {
	d := decls(t, `
project: p
providers:
  - plugin: fake
    defaults:
      tags:
        environment: ${var.environment}
        team: payments
`)
	table, ds := Resolve(d, scopeWith(map[string]string{"environment": "production"}))
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	tags, ok := table["fake"].Defaults["tags"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags is not a map: %#v", table["fake"].Defaults["tags"].Raw)
	}
	if got, _ := tags["environment"].AsString(); got != "production" {
		t.Errorf("the interpolated leaf = %q, want production", got)
	}
	// The literal leaf survives untouched alongside it.
	if got, _ := tags["team"].AsString(); got != "payments" {
		t.Errorf("the literal leaf = %q", got)
	}
}

// A provider is configured before any resource exists, so this would have it create
// the thing its own credentials depend on.
//
// The message matters as much as the refusal. An unknown value would read as an
// engine bug; naming the reference says what cannot work.
func TestAResourceReferenceInProviderConfigIsAnError(t *testing.T) {
	d := decls(t, "project: p\nproviders:\n  - plugin: fake\n    iam-role: ${db.arn}\n")
	_, ds := Resolve(d, variables.Scope{})
	if !ds.HasErrors() {
		t.Fatal("a resource reference in provider configuration must be refused")
	}
	var sb strings.Builder
	ds.Render(&sb)
	out := sb.String()
	for _, want := range []string{"db.arn", "iam-role", "before any resource exists"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
	// And it must say what to do instead.
	if !strings.Contains(out, "variable") {
		t.Errorf("the diagnostic offers no alternative:\n%s", out)
	}
}

func TestTheTableIsKeyedByInstanceNameWithOneDefault(t *testing.T) {
	d := decls(t, `
project: p
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
`)
	table, ds := Resolve(d, variables.Scope{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(table) != 2 {
		t.Fatalf("table holds %d instances: %v", len(table), table.Names())
	}
	if table.DefaultName() != "main" {
		t.Errorf("DefaultName = %q, want main (first)", table.DefaultName())
	}
	// Both share a plugin, which is the case multiple instances exist for.
	if table["main"].Plugin != "fake" || table["acct2"].Plugin != "fake" {
		t.Error("two instances of one plugin did not both record it")
	}
	if strings.Join(table.Names(), ",") != "acct2,main" {
		t.Errorf("Names() = %v, want them sorted", table.Names())
	}
}

// A literal must not be reparsed.
func TestAnInstanceWithNoInterpolationIsUnchanged(t *testing.T) {
	d := decls(t, "project: p\nproviders:\n  - plugin: fake\n    iam-role: plain-value\n")
	table, ds := Resolve(d, variables.Scope{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got, _ := table["fake"].Config["iam-role"].AsString(); got != "plain-value" {
		t.Errorf("iam-role = %q", got)
	}
}

// Not an unknown that surfaces as a credential failure at apply, which is where it
// would otherwise land.
func TestAnUndefinedVariableInProviderConfigIsReported(t *testing.T) {
	d := decls(t, "project: p\nproviders:\n  - plugin: fake\n    iam-role: ${var.nosuch}\n")
	if _, ds := Resolve(d, variables.Scope{}); !ds.HasErrors() {
		t.Fatal("an undefined variable must be reported here, not left to fail at apply")
	}
}
