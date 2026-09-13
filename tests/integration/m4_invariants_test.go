package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

// writeIn writes one file into an existing project directory. `project` only
// writes infra.yml; M4 fixtures need variables.yml and environments/*.yml too.
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
// struct rather than planner.Plan because Plan has no UnmarshalJSON until M6 —
// but the map values are real value.Value, so decoding exercises the
// production codec on bytes the production binary wrote.
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading plan artifact: %v", err)
	}
	var p planArtifact
	if err := json.Unmarshal(data, &p); err != nil {
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

// varProject writes a project whose one attribute is fed by a variable. cidr
// is a plain string attribute, so no typed variable declaration is needed and
// these tests do not depend on Task 4.
func varProject(t *testing.T, variablesYML string) string {
	t.Helper()
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${cidr}
`)
	writeIn(t, dir, "variables.yml", variablesYML)
	return dir
}

// TestAVarMatchingTheFileValueIsNotAChange is acceptance invariant 2 (no-op
// plan) under BOTH new-since-Task-1 fields, not just Scope. A --var that
// repeats what variables.yml already said changes the Scope AND the
// SuppliedBy of the desired value and nothing else: after Amendment 6 the
// desired value carries Scope=ScopeCLIOverride, SuppliedBy="--var", while the
// value state records (because providers/test/provider.go restamps every
// reported value SourceProvider — see TestStateRecordsObservationNotConfigurationProvenance
// below) carries Scope=ScopeUnset, SuppliedBy="". If value.Equal compared
// either field, the desired value would differ from the recorded one and
// every plan would propose a change forever — the phantom-diff shape M3 spent
// a Critical fixing.
//
// This test is the incidental coverage Amendment 6 called out: it was
// written for Scope alone, but the --var-vs-variables.yml pairing means a
// regression in EITHER field's Equal exclusion fails it, because both differ
// between the two arms simultaneously. cidr is ForceNew, so the failure is
// loud: a proposed REPLACEMENT of a resource nobody asked to change, not a
// quiet extra line in a diff.
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

// TestConfigHashIgnoresWhichLevelSuppliedAValue proves the M4 contract's
// invariant 2 end to end, for both Scope and SuppliedBy. The same effective
// configuration, supplied two different ways, must fingerprint identically: a
// value of 10.0.0.0/16 is the same input whether it came from a file or a
// flag, and hashing either field would make an unchanged configuration look
// stale in M6 — SuppliedBy's failure mode is the same as Scope's here, since
// both change between the two arms below and hashValue excludes both by
// omission (it only ever writes "kind" and "source", never touches v.Scope or
// v.SuppliedBy).
//
// Both arms hold Source constant at SourceVariable — ResolvedConfig.Hash
// DOES fold Source (internal/compiler/resolved.go, hashValue), by design, so a
// pair that differed in Source would fail this test for an unrelated reason.
// If the two arms disagree on Source, fix the producer, not this test.
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

// TestScopeSurvivesTheSavedPlanArtifact is the M4 contract's invariant 3 at
// the level the binary can show it, for both Scope and SuppliedBy. The binary
// marshals the value; this test unmarshals it with the production codec
// (value.Value.UnmarshalJSON) and reads both fields back. Either written
// without its JSON tag fails silently: Scope reads back as ScopeUnset,
// SuppliedBy as "" — both confident wrong answers rather than errors — so the
// test asserts the exact scope and the exact input name, never merely that
// decoding succeeded.
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
	// is also ScopeCLIOverride's own fallback annotation label (Amendment 6),
	// so a codec that dropped the stamp and a renderer that fell back to the
	// label would both produce the string "--var" here. Asserting the literal
	// value from a wire field distinct from any rendered annotation is what
	// tells them apart.
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
	// other legal SuppliedBy value (Amendment 6): "--var" is not the only
	// input a CLI-rung value can name, so a codec (or a producer) that
	// special-cased the literal string "--var" and dropped anything else
	// would pass every assertion above and still be broken. variables.yml
	// itself, re-supplied through --var-file instead of --var, is used as
	// the file: its path is what the binary is asked to name.
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

// TestStateRecordsObservationNotConfigurationProvenance pins the boundary the
// brief's "apply, then read the scope back from state" assumed away.
// providers/test/provider.go restamps every reported value SourceProvider, so
// state describes what the provider said, not which configuration layer (or
// which input) asked for it. If a later change starts stamping configuration
// Scope or SuppliedBy into state, `state show` and `explain` would attribute
// an observed value to --var or to a --var-file that was never re-consulted
// at read time.
func TestStateRecordsObservationNotConfigurationProvenance(t *testing.T) {
	dir := varProject(t, "cidr: 192.168.0.0/16\n")
	if r := run(t, dir, "apply", "dev", "--auto-approve", "--var", "cidr=10.0.0.0/16"); r.ExitCode != 2 {
		t.Fatalf("apply exit = %d, want 2\n%s", r.ExitCode, r.combined())
	}

	data, err := os.ReadFile(filepath.Join(dir, ".infra", "state", "dev.json"))
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
