package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/diag"
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

func TestDecodeAttributeBooleansAreTagAware(t *testing.T) {
	// The same defect class as the lifecycle booleans, but for ordinary
	// attributes: `True` carries the !!bool tag with scalar text that is not
	// literally "true".
	for _, written := range []string{"true", "True", "TRUE"} {
		files := writeConfig(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
    enabled: `+written+`
`)
		got, ds := Decode(files)
		if ds.HasErrors() {
			t.Fatalf("%s: unexpected diagnostics: %+v", written, ds)
		}
		attr := got.Resources[0].Attributes["enabled"]
		if attr.Value.Kind != value.KindBool {
			t.Fatalf("%s: Kind = %v, want KindBool", written, attr.Value.Kind)
		}
		if b, _ := attr.Value.AsBool(); !b {
			t.Errorf("enabled: %s decoded as false — boolean attributes must not depend on capitalisation", written)
		}
	}
}

func TestDecodeRejectsScalarDependsOn(t *testing.T) {
	files := writeConfig(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
    depends_on: network
`)
	got, ds := Decode(files)
	if !ds.HasErrors() {
		t.Fatal("a scalar depends_on must be an error: it silently yields no dependencies, and a missing edge lets a resource run before what it depends on")
	}
	if len(got.Resources) > 0 && len(got.Resources[0].DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty after the error", got.Resources[0].DependsOn)
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

// TestDuplicateResourceNamesAreRejected covers silent resource loss.
//
// decodeResources walked the mapping pairs and appended unconditionally, so two
// entries with the same key produced two ResourceDecls sharing a Name. Spec
// §5.2 requires logical names be unique within a module, and M2's stage 7 keys
// ResolvedConfig.Resources by Address.String(), so one would silently win and
// an entire resource would vanish from the plan without a word. Stage 2 is the
// only stage that still holds the line numbers for a diagnostic worth reading.
func TestDuplicateResourceNamesAreRejected(t *testing.T) {
	files := writeConfig(t, `
project: myapp

resources:
  database:
    type: test.database
    engine: postgres

  network:
    type: test.network
    cidr: 10.0.0.0/16

  database:
    type: test.database
    engine: mysql
`)
	_, ds := Decode(files)
	if !ds.HasErrors() {
		t.Fatal("a duplicate resource name must be an error")
	}

	rendered := render(t, ds)
	if !strings.Contains(rendered, "database") {
		t.Errorf("diagnostic does not name the duplicated resource:\n%s", rendered)
	}
	// Both origins: the second definition is at line 13, the first at line 5.
	if !strings.Contains(rendered, ":13:") {
		t.Errorf("diagnostic does not point at the duplicate (line 13):\n%s", rendered)
	}
	if !strings.Contains(rendered, "line 5") {
		t.Errorf("diagnostic does not name the first definition (line 5):\n%s", rendered)
	}
}

// TestDuplicateAttributeKeysAreRejected covers the same defect one level down,
// where the last assignment silently won.
func TestDuplicateAttributeKeysAreRejected(t *testing.T) {
	files := writeConfig(t, `
project: myapp

resources:
  database:
    type: test.database
    engine: postgres
    size: 10
    engine: mysql
`)
	_, ds := Decode(files)
	if !ds.HasErrors() {
		t.Fatal("a duplicate attribute key must be an error")
	}

	rendered := render(t, ds)
	if !strings.Contains(rendered, "engine") {
		t.Errorf("diagnostic does not name the duplicated attribute:\n%s", rendered)
	}
	if !strings.Contains(rendered, ":9:") {
		t.Errorf("diagnostic does not point at the duplicate (line 9):\n%s", rendered)
	}
	if !strings.Contains(rendered, "line 7") {
		t.Errorf("diagnostic does not name the first assignment (line 7):\n%s", rendered)
	}
}

// render produces the text a user would actually see, so a diagnostic test
// covers the message as rendered rather than the struct fields.
func render(t *testing.T, ds diag.Diagnostics) string {
	t.Helper()
	var buf bytes.Buffer
	ds.Render(&buf)
	return buf.String()
}

// The surviving instances of the branch's recurring defect class: a value read
// by its surface text rather than its type. Each produced a silently wrong
// string — usually empty — with no diagnostic at all.
func TestNonScalarsAreRejectedRatherThanReadAsText(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wants   []string
		notWant string
	}{
		{
			name: "project given a mapping",
			body: `
project:
  a: b

resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
`,
			wants: []string{"`project`", "must be"},
		},
		{
			name: "project given a list",
			body: `
project: [myapp]

resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
`,
			wants: []string{"`project`", "must be"},
		},
		{
			name: "type given a list",
			body: `
project: myapp

resources:
  net:
    type: [test.network]
    cidr: 10.0.0.0/16
`,
			wants: []string{"`type`", "must be"},
			// "has no `type`" is the confusing downstream message the old code
			// produced instead of naming the real problem.
			notWant: "has no `type`",
		},
		{
			name: "depends_on item is not a scalar",
			body: `
project: myapp

resources:
  net:
    type: test.network
    cidr: 10.0.0.0/16
  db:
    type: test.database
    engine: postgres
    depends_on: [[net]]
`,
			wants: []string{"depends_on", "must be"},
		},
		{
			name: "attribute given no value at all",
			body: `
project: myapp

resources:
  db:
    type: test.database
    engine:
`,
			wants: []string{"engine", "no value"},
		},
		{
			name: "attribute given a YAML alias",
			body: `
project: myapp

resources:
  net:
    type: test.network
    cidr: &shared 10.0.0.0/16
  db:
    type: test.database
    engine: *shared
`,
			wants: []string{"engine", "alias"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := Decode(writeConfig(t, tc.body))
			rendered := render(t, ds)
			if !ds.HasErrors() {
				t.Fatalf("expected an error diagnostic, got:\n%s", rendered)
			}
			for _, want := range tc.wants {
				if !strings.Contains(rendered, want) {
					t.Errorf("diagnostic does not mention %q:\n%s", want, rendered)
				}
			}
			if tc.notWant != "" && strings.Contains(rendered, tc.notWant) {
				t.Errorf("diagnostic still reports the downstream symptom %q:\n%s", tc.notWant, rendered)
			}
		})
	}
}

// TestAliasDoesNotBecomeTheAnchorName is the sharpest form of the alias defect:
// `engine: *shared` decoded to the string "shared", the anchor's name, so a
// resource silently got a plausible-looking wrong value rather than an empty
// one.
func TestAliasDoesNotBecomeTheAnchorName(t *testing.T) {
	got, ds := Decode(writeConfig(t, `
project: myapp

resources:
  net:
    type: test.network
    cidr: &shared 10.0.0.0/16
  db:
    type: test.database
    engine: *shared
`))
	if !ds.HasErrors() {
		t.Fatal("a YAML alias must produce a diagnostic")
	}
	for _, r := range got.Resources {
		if r.Name != "db" {
			continue
		}
		if s, ok := r.Attributes["engine"].Value.AsString(); ok && s == "shared" {
			t.Errorf("engine decoded to the anchor's name %q", s)
		}
	}
}
