package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/report"
)

// planLines returns the "applying" lines a report holds, in the order they
// were written, and the index of the first "event" line — or len(lines) when
// there is none.
//
// Order is half of what these tests assert, so this deliberately reads
// positions rather than filtering the file down to the lines it cares about:
// a plan line that arrived after the executor had already started work would
// be a plan of a run that was underway, and a filter would have hidden it.
func planLines(t *testing.T, path string) ([]report.PlanChanges, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	var plans []report.PlanChanges
	firstEvent := -1
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, line := range lines {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
		}
		switch probe.Type {
		case "applying":
			var pc report.PlanChanges
			if err := json.Unmarshal([]byte(line), &pc); err != nil {
				t.Fatalf("line %d does not decode as a plan line: %v\n%s", i, err, line)
			}
			if firstEvent >= 0 {
				t.Errorf("a plan line arrived after the executor had started work:\n%s", line)
			}
			plans = append(plans, pc)
		case "event":
			if firstEvent < 0 {
				firstEvent = i
			}
		}
	}
	if firstEvent < 0 {
		firstEvent = len(lines)
	}
	return plans, firstEvent
}

func stages(plans []report.PlanChanges) []string {
	out := make([]string, len(plans))
	for i, p := range plans {
		out[i] = p.Stage
	}
	return out
}

// The shape a frontend reads: what the run intends, then what it is about to
// execute, then the work itself.
func TestApplyReportsTheProposedThenTheExecutingPlan(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve")

	plans, _ := planLines(t, out)
	if got := stages(plans); len(got) != 2 || got[0] != report.StageProposed || got[1] != report.StageExecuting {
		t.Fatalf("stages = %v, want [proposed executing]", got)
	}
	if plans[0].Environment != "dev" {
		t.Errorf("environment = %q, want dev", plans[0].Environment)
	}
	if len(plans[1].Changes) != 1 || plans[1].Changes[0].Address != "network" {
		t.Fatalf("executing plan does not name the resource being created: %+v", plans[1].Changes)
	}
	if kind := plans[1].Changes[0].Kind; kind != "create" {
		t.Errorf("kind = %q, want create — the word, not a number", kind)
	}
}

// A run that has nothing to do still says what it looked at. The executing
// line is the claim that the executor received a plan, so a run that never
// reached the executor must not carry one.
func TestApplyWithNothingToDoReportsOnlyTheProposedPlan(t *testing.T) {
	dir := newProjectFixture(t)
	if _, _, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code == ExitError {
		t.Fatal("the first apply failed")
	}
	out := filepath.Join(t.TempDir(), "run.ndjson")

	runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve")

	plans, _ := planLines(t, out)
	if got := stages(plans); len(got) != 1 || got[0] != report.StageProposed {
		t.Fatalf("stages = %v, want [proposed] — nothing executed", got)
	}
	if len(plans[0].Changes) != 0 {
		t.Errorf("proposed plan carries changes on a no-change run: %+v", plans[0].Changes)
	}
}

// `apply --plan` computes no plan at all — it reads one. It reports the same
// two stages anyway, because a frontend must not have to know which route an
// apply took to find out what it did.
func TestApplySavedPlanReportsBothStages(t *testing.T) {
	dir := newProjectFixture(t)
	planPath := filepath.Join(t.TempDir(), "plan.ndjson")
	if _, _, code := runCommand(t, dir, "plan", "dev", "--output", planPath); code == ExitError {
		t.Fatal("plan --output failed")
	}
	out := filepath.Join(t.TempDir(), "run.ndjson")

	runCommand(t, dir, "apply", "dev", "--plan", planPath, "--output", out, "--auto-approve")

	plans, _ := planLines(t, out)
	if got := stages(plans); len(got) != 2 || got[0] != report.StageProposed || got[1] != report.StageExecuting {
		t.Fatalf("stages = %v, want [proposed executing]", got)
	}
}

func TestDestroyReportsTheProposedThenTheExecutingPlan(t *testing.T) {
	dir := newProjectFixture(t)
	if _, _, code := runCommand(t, dir, "apply", "dev", "--auto-approve"); code == ExitError {
		t.Fatal("the apply that sets destroy up failed")
	}
	out := filepath.Join(t.TempDir(), "run.ndjson")

	runCommandWithStdin(t, dir, "dev\n", "destroy", "dev", "--output", out, "--auto-approve")

	plans, _ := planLines(t, out)
	if got := stages(plans); len(got) != 2 || got[0] != report.StageProposed || got[1] != report.StageExecuting {
		t.Fatalf("stages = %v, want [proposed executing]", got)
	}
	if len(plans[1].Changes) != 1 || plans[1].Changes[0].Kind != "destroy" {
		t.Fatalf("executing plan does not describe a destroy: %+v", plans[1].Changes)
	}
}

// The reason this line is not the `plan` line: an apply report goes to a
// wider audience than a plan artifact, so the values in it are redacted.
func TestApplyPlanLineRedactsASensitiveAttribute(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    password: hunter2
    network: ${network.id}
`)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve")

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("the password reached the report in cleartext:\n%s", data)
	}
	plans, _ := planLines(t, out)
	if len(plans) == 0 {
		t.Fatal("no plan line")
	}
	found := false
	for _, c := range plans[0].Changes {
		for _, ac := range c.Changes {
			if ac.Attribute == "password" {
				found = true
				if ac.After != "<sensitive>" {
					t.Errorf("password after = %q, want the redacted marker", ac.After)
				}
			}
		}
	}
	if !found {
		t.Error("the plan line does not mention the password attribute at all")
	}
}

// An apply report now contains a plan-shaped line, and `apply --plan` must
// still refuse it. This is the guard the type name buys: readSavedPlan
// applies what it finds on a line typed "plan", and a redacted plan applied
// as if its values were real would write "<sensitive>" into infrastructure.
func TestApplyRefusesItsOwnReportAsASavedPlan(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")
	runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve")

	_, err := readSavedPlan(out)
	if err == nil {
		t.Fatal("readSavedPlan accepted an apply report")
	}
	if !strings.Contains(err.Error(), "apply") {
		t.Errorf("the refusal does not name what the file actually is: %v", err)
	}
}

// An attribute the provider has not computed yet is a PROMISE in a plan, and
// the words have to match the ones the terminal shows for the same plan —
// see report.FormatPlanned. A UI put beside a terminal is the whole reason
// this line exists.
func TestApplyPlanLineSaysKnownAfterApply(t *testing.T) {
	dir := newProjectFixture(t)
	out := filepath.Join(t.TempDir(), "run.ndjson")

	runCommand(t, dir, "apply", "dev", "--output", out, "--auto-approve")

	plans, _ := planLines(t, out)
	if len(plans) == 0 || len(plans[0].Changes) == 0 {
		t.Fatal("no plan line to inspect")
	}
	for _, ac := range plans[0].Changes[0].Changes {
		if ac.Attribute != "id" {
			continue
		}
		if ac.After != "(known after apply)" {
			t.Errorf("id after = %q, want the plan's wording for an unknown value", ac.After)
		}
		return
	}
	t.Error("the computed attribute is missing from the plan line")
}
