package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

// writeIn writes one file into an existing project directory. `project` only
// writes infrena.yml, and fixtures need variables.yml and environments/*.yml too.
func writeIn(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// planArtifact is the part of a saved plan these tests read. It is a local
// struct rather than planner.Plan because Plan has no UnmarshalJSON, but the
// map values are real value.Value, so decoding exercises the production codec
// on bytes the production binary wrote.
type planArtifact struct {
	ConfigHash string `json:"config_hash"`
	Operations []struct {
		Address string                 `json:"address"`
		Before  map[string]value.Value `json:"before"`
		After   map[string]value.Value `json:"after"`
	} `json:"operations"`
}

func readPlanArtifact(t *testing.T, path string) planArtifact {
	t.Helper()
	var p planArtifact
	if err := json.Unmarshal(readPlanArtifactBytes(t, path), &p); err != nil {
		t.Fatalf("decoding plan artifact: %v", err)
	}
	return p
}

func opAfter(t *testing.T, p planArtifact, address, attr string) value.Value {
	t.Helper()
	for _, op := range p.Operations {
		if op.Address == address {
			v, ok := op.After[attr]
			if !ok {
				t.Fatalf("operation %s has no attribute %q", address, attr)
			}
			return v
		}
	}
	t.Fatalf("no operation for %s in the plan artifact", address)
	return value.Value{}
}

// varProject writes a project whose one attribute is fed by a variable. cidr is
// a plain string attribute, so no typed variable declaration is needed.
func varProject(t *testing.T, variablesYML string) string {
	t.Helper()
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${var.cidr}
`)
	writeIn(t, dir, "variables.yml", variablesYML)
	return dir
}

// TestAVarMatchingTheFileValueIsNotAChange pins that value.Equal ignores BOTH
// Scope and SuppliedBy.
//
// A --var that repeats what variables.yml already said changes those two fields
// and nothing else: the desired value carries Scope=ScopeCLIOverride,
// SuppliedBy="--var", while the value state records carries Scope=ScopeUnset,
// SuppliedBy="". If Equal compared either, every plan would propose a change
// forever. cidr is ForceNew, so the failure is loud — a proposed REPLACEMENT of
// a resource nobody asked to change, not a quiet extra line in a diff.
func TestAVarMatchingTheFileValueIsNotAChange(t *testing.T) {
	dir := varProject(t, "cidr: 10.0.0.0/16\n")

	if r := run(t, dir, "apply", "dev", "--auto-approve"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2 (success with changes)\n%s", r.ExitCode, r.combined())
	}

	r := run(t, dir, "plan", "dev", "--var", "cidr=10.0.0.0/16")
	if r.ExitCode != 0 {
		t.Errorf("plan exit = %d, want 0 (no changes)\n%s", r.ExitCode, r.combined())
	}
	requireContains(t, r.Stdout, "No changes.")
	// Presence of "No changes." is not enough on its own: assert the absence
	// of every changing marker too, so a renderer that printed both could not
	// pass.
	for _, marker := range []string{" -/+ ", "  ~ ", "  + ", "  - "} {
		if strings.Contains(r.Stdout, marker) {
			t.Errorf("plan proposes an operation (%q) for a value that did not change:\n%s", marker, r.Stdout)
		}
	}
	requireContains(t, r.Stdout, "Plan: 0 to create, 0 to update, 0 to replace, 0 to destroy, 0 to forget.")
}

// TestConfigHashIgnoresWhichLevelSuppliedAValue. The same effective
// configuration supplied two different ways must fingerprint identically: a
// cidr of 10.0.0.0/16 is the same input whether it came from a file or a flag,
// and hashing Scope or SuppliedBy would make an unchanged configuration look
// stale against a saved plan.
//
// Both arms hold Source constant, because ResolvedConfig.Hash DOES fold Source
// by design. If the two arms disagree on Source, fix the producer, not this
// test.
func TestConfigHashIgnoresWhichLevelSuppliedAValue(t *testing.T) {
	hashOf := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		out := filepath.Join(dir, "plan.json")
		r := run(t, dir, append([]string{"plan", "dev", "--output", out}, args...)...)
		if r.ExitCode != 2 {
			t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
		}
		h := readPlanArtifact(t, out).ConfigHash
		if h == "" {
			t.Fatal("the plan artifact carries no config_hash")
		}
		return h
	}

	fromFile := hashOf(t, varProject(t, "cidr: 10.0.0.0/16\n"))
	fromFlag := hashOf(t, varProject(t, "cidr: 192.168.0.0/16\n"), "--var", "cidr=10.0.0.0/16")
	if fromFile != fromFlag {
		t.Errorf("config_hash differs for one effective configuration supplied two ways:\n"+
			"  variables.yml: %s\n  --var:         %s", fromFile, fromFlag)
	}

	// The fixture must be able to fail: a Hash that ignored the value
	// altogether would pass the assertion above. A different value must
	// produce a different hash.
	different := hashOf(t, varProject(t, "cidr: 192.168.0.0/16\n"), "--var", "cidr=10.9.0.0/16")
	if different == fromFile {
		t.Errorf("config_hash is identical for two different cidr values (%s) — "+
			"the hash does not see the value at all", different)
	}
}

// TestScopeSurvivesTheSavedPlanArtifact reads Scope and SuppliedBy back out of
// a plan the binary wrote, using the production codec.
//
// Either field written without its JSON tag fails SILENTLY: Scope reads back as
// ScopeUnset and SuppliedBy as "", both confident wrong answers rather than
// errors. So this asserts the exact scope and the exact input name, never
// merely that decoding succeeded.
func TestScopeSurvivesTheSavedPlanArtifact(t *testing.T) {
	dir := varProject(t, "cidr: 192.168.0.0/16\n")
	out := filepath.Join(dir, "plan.json")
	if r := run(t, dir, "plan", "dev", "--output", out, "--var", "cidr=10.0.0.0/16"); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	got := opAfter(t, readPlanArtifact(t, out), "net", "cidr")
	if s, _ := got.AsString(); s != "10.0.0.0/16" {
		t.Fatalf("cidr = %#v, want 10.0.0.0/16", got)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("cidr: Scope = %v, want ScopeCLIOverride", got.Scope)
	}
	// SuppliedBy must name the actual input, not merely be non-empty: "--var"
	// is also ScopeCLIOverride's own fallback annotation label, so a codec that
	// dropped the stamp and a renderer that fell back to the label both produce
	// "--var" here. Reading the wire field, distinct from any rendered
	// annotation, is what tells them apart.
	if got.SuppliedBy != "--var" {
		t.Errorf("cidr: SuppliedBy = %q, want %q", got.SuppliedBy, "--var")
	}

	// The middle of the ladder, so the test cannot be satisfied by a codec
	// that returns a constant: the same attribute, supplied by variables.yml
	// instead, must read back ScopeBaseConfig and no SuppliedBy at all —
	// SuppliedBy is only meaningful at ScopeCLIOverride (pkg/value/value.go).
	dir2 := varProject(t, "cidr: 10.0.0.0/16\n")
	out2 := filepath.Join(dir2, "plan.json")
	if r := run(t, dir2, "plan", "dev", "--output", out2); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	fromFile := opAfter(t, readPlanArtifact(t, out2), "net", "cidr")
	if fromFile.Scope != value.ScopeBaseConfig {
		t.Errorf("cidr from variables.yml: Scope = %v, want ScopeBaseConfig", fromFile.Scope)
	}
	if fromFile.SuppliedBy != "" {
		t.Errorf("cidr from variables.yml: SuppliedBy = %q, want empty", fromFile.SuppliedBy)
	}

	// A --var-file path, typed exactly as given on the command line, is the
	// other legal SuppliedBy value. Without this arm, a codec or producer that
	// special-cased the literal string "--var" and dropped anything else would
	// pass every assertion above and still be broken.
	dir3 := varProject(t, "cidr: 192.168.0.0/16\n")
	varFile := filepath.Join(dir3, "override.yml")
	writeIn(t, dir3, "override.yml", "cidr: 10.0.0.0/16\n")
	out3 := filepath.Join(dir3, "plan.json")
	if r := run(t, dir3, "plan", "dev", "--output", out3, "--var-file", varFile); r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}
	fromVarFile := opAfter(t, readPlanArtifact(t, out3), "net", "cidr")
	if fromVarFile.Scope != value.ScopeCLIOverride {
		t.Errorf("cidr from --var-file: Scope = %v, want ScopeCLIOverride", fromVarFile.Scope)
	}
	if fromVarFile.SuppliedBy != varFile {
		t.Errorf("cidr from --var-file: SuppliedBy = %q, want %q (the path as typed)", fromVarFile.SuppliedBy, varFile)
	}
}

// TestStateRecordsObservationNotConfigurationProvenance pins the boundary:
// state describes what the PROVIDER said, not which configuration layer or CLI
// input asked for it. Start stamping configuration Scope or SuppliedBy into
// state and `state show` and `explain` attribute an observed value to a --var
// or --var-file that was never re-consulted at read time.
func TestStateRecordsObservationNotConfigurationProvenance(t *testing.T) {
	dir := varProject(t, "cidr: 192.168.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve", "--var", "cidr=10.0.0.0/16"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	data, err := os.ReadFile(filepath.Join(dir, ".infrena", "state", "dev.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var st struct {
		Resources map[string]struct {
			Attributes map[string]value.Value `json:"attributes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	if len(st.Resources) == 0 {
		t.Fatal("state records no resources; the fixture applied nothing")
	}
	for name, r := range st.Resources {
		if len(r.Attributes) == 0 {
			t.Errorf("%s: state records no attributes", name)
		}
		for attr, v := range r.Attributes {
			if v.Source != value.SourceProvider {
				t.Errorf("%s.%s: Source = %q, want %q — state records what the provider reported",
					name, attr, v.Source, value.SourceProvider)
			}
			if v.Scope != value.ScopeUnset {
				t.Errorf("%s.%s: Scope = %v, want ScopeUnset — configuration provenance must not "+
					"be written into state", name, attr, v.Scope)
			}
			if v.SuppliedBy != "" {
				t.Errorf("%s.%s: SuppliedBy = %q, want empty — which CLI input supplied desired "+
					"configuration must not be written into state", name, attr, v.SuppliedBy)
			}
		}
	}
	// The applied value is still the --var one: this test asserts where
	// provenance goes, not that --var was ignored.
	requireContains(t, string(data), "10.0.0.0/16")
}
