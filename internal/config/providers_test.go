package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// linePattern matches both spellings a position takes in rendered output: the
// Origin's own "file.yml:4:5" and describeOrigin's "line 3 of file.yml".
var linePattern = regexp.MustCompile(`\.yml:(\d+)|line (\d+)`)

// distinctLines lists the line numbers a diagnostic names, so a test can assert
// that two positions were reported without depending on which spelling.
func distinctLines(out string) []string {
	seen := map[string]bool{}
	for _, m := range linePattern.FindAllStringSubmatch(out, -1) {
		for _, g := range m[1:] {
			if g != "" {
				seen[g] = true
			}
		}
	}
	lines := make([]string, 0, len(seen))
	for l := range seen {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	return lines
}

// PLAN.md §12.1 — decoding `providers:`. No resolution, no registry, no dispatch.

func providersIn(t *testing.T, body string) (*ProjectDecl, string) {
	t.Helper()
	decl, ds := Decode(writeConfig(t, body))
	return decl, render(t, ds)
}

// TestASingleProviderNeedsOnlyItsPlugin — §12.1's first example.
func TestASingleProviderNeedsOnlyItsPlugin(t *testing.T) {
	decl, out := providersIn(t, `
project: p
providers:
  - plugin: fake
    iam-role: some-role
`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if len(decl.Providers) != 1 {
		t.Fatalf("decoded %d providers, want 1", len(decl.Providers))
	}
	p := decl.Providers[0]
	if p.Plugin != "fake" {
		t.Errorf("Plugin = %q", p.Plugin)
	}
	// Its configuration travels, with origins, because a diagnostic about a bad
	// credential must point at the line that set it.
	if c, ok := p.Config["iam-role"]; !ok {
		t.Error("iam-role was not recorded as configuration")
	} else if c.Origin.File == "" {
		t.Error("iam-role carries no origin")
	}
}

// TestNameDefaultsToThePluginName. One unnamed `test` entry is called `test`,
// which is also what makes a v1 state file's existing provider name correct
// without a migration.
func TestNameDefaultsToThePluginName(t *testing.T) {
	decl, out := providersIn(t, "project: p\nproviders:\n  - plugin: fake\n")
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if decl.Providers[0].Name != "fake" {
		t.Errorf("Name = %q, want the plugin name", decl.Providers[0].Name)
	}
}

// TestTwoInstancesWithTheSameNameIsAnError, in BOTH spellings §12.1 names.
func TestTwoInstancesWithTheSameNameIsAnError(t *testing.T) {
	for _, tc := range []struct{ label, body string }{
		{"two unnamed entries of one plugin", "project: p\nproviders:\n  - plugin: fake\n  - plugin: fake\n"},
		{"two entries named the same", "project: p\nproviders:\n  - plugin: fake\n    name: a\n  - plugin: fake\n    name: a\n"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, out := providersIn(t, tc.body)
			if out == "" {
				t.Fatal("two instances with one name must be refused; whichever won, the other's " +
					"resources would silently go to the wrong account")
			}
			// BOTH entries, not just the second. A user with six instances needs
			// to know which two collided.
			//
			// Counted as distinct LINE NUMBERS rather than by matching a
			// rendering: the first draft of this looked for ".yml:" twice and
			// failed against a diagnostic that was already correct, because
			// describeOrigin writes "line 3 of <file>" while the Origin itself
			// renders "<file>:4:5". Two spellings of a position, one test
			// assuming one of them.
			if len(distinctLines(out)) < 2 {
				t.Errorf("the diagnostic names %v, fewer than two positions, so a reader cannot "+
					"tell which entries collided:\n%s", distinctLines(out), out)
			}
		})
	}
}

// TestTheFirstEntryIsTheDefault.
func TestTheFirstEntryIsTheDefault(t *testing.T) {
	decl, out := providersIn(t, `
project: p
providers:
  - plugin: fake
    name: first
  - plugin: fake
    name: second
`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if !decl.Providers[0].Default {
		t.Error("the first entry is not marked default")
	}
	// The absence half: exactly one default, or "the default" means nothing.
	if decl.Providers[1].Default {
		t.Error("the second entry is also marked default")
	}
}

// TestDefaultTrueOverridesOrder. Asserting only the first-entry rule would pass
// against an implementation that ignores `default:` entirely.
func TestDefaultTrueOverridesOrder(t *testing.T) {
	decl, out := providersIn(t, `
project: p
providers:
  - plugin: fake
    name: first
  - plugin: fake
    name: second
    default: true
`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if decl.Providers[0].Default {
		t.Error("the first entry won despite the second being marked default")
	}
	if !decl.Providers[1].Default {
		t.Error("`default: true` was ignored")
	}
}

// TestTwoEntriesMarkedDefaultIsAnError. No precedence to invent, and either
// choice sends some resources to the wrong account.
func TestTwoEntriesMarkedDefaultIsAnError(t *testing.T) {
	_, out := providersIn(t, `
project: p
providers:
  - plugin: fake
    name: a
    default: true
  - plugin: fake
    name: b
    default: true
`)
	if out == "" {
		t.Fatal("two entries marked default must be refused")
	}
	if !strings.Contains(out, "default") {
		t.Errorf("the diagnostic does not mention the key:\n%s", out)
	}
}

// TestConfigAndDefaultsAreSeparate is what §12.1's nesting exists for. BOTH
// absences, because a decoder that put everything in one map would pass a test
// that only checked presence.
func TestConfigAndDefaultsAreSeparate(t *testing.T) {
	decl, out := providersIn(t, `
project: p
providers:
  - plugin: fake
    iam-role: some-role
    defaults:
      tags:
        team: payments
`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	p := decl.Providers[0]
	if _, ok := p.Config["iam-role"]; !ok {
		t.Error("iam-role is not in Config")
	}
	if _, ok := p.Defaults["tags"]; !ok {
		t.Error("tags is not in Defaults")
	}
	// The separation. `iam-role` configures the PROVIDER; `tags` defaults a
	// RESOURCE. Mixing them would apply a credential to every resource or send a
	// tag to the plugin.
	if _, leaked := p.Defaults["iam-role"]; leaked {
		t.Error("iam-role leaked into Defaults, where it would be applied to every resource")
	}
	if _, leaked := p.Config["tags"]; leaked {
		t.Error("tags leaked into Config, where it would be handed to the plugin as configuration")
	}
	if _, leaked := p.Config["defaults"]; leaked {
		t.Error("the `defaults` key itself was recorded as configuration")
	}
}

// TestAProvidersBlockThatIsAMapIsRefused. A YAML map cannot hold two `aws` keys,
// which is exactly the collision §12.1 refuses — a shape that cannot express it
// would refuse it SILENTLY.
func TestAProvidersBlockThatIsAMapIsRefused(t *testing.T) {
	_, out := providersIn(t, "project: p\nproviders:\n  test:\n    iam-role: x\n")
	if out == "" {
		t.Fatal("a mapping `providers:` must be refused")
	}
	// The SHAPE must be what is reported, not a consequence of it. Removing the
	// list check still produces an error — iterating a mapping's content hands the
	// entry decoder a scalar key, which complains that entry zero is not a
	// mapping — and that message sends a reader to fix the wrong thing. This
	// assertion is what tells the two apart; a sabotage proved the first draft
	// could not.
	if !strings.Contains(out, "must be a list") {
		t.Errorf("the diagnostic reports something other than the shape, so a reader is sent to "+
			"fix the wrong thing:\n%s", out)
	}
	// And it shows the list form, or there is nothing to act on.
	if !strings.Contains(out, "plugin") {
		t.Errorf("the diagnostic does not show the list form:\n%s", out)
	}
}

// TestPluginIsRequired.
func TestPluginIsRequired(t *testing.T) {
	_, out := providersIn(t, "project: p\nproviders:\n  - name: lonely\n")
	if out == "" {
		t.Fatal("an entry with no `plugin` must be refused")
	}
	if !strings.Contains(out, "plugin") {
		t.Errorf("the diagnostic does not name the missing key:\n%s", out)
	}
}

// A resource names its provider INSTANCE (PLAN.md §12.1).

func TestAResourceRecordsItsProviderInstance(t *testing.T) {
	decl, out := providersIn(t, `
project: p
providers:
  - plugin: fake
    name: main
  - plugin: fake
    name: acct2
resources:
  a:
    type: fake.network
    cidr: 10.0.0.0/16
    provider: acct2
  b:
    type: fake.network
    cidr: 10.1.0.0/16
`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	byName := map[string]*ResourceDecl{}
	for _, r := range decl.Resources {
		byName[r.Name] = r
	}
	if got, _ := byName["a"].Provider.Value.AsString(); got != "acct2" {
		t.Errorf("a's provider = %q, want acct2", got)
	}
	if byName["a"].Provider.Origin.File == "" {
		t.Error("the provider key carries no origin, so `no such instance` cannot point at it")
	}
	// Unset, not defaulted here: which instance is the default is resolved later,
	// and stage 2 declares rather than resolves.
	if byName["b"].Provider.Name != "" {
		t.Errorf("b names no provider, but decoded %+v", byName["b"].Provider)
	}
}

// TestProviderIsNotAResourceAttribute keeps the namespace honest: everything the
// resource switch does not recognise becomes an ATTRIBUTE, so a `provider` that
// fell through would reach stage 7 as "fake.network has no attribute provider" on
// every resource that names one.
func TestProviderIsNotAResourceAttribute(t *testing.T) {
	decl, _ := providersIn(t, `
project: p
providers:
  - plugin: fake
resources:
  a:
    type: fake.network
    cidr: 10.0.0.0/16
    provider: test
`)
	for _, r := range decl.Resources {
		if _, leaked := r.Attributes["provider"]; leaked {
			t.Error("`provider` leaked into Attributes, where stage 7 would reject it on every " +
				"resource that uses the feature")
		}
	}
}

// TestProviderSurvivesInAModuleFile. decodeResources is shared between project
// files and module files, so this passes by construction — and pins the sharing,
// which would fail the day module files got a decoder of their own.
func TestProviderSurvivesInAModuleFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "module.yml"), []byte(`
resources:
  inner:
    type: fake.network
    cidr: 10.0.0.0/16
    provider: acct2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := LoadModule(dir)
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	mod, ds := DecodeModule(f)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", render(t, ds))
	}
	if got, _ := mod.Resources[0].Provider.Value.AsString(); got != "acct2" {
		t.Errorf("the module's resource did not record its provider: %q", got)
	}
}

// TestANonScalarProviderIsRefused. An instance name is one name; a list would
// silently pick one, and which resources went where would depend on that.
func TestANonScalarProviderIsRefused(t *testing.T) {
	_, out := providersIn(t, `
project: p
providers:
  - plugin: fake
resources:
  a:
    type: fake.network
    cidr: 10.0.0.0/16
    provider: [test, other]
`)
	if out == "" {
		t.Fatal("a list-valued `provider` must be refused")
	}
}

// TestTheInfrenaFloorIsDecodedAsAConstraint — PLAN.md §61.2's optional floor.
func TestTheInfrenaFloorIsDecodedAsAConstraint(t *testing.T) {
	decl, out := providersIn(t, "project: p\ninfrena: \">= 0.4, < 1.0\"\n")
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if decl.RequiredVersion.IsZero() {
		t.Fatal("`infrena:` was not decoded")
	}
	if got := decl.RequiredVersion.String(); got != ">= 0.4, < 1.0" {
		t.Errorf("constraint = %q, want the text as written for a diagnostic to quote", got)
	}
	if decl.RequiredVersionOrigin.File == "" {
		t.Error("no origin, so a diagnostic cannot point at the line")
	}
}

// TestAProjectWithNoInfrenaKeyIsUnconstrained — every project written before the key
// existed, which is all of them.
func TestAProjectWithNoInfrenaKeyIsUnconstrained(t *testing.T) {
	decl, _ := providersIn(t, "project: p\n")
	if !decl.RequiredVersion.IsZero() {
		t.Error("a project stating no floor must decode as unconstrained")
	}
}

// TestAMalformedInfrenaFloorIsRefused, rather than silently ignored — a floor nobody
// checks is worse than no floor, because the file claims a guarantee it has not got.
func TestAMalformedInfrenaFloorIsRefused(t *testing.T) {
	_, out := providersIn(t, "project: p\ninfrena: \"at least 4\"\n")
	if out == "" {
		t.Fatal("`infrena: \"at least 4\"` must be refused")
	}
	if !strings.Contains(out, "MAJOR.MINOR.PATCH") {
		t.Errorf("the diagnostic does not say what a constraint looks like:\n%s", out)
	}
}

// TestTheInfrenaFloorDeclaredTwiceIsAnErrorNamingBothLines.
//
// Two floors can contradict each other — `>= 0.4` and `< 0.4` — and YAML is perfectly
// happy with the repeated key, taking the last silently. Which floor won would then
// depend on document order, and nothing in any output would say so.
//
// A resources file already refuses foreign keys, so the reachable case is twice in the
// project file. Checked before writing the guard, because a guard for an unreachable
// case is code nothing can exercise.
func TestTheInfrenaFloorDeclaredTwiceIsAnErrorNamingBothLines(t *testing.T) {
	_, out := providersIn(t, "project: p\ninfrena: \">= 0.4\"\ninfrena: \"< 0.4\"\n")
	if out == "" {
		t.Fatal("`infrena` declared twice must be an error")
	}
	if !strings.Contains(out, "declared twice") {
		t.Errorf("the diagnostic does not say it is a duplicate:\n%s", out)
	}
	// BOTH lines — 2 and 3 — because a reader shown one of the pair has to find the
	// other by hand. distinctLines copes with either spelling of a position.
	if got := distinctLines(out); len(got) < 2 {
		t.Errorf("the diagnostic names %v, want both lines:\n%s", got, out)
	}
}
