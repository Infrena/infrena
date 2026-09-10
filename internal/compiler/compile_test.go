package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/internal/config"
	"infra/pkg/address"
	"infra/pkg/value"
)

func loadFiles(t *testing.T, body string) []config.File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "infra.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	files, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return files
}

func TestCompileRunsTheFullPipelineOnAValidProject(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${network.id}
`)
	resolved, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if resolved.Project != "myapp" || resolved.Environment != "dev" {
		t.Errorf("Project/Environment = %q/%q, want myapp/dev", resolved.Project, resolved.Environment)
	}

	db, ok := resolved.Get(address.Address{Name: "database"})
	if !ok {
		t.Fatal("database resource missing from the resolved config")
	}
	size, ok := db.Attrs["size"]
	if !ok || size.Source != value.SourceDefault {
		t.Fatalf("stage 7's default for size was not filled in: %+v", size)
	}
	if n, _ := size.AsInt(); n != 10 {
		t.Errorf("size = %d, want the dev default of 10", n)
	}
}

func TestCompileStopsAfterDecodeErrors(t *testing.T) {
	// A duplicate definition leaves the decoder unable to build a complete
	// config. If later stages ran anyway, this lone database — which has no
	// network anywhere in the file — would also trip stage 8's missing-
	// requirement check, burying the real problem: the duplicate itself.
	files := loadFiles(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
  database:
    type: test.database
    engine: postgres
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a duplicate resource definition must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "defined more than once") {
		t.Errorf("expected the duplicate-definition diagnostic:\n%s", out.String())
	}
	if strings.Contains(out.String(), "network") {
		t.Errorf("stage 8 must not run against a config the decoder could not build; got noise:\n%s", out.String())
	}
}

func TestCompileStopsAfterSchemaErrorsBeforeGraphValidation(t *testing.T) {
	// bogus's unresolved type keeps stage 7 from finishing cleanly. guarded's
	// contradictory lifecycle would be caught by stage 8 — if stage 8 ran. It
	// must not: a resource whose type never resolved is exactly the config
	// later stages cannot build on.
	files := loadFiles(t, `
project: myapp
resources:
  bogus:
    type: not.a.real.type
  guarded:
    type: test.network
    cidr: 10.0.0.0/16
    lifecycle:
      prevent_destroy: true
      retain: true
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("an unregistered type must be an error")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "unknown resource type") {
		t.Errorf("expected stage 7's unknown-type diagnostic:\n%s", out.String())
	}
	if strings.Contains(out.String(), "prevent_destroy") {
		t.Errorf("stage 8 must not run once stage 7 has errors; got its lifecycle diagnostic anyway:\n%s", out.String())
	}
}

func TestCompileAccumulatesDiagnosticsWithinAStage(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  a:
    type: nope.one
  b:
    type: nope.two
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — one bad type must not mask the other", len(ds))
	}
}

func TestCompileReportsStage8Diagnostics(t *testing.T) {
	files := loadFiles(t, `
project: myapp
resources:
  database:
    type: test.database
    engine: postgres
`)
	_, ds := Compile(files, testRegistry(t), Options{Environment: "dev"})
	if !ds.HasErrors() {
		t.Fatal("a database with no network anywhere in the project must be caught before any provider call")
	}

	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "network") {
		t.Errorf("expected stage 8's missing-requirement diagnostic:\n%s", out.String())
	}
}
