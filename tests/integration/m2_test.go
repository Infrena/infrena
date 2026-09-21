package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeM2State seeds a state file directly, so a test can set up state without
// running an apply. resources is keyed by address string in the shape
// state.Decode expects: the JSON tags resource.ResourceState carries — lowercase
// snake_case, e.g. "provider_id" — plus value.Value's wire format
// (kind/known/raw/source/sensitive) for each attribute. See stateResource and
// wireAttr below.
//
// Go field names do NOT work here. encoding/json's case-insensitive fallback
// saves spellings that still fold onto the tag ("Address" -> "address"), but
// "ProviderID" does not fold onto "provider_id" — the extra underscore breaks
// it — so the resource decodes with an empty ProviderID, the fake provider's
// Read finds nothing, and every resource reads back as deleted outside infrena.
func writeM2State(t *testing.T, dir, environment string, resources map[string]any) {
	t.Helper()
	stateDir := filepath.Join(dir, ".infrena", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{
		// CURRENT, not 1. A fixture written at an older version is MIGRATED on load,
		// and version 1 → 2 rewrites test.* to fake.*, so a hand-written version-1
		// fixture silently becomes a project whose types no loaded plugin serves.
		"version":     stateVersion,
		"serial":      1,
		"project":     "myapp",
		"environment": environment,
		"resources":   resources,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, environment+".json"), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// stateResource builds one resource entry for writeM2State, using the actual
// wire tags from resource.ResourceState. lifecycle may be nil; pass e.g.
// map[string]any{"prevent_destroy": true} to seed a lifecycle guard on a
// resource that no longer exists in configuration.
func stateResource(name, resourceType, providerID string, attrs map[string]any, lifecycle map[string]any) map[string]any {
	r := map[string]any{
		"address":     map[string]any{"name": name},
		"type":        resourceType,
		"provider":    "fake",
		"provider_id": providerID,
		"attributes":  attrs,
	}
	if lifecycle != nil {
		r["lifecycle"] = lifecycle
	}
	return r
}

// wireAttr builds one attribute in value.Value's wire format. kind must be
// one of the frozen wire names in pkg/value/json.go's kindWireNames —
// "string", "integer", "float", "boolean", "list", "map" — not a Go type
// name: "int" is not a recognised kind and would fail to decode.
func wireAttr(kind string, raw any, sensitive bool) map[string]any {
	a := map[string]any{"kind": kind, "known": true, "raw": raw, "source": "provider"}
	if sensitive {
		a["sensitive"] = true
	}
	return a
}

// writeFakeCloud seeds .infrena/fake-cloud.json directly; the file is
// hand-editable by design, which is how a test simulates drift.
//
// Unlike state's attributes, a CloudResource's attributes are plain JSON values
// rather than value.Value's wire format: the cloud models an external system,
// not internal configuration.
func writeFakeCloud(t *testing.T, dir string, resources map[string]map[string]any) {
	t.Helper()
	cloudDir := filepath.Join(dir, ".infrena")
	if err := os.MkdirAll(cloudDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	doc := map[string]any{"resources": resources}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal fake cloud: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "fake-cloud.json"), data, 0o600); err != nil {
		t.Fatalf("write fake cloud: %v", err)
	}
}

func cloudResource(resourceType string, attrs map[string]any) map[string]any {
	return map[string]any{"type": resourceType, "attributes": attrs}
}

func TestPlanOnFreshProjectProposesCreatesForEverything(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`)
	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2 (changes present)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "+ fake.network.network")
	requireContains(t, res.Stdout, "+ fake.database.database")
}

func TestPlanAgainstMatchingStateReportsNoChanges(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	writeM2State(t, dir, "dev", map[string]any{
		"network": stateResource("network", "fake.network", "net-1", map[string]any{
			"cidr": wireAttr("string", "10.20.0.0/16", false),
			"id":   wireAttr("string", "net-1", false),
		}, nil),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-1": cloudResource("fake.network", map[string]any{
			"cidr": "10.20.0.0/16",
			"id":   "net-1",
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d, want 0 (no changes)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "No changes")
	// "Contains" alone would still pass if cobra usage text or a stray error
	// leaked onto stdout beside the clean plan. A no-changes run must produce
	// ONLY the plan text.
	if strings.Contains(res.Stdout, "Usage:") {
		t.Errorf("stdout leaked cobra usage text:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "Error:") {
		t.Errorf("stdout leaked an error on a clean run:\n%s", res.Stdout)
	}
	if res.Stderr != "" {
		t.Errorf("a clean plan should write nothing to stderr, got:\n%s", res.Stderr)
	}
}

func TestPlanShowsExternalDriftAsAnUpdate(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    size: 50
    network: ${network.id}
`)
	writeM2State(t, dir, "dev", map[string]any{
		"network": stateResource("network", "fake.network", "net-1", map[string]any{
			"cidr": wireAttr("string", "10.20.0.0/16", false),
			"id":   wireAttr("string", "net-1", false),
		}, nil),
		"database": stateResource("database", "fake.database", "db-1", map[string]any{
			"engine": wireAttr("string", "postgres", false),
			"size":   wireAttr("integer", 50, false),
		}, nil),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-1": cloudResource("fake.network", map[string]any{
			"cidr": "10.20.0.0/16",
			"id":   "net-1",
		}),
		// Someone resized the database by hand, outside infrena entirely — the
		// mutation the drift check exists to catch.
		"db-1": cloudResource("fake.database", map[string]any{
			"engine": "postgres",
			"size":   90,
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2 (drift is a change)\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "~ fake.database.database")
	// The specific drifted attribute with its old (cloud, 90) and new
	// (configured, 50) values, so a planner that noticed something changed but
	// named the wrong attribute or the wrong direction fails here.
	requireContains(t, res.Stdout, "size: 90 -> 50")
}

func TestPlanProposesDestroyForResourceRemovedFromConfiguration(t *testing.T) {
	dir := project(t, `
project: myapp
resources: {}
`)
	writeM2State(t, dir, "dev", map[string]any{
		"orphan": stateResource("orphan", "fake.network", "net-99", map[string]any{
			"cidr": wireAttr("string", "10.5.0.0/16", false),
			"id":   wireAttr("string", "net-99", false),
		}, nil),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-99": cloudResource("fake.network", map[string]any{
			"cidr": "10.5.0.0/16",
			"id":   "net-99",
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2\n%s", res.ExitCode, res.combined())
	}
	requireContains(t, res.Stdout, "- fake.network.orphan")
}

func TestPlanErrorsOnPreventDestroyForRemovedResource(t *testing.T) {
	dir := project(t, `
project: myapp
resources: {}
`)
	writeM2State(t, dir, "dev", map[string]any{
		"protected": stateResource("protected", "fake.network", "net-42", map[string]any{
			"cidr": wireAttr("string", "10.6.0.0/16", false),
			"id":   wireAttr("string", "net-42", false),
		}, map[string]any{"prevent_destroy": true}),
	})
	writeFakeCloud(t, dir, map[string]map[string]any{
		"net-42": cloudResource("fake.network", map[string]any{
			"cidr": "10.6.0.0/16",
			"id":   "net-42",
		}),
	})

	res := run(t, dir, "plan", "dev")
	if res.ExitCode != 1 {
		t.Fatalf("exit code %d, want 1 (a plan-time error, not a proposed change)\n%s", res.ExitCode, res.combined())
	}
	// Deliberately not the planner's exact diagnostic wording, which is its to
	// change. What matters: the protected address appears on stderr, and no
	// plan reaches stdout — a plan that is refused must not also be printed.
	requireContains(t, res.Stderr, "protected")
	// Progress lines reach stdout while the refresh runs, so this asserts that
	// no PLAN is rendered rather than that stdout is empty.
	if strings.Contains(res.Stdout, "Plan:") {
		t.Errorf("no plan should be rendered when prevent_destroy blocks it, got:\n%s", res.Stdout)
	}
}

func TestPlanNeverLeaksASecret(t *testing.T) {
	dir := project(t, `
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
	res := run(t, dir, "plan", "dev")
	// combined() is deliberate: the secret must never appear on EITHER stream.
	if strings.Contains(res.combined(), "hunter2") {
		t.Errorf("plan output leaked the secret:\n%s", res.combined())
	}
	requireContains(t, res.Stdout, "<sensitive>")
}

func TestPlanOutputWritesValidJSONMode0600(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	outPath := filepath.Join(dir, "plan.json")
	res := run(t, dir, "plan", "dev", "--output", outPath)
	if res.ExitCode != 2 {
		t.Fatalf("exit code %d, want 2\n%s", res.ExitCode, res.combined())
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("--output did not write a file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("plan file mode = %v, want 0600", info.Mode().Perm())
	}

	var decoded map[string]any
	if err := json.Unmarshal(readPlanArtifactBytes(t, outPath), &decoded); err != nil {
		t.Fatalf("--output file carries no valid plan artifact: %v", err)
	}
}

func TestPlanIsDeterministicAcrossRuns(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    size: 50
    network: ${network.id}
`)
	first := run(t, dir, "plan", "dev")
	second := run(t, dir, "plan", "dev")

	if first.ExitCode != second.ExitCode {
		t.Fatalf("exit codes differ: %d vs %d", first.ExitCode, second.ExitCode)
	}
	if first.Stdout != second.Stdout {
		t.Errorf("Stdout differs between runs — two identical runs must produce identical output:\n--- first ---\n%s\n--- second ---\n%s", first.Stdout, second.Stdout)
	}

	// Cross-check via --output too, ignoring created_at: it records when each
	// plan was produced rather than any property of its inputs.
	out1 := filepath.Join(t.TempDir(), "plan1.json")
	out2 := filepath.Join(t.TempDir(), "plan2.json")
	run(t, dir, "plan", "dev", "--output", out1)
	run(t, dir, "plan", "dev", "--output", out2)

	p1 := readPlanIgnoringCreatedAt(t, out1)
	p2 := readPlanIgnoringCreatedAt(t, out2)
	if p1 != p2 {
		t.Errorf("saved plans differ once created_at is excluded — two identical runs must produce identical output:\n--- first ---\n%s\n--- second ---\n%s", p1, p2)
	}
}

// readPlanIgnoringCreatedAt strips the plan artifact's timestamp field before
// comparing.
//
// The key is the WIRE name, "created_at", not the Go field name: deleting a key
// the map does not have is a silent no-op, which leaves two genuinely different
// timestamps in place and fails the comparison on every run whether determinism
// holds or not.
func readPlanIgnoringCreatedAt(t *testing.T, path string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(readPlanArtifactBytes(t, path), &decoded); err != nil {
		t.Fatalf("%s carries no valid plan artifact: %v", path, err)
	}
	delete(decoded, "created_at")
	stripped, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshaling %s: %v", path, err)
	}
	return string(stripped)
}
