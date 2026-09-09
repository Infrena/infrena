package diag

import (
	"bytes"
	"strings"
	"testing"

	"infra/pkg/value"
)

func TestHasErrorsIgnoresWarnings(t *testing.T) {
	var ds Diagnostics
	ds.Add(Diagnostic{Severity: SeverityWarning, Summary: "unused variable"})
	if ds.HasErrors() {
		t.Error("warnings must not make HasErrors true")
	}
	ds.Add(Diagnostic{Severity: SeverityError, Summary: "unknown type"})
	if !ds.HasErrors() {
		t.Error("an error must make HasErrors true")
	}
}

func TestCollectsRatherThanStopping(t *testing.T) {
	var ds Diagnostics
	ds.Add(Diagnostic{Severity: SeverityError, Summary: "first"})
	ds.Add(Diagnostic{Severity: SeverityError, Summary: "second"})
	if len(ds) != 2 {
		t.Fatalf("len = %d, want 2 — validate reports every problem in one pass (spec §7.4)", len(ds))
	}
}

func TestRenderIncludesLocationExpectationAndAction(t *testing.T) {
	ds := Diagnostics{{
		Severity: SeverityError,
		Summary:  "application requires a network",
		Detail:   "No network resource was found in environment \"production\".\nExpected one of:\n  test.network",
		Action:   "Add a networking module or resource.",
		Origin:   value.Origin{File: "infra.yml", Line: 12, Column: 3},
	}}

	var out bytes.Buffer
	ds.Render(&out)
	got := out.String()

	for _, want := range []string{
		"application requires a network",
		"infra.yml:12:3",
		"Expected one of:",
		"Suggested action:",
		"Add a networking module or resource.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output is missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderNamesTheModuleChain(t *testing.T) {
	ds := Diagnostics{{
		Severity: SeverityError,
		Summary:  "unknown attribute",
		Origin:   value.Origin{File: "modules/db/module.yml", Line: 4, Module: []string{"platform", "database"}},
	}}
	var out bytes.Buffer
	ds.Render(&out)
	if !strings.Contains(out.String(), "module.platform.module.database") {
		t.Errorf("module chain missing — flattening makes this the only way to locate the instantiation (spec §7.2)\n%s", out.String())
	}
}
