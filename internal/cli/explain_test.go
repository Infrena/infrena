package cli

import (
	"strings"
	"testing"
)

func explainOut(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetErr(&sb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return sb.String(), err
}

// TestExplainRendersEveryGroup. §30 asks for Required / Optional / Computed
// and Capabilities, and an attribute must appear in exactly one group — a
// reader counting them is entitled to see the whole surface.
func TestExplainRendersEveryGroup(t *testing.T) {
	out, err := explainOut(t, "explain", "test.database")
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Required:", "engine",
		"Optional:", "password", "size",
		"Computed:", "endpoint",
		"Capabilities:", "create",
		"Import ID:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain output is missing %q:\n%s", want, out)
		}
	}
	// A computed attribute must NOT also be offered as settable: `endpoint`
	// appearing under Optional would tell a user to write something the
	// provider owns.
	optional := sectionOf(t, out, "Optional:")
	if strings.Contains(optional, "endpoint") {
		t.Errorf("a computed attribute was listed as settable:\n%s", optional)
	}
}

// TestExplainStatesADefaultOnce replaces
// TestExplainStatesAnEnvironmentDependentDefaultAsBoth, which asserted that
// `explain` printed "default: 10, or 100 in production". M9 withdrew PLAN.md
// §13, so a default no longer varies and there is one number to print.
//
// The absence assertion is the load-bearing half: if a classifier survived
// anywhere, this output is where it would surface first, because `explain`
// reads the schema directly rather than through a compile.
func TestExplainStatesADefaultOnce(t *testing.T) {
	out, _ := explainOut(t, "explain", "test.database")
	if !strings.Contains(out, "default: 10") {
		t.Errorf("the default must be stated:\n%s", out)
	}
	if strings.Contains(out, "in production") || strings.Contains(out, "100") {
		t.Errorf("`explain` still renders an environment-varying default:\n%s", out)
	}
}

// TestExplainSensitiveAndForceNewAreMarked. Both change what a user may safely
// do with an attribute, and neither is visible from its name or kind.
func TestExplainMarksSensitiveAndForceNew(t *testing.T) {
	out, _ := explainOut(t, "explain", "test.database")
	if !strings.Contains(out, "sensitive") {
		t.Errorf("a sensitive attribute must be marked:\n%s", out)
	}
	if !strings.Contains(out, "replaces on change") {
		t.Errorf("a ForceNew attribute must be marked — changing it destroys the resource:\n%s", out)
	}
}

// TestExplainUnknownTypeListsTheKnownOnes. An error that only says no is an
// error a user cannot act on, and the registry already sorts.
func TestExplainUnknownTypeListsTheKnownOnes(t *testing.T) {
	out, err := explainOut(t, "explain", "aws.rds")
	if err == nil {
		t.Fatal("an unknown resource type must be an error")
	}
	msg := err.Error() + out
	if !strings.Contains(msg, "test.database") {
		t.Errorf("the error must list the types that DO exist:\n%s", msg)
	}
}

// section returns the lines under a heading, up to the next blank line.
func sectionOf(t *testing.T, out, heading string) string {
	t.Helper()
	i := strings.Index(out, heading)
	if i < 0 {
		t.Fatalf("no %q section in:\n%s", heading, out)
	}
	rest := out[i+len(heading):]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}
