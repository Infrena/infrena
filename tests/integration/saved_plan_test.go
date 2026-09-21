package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedPlanIndependent has NO resource referring to another, which is what a saved plan
// can currently carry: an attribute referring to a resource created in the same plan is
// unknown at plan time and its expression does not survive the artifact
// (planner.UnappliableFromFile).
const savedPlanIndependent = `
project: sp
environments:
  dev: {}
  prod: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  other:
    type: fake.network
    cidr: 10.9.0.0/16
`

const savedPlanProject = `
project: sp
environments:
  dev: {}
  prod: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
`

// TestASavedPlanAppliesAndRecordsWhatTheNormalPathWould.
//
// The assertions are about what ends up in STATE, not about the apply's output, because
// the hazard of a second apply path is that it silently skips something the first one
// does. Two such skips are pinned here: the project name, which nothing else on this
// path sets, so state would record ""; and the dependency edges, which the artifact has
// to carry, because state's Dependencies is the only record of what a resource depended
// on once it leaves configuration, and that is what orders a LATER destroy.
func TestASavedPlanAppliesAndRecordsWhatTheNormalPathWould(t *testing.T) {
	dir := project(t, savedPlanIndependent)
	artifact := filepath.Join(dir, "p.json")

	if r := run(t, dir, "plan", "dev", "--output", artifact); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", r.ExitCode, r.combined())
	}
	if r := run(t, dir, "apply", "dev", "--plan", artifact, "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("applying a saved plan exit = %d:\n%s", r.ExitCode, r.combined())
	}

	st := readState(t, dir, "dev")
	if st.Project != "sp" {
		t.Errorf("state records project %q, want \"sp\" — nothing else on this path stamps it", st.Project)
	}
	if len(st.Resources) != 2 {
		t.Fatalf("state holds %d resources, want 2", len(st.Resources))
	}

	// And it converged: applying the saved plan left the project in the state a normal
	// apply would have. A re-plan with no changes is the strongest single check of that.
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Errorf("re-plan after applying a saved plan exit = %d, want 0:\n%s",
			again.ExitCode, again.combined())
	}
}

// TestASavedPlanIsRefusedOnceTheStateHasMoved.
//
// The guarantee the whole feature rests on, and the reason there is no --force. The
// normal apply path re-plans INSIDE the lock so it never executes against state gathered
// before the lock was held; a saved plan cannot be re-planned without defeating itself,
// so it is refused instead. Applying the same artifact twice is the cheapest way to move
// the state out from under it, and is also a real mistake — a CI job that retries.
func TestASavedPlanIsRefusedOnceTheStateHasMoved(t *testing.T) {
	dir := project(t, savedPlanIndependent)
	artifact := filepath.Join(dir, "p.json")

	if r := run(t, dir, "plan", "dev", "--output", artifact); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", r.ExitCode, r.combined())
	}
	if r := run(t, dir, "apply", "dev", "--plan", artifact, "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("first apply exit = %d:\n%s", r.ExitCode, r.combined())
	}

	again := run(t, dir, "apply", "dev", "--plan", artifact, "--auto-approve")
	if again.ExitCode == 0 {
		t.Fatalf("re-applying a spent plan must be refused, or the resources are created twice:\n%s",
			again.combined())
	}
	requireContains(t, again.combined(), "the state has changed")
	requireContains(t, again.combined(), "infrena plan dev")

	// Nothing was created a second time, which is the actual harm being prevented.
	st := readState(t, dir, "dev")
	if len(st.Resources) != 2 {
		t.Errorf("state holds %d resources after the refused re-apply, want 2", len(st.Resources))
	}
}

// TestASavedPlanIsRefusedForAnotherEnvironmentBeforeItIsEvenRendered.
//
// A plan artifact is a file, so it gets copied, attached to tickets and passed around. The
// mistake it makes easy is applying one environment's plan to another — which would create
// dev's resources in production and report success. Refused before the plan is rendered,
// because identity cannot change under the caller and making a reader study a plan that
// was never going to run here is its own small harm.
func TestASavedPlanIsRefusedForAnotherEnvironmentBeforeItIsEvenRendered(t *testing.T) {
	dir := project(t, savedPlanIndependent)
	artifact := filepath.Join(dir, "p.json")

	if r := run(t, dir, "plan", "dev", "--output", artifact); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", r.ExitCode, r.combined())
	}

	wrong := run(t, dir, "apply", "prod", "--plan", artifact, "--auto-approve")
	if wrong.ExitCode == 0 {
		t.Fatalf("dev's plan must not apply to prod:\n%s", wrong.combined())
	}
	requireContains(t, wrong.combined(), `environment "dev"`)
	// Rendered NOTHING: no operation lines, so the reader is not asked to read a plan
	// that was refused.
	if strings.Contains(wrong.Stdout, "fake.network") {
		t.Errorf("the plan was rendered before being refused:\n%s", wrong.Stdout)
	}
	// And prod is untouched.
	if _, err := os.Stat(filepath.Join(dir, ".infrena", "state", "prod.json")); err == nil {
		t.Error("prod gained a state file from a refused apply")
	}
}

