package diag

import (
	"bytes"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
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
		Detail:   "No network resource was found in environment \"production\".\nExpected one of:\n  fake.network",
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

// TestSeverityStringNamesUnknownValues pins the explicit default. The earlier
// if/else form returned "Error" for every value that was not SeverityWarning,
// so a corrupt severity was indistinguishable from a real error — and it
// reaches the persisted plan artifact through the planner's wire form.
func TestSeverityStringNamesUnknownValues(t *testing.T) {
	if got := SeverityError.String(); got != "Error" {
		t.Errorf("SeverityError.String() = %q, want %q", got, "Error")
	}
	if got := SeverityWarning.String(); got != "Warning" {
		t.Errorf("SeverityWarning.String() = %q, want %q", got, "Warning")
	}
	// The zero value IS SeverityError, so that case is legitimately "Error".
	var unset Severity
	if got := unset.String(); got != "Error" {
		t.Errorf("the zero value is SeverityError; String() = %q, want %q", got, "Error")
	}
	for _, s := range []Severity{2, 7, 255} {
		got := s.String()
		if got == "Error" || got == "Warning" {
			t.Errorf("Severity(%d).String() = %q — an unrecognised severity must not "+
				"masquerade as a defined one", s, got)
		}
		if got == "" {
			t.Errorf("Severity(%d).String() is empty; it must say something", s)
		}
	}
}
