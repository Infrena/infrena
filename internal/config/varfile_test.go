package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
)

func fileFrom(t *testing.T, path, body string) File {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return File{Path: path, Root: &root}
}

// TestParseVariableFileReturnsAFileDecodeVariableFileAccepts pins the seam
// internal/cli's loadVarFiles depends on: ParseVariableFile is the only place
// outside this package a --var-file's bytes become a yaml.Node, and its
// output must be exactly what DecodeVariableFile expects — Path set verbatim
// to what was passed, Kind FileVariables, Root walkable.
func TestParseVariableFileReturnsAFileDecodeVariableFileAccepts(t *testing.T) {
	f, err := ParseVariableFile("vars.yml", []byte("region: us-east-1\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Path != "vars.yml" {
		t.Errorf("Path = %q, want the path passed in verbatim", f.Path)
	}
	if f.Kind != FileVariables {
		t.Errorf("Kind = %v, want FileVariables", f.Kind)
	}
	got, ds := DecodeVariableFile(f, value.ScopeCLIOverride)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if s, _ := got["region"].AsString(); s != "us-east-1" {
		t.Errorf("region = %#v", got["region"])
	}
}

// TestParseVariableFileStoresThePathVerbatim guards the "as written, not
// resolved" contract specifically: a caller that passes a --chdir-relative
// display path, distinct from whatever absolute path it actually opened to
// get these bytes, must get that same display path back on File — not a
// resolved or re-derived one — so a diagnostic built from the result names
// what the user typed.
func TestParseVariableFileStoresThePathVerbatim(t *testing.T) {
	f, err := ParseVariableFile("../shared/vars.yml", []byte("a: 1\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Path != "../shared/vars.yml" {
		t.Errorf("Path = %q, want the exact string passed in", f.Path)
	}
}

// TestParseVariableFileReportsAYAMLSyntaxError is the failure half: malformed
// bytes must return an error rather than a File with a nil or partial Root
// that DecodeVariableFile would then have to guard against.
func TestParseVariableFileReportsAYAMLSyntaxError(t *testing.T) {
	_, err := ParseVariableFile("vars.yml", []byte("a: [1, 2\n"))
	if err == nil {
		t.Fatal("malformed YAML must be reported, not silently produce an empty or partial File")
	}
}

func TestDecodeVariableFileStampsSourceAndScopeOnEveryLeaf(t *testing.T) {
	f := fileFrom(t, "variables.yml", `
project_name: myapp
replicas: 3
enabled: true
regions:
  - us-east-1
  - eu-west-1
tags:
  team: platform
`)

	got, ds := DecodeVariableFile(f, value.ScopeCLIOverride)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	if s, ok := got["project_name"].AsString(); !ok || s != "myapp" {
		t.Errorf("project_name = %#v, want the string myapp", got["project_name"])
	}
	// An integer must stay an integer. If a variable file flattened everything
	// to a string, a variable feeding an integer attribute would fail schema
	// validation with a kind mismatch the user cannot fix from YAML.
	if n, ok := got["replicas"].AsInt(); !ok || n != 3 {
		t.Errorf("replicas = %#v, want the integer 3", got["replicas"])
	}
	if b, ok := got["enabled"].AsBool(); !ok || !b {
		t.Errorf("enabled = %#v, want the boolean true", got["enabled"])
	}

	for name, v := range got {
		if v.Source != value.SourceVariable {
			t.Errorf("%s: Source = %q, want %q — a variable file supplies variables at every level",
				name, v.Source, value.SourceVariable)
		}
		if v.Scope != value.ScopeCLIOverride {
			t.Errorf("%s: Scope = %v, want ScopeCLIOverride", name, v.Scope)
		}
	}

	// Provenance is per-leaf (spec §5.1), so a composite's children carry it
	// too: ConfigHash folds every leaf's Source, and a child left
	// SourceExplicit would make the same map hash differently depending on
	// which layer supplied it.
	items, ok := got["regions"].Raw.([]value.Value)
	if !ok || len(items) != 2 {
		t.Fatalf("regions did not decode as a two-item list: %#v", got["regions"])
	}
	for i, item := range items {
		if item.Source != value.SourceVariable || item.Scope != value.ScopeCLIOverride {
			t.Errorf("regions[%d]: Source=%q Scope=%v, want variable/ScopeCLIOverride",
				i, item.Source, item.Scope)
		}
	}
	m, ok := got["tags"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags did not decode as a map: %#v", got["tags"])
	}
	if m["team"].Source != value.SourceVariable || m["team"].Scope != value.ScopeCLIOverride {
		t.Errorf("tags.team: Source=%q Scope=%v, want variable/ScopeCLIOverride",
			m["team"].Source, m["team"].Scope)
	}
}

// TestDecodeVariableFileStampsSuppliedByOnEveryLeafAtCLIOverride pins
// Amendment 6 (contract.md): a --var-file decode (ScopeCLIOverride) stamps
// every leaf's SuppliedBy with f.Path, per-leaf for the same reason Source
// and Scope are — a composite's children must carry it too, or a nested
// value could not name where it came from any better than the composite
// shell around it.
func TestDecodeVariableFileStampsSuppliedByOnEveryLeafAtCLIOverride(t *testing.T) {
	f := fileFrom(t, "shared/vars.yml", `
project_name: myapp
regions:
  - us-east-1
  - eu-west-1
tags:
  team: platform
`)

	got, ds := DecodeVariableFile(f, value.ScopeCLIOverride)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	if got["project_name"].SuppliedBy != "shared/vars.yml" {
		t.Errorf("project_name: SuppliedBy = %q, want %q", got["project_name"].SuppliedBy, "shared/vars.yml")
	}
	items, ok := got["regions"].Raw.([]value.Value)
	if !ok || len(items) != 2 {
		t.Fatalf("regions did not decode as a two-item list: %#v", got["regions"])
	}
	for i, item := range items {
		if item.SuppliedBy != "shared/vars.yml" {
			t.Errorf("regions[%d]: SuppliedBy = %q, want %q", i, item.SuppliedBy, "shared/vars.yml")
		}
	}
	m, ok := got["tags"].Raw.(map[string]value.Value)
	if !ok {
		t.Fatalf("tags did not decode as a map: %#v", got["tags"])
	}
	if m["team"].SuppliedBy != "shared/vars.yml" {
		t.Errorf("tags.team: SuppliedBy = %q, want %q", m["team"].SuppliedBy, "shared/vars.yml")
	}
}

// TestDecodeVariableFileLeavesSuppliedByEmptyAtScopeUnset pins the other
// half of the restriction: variables.yml (decoded at ScopeUnset, not a
// --var-file) must NOT stamp SuppliedBy. SuppliedBy is meaningful only at
// ScopeCLIOverride (see Value.SuppliedBy's doc comment) — a variables.yml
// entry stamped with its own path would be harmless today (annotation()
// only reads SuppliedBy at ScopeCLIOverride) but would misrepresent what the
// field means the moment anything else starts trusting it being set as a
// signal of "supplied from the command line".
func TestDecodeVariableFileLeavesSuppliedByEmptyAtScopeUnset(t *testing.T) {
	f := fileFrom(t, "variables.yml", `cidr: 10.0.0.0/16`)

	got, ds := DecodeVariableFile(f, value.ScopeUnset)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if got["cidr"].SuppliedBy != "" {
		t.Errorf("SuppliedBy = %q, want empty at ScopeUnset", got["cidr"].SuppliedBy)
	}
}

func TestDecodeVariableFileScopeIsAParameterNotAConstant(t *testing.T) {
	// One decoder serves two precedence levels. If it hard-coded a scope,
	// variables.yml and --var-file could not be told apart, which is the
	// whole point of the field.
	f := fileFrom(t, "variables.yml", "a: 1\n")
	got, _ := DecodeVariableFile(f, value.ScopeBaseConfig)
	if got["a"].Scope != value.ScopeBaseConfig {
		t.Errorf("Scope = %v, want ScopeBaseConfig", got["a"].Scope)
	}
}

func TestDecodeVariableFileRejectsANonMappingTopLevel(t *testing.T) {
	_, ds := DecodeVariableFile(fileFrom(t, "vars.yml", "- a\n- b\n"), value.ScopeBaseConfig)
	if !ds.HasErrors() {
		t.Fatal("a sequence at the top level of a variable file must be an error")
	}
}

func TestDecodeVariableFileRejectsInterpolation(t *testing.T) {
	// Accepting it would put the literal text ${project_name} into a resource
	// attribute: a silent wrong answer, which is worse than a refusal.
	_, ds := DecodeVariableFile(fileFrom(t, "vars.yml", "name: ${project_name}-web\n"), value.ScopeBaseConfig)
	if !ds.HasErrors() {
		t.Fatal("an interpolation inside a variable file must be an error while expressions in variable files are unsupported")
	}
}

func TestDecodeVariableFileAcceptsAnEmptyFile(t *testing.T) {
	got, ds := DecodeVariableFile(fileFrom(t, "variables.yml", ""), value.ScopeBaseConfig)
	if ds.HasErrors() {
		t.Fatalf("an empty variable file is not an error: %v", ds)
	}
	if len(got) != 0 {
		t.Errorf("got %d variables from an empty file", len(got))
	}
}

// TestDecodeVariableFileRejectsACompositeKey covers the key.Kind guard: a
// mapping key that is itself a sequence has no string to serve as a variable
// name, and key.Value would silently read as "" without this check —
// collapsing every complex key in the file into one entry named "".
func TestDecodeVariableFileRejectsACompositeKey(t *testing.T) {
	_, ds := DecodeVariableFile(fileFrom(t, "vars.yml", "? [a, b]\n: 1\n"), value.ScopeBaseConfig)
	if !ds.HasErrors() {
		t.Fatal("a composite (non-scalar) key must be rejected, not silently read as an empty name")
	}
}

// TestDecodeVariableFileWarnsOnReservedBlockName pins the warning that fires
// when a variable file uses one of infra.yml's own top-level block names —
// almost always a sign the value belongs in infra.yml, not here.
func TestDecodeVariableFileWarnsOnReservedBlockName(t *testing.T) {
	_, ds := DecodeVariableFile(fileFrom(t, "variables.yml", "resources: 3\n"), value.ScopeBaseConfig)
	var found bool
	for _, d := range ds {
		if d.Severity == diag.SeverityWarning && strings.Contains(d.Summary, "resources") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming \"resources\": %v", ds)
	}
	// It is a warning, not an error: "resources" may legitimately be a
	// variable name, and the value must still decode.
	if ds.HasErrors() {
		t.Fatalf("a reserved-looking name must not itself be an error: %v", ds)
	}
}

// TestDecodeVariableFileRejectsADuplicateKey. yaml.v3 does not deduplicate
// mapping keys itself, so both entries reach this loop and the second would
// silently win over the first without this check — discarding whichever
// value the user actually intended.
func TestDecodeVariableFileRejectsADuplicateKey(t *testing.T) {
	_, ds := DecodeVariableFile(fileFrom(t, "vars.yml", "region: us-east-1\nregion: eu-west-1\n"), value.ScopeBaseConfig)
	if !ds.HasErrors() {
		t.Fatal("a variable set twice in one file must be an error, not a silent last-write-wins")
	}
}
