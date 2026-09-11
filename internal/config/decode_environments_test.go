package config

import (
	"strings"
	"testing"

	"infra/pkg/value"
)

func findEnvironment(t *testing.T, p *ProjectDecl, name string) EnvironmentDecl {
	t.Helper()
	for _, e := range p.Environments {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no environment %q in %v", name, p.Environments)
	return EnvironmentDecl{}
}

func overrideOf(t *testing.T, e EnvironmentDecl, name string) OverrideDecl {
	t.Helper()
	for _, o := range e.Overrides {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("environment %q has no override %q: %v", e.Name, name, e.Overrides)
	return OverrideDecl{}
}

// TestDecodeEnvironmentsBlock decodes PLAN.md §7's example verbatim, which
// uses the FLAT override spelling.
func TestDecodeEnvironmentsBlock(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
environments:
  default:
    replicas: 1
    instance_class: db.t4g.small
  production:
    extends: default
    replicas: 10
    instance_class: db.t4g.large
`,
	})
	requireNoErrors(t, ds)

	prod := findEnvironment(t, p, "production")
	if prod.Extends != "default" {
		t.Errorf("Extends = %q, want %q", prod.Extends, "default")
	}
	if prod.ExtendsOrigin.Line == 0 {
		t.Error("ExtendsOrigin has no line; stage 3's cycle diagnostic needs one")
	}
	// `extends` is a keyword, not an override. If it leaks into Overrides,
	// stage 4 resolves a variable literally called "extends".
	for _, o := range prod.Overrides {
		if o.Name == "extends" {
			t.Error("`extends` leaked into Overrides")
		}
	}
	if got, ok := overrideOf(t, prod, "replicas").Value.AsInt(); !ok || got != 10 {
		t.Errorf("replicas override = %#v, want Int(10)", overrideOf(t, prod, "replicas").Value)
	}

	def := findEnvironment(t, p, "default")
	if def.Extends != "" {
		t.Errorf("default.Extends = %q, want empty", def.Extends)
	}
}

// TestBothOverrideSpellingsAreAccepted: PLAN.md §6 nests overrides under
// `variables:` and §7 writes them as flat keys. Both are in the authoritative
// spec, so both must work.
func TestBothOverrideSpellingsAreAccepted(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
environments:
  nested:
    variables:
      replicas: 1
  flat:
    replicas: 2
`,
	})
	requireNoErrors(t, ds)
	if got, ok := overrideOf(t, findEnvironment(t, p, "nested"), "replicas").Value.AsInt(); !ok || got != 1 {
		t.Errorf("nested spelling did not produce an override (got %v)", got)
	}
	if got, ok := overrideOf(t, findEnvironment(t, p, "flat"), "replicas").Value.AsInt(); !ok || got != 2 {
		t.Errorf("flat spelling did not produce an override (got %v)", got)
	}
}

// TestEnvironmentFileMergesWithTheBlock pins ruling 8's accepting direction:
// PLAN.md §7 puts `extends` in the block and §8 puts overrides in the file, so
// a user doing both is following the spec.
func TestEnvironmentFileMergesWithTheBlock(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"environments:\n  production:\n    extends: default\n  default:\n    replicas: 1\n",
		"environments/production.yml": "replicas: 10\ndomain: example.com\n",
	})
	requireNoErrors(t, ds)

	prod := findEnvironment(t, p, "production")
	if prod.Extends != "default" {
		t.Errorf("Extends = %q, want %q", prod.Extends, "default")
	}
	if got, ok := overrideOf(t, prod, "replicas").Value.AsInt(); !ok || got != 10 {
		t.Errorf("replicas from the file = %#v", overrideOf(t, prod, "replicas").Value)
	}
	// If this is empty, the duplicate diagnostic in addOverride cannot say
	// where the other assignment is.
	if !strings.HasSuffix(overrideOf(t, prod, "domain").Origin.File, "production.yml") {
		t.Errorf("override Origin should name the file it came from: %v", overrideOf(t, prod, "domain").Origin)
	}
	count := 0
	for _, e := range p.Environments {
		if e.Name == "production" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("production appears %d times; the block and the file must merge into one decl", count)
	}
}

// TestEnvironmentFileCanSetExtends: `extends` is reserved in the file form
// too, so a file-only environment can still inherit.
func TestEnvironmentFileCanSetExtends(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:            projectWithNoResources,
		"environments/staging.yml": "extends: default\nreplicas: 2\n",
		"environments/default.yml": "replicas: 1\n",
	})
	requireNoErrors(t, ds)
	if got := findEnvironment(t, p, "staging").Extends; got != "default" {
		t.Errorf("Extends = %q, want %q", got, "default")
	}
}

