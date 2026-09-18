package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/pkg/report"
)

// Saved plans already exist on disk, so the old bare artifact must keep
// working. Refusing it would break apply --plan for every plan saved before
// this release.
func TestReadSavedPlanAcceptsABareArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	p := &planner.Plan{Version: planner.PlanVersion, Project: "proj", Environment: "dev"}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readSavedPlan(path)
	if err != nil {
		t.Fatalf("readSavedPlan refused a bare artifact: %v", err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}

func TestReadSavedPlanAcceptsAStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	w := report.NewWriter(f)
	if err := w.Meta("plan", "dev", "0.0.0-test", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	artifact, err := json.Marshal(&planner.Plan{Version: planner.PlanVersion, Project: "proj", Environment: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WritePlan(artifact); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := readSavedPlan(path)
	if err != nil {
		t.Fatalf("readSavedPlan refused a stream: %v", err)
	}
	if got.Environment != "dev" {
		t.Errorf("Environment = %q, want dev", got.Environment)
	}
}

// The hazard from spec 2.4, at the level a user meets it: a stream with no
// plan line is an error naming the problem, never an empty plan that applies
// nothing.
func TestReadSavedPlanRefusesAStreamWithNoPlanLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.ndjson")
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	w := report.NewWriter(f)
	_ = w.Meta("apply", "dev", "0.0.0-test", time.Unix(0, 0))
	_ = w.WriteApplyResult(report.ApplyResult{})
	f.Close()

	_, err := readSavedPlan(path)
	if err == nil {
		t.Fatal("readSavedPlan accepted a report with no plan line")
	}
	if !strings.Contains(err.Error(), "no plan") {
		t.Errorf("error does not say what is missing: %v", err)
	}
	// Naming the command the file actually came from is what turns "wrong
	// file" into "you passed the apply report instead of the plan".
	if !strings.Contains(err.Error(), `"apply"`) {
		t.Errorf("error does not name the run the file reports on: %v", err)
	}
}

// Round trip through the real command, which is the only test that proves the
// two halves agree about the format.
func TestPlanOutputRoundTripsThroughApply(t *testing.T) {
	dir := newProjectFixture(t)
	path := filepath.Join(t.TempDir(), "plan.ndjson")

	if _, _, code := runCommand(t, dir, "plan", "dev", "--output", path); code != ExitChanges {
		t.Fatalf("plan exit = %d, want %d", code, ExitChanges)
	}
	if _, _, code := runCommand(t, dir, "apply", "dev", "--plan", path, "--auto-approve"); code != ExitChanges {
		t.Fatalf("apply --plan exit = %d, want %d", code, ExitChanges)
	}
}
