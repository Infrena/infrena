package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/diag"
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
