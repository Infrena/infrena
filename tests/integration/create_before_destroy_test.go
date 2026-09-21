package integration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// create_before_destroy reverses the two halves of a replacement, so there is
// no moment when the resource does not exist.

const cbdProject = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: CIDR
    lifecycle:
      create_before_destroy: true
`

func TestCreateBeforeDestroyLeavesTheNewObjectInState(t *testing.T) {
	dir := project(t, strings.Replace(cbdProject, "CIDR", "10.0.0.0/16", 1))
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}
	before := providerID(t, dir, "net")

	writeIn(t, dir, "infrena.yml", strings.Replace(cbdProject, "CIDR", "10.9.0.0/16", 1))
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("replace: %d\n%s", a.ExitCode, a.combined())
	}

	after := providerID(t, dir, "net")
	if after == before {
		t.Errorf("provider id is unchanged (%q) — nothing was replaced", after)
	}
	// Nothing outstanding: the old object was deleted, so its record is gone.
	if d := deposedCount(t, dir, "net"); d != 0 {
		t.Errorf("state holds %d deposed objects after a clean replacement, want 0", d)
	}
}

// TestADependentFollowsTheNewObjectBeforeTheOldOneGoes is the edge that makes
// the flag mean something rather than merely reorder two calls.
//
// Everything pointing at the resource has to be moved across while the old
// object is still there. Without that edge the old one could be torn down the
// instant the new one existed, with every dependent still referring to it —
// the outage the flag was set to avoid, arriving one step later.
//
// The planner already proposes the dependent's update, so unlike Terraform's
// create_before_destroy this one need not propagate to dependents: the
// dependent is updated in place rather than replaced.
func TestADependentFollowsTheNewObjectBeforeTheOldOneGoes(t *testing.T) {
	const withDependent = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: CIDR
    lifecycle:
      create_before_destroy: true
  db:
    type: fake.database
    engine: postgres
    network: ${net.id}
`
	dir := project(t, strings.Replace(withDependent, "CIDR", "10.0.0.0/16", 1))
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}

	writeIn(t, dir, "infrena.yml", strings.Replace(withDependent, "CIDR", "10.9.0.0/16", 1))
	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan: %d\n%s", r.ExitCode, r.combined())
	}
	// The dependent is UPDATED, not replaced: it follows the new id in place.
	if !strings.Contains(r.combined(), "1 to update") {
		t.Errorf("the dependent is not being updated to follow the new object:\n%s", r.combined())
	}

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	netID := providerID(t, dir, "net")
	if got := attribute(t, dir, "db", "network"); got != netID {
		t.Errorf("db.network = %q, want the NEW network %q — the dependent was left pointing at the destroyed object", got, netID)
	}
}

// TestAnOrdinaryReplacementIsUnchanged. The flag is opt-in, and every resource
// without it must keep destroying first — a great many cannot exist twice.
func TestAnOrdinaryReplacementIsUnchanged(t *testing.T) {
	const plain = `
project: myapp
resources:
  net:
    type: fake.network
    cidr: CIDR
`
	dir := project(t, strings.Replace(plain, "CIDR", "10.0.0.0/16", 1))
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("seeding: %d\n%s", a.ExitCode, a.combined())
	}
	writeIn(t, dir, "infrena.yml", strings.Replace(plain, "CIDR", "10.9.0.0/16", 1))
	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("replace: %d\n%s", a.ExitCode, a.combined())
	}
	if d := deposedCount(t, dir, "net"); d != 0 {
		t.Errorf("an ordinary replacement deposited %d objects; it must not deposit any", d)
	}
}

func stateOf(t *testing.T, dir string) map[string]struct {
	ProviderID string `json:"provider_id"`
	Attributes map[string]struct {
		Raw any `json:"raw"`
	} `json:"attributes"`
	Deposed []json.RawMessage `json:"deposed"`
} {
	t.Helper()
	var st struct {
		Resources map[string]struct {
			ProviderID string `json:"provider_id"`
			Attributes map[string]struct {
				Raw any `json:"raw"`
			} `json:"attributes"`
			Deposed []json.RawMessage `json:"deposed"`
		} `json:"resources"`
	}
	raw := readFileString(t, filepath.Join(dir, ".infrena", "state", "dev.json"))
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("decoding state: %v\n%s", err, raw)
	}
	return st.Resources
}

func providerID(t *testing.T, dir, name string) string {
	t.Helper()
	return stateOf(t, dir)[name].ProviderID
}

func deposedCount(t *testing.T, dir, name string) int {
	t.Helper()
	return len(stateOf(t, dir)[name].Deposed)
}

func attribute(t *testing.T, dir, name, attr string) string {
	t.Helper()
	s, _ := stateOf(t, dir)[name].Attributes[attr].Raw.(string)
	return s
}
