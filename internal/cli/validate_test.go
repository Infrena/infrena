package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
