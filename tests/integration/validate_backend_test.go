package integration

import (
	"strings"
	"testing"
)

// TestValidateRefusesABackendThatIsNotInstalled.
//
// validate already refuses a missing PROVIDER plugin, and used to report
// "✓ Configuration valid" for a project whose `backend:` named one that was not
// installed. `plan` then failed on the same project with "no binary for state
// backend". validate is the cheap gate a CI pipeline runs before anything else,
// so that combination is a pipeline approving a project that cannot run.
//
// Resolving a binary is a filesystem lookup, so this stays inside validate's
// promise not to contact providers — see TestValidateDoesNotContactAnInstalledBackend
// for the other half of that, which is the half that could regress silently.
func TestValidateRefusesABackendThatIsNotInstalled(t *testing.T) {
	dir := project(t, `
project: myapp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
backend:
  plugin: s3
  bucket: some-bucket
`)

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1 — the backend is not installed\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	for _, want := range []string{"s3", "not installed", "infrena-backend-s3"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q, so it does not say what to do:\n%s", want, out)
		}
	}
}

// TestValidateRefusesAMigrateFromBackendThatIsNotInstalled. `migrate_from:`
// takes the same shape and the same rules as `backend:`, so it gets the same
// check — a migration whose SOURCE cannot be opened fails just as completely as
// one whose destination cannot.
func TestValidateRefusesAMigrateFromBackendThatIsNotInstalled(t *testing.T) {
	dir := project(t, `
project: myapp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
backend:
  plugin: local
migrate_from:
  plugin: gcs
`)

	r := run(t, dir, "validate", "dev")
	if r.ExitCode != 1 {
		t.Fatalf("validate exit = %d, want 1\n%s", r.ExitCode, r.combined())
	}
	if !strings.Contains(r.combined(), "migrate_from") {
		t.Errorf("the diagnostic does not say which block named the backend:\n%s", r.combined())
	}
}

// TestValidateAcceptsAProjectWithNoBackendBlock, and one that names the
// built-in. Neither starts a process or needs anything installed, and a check
// that refused them would break every project that has never thought about
// where its state lives.
func TestValidateAcceptsAProjectWithNoBackendBlock(t *testing.T) {
	for name, block := range map[string]string{
		"no block":     "",
		"plugin local": "backend:\n  plugin: local\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := project(t, `
project: myapp
environments:
  dev: {}
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`+block)

			r := run(t, dir, "validate", "dev")
			if r.ExitCode != 0 {
				t.Errorf("validate exit = %d, want 0\n%s", r.ExitCode, r.combined())
			}
		})
	}
}
