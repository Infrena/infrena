package config

import (
	"strings"
	"testing"
)

// varsProject decodes a project with the given files and fails on any error.
func varsProject(t *testing.T, files map[string]string) *ProjectDecl {
	t.Helper()
	loaded, err := Load(writeTree(t, files))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	decl, ds := Decode(loaded)
	if ds.HasErrors() {
		var sb strings.Builder
		ds.Render(&sb)
		t.Fatalf("unexpected diagnostics:\n%s", sb.String())
	}
	return decl
}

func hasOverride(e EnvironmentDecl, name string) bool {
	for _, o := range e.Overrides {
		if o.Name == name {
			return true
		}
	}
	return false
}

func isInt(o OverrideDecl, want int64) bool {
	n, ok := o.Value.AsInt()
	return ok && n == want
}

const varsBase = "project: p\nenvironments:\n  dev: {type: development}\n  production: {type: production}\n"

// TestDefaultYmlIsBaseConfiguration — it applies to every environment, so it lands on the base
// configuration rung rather than on any one environment.
func TestDefaultYmlIsBaseConfiguration(t *testing.T) {
	decl := varsProject(t, map[string]string{
		"infra.yml":        varsBase,
		"vars/default.yml": "size: 50\nregion: eu-west-1\n",
	})
	if len(decl.VariableValues) != 2 {
		t.Fatalf("VariableValues = %+v, want size and region", decl.VariableValues)
	}
	for _, env := range []string{"dev", "production"} {
		if hasOverride(findEnvironment(t, decl, env), "size") {
			t.Errorf("default.yml put size on environment %q; it belongs to every environment, "+
				"which is the base configuration rung", env)
		}
	}
}

// TestAnEnvironmentVarsFileOverridesPerVALUE is the one that matters.
//
// default.yml sets size and region; production.yml sets only size. Production must get its own
// size AND DEFAULT'S REGION — a file naming an environment is a set of differences, not a
// replacement. An implementation that swapped whole files would pass a test that only checked
// size, and fail only on region.
func TestAnEnvironmentVarsFileOverridesPerVALUE(t *testing.T) {
	decl := varsProject(t, map[string]string{
		"infra.yml":           varsBase,
		"vars/default.yml":    "size: 50\nregion: eu-west-1\n",
		"vars/production.yml": "size: 100\n",
	})

	// region stays base configuration, reaching every environment.
	if _, ok := decl.VariableValues["region"]; !ok {
		t.Error("region left base configuration; production.yml replaced default.yml rather than overriding it")
	}
	// size is overridden for production only.
	if got := overrideOf(t, findEnvironment(t, decl, "production"), "size"); !isInt(got, 100) {
		t.Errorf("production size = %+v, want 100", got.Value)
	}
	if hasOverride(findEnvironment(t, decl, "dev"), "size") {
		t.Error("production.yml leaked into dev")
	}
}

// TestADeeperVarsFileCarriesItsEnvironmentsInside — no filename to lean on, so the environments
// are named in the file.
func TestADeeperVarsFileCarriesItsEnvironmentsInside(t *testing.T) {
	decl := varsProject(t, map[string]string{
		"infra.yml":          varsBase,
		"vars/app/sizes.yml": "size: 40\nregion: eu-west-1\nproduction:\n  size: 100\ndev:\n  size: 10\n",
	})
	if _, ok := decl.VariableValues["size"]; !ok {
		t.Error("the bare key did not become a default")
	}
	if got := overrideOf(t, findEnvironment(t, decl, "production"), "size"); !isInt(got, 100) {
		t.Errorf("production size = %+v, want 100", got.Value)
	}
	if got := overrideOf(t, findEnvironment(t, decl, "dev"), "size"); !isInt(got, 10) {
		t.Errorf("dev size = %+v, want 10", got.Value)
	}
}

// TestAMapValuedVariableStaysAVariable is the boundary, and the half that keeps the rule honest.
//
// A key is an environment block only if it names a DECLARED environment. Treating any
// map-valued key as a block would make a file's meaning depend on its value's shape — and a map
// is an ordinary variable value in this language.
func TestAMapValuedVariableStaysAVariable(t *testing.T) {
	decl := varsProject(t, map[string]string{
		"infra.yml":         varsBase,
		"vars/app/tags.yml": "tags:\n  team: platform\n  tier: web\n",
	})
	v, ok := decl.VariableValues["tags"]
	if !ok {
		t.Fatalf("tags did not become a variable: %+v", decl.VariableValues)
	}
	if v.Kind != 4 && v.Kind.String() != "map" {
		t.Errorf("tags kind = %s, want map", v.Kind)
	}
	if len(decl.Environments) != 2 {
		t.Errorf("a map-valued variable invented an environment: %+v", decl.Environments)
	}
}

// TestAVariableCollidingWithAnEnvironmentNameIsAnError. Silently choosing a reading means adding
// an environment months later changes what an existing file means without anyone touching it.
func TestAVariableCollidingWithAnEnvironmentNameIsAnError(t *testing.T) {
	loaded, err := Load(writeTree(t, map[string]string{
		"infra.yml":          varsBase,
		"vars/app/flags.yml": "production: true\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, ds := Decode(loaded)
	if !ds.HasErrors() {
		t.Fatal("a variable sharing a name with an environment must be an error, not a guess")
	}
	var sb strings.Builder
	ds.Render(&sb)
	got := sb.String()
	for _, want := range []string{"production", "collides", "Rename"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, got)
		}
	}
}

// TestATopLevelVarsFileNamingNoEnvironmentUsesTheInFileForm — vars/sizes.yml is an ordinary
// file grouped by topic, not an error.
func TestATopLevelVarsFileNamingNoEnvironmentUsesTheInFileForm(t *testing.T) {
	decl := varsProject(t, map[string]string{
		"infra.yml":      varsBase,
		"vars/sizes.yml": "size: 40\nproduction:\n  size: 100\n",
	})
	if _, ok := decl.VariableValues["size"]; !ok {
		t.Error("a topic-grouped vars file at the top level must still work")
	}
	if got := overrideOf(t, findEnvironment(t, decl, "production"), "size"); !isInt(got, 100) {
		t.Errorf("production size = %+v, want 100", got.Value)
	}
}
