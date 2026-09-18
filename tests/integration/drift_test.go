package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefreshSaysWhatDrifted.
//
// `refresh` is the on-demand drift detection command, and it used to print
// "refreshed" for every resource whether reality matched state or not — so the
// command whose whole job is finding drift could not tell you it had found any.
// The information existed and reached `--output`; it was discarded on the way
// to the terminal.
func TestRefreshSaysWhatDrifted(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding apply: %d\n%s", a.ExitCode, a.combined())
	}
	driftCloud(t, dir, "cidr", "10.99.0.0/16")

	r := run(t, dir, "refresh", "dev")
	if r.ExitCode != 0 {
		t.Fatalf("refresh exit = %d\n%s", r.ExitCode, r.combined())
	}
	out := r.combined()
	for _, want := range []string{"cidr", "10.0.0.0/16", "10.99.0.0/16"} {
		if !strings.Contains(out, want) {
			t.Errorf("refresh never mentions %q, so it does not say what drifted:\n%s", want, out)
		}
	}
}

// TestRefreshSaysWhenNothingDrifted is the other half, and the pair is what
// makes either useful: a command that shouted DRIFTED at everything would pass
// the test above while being exactly as uninformative as one that shouted
// "refreshed".
func TestRefreshSaysWhenNothingDrifted(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding apply: %d\n%s", a.ExitCode, a.combined())
	}

	r := run(t, dir, "refresh", "dev")
	if strings.Contains(r.combined(), "DRIFTED") {
		t.Errorf("refresh reported drift against an untouched cloud:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "no drift") {
		t.Errorf("refresh does not say it checked and found nothing:\n%s", r.combined())
	}
}

// TestDriftedSecretIsRedacted. This added a NEW path by which a value reaches a
// terminal, so it gets the same guarantee every other one has: attributeChanges
// formats through report.Format, the single redaction path. A private diff
// written into the refresh renderer would have been a second place for a secret
// to escape, which is the defect that put one in M2's output.
func TestDriftedSecretIsRedacted(t *testing.T) {
	dir := project(t, `
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
`)
	t.Setenv("DB_PASSWORD", "hunter2")
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding apply: %d\n%s", a.ExitCode, a.combined())
	}
	driftCloud(t, dir, "password", "rotated-behind-our-back")

	r := run(t, dir, "refresh", "dev")
	for _, secret := range []string{"hunter2", "rotated-behind-our-back"} {
		if strings.Contains(r.combined(), secret) {
			t.Errorf("a drifted secret was printed in clear (%q):\n%s", secret, r.combined())
		}
	}
	if !strings.Contains(r.combined(), "<sensitive>") {
		t.Errorf("the drifted secret was not redacted:\n%s", r.combined())
	}
}

// driftCloud edits the fake provider's cloud file directly, which is how this
// suite simulates somebody changing infrastructure outside infrena.
func driftCloud(t *testing.T, dir, attribute, to string) {
	t.Helper()
	path := filepath.Join(dir, ".infra", "fake-cloud.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the cloud file: %v", err)
	}
	var cloud struct {
		Resources map[string]struct {
			Type       string         `json:"type"`
			Address    string         `json:"address,omitempty"`
			Attributes map[string]any `json:"attributes"`
		} `json:"resources"`
		NextID int `json:"next_id"`
	}
	if err := json.Unmarshal(data, &cloud); err != nil {
		t.Fatalf("decoding the cloud file: %v", err)
	}
	found := false
	for id, r := range cloud.Resources {
		if _, ok := r.Attributes[attribute]; !ok {
			continue
		}
		r.Attributes[attribute] = to
		cloud.Resources[id] = r
		found = true
	}
	if !found {
		t.Fatalf("no resource in the cloud file has an %q attribute:\n%s", attribute, data)
	}
	out, err := json.MarshalIndent(cloud, "", "  ")
	if err != nil {
		t.Fatalf("encoding the cloud file: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("writing the cloud file: %v", err)
	}
}