// TestOverrideSetInTwoPlacesIsRejected pins ruling 8's refusing direction, in
// all three shapes a collision can take.
func TestOverrideSetInTwoPlacesIsRejected(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "block and file",
			files: map[string]string{
				ProjectFileName:         projectWithNoResources + "environments:\n  prod:\n    replicas: 1\n",
				"environments/prod.yml": "replicas: 10\n",
			},
		},
		{
			name: "flat and nested in one environment",
			files: map[string]string{
				ProjectFileName: projectWithNoResources +
					"environments:\n  prod:\n    replicas: 1\n    variables:\n      replicas: 10\n",
			},
		},
		{
			name: "twice in one file",
			files: map[string]string{
				ProjectFileName:         projectWithNoResources,
				"environments/prod.yml": "replicas: 1\nreplicas: 10\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := decodeTree(t, tc.files)
			requireErrorAbout(t, ds, "replicas", "prod", "more than once")
		})
	}
}

// TestExtendsSetInTwoPlacesIsRejected: a silently-dropped `extends` changes
// which values an environment inherits, with nothing printed.
func TestExtendsSetInTwoPlacesIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources + "environments:\n  prod:\n    extends: a\n  a: {}\n  b: {}\n",
		"environments/prod.yml": "extends: b\n",
	})
	requireErrorAbout(t, ds, "extends", "prod")
}

// TestMalformedExtendsIsRejected covers every non-scalar shape. The alias case
// is the sharp one: an alias node's Value is the ANCHOR'S NAME, so `extends:
// *base` would silently become the string "base" — which might even name a
// real environment.
func TestMalformedExtendsIsRejected(t *testing.T) {
	cases := map[string]string{
		"sequence": "environments:\n  prod:\n    extends: [a, b]\n",
		"mapping":  "environments:\n  prod:\n    extends:\n      name: a\n",
		"null":     "environments:\n  prod:\n    extends:\n",
		"alias":    "base: &anchor other\nenvironments:\n  prod:\n    extends: *anchor\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + body,
			})
			if !ds.HasErrors() {
				t.Fatalf("malformed extends (%s) was accepted: %v", name, p.Environments)
			}
			for _, e := range p.Environments {
				if e.Name == "prod" && e.Extends != "" {
					t.Errorf("malformed extends (%s) still produced Extends = %q", name, e.Extends)
				}
			}
		})
	}
}

// TestValidExtendsIsAccepted is the other direction; without it every test
// above passes against an implementation that rejects all extends.
func TestValidExtendsIsAccepted(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "environments:\n  default: {}\n  prod:\n    extends: default\n",
	})
	requireNoErrors(t, ds)
	if got := findEnvironment(t, p, "prod").Extends; got != "default" {
		t.Errorf("Extends = %q, want %q", got, "default")
	}
}

// TestStageTwoDoesNotJudgeExtendsTargets pins ruling 6. Cycles and unknown
// parents need the whole set of environments, which is stage 3's job (spec §7).
// Two implementations of one concept is what leaked a plaintext secret in M2.
func TestStageTwoDoesNotJudgeExtendsTargets(t *testing.T) {
	for name, body := range map[string]string{
		"unknown parent": "environments:\n  prod:\n    extends: nosuch\n",
		"self":           "environments:\n  prod:\n    extends: prod\n",
		"two-cycle":      "environments:\n  a:\n    extends: b\n  b:\n    extends: a\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{ProjectFileName: projectWithNoResources + body})
			requireNoErrors(t, ds)
		})
	}
}

// TestEnvironmentOverridesAreTaggedSourceEnvironment: provenance is per-leaf,
// so a list override must not leave its elements claiming SourceExplicit.
func TestEnvironmentOverridesAreTaggedSourceEnvironment(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources,
		"environments/prod.yml": "tags:\n  - web\n  - api\n",
	})
	requireNoErrors(t, ds)
	o := overrideOf(t, findEnvironment(t, p, "prod"), "tags")
	if o.Value.Source != value.SourceEnvironment {
		t.Errorf("Source = %v, want SourceEnvironment", o.Value.Source)
	}
	items, ok := o.Value.Raw.([]value.Value)
	if !ok || len(items) != 2 {
		t.Fatalf("Raw = %#v", o.Value.Raw)
	}
	for i, item := range items {
		if item.Source != value.SourceEnvironment {
			t.Errorf("tags[%d].Source = %v, want SourceEnvironment", i, item.Source)
		}
	}
}

