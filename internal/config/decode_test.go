package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/pkg/value"
)

func writeConfig(t *testing.T, body string) []File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files
}

func TestDecodeResources(t *testing.T) {
	files := writeConfig(t, `
project: myapp

resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16

  database:
    type: test.database
    engine: postgres
    size: 50
`)
	got, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.Project != "myapp" {
		t.Errorf("Project = %q", got.Project)
	}
	if len(got.Resources) != 2 {
		t.Fatalf("decoded %d resources, want 2", len(got.Resources))
	}

	// Resources are returned in a deterministic order.
	if got.Resources[0].Name != "database" || got.Resources[1].Name != "network" {
		t.Errorf("resources are not sorted by name: %s, %s", got.Resources[0].Name, got.Resources[1].Name)
	}

	db := got.Resources[0]
	if db.Type != "test.database" {
		t.Errorf("Type = %q", db.Type)
	}
	size, ok := db.Attributes["size"]
	if !ok {
		t.Fatal("size attribute missing")
	}
	if size.Value.Kind != value.KindInt {
		t.Errorf("size Kind = %v, want KindInt", size.Value.Kind)
	}
	if n, _ := size.Value.AsInt(); n != 50 {
		t.Errorf("size = %d, want 50", n)
	}
	if size.Value.Source != value.SourceExplicit {
		t.Errorf("Source = %v, want SourceExplicit — everything written in configuration is explicit", size.Value.Source)
	}
}

func TestDecodeRecordsOrigins(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.20.0.0/16
`)
	got, _ := Decode(files)
	attr := got.Resources[0].Attributes["cidr"]
	if attr.Origin.Line == 0 {
		t.Error("attribute origin has no line number; diagnostics depend on it")
	}
	if filepath.Base(attr.Origin.File) != "infra.yml" {
		t.Errorf("origin file = %q", attr.Origin.File)
	}
}

func TestDecodePreservesExpressionSource(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  application:
    type: test.application
    image: myapp:latest
    database_url: ${database.endpoint}
`)
	got, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	attr := got.Resources[0].Attributes["database_url"]
	if !attr.HasExpressions {
		t.Error("an attribute containing ${...} must be flagged so stage 6 can parse it in M2")
	}
	if s, _ := attr.Value.AsString(); s != "${database.endpoint}" {
		t.Errorf("expression source = %q; it must be preserved verbatim", s)
	}
}

func TestDecodeLifecycleAndDependsOn(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    depends_on: [network]
    lifecycle:
      prevent_destroy: true
      retain: true
`)
	got, ds := Decode(files)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	r := got.Resources[0]
	if !r.Lifecycle.PreventDestroy || !r.Lifecycle.Retain {
		t.Errorf("lifecycle not decoded: %#v", r.Lifecycle)
	}
	if len(r.DependsOn) != 1 || r.DependsOn[0] != "network" {
		t.Errorf("depends_on = %v", r.DependsOn)
	}
	if _, ok := r.Attributes["lifecycle"]; ok {
		t.Error("lifecycle is structure, not an attribute, and must not appear in Attributes")
	}
	if _, ok := r.Attributes["depends_on"]; ok {
		t.Error("depends_on is structure, not an attribute")
	}
}

func TestDecodeLifecycleAcceptsCapitalisedBooleans(t *testing.T) {
	// `True` and `TRUE` carry the !!bool tag but scalar text that is not
	// literally "true". A raw string comparison would silently leave a
	// destruction guard disabled.
	for _, written := range []string{"true", "True", "TRUE"} {
		files := writeConfig(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    lifecycle:
      prevent_destroy: `+written+`
`)
		got, ds := Decode(files)
		if ds.HasErrors() {
			t.Fatalf("%s: unexpected diagnostics: %+v", written, ds)
		}
		if !got.Resources[0].Lifecycle.PreventDestroy {
			t.Errorf("prevent_destroy: %s was silently ignored — a destruction guard must not depend on capitalisation", written)
		}
	}
}

func TestDecodeLifecycleRejectsQuotedBoolean(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    lifecycle:
      prevent_destroy: "true"
`)
	_, ds := Decode(files)
	if !ds.HasErrors() {
		t.Error("a quoted \"true\" is a string, not a boolean, and must be rejected rather than silently accepted")
	}
}

func TestDecodeReportsEmptyFile(t *testing.T) {
	files := writeConfig(t, "")
	_, ds := Decode(files)
	if !ds.HasErrors() {
		t.Fatal("an empty configuration file must be an error")
	}
	if !strings.Contains(ds[0].Summary, "empty") {
		t.Errorf("diagnostic should say the file is empty, got %q", ds[0].Summary)
	}
}

func TestDecodeReportsMissingType(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  database:
    engine: postgres
`)
	_, ds := Decode(files)
	if !ds.HasErrors() {
		t.Fatal("a resource with no type must be an error")
	}
	if ds[0].Origin.Line == 0 {
		t.Error("the diagnostic must point at the offending resource")
	}
}

func TestDecodeCollectsEveryError(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  a:
    engine: postgres
  b:
    engine: mysql
  c:
    engine: sqlite
`)
	_, ds := Decode(files)
	if len(ds) < 3 {
		t.Errorf("got %d diagnostics, want at least 3 — validation reports every problem in one pass (spec §7.4)", len(ds))
	}
}

func TestLoadReportsMissingProjectFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("a directory with no infra.yml must be an error naming the missing file")
	}
}

func TestDecodeReportsMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte("project: [unclosed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("malformed YAML must be reported")
	}
}
