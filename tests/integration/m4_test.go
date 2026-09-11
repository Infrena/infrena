package integration

import (
	"strings"
	"testing"
)

// TestValidatePlanAndApplyResolveTheSameVariableLayers is Task 11's core
// property: validate, plan and apply must resolve ${cidr} to the same value.
// It asserts behaviour, not structure, because "they share a helper" is not
// the property; "they resolve the same value" is — a test asserting all
// three call compilerOptions would pass even if compilerOptions itself were
// wrong.
func TestValidatePlanAndApplyResolveTheSameVariableLayers(t *testing.T) {
	newDir := func(t *testing.T) string {
		dir := project(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: ${cidr}
`)
		writeIn(t, dir, "overrides.yml", "cidr: 10.7.0.0/16\n")
		return dir
	}

	// With the file, every command resolves ${cidr}.
	for _, cmd := range [][]string{
		{"validate"},
		{"plan", "dev"},
		{"apply", "dev", "--auto-approve"},
	} {
		dir := newDir(t)
		r := run(t, dir, append(append([]string{}, cmd...), "--var-file", "overrides.yml")...)
		if r.ExitCode == 1 {
			t.Errorf("infra %v --var-file overrides.yml failed:\n%s", cmd, r.combined())
		}
		if strings.Contains(r.combined(), "undefined variable") {
			t.Errorf("infra %v did not consult --var-file:\n%s", cmd, r.combined())
		}
	}

	// Without it, every command fails the same way. The negative arm is what
	// makes the positive arm mean something: a command that ignored the
	// reference entirely would pass the first loop.
	for _, cmd := range [][]string{
		{"validate"},
		{"plan", "dev"},
		{"apply", "dev", "--auto-approve"},
	} {
		dir := newDir(t)
		r := run(t, dir, cmd...)
		if r.ExitCode != 1 {
			t.Errorf("infra %v with no value for ${cidr}: exit = %d, want 1\n%s", cmd, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "undefined variable")
	}
}

// TestValidateWithNoArgumentChecksEveryDeclaredEnvironment covers validate's
// optional environment argument. Six environments, not two: with six keys an
// implementation that iterates a Go map without sorting produces the sorted
// order by chance once in 720 runs, so this fails 719 times in 720 against an
// unsorted implementation. Two keys would pass roughly 88% of the time
// against broken code, which is not a test.
func TestValidateWithNoArgumentChecksEveryDeclaredEnvironment(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: test.network
    cidr: ${cidr}
`)
	// Five environments set cidr; the sixth does not, so validate must fail
	// and must name the one that is broken.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		writeIn(t, dir, "environments/"+env+".yml", "cidr: 10.0.0.0/16\n")
	}
	writeIn(t, dir, "environments/foxtrot.yml", "unrelated: 1\n")

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — an environment with no value for ${cidr} is invalid\n%s",
			r.ExitCode, r.combined())
	}
	requireContains(t, r.combined(), "foxtrot")
	// Absence as well as presence: the five good environments must not be
	// blamed for the sixth's problem.
	for _, env := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		if strings.Contains(r.combined(), "environment \""+env+"\"") {
			t.Errorf("validate blames %s for foxtrot's missing variable:\n%s", env, r.combined())
		}
	}

	// Naming an environment validates only that one.
	if g := run(t, dir, "validate", "alpha"); g.ExitCode != 0 {
		t.Errorf("validate alpha exit = %d, want 0\n%s", g.ExitCode, g.combined())
	}
	if b := run(t, dir, "validate", "foxtrot"); b.ExitCode != 1 {
		t.Errorf("validate foxtrot exit = %d, want 1\n%s", b.ExitCode, b.combined())
	}
}

// TestValidateReportsAnEnvironmentIndependentErrorOnce covers
// foldByEnvironment's other arm: a diagnostic identical across every
// validated environment (an unknown resource type does not become known in
// staging) is reported once, untagged — not once per environment.
func TestValidateReportsAnEnvironmentIndependentErrorOnce(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: test.nosuchtype
    cidr: 10.0.0.0/16
`)
	for _, env := range []string{"alpha", "bravo", "charlie"} {
		writeIn(t, dir, "environments/"+env+".yml", "unused: 1\n")
	}

	r := run(t, dir, "validate")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	if n := strings.Count(r.combined(), "test.nosuchtype"); n != 1 {
		t.Errorf("an unknown resource type is environment-independent and must be reported once, got %d:\n%s",
			n, r.combined())
	}
}

// TestDestroyAndRefreshRefuseVariableFlags is the CLI-level counterpart to
// internal/cli's TestDestroyRejectsVarFlag/TestRefreshRejectsVarFlag: it
// drives the real built binary, so it also proves the refusal happens before
// the environment is touched at all.
func TestDestroyAndRefreshRefuseVariableFlags(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d\n%s", r.ExitCode, r.combined())
	}

	for _, tc := range []struct {
		args []string
	}{
		{[]string{"destroy", "dev", "--auto-approve", "--var", "cidr=10.1.0.0/16"}},
		{[]string{"destroy", "dev", "--auto-approve", "--var-file", "variables.yml"}},
		{[]string{"refresh", "dev", "--var", "cidr=10.1.0.0/16"}},
		{[]string{"refresh", "dev", "--var-file", "variables.yml"}},
	} {
		r := run(t, dir, tc.args...)
		if r.ExitCode != 1 {
			t.Errorf("infra %v exit = %d, want 1 — a flag that cannot affect the outcome must be "+
				"refused, not ignored\n%s", tc.args, r.ExitCode, r.combined())
		}
		requireContains(t, r.combined(), "does not take --var")
	}

	// And the environment still exists: a refused command must not have run.
	if r := run(t, dir, "plan", "dev"); r.ExitCode != 0 {
		t.Errorf("plan after the refused commands exit = %d, want 0 (nothing should have changed)\n%s",
			r.ExitCode, r.combined())
	}
}