// TestEnvironmentsAndOverridesAreSorted. Both insertion orders CONTRADICT the
// sorted order, and both assertions loop over every adjacent pair rather than
// sampling — six names, because a two-name check passes far too often against
// broken code.
//
// Sorting here is what spares every consumer from re-sorting. M3 measured
// eleven redundant sorts in this tree whose only job was undoing map
// iteration; this test is what makes the twelfth unnecessary.
func TestEnvironmentsAndOverridesAreSorted(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
environments:
  zulu:
    zeta: 1
    mike: 2
    alpha: 3
    yankee: 4
    bravo: 5
    november: 6
  mike: {}
  alpha: {}
  yankee: {}
  bravo: {}
  november: {}
`,
	})
	requireNoErrors(t, ds)
	if len(p.Environments) != 6 {
		t.Fatalf("got %d environments, want 6", len(p.Environments))
	}
	for i := 1; i < len(p.Environments); i++ {
		if p.Environments[i-1].Name >= p.Environments[i].Name {
			t.Fatalf("Environments not sorted: %q before %q", p.Environments[i-1].Name, p.Environments[i].Name)
		}
	}
	zulu := findEnvironment(t, p, "zulu")
	if len(zulu.Overrides) != 6 {
		t.Fatalf("got %d overrides, want 6", len(zulu.Overrides))
	}
	for i := 1; i < len(zulu.Overrides); i++ {
		if zulu.Overrides[i-1].Name >= zulu.Overrides[i].Name {
			t.Fatalf("Overrides not sorted: %q before %q", zulu.Overrides[i-1].Name, zulu.Overrides[i].Name)
		}
	}
}

// TestOverridesFromTheBlockAndTheFileAreSortedTogether. The merge appends the
// file's overrides after the block's, so sorting only within each source would
// leave the combined slice unsorted. The names are chosen so that the file's
// override sorts BEFORE the block's.
func TestOverridesFromTheBlockAndTheFileAreSortedTogether(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources + "environments:\n  prod:\n    zulu: 1\n",
		"environments/prod.yml": "alpha: 2\n",
	})
	requireNoErrors(t, ds)
	prod := findEnvironment(t, p, "prod")
	if len(prod.Overrides) != 2 || prod.Overrides[0].Name != "alpha" {
		t.Fatalf("overrides not sorted across sources: %v", prod.Overrides)
	}
}

// TestProjectOriginComesFromTheProjectFile.
//
// Decode assigns out.Origin inside its per-file loop. With three files that
// makes ProjectDecl.Origin the LAST file's — so every diagnostic that points
// at "the project" would point at environments/zulu.yml. The fixture uses
// "zulu" so it sorts last and the bug is reachable.
func TestProjectOriginComesFromTheProjectFile(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources,
		VariablesFileName:       "region: us-east-1\n",
		"environments/zulu.yml": "replicas: 1\n",
	})
	requireNoErrors(t, ds)
	if !strings.HasSuffix(p.Origin.File, ProjectFileName) {
		t.Errorf("ProjectDecl.Origin.File = %q, want %s", p.Origin.File, ProjectFileName)
	}
	if p.Project != "demo" {
		t.Errorf("Project = %q, want %q", p.Project, "demo")
	}
}

// TestEnvironmentOverrideWithInterpolationIsRejected: environment overrides
// are resolved before any expression scope exists, so an unresolved `${...}`
// here must be REJECTED rather than becoming part of the plan as though it
// were a literal. Same rule as a variable's `default:` and variables.yml's
// values, exercised in the third place it can appear.
func TestEnvironmentOverrideWithInterpolationIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:         projectWithNoResources,
		"environments/prod.yml": "domain: ${var.other}\n",
	})
	requireErrorAbout(t, ds, "domain", "interpolation")
}

// TestBlockLevelKeysMustBeMappings pins the three "must be a mapping" guards
// that sit above a single variable's or environment's own body: the
// `variables:` block itself, the `environments:` block itself, and one
// environment's body (as opposed to its `variables:` sub-block, which is
// checked separately). Each of the three has a sibling test at a DIFFERENT
// level — a variable's own body (TestVariableBodyMustBeAMapping), an
// environment's nested `variables:` sub-block — but none at this level, so a
// broken guard here would pass every other test in the suite.
//
// The "variables block" and "environments block" fixtures are two-element
// sequences deliberately, not one: with `node.Kind != yaml.MappingNode`
// disabled, decodeVariables'/decodeEnvironments' pair-walking loop still
// reads the sequence two elements at a time and calls decodeVariable /
// decodeEnvironmentBody on the second element as a body — which, for a
// scalar body, raises ITS OWN "must be a mapping" diagnostic worded closely
// enough (both mention "variable", "mapping", even "declaration" — that word
// appears in decodeVariable's Detail too, describing what a bare value would
// be ambiguous with) that a loosely chosen fragment passes against the
// disabled guard as readily as against the real one. The phrases below —
// "mapping of variable name to" and "mapping of environment name to" — were
// checked against BOTH diagnostics' full text while developing this test and
// appear only in the block-level message, never in the per-item fallback.
func TestBlockLevelKeysMustBeMappings(t *testing.T) {
	cases := []struct{ name, body, fragment string }{
		{"variables block", "variables:\n  - a\n  - b\n", "mapping of variable name to"},
		{"environments block", "environments:\n  - a\n  - b\n", "mapping of environment name to"},
		{"one environment body", "environments:\n  prod: 5\n", "prod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + tc.body,
			})
			requireErrorAbout(t, ds, tc.fragment)
		})
	}
}

// TestVariablesFileWithAConfigurationBlockWarns: a `resources:` key in
// variables.yml is almost certainly the wrong file. It is a WARNING, not an
// error, because a variable may legitimately be called "project" and refusing
// it would break a valid configuration to catch a mistake.
func TestVariablesFileWithAConfigurationBlockWarns(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:   projectWithNoResources,
		VariablesFileName: "resources:\n  db: {}\n",
	})
	if ds.HasErrors() {
		t.Fatalf("must be a warning, not an error: %v", errorSummaries(ds))
	}
	if len(ds) != 1 || !strings.Contains(ds[0].Summary, "resources") {
		t.Fatalf("want one warning about `resources`, got %v", ds)
	}
}
