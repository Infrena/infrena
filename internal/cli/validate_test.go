package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"infra/internal/diag"
	"infra/pkg/value"
)

func projectDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestValidateAcceptsAGoodProject(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	ds := validateProject(dir, buildRegistry(dir))
	if ds.HasErrors() {
		t.Fatalf("valid project reported errors: %+v", ds)
	}
}

func TestValidateRejectsUnknownResourceType(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: aws.rds
    engine: postgres
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("an unregistered resource type must be an error")
	}
	joined := renderToString(ds)
	if !strings.Contains(joined, "aws.rds") {
		t.Errorf("diagnostic does not name the offending type:\n%s", joined)
	}
	if !strings.Contains(joined, "test.database") {
		t.Errorf("diagnostic should suggest the known types:\n%s", joined)
	}
}

func TestValidateRejectsUnknownAttribute(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    nonsense: true
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("an attribute the schema does not define must be an error")
	}
	if !strings.Contains(renderToString(ds), "nonsense") {
		t.Error("diagnostic must name the unknown attribute")
	}
}

func TestValidateRejectsSettingComputedAttribute(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    endpoint: nope.example.com
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("configuration must not set a computed attribute")
	}
}

func TestFormatValueRedactsNestedSensitiveLeaves(t *testing.T) {
	// Sensitivity is per-leaf: a non-sensitive composite can hold a sensitive
	// element. Redacting only the top level would leak it through %v.
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)

	cases := []struct {
		name string
		in   value.Value
	}{
		{"sensitive scalar", secret},
		{"sensitive map leaf", value.Map(map[string]value.Value{
			"user": value.String("admin", value.SourceProvider),
			"pass": secret,
		}, value.SourceProvider)},
		{"sensitive list element", value.List([]value.Value{
			value.String("public", value.SourceProvider),
			secret,
		}, value.SourceProvider)},
		{"map nested inside a list", value.List([]value.Value{
			value.Map(map[string]value.Value{"pass": secret}, value.SourceProvider),
		}, value.SourceProvider)},
	}

	for _, tc := range cases {
		got := formatValue(tc.in)
		if strings.Contains(got, "hunter2") {
			t.Errorf("%s: rendered %q, which leaks the secret", tc.name, got)
		}
		if !strings.Contains(got, "<sensitive>") {
			t.Errorf("%s: rendered %q, want a <sensitive> marker", tc.name, got)
		}
	}
}

func TestFormatValueSortsMapKeys(t *testing.T) {
	got := formatValue(value.Map(map[string]value.Value{
		"b": value.Int(2, value.SourceProvider),
		"a": value.String("x", value.SourceProvider),
	}, value.SourceProvider))
	if got != "{a: x, b: 2}" {
		t.Errorf("formatValue = %q, want %q — map keys must be sorted or output churns between runs", got, "{a: x, b: 2}")
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  a:
    type: nope.one
  b:
    type: nope.two
  c:
    type: nope.three
`)
	ds := validateProject(dir, buildRegistry(dir))
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3", len(ds))
	}
}

func renderToString(ds diag.Diagnostics) string {
	var b strings.Builder
	ds.Render(&b)
	return b.String()
}

// TestDiagnosticsAdviseOnlyRegisteredCommands keeps the tool from telling a
// user to run something it does not have. The unknown-attribute diagnostic
// suggested `infra explain <type>`, which is not registered until M7, so
// following the advice yielded "unknown command". Spec §16 refuses command
// stubs on the grounds that a stub promises a capability that does not exist;
// a diagnostic makes the same promise.
func TestDiagnosticsAdviseOnlyRegisteredCommands(t *testing.T) {
	registered := map[string]bool{}
	var collect func(c *cobra.Command)
	collect = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			registered[sub.Name()] = true
			collect(sub)
		}
	}
	collect(NewRootCommand())

	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    nonexistent: 1
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("an unknown attribute must be an error")
	}

	var buf bytes.Buffer
	ds.Render(&buf)
	rendered := buf.String()
	if !strings.Contains(rendered, "nonexistent") {
		t.Fatalf("expected an unknown-attribute diagnostic, got:\n%s", rendered)
	}

	for _, m := range regexp.MustCompile("`infra ([a-z-]+)").FindAllStringSubmatch(rendered, -1) {
		if !registered[m[1]] {
			t.Errorf("diagnostic advises `infra %s`, which is not a registered command:\n%s", m[1], rendered)
		}
	}
}

// TestValidateCatchesMissingRequiredInfrastructure is stage 8, reachable only
// because validate now delegates to the full Compile pipeline. M1's
// three-check subset — type registered, attribute exists, attribute not
// computed — had no way to catch this.
func TestValidateCatchesMissingRequiredInfrastructure(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
`)
	ds := validateProject(dir, buildRegistry(dir))
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in the project must be an error")
	}
	if !strings.Contains(renderToString(ds), "network") {
		t.Errorf("diagnostic must name the missing requirement:\n%s", renderToString(ds))
	}
}

// TestFormatValueFailsClosedOnUnexpectedShapes covers the escape hatch that
// M1's redaction fix left open. TestFormatValueRedactsNestedSensitiveLeaves
// already covers a correctly-constructed composite; this covers the ones
// that reach the default branch.
//
// The leak was measured before it was fixed. A Value carrying a map in Raw
// with its Kind left at the zero value (KindInvalid) fell through to
// fmt.Sprintf("%v", v.Raw) and rendered:
//
//	map[password:{string true hunter2 provider true  <generated>}]
//
// — the secret in clear text, with its own Sensitive flag printed next to it.
func TestFormatValueFailsClosedOnUnexpectedShapes(t *testing.T) {
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)

	cases := []struct {
		name string
		v    value.Value
	}{
		{
			// The leak as measured: Kind never set, Raw holds a composite.
			name: "kind left at the zero value with a composite Raw",
			v:    value.Value{Known: true, Raw: map[string]value.Value{"password": secret}},
		},
		{
			name: "kind says map, Raw is a different map type",
			v:    value.Value{Kind: value.KindMap, Known: true, Raw: map[string]any{"password": "hunter2"}},
		},
		{
			name: "kind says list, Raw is a different slice type",
			v:    value.Value{Kind: value.KindList, Known: true, Raw: []any{"hunter2"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatValue(tc.v)
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret rendered in clear text: %s", got)
			}
			if got != "<unrenderable>" {
				t.Errorf("formatValue = %q, want %q — an unreadable value must say so, "+
					"not render as empty, which would claim the composite held nothing", got, "<unrenderable>")
			}
		})
	}
}

// TestFormatValueStillRendersScalars guards the other direction: failing
// closed must not turn ordinary values into <unrenderable>.
func TestFormatValueStillRendersScalars(t *testing.T) {
	cases := []struct {
		v    value.Value
		want string
	}{
		{value.String("eu-west-1", value.SourceExplicit), "eu-west-1"},
		{value.Int(20, value.SourceDefault), "20"},
		{value.Bool(true, value.SourceExplicit), "true"},
	}
	for _, tc := range cases {
		if got := formatValue(tc.v); got != tc.want {
			t.Errorf("formatValue = %q, want %q", got, tc.want)
		}
	}
}
