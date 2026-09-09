package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodProject = `
project: myapp

resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16

  database:
    type: test.database
    engine: postgres
    size: 50
`

func TestValidateAcceptsGoodProject(t *testing.T) {
	dir := project(t, goodProject)
	res := run(t, dir, "validate")
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "Configuration valid")
}

func TestValidateRejectsUnknownTypeWithUsefulMessage(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	res := run(t, dir, "validate")
	if res.ExitCode == 0 {
		t.Fatal("expected a non-zero exit code for invalid configuration")
	}
	requireContains(t, res.Stderr, "aws.rds")
	requireContains(t, res.Stderr, "test.database")
	requireContains(t, res.Stderr, "Suggested action:")
}

func TestValidateFailsWithoutProjectFile(t *testing.T) {
	res := run(t, t.TempDir(), "validate")
	if res.ExitCode == 0 {
		t.Fatal("expected failure when infra.yml is absent")
	}
	requireContains(t, res.combined(), "infra.yml")
}

func TestStateListOnFreshProject(t *testing.T) {
	dir := project(t, goodProject)
	res := run(t, dir, "state", "list", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "No resources are managed")
}

// writeState seeds a state file directly, standing in for the apply that M3
// will provide.
func writeState(t *testing.T, dir, environment string) {
	t.Helper()
	stateDir := filepath.Join(dir, ".infra", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{
		"version":     1,
		"serial":      1,
		"project":     "myapp",
		"environment": environment,
		"resources": map[string]any{
			"database": map[string]any{
				"Address":    map[string]any{"Name": "database"},
				"Type":       "test.database",
				"Provider":   "test",
				"ProviderID": "db-1",
				"Attributes": map[string]any{
					"engine":   map[string]any{"kind": "string", "known": true, "raw": "postgres", "source": "provider"},
					"password": map[string]any{"kind": "string", "known": true, "raw": "hunter2", "source": "provider", "sensitive": true},
				},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, environment+".json"), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestStateListAndShow(t *testing.T) {
	dir := project(t, goodProject)
	writeState(t, dir, "dev")

	list := run(t, dir, "state", "list", "dev")
	requireContains(t, list.Stdout, "test.database.database")

	show := run(t, dir, "state", "show", "dev", "database")
	if show.ExitCode != 0 {
		t.Fatalf("state show exit %d\n%s", show.ExitCode, show.combined())
	}
	requireContains(t, show.Stdout, "db-1")
	requireContains(t, show.Stdout, "postgres")
}

func TestStateShowRedactsSensitiveValues(t *testing.T) {
	dir := project(t, goodProject)
	writeState(t, dir, "dev")

	show := run(t, dir, "state", "show", "dev", "database")
	requireContains(t, show.Stdout, "<sensitive>")
	if strings.Contains(show.combined(), "hunter2") {
		t.Error("a sensitive value was printed in clear text")
	}
}

func TestStateUnlockReportsHolderAndReleases(t *testing.T) {
	dir := project(t, goodProject)
	stateDir := filepath.Join(dir, ".infra", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lock := `{"environment":"dev","pid":4242,"host":"buildbox","user":"someone","operation":"apply","at":"2026-09-09T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(stateDir, "dev.lock"), []byte(lock), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	res := run(t, dir, "state", "unlock", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("unlock exit %d\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "buildbox")
	requireContains(t, res.Stdout, "4242")

	if _, err := os.Stat(filepath.Join(stateDir, "dev.lock")); !os.IsNotExist(err) {
		t.Error("the lock file still exists after unlock")
	}
}

func TestStateUnlockOnUnlockedEnvironment(t *testing.T) {
	dir := project(t, goodProject)
	res := run(t, dir, "state", "unlock", "dev")
	if res.ExitCode == 0 {
		t.Error("unlocking an environment that is not locked must fail clearly")
	}
	requireContains(t, res.combined(), "not locked")
}
