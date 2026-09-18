package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const envSecretProject = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    password: ${secret.DB_PASSWORD}
`

// TestASecretComesFromTheEnvironmentAndIsNeverPrinted is §36's core half.
//
// The environment is the only source the free CLI reads, and that is what makes
// the open-core line easy to hold: infrena never stores or transports the value.
// Vault, AWS Secrets Manager and 1Password are §60's, on the paid side.
func TestASecretComesFromTheEnvironmentAndIsNeverPrinted(t *testing.T) {
	dir := project(t, envSecretProject)
	t.Setenv("DB_PASSWORD", "hunter2")

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), "hunter2") {
		t.Error("the secret was printed in the plan")
	}
	if !strings.Contains(r.combined(), "<sensitive>") {
		t.Errorf("the secret's attribute is not redacted, so it is not being treated as one:\n%s", r.combined())
	}
}

// TestAnUnsetSecretIsRefusedRatherThanRead. An empty credential does not fail
// at the plan — it fails at the provider, after somebody approved the run.
//
// The EMPTY case is tested beside the missing one because it is the shape a CI
// failure actually takes: a repository secret that was never created expands to
// an empty string rather than disappearing, so treating empty as "set" would
// make the common failure the silent one.
func TestAnUnsetSecretIsRefusedRatherThanRead(t *testing.T) {
	dir := project(t, envSecretProject)

	for name, value := range map[string]string{"missing": "", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			if name == "empty" {
				t.Setenv("DB_PASSWORD", value)
			} else {
				os.Unsetenv("DB_PASSWORD")
			}
			r := run(t, dir, "plan", "dev")
			if r.ExitCode == 2 || r.ExitCode == 0 {
				t.Fatalf("plan succeeded with no secret, exit = %d\n%s", r.ExitCode, r.combined())
			}
			for _, want := range []string{"DB_PASSWORD", "is not set"} {
				if !strings.Contains(r.combined(), want) {
					t.Errorf("the diagnostic never mentions %q:\n%s", want, r.combined())
				}
			}
		})
	}
}

// TestASecretDoesNotReachTheReport. --output is what a CI system archives and
// what a frontend reads, so it is the single most likely place for a secret to
// end up somewhere it outlives the run.
func TestASecretDoesNotReachTheReport(t *testing.T) {
	dir := project(t, envSecretProject)
	t.Setenv("DB_PASSWORD", "hunter2")
	reportPath := filepath.Join(t.TempDir(), "run.json")

	if r := run(t, dir, "apply", "dev", "--auto-approve", "--output", reportPath); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d\n%s", r.ExitCode, r.combined())
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Error("the secret is in the --output report, which is the file a pipeline archives")
	}

	// And `state show` redacts it, which is the other way a person meets it.
	s := run(t, dir, "state", "show", "dev", "db")
	if strings.Contains(s.combined(), "hunter2") {
		t.Errorf("state show printed the secret:\n%s", s.combined())
	}
}

// TestASecretIsSensitiveEvenWhereTheSchemaIsNot.
//
// Schema-declared sensitivity describes the ATTRIBUTE; a secret describes the
// VALUE. `fake.network.cidr` is an ordinary attribute nobody marked, and a
// secret written into it must still be redacted — otherwise redaction depends
// on a provider having anticipated where somebody would put a credential.
func TestASecretIsSensitiveEvenWhereTheSchemaIsNot(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${secret.CIDR_SECRET}
`)
	t.Setenv("CIDR_SECRET", "10.44.0.0/16")

	r := run(t, dir, "plan", "dev")
	if strings.Contains(r.combined(), "10.44.0.0/16") {
		t.Errorf("a secret in an unmarked attribute was printed in clear:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "<sensitive>") {
		t.Errorf("it was not redacted:\n%s", r.combined())
	}
}

// TestSecretComposesIntoAString is the reason for the `${secret.NAME}` spelling
// rather than §36's `{secret: NAME}` mapping sketch: a connection string is the
// common shape, and a mapping cannot express it at all.
func TestSecretComposesIntoAString(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  db:
    type: fake.database
    engine: postgres
    database_url: postgres://app:${secret.DB_PASSWORD}@db.internal/app
`)
	t.Setenv("DB_PASSWORD", "hunter2")

	r := run(t, dir, "plan", "dev")
	if strings.Contains(r.combined(), "hunter2") {
		t.Errorf("the secret leaked through interpolation:\n%s", r.combined())
	}
}