// TestSavedPlanAndVariableFlagsAreRefusedTogether.
//
// The inverse of the reasoning that made `refresh` and `destroy` ACCEPT --var: there, an
// instance's configuration interpolates variables, so the flag could affect the outcome
// and refusing it was wrong. Here nothing is left to interpolate — the artifact is already
// resolved and carries its own provider instances — so accepting the flag would be the
// advertised-and-ignored shape. Same rule, applied in the direction the facts point.
func TestSavedPlanAndVariableFlagsAreRefusedTogether(t *testing.T) {
	dir := project(t, savedPlanIndependent)
	artifact := filepath.Join(dir, "p.json")
	if r := run(t, dir, "plan", "dev", "--output", artifact); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", r.ExitCode, r.combined())
	}

	for _, args := range [][]string{
		{"apply", "dev", "--plan", artifact, "--var", "x=1", "--auto-approve"},
		{"apply", "dev", "--plan", artifact, "--var-file", "vars.yml", "--auto-approve"},
	} {
		got := run(t, dir, args...)
		if got.ExitCode == 0 {
			t.Errorf("infrena %v was accepted:\n%s", args, got.combined())
		}
		requireContains(t, got.combined(), "does not combine with --var")
	}
}

// TestAnUnreadablePlanArtifactIsRefusedByFormatRatherThanByField.
//
// A plan from a future build carries operations this one may not understand, so the
// version is probed before the rest is decoded — the same shape state.Decode and
// pluginmanifest.Parse use. The message must name the format, not an unknown field.
func TestAnUnreadablePlanArtifactIsRefusedByFormatRatherThanByField(t *testing.T) {
	dir := project(t, savedPlanProject)
	artifact := filepath.Join(dir, "future.json")
	writeFile(t, artifact, `{"version":99,"project":"sp","environment":"dev","operations":[]}`)

	got := run(t, dir, "apply", "dev", "--plan", artifact, "--auto-approve")
	if got.ExitCode == 0 {
		t.Fatalf("a plan from another format version must be refused:\n%s", got.combined())
	}
	requireContains(t, got.combined(), "version 99")
}

// stateFile is the part of a state file these tests read. Declared here rather than
// imported from internal/state, because this suite deliberately imports no infrena
// package — it shells out to the built binary, which is what makes it an integration
// suite.
type stateFile struct {
	Project   string `json:"project"`
	Resources map[string]struct {
		Dependencies []struct {
			Name string `json:"name"`
		} `json:"dependencies"`
	} `json:"resources"`
}

func readState(t *testing.T, dir, environment string) stateFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", environment+".json"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return st
}

// TestASavedPlanResolvesAReferenceToAResourceItAlsoCreates.
//
// This is why pkg/value serialises expressions. `network: ${net.id}` is unknown at plan
// time and carries the expression the executor evaluates once net exists. Leave `Expr`
// out of the artifact and the saved plan carries the unknown without the expression,
// which is indistinguishable from a computed attribute, so the executor drops it: both
// resources created, success reported, `db.network` ABSENT, and the next plan proposing
// an update — convergence broken by a command that said it had succeeded.
//
// It asserts the outcome rather than the mechanism: the referencing attribute holds the
// created resource's real ID, state records the dependency edge, and a re-plan is clean.
func TestASavedPlanResolvesAReferenceToAResourceItAlsoCreates(t *testing.T) {
	dir := project(t, savedPlanProject) // db: network: ${net.id}
	artifact := filepath.Join(dir, "p.json")

	if r := run(t, dir, "plan", "dev", "--output", artifact); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d:\n%s", r.ExitCode, r.combined())
	}
	// The expression must be IN the artifact, or what follows only proves the executor
	// works. Read through readPlanArtifactBytes: --output writes a report stream, so
	// grepping the whole file would also match text outside the artifact.
	saved := readPlanArtifactBytes(t, artifact)
	if !strings.Contains(string(saved), `"op":"resource_ref"`) {
		t.Fatalf("the artifact carries no expression, so the apply below cannot be resolving one:\n%s", saved)
	}

	applied := run(t, dir, "apply", "dev", "--plan", artifact, "--auto-approve")
	if applied.ExitCode != 2 {
		t.Fatalf("applying it exit = %d:\n%s", applied.ExitCode, applied.combined())
	}
	// Resolved to the real ID, not left unset: this is the exact line that used to be
	// missing.
	requireContains(t, applied.Stdout, `network: "net-1"`)

	st := readState(t, dir, "dev")
	db, ok := st.Resources["db"]
	if !ok {
		t.Fatal("db is not in state")
	}
	if len(db.Dependencies) != 1 || db.Dependencies[0].Name != "net" {
		t.Errorf("db records dependencies %v, want [net]", db.Dependencies)
	}

	// Convergence, which is what the silent failure broke.
	if again := run(t, dir, "plan", "dev"); again.ExitCode != 0 {
		t.Errorf("does not converge after applying a saved plan; re-plan exit = %d:\n%s",
			again.ExitCode, again.combined())
	}
}
