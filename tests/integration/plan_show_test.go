package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanShowReadsASavedPlanWithNothingInstalled.
//
// A saved plan is how CI applies to a protected environment, and that only works
// if somebody reads the plan first. Without `--show`, the only ways to read one
// are `apply --plan` answered with "no" — asking the reviewer the protection
// depends on to invoke the APPLY command in order to decide whether to apply —
// or NDJSON by hand.
//
// The directory here holds the artifact and nothing else: no infrena.yml, no
// state, no plugins. That is the case that matters, because the reviewer is by
// definition somewhere other than where the plan was made. A rendering that
// needed the project would put the review behind the same setup the pipeline has.
func TestPlanShowReadsASavedPlanWithNothingInstalled(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	artifact := filepath.Join(dir, "plan.json")
	if p := run(t, dir, "plan", "dev", "--output", artifact); p.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", p.ExitCode, p.combined())
	}

	// A bare directory: the artifact, and nothing else at all.
	reviewer := t.TempDir()
	data, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}
	copied := filepath.Join(reviewer, "plan.json")
	if err := os.WriteFile(copied, data, 0o600); err != nil {
		t.Fatalf("copying the artifact: %v", err)
	}

	r := run(t, reviewer, "plan", "--show", copied)
	// Exit 2 is `plan`'s "there are changes", so a reviewer's eye and a
	// pipeline's $? get the same answer and nothing learns a third convention.
	if r.ExitCode != 2 {
		t.Fatalf("plan --show exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	for _, want := range []string{"net", "10.0.0.0/16", "Plan:"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the rendered plan never mentions %q:\n%s", want, r.combined())
		}
	}
}

// TestPlanShowRefusesAnEnvironmentArgument. A saved plan already names the
// environment it was made for; accepting a second one invites the two to
// disagree, and the disagreement would be silent.
func TestPlanShowRefusesAnEnvironmentArgument(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	artifact := filepath.Join(dir, "plan.json")
	run(t, dir, "plan", "dev", "--output", artifact)

	r := run(t, dir, "plan", "dev", "--show", artifact)
	if r.ExitCode == 0 || r.ExitCode == 2 {
		t.Fatalf("plan --show accepted an environment as well, exit = %d\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "takes no environment") {
		t.Errorf("the refusal does not explain itself:\n%s", r.combined())
	}
}

// TestPlanStillNeedsAnEnvironmentWithoutShow. The exemption is keyed on the
// flag, so `plan` on its own must keep demanding what it always did.
func TestPlanStillNeedsAnEnvironmentWithoutShow(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	r := run(t, dir, "plan")
	if r.ExitCode == 0 || r.ExitCode == 2 {
		t.Fatalf("plan with no environment succeeded, exit = %d\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "environment") {
		t.Errorf("the refusal does not say what is missing:\n%s", r.combined())
	}
}
