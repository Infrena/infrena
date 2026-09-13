package cli

import (
	"bytes"
	"strings"
	"testing"
)

// PLAN.md §31.1's `plugins:` block, through a command — which is the WIRING, and it was
// the gap: the loader's own tests construct a Loader directly, so buildRegistry could
// stop passing the constraints along and nothing would notice.

func validateOutput(t *testing.T, body string) (string, error) {
	t.Helper()
	dir := projectDir(t, body)
	opts := &GlobalOptions{Dir: dir}
	cmd := newValidateCommand(opts)
	cmd.SetArgs(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

const constrainedProject = `
project: p
plugins:
  fake: "%s"
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`

// TestAConstraintReachesTheLoader. The in-process fake double reports no version, so any
// constraint above 0.0.0 refuses it — which makes it the cheapest possible probe that the
// constraint travelled from configuration to the thing that loads plugins.
func TestAConstraintReachesTheLoader(t *testing.T) {
	out, err := validateOutput(t, strings.Replace(constrainedProject, "%s", ">= 0.3.0", 1))
	if err == nil {
		t.Fatalf("a constraint the plugin cannot satisfy must fail validate:\n%s", out)
	}
	for _, want := range []string{">= 0.3.0", "does not report a version"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not mention %q:\n%s", want, out)
		}
	}
	// The summary must not claim the plugin is MISSING — it is sitting right there,
	// and "install the plugin" is the wrong thing to tell someone whose problem is a
	// version. That message was wrong when first written.
	if strings.Contains(out, "is not available") {
		t.Errorf("a version failure is reported as a missing plugin:\n%s", out)
	}
}

// TestAnUnconstrainedProjectIsUnaffected — `plugins:` is optional, and every project
// written before it existed must behave exactly as it did.
func TestAnUnconstrainedProjectIsUnaffected(t *testing.T) {
	out, err := validateOutput(t, `
project: p
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if err != nil {
		t.Fatalf("a project stating no constraint must validate:\n%s", out)
	}
}

// TestASatisfiableConstraintValidates. Without this, the test above passes against a
// build that refuses every constrained project — so it is the half that says the
// constraint is CHECKED rather than merely fatal.
func TestASatisfiableConstraintValidates(t *testing.T) {
	// The builtin reports 0.0.0, so a constraint it meets is one that allows it.
	out, err := validateOutput(t, strings.Replace(constrainedProject, "%s", ">= 0.0.0", 1))
	if err != nil {
		t.Fatalf("0.0.0 satisfies >= 0.0.0, so this must validate:\n%s", out)
	}
}
