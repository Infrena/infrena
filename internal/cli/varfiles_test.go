package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadVarFilesAppliesFilesInFlagOrderSoALaterFileWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "one.yml", "a: one\nb: one\nc: one\n")
	writeFile(t, dir, "two.yml", "b: two\nc: two\n")
	writeFile(t, dir, "three.yml", "c: three\n")

	got, ds := loadVarFiles(dir, []string{"one.yml", "two.yml", "three.yml"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	// a from the FIRST file, b from the MIDDLE one, c from the last. Only the
	// middle rung distinguishes "applies every file in order" from "applies
	// the last file" or "applies the first file".
	for _, tc := range []struct{ name, want string }{
		{"a", "one"}, {"b", "two"}, {"c", "three"},
	} {
		s, ok := got[tc.name].AsString()
		if !ok || s != tc.want {
			t.Errorf("%s = %#v, want %q", tc.name, got[tc.name], tc.want)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d variables, want 3 — every file must contribute", len(got))
	}
	if got["a"].Scope != value.ScopeCLIOverride {
		t.Errorf("a: Scope = %v, want ScopeCLIOverride", got["a"].Scope)
	}
}

func TestLoadVarFilesResolvesRelativePathsAgainstChdir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vars.yml", "a: yes-please\n")
	// The process working directory is the package directory, not dir. If
	// loadVarFiles resolved against os.Getwd, every --var-file under --chdir
	// would fail, and every integration test would have to pass an absolute
	// path.
	got, ds := loadVarFiles(dir, []string{"vars.yml"})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if s, _ := got["a"].AsString(); s != "yes-please" {
		t.Errorf("a = %#v", got["a"])
	}
}

func TestLoadVarFilesAcceptsAnAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "abs.yml", "a: from-absolute\n")
	// An absolute --var-file must be used as given, not re-joined onto dir —
	// filepath.Join(dir, "/abs/path") does not reproduce the original
	// absolute path on every platform, and a --var-file outside the project
	// directory is a legitimate use (a shared secrets file, say).
	got, ds := loadVarFiles(t.TempDir(), []string{filepath.Join(dir, "abs.yml")})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if s, _ := got["a"].AsString(); s != "from-absolute" {
		t.Errorf("a = %#v", got["a"])
	}
}

func TestLoadVarFilesReportsAYAMLSyntaxError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "broken.yml", "a: [1, 2\n")
	_, ds := loadVarFiles(dir, []string{"broken.yml"})
	if !ds.HasErrors() {
		t.Fatal("malformed YAML in a --var-file must be reported, not silently produce an empty layer")
	}
}

// TestLoadVarFilesPropagatesDecodeVariableFileDiagnostics guards the
// ds.Extend(fds) call: loadVarFiles reads and parses the file itself, but a
// SHAPE problem — here, an interpolation, which YAML parses fine — is
// DecodeVariableFile's diagnostic. Dropping that extend would make a
// --var-file with a shape error look identical to a clean one.
func TestLoadVarFilesPropagatesDecodeVariableFileDiagnostics(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vars.yml", "name: ${project_name}-web\n")
	_, ds := loadVarFiles(dir, []string{"vars.yml"})
	if !ds.HasErrors() {
		t.Fatal("an interpolation inside a --var-file must surface as a diagnostic from loadVarFiles, not be swallowed")
	}
}

// TestLoadVarFilesDecodeDiagnosticsNameThePathAsTyped guards the File passed
// to config.DecodeVariableFile (via config.ParseVariableFile): it must carry
// the path AS THE USER TYPED IT (here, "vars.yml"), not the --chdir-joined
// path loadVarFiles actually opened (dir + "/vars.yml"). A diagnostic that
// originates INSIDE DecodeVariableFile — this one, the interpolation
// check — builds its own Origin/Detail from File.Path, so passing the joined
// path through by mistake would leak the test's own temporary directory into
// a message meant to show the user what they wrote.
func TestLoadVarFilesDecodeDiagnosticsNameThePathAsTyped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vars.yml", "name: ${project_name}-web\n")
	_, ds := loadVarFiles(dir, []string{"vars.yml"})
	text := renderToString(ds)
	if !strings.Contains(text, "vars.yml") {
		t.Fatalf("expected the diagnostic to name vars.yml:\n%s", text)
	}
	if strings.Contains(text, dir) {
		t.Errorf("the diagnostic leaked the joined/temporary path instead of the typed one:\n%s", text)
	}
}

// TestPlanCompilesWithAVarFile proves internal/cli/plan.go's wiring: that
// opts.VarFiles is loaded and threaded into compiler.Options.FileVars, not
// just that loadVarFiles itself works in isolation. The assertion names the
// exact value, not a substring that could appear by coincidence — see the
// brief's own cautionary example, where asserting stdout contained "10" after
// --var replicas=10 passed even with --var completely disabled, because the
// fixture's cidr happened to contain "10" too.
func TestPlanCompilesWithAVarFile(t *testing.T) {
	dir := projectDir(t, `
project: myapp
variables:
  cidr:
    type: string
resources:
  network:
    type: fake.network
    cidr: ${cidr}
`)
	writeFile(t, dir, "vars.yml", "cidr: 10.77.0.0/16\n")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, VarFiles: []string{"vars.yml"}}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v (stderr: %s), want errChanges", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `cidr: "10.77.0.0/16"`) {
		t.Errorf("plan did not use the --var-file value:\n%s", stdout.String())
	}
}

// TestPlanVarFileOutranksEnvironmentConfiguration is the end-to-end version of
// TestFileEntryAtCLIScopeOutranksEnvironmentConfiguration
// (internal/variables/resolve_test.go): it exercises the same precedence
// claim through the real `infra plan` command rather than calling
// variables.Resolve directly, so it also proves plan.go's own wiring — not
// just Resolve's — honours the ordering.
func TestPlanVarFileOutranksEnvironmentConfiguration(t *testing.T) {
	dir := projectDir(t, `
project: myapp
variables:
  cidr:
    type: string
environments:
  production:
    cidr: 10.1.0.0/16
resources:
  network:
    type: fake.network
    cidr: ${cidr}
`)
	writeFile(t, dir, "vars.yml", "cidr: 10.77.0.0/16\n")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, VarFiles: []string{"vars.yml"}}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"production"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v (stderr: %s), want errChanges", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `cidr: "10.77.0.0/16"`) {
		t.Errorf("--var-file did not outrank the environment:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "10.1.0.0/16") {
		t.Errorf("the environment's value leaked into the plan despite --var-file:\n%s", stdout.String())
	}
}

// TestPlanAnnotatesVarFileAndVarWithTheirOwnSource is the end-to-end proof
// Amendment 6 (owner ruling, contract.md) exists because unit-level tests
// could not provide one.
//
// Amendment 5 tried to make a CLI-override annotation name its own source by
// reusing Value.Origin, and every test written for it — constructing a
// Value directly and rendering it — passed. The binary still regressed: a
// real plan reaches its resource attributes through "${cidr}" interpolation,
// and internal/expressions/eval.go's OpVarRef case re-origins every such
// reference to the referencing expression's site BEFORE the value reaches
// the renderer, which a test that never evaluates an expression cannot see.
// Amendment 6 replaced Origin with the dedicated Value.SuppliedBy field
// specifically because WithOrigin's overwrite cannot touch it — but that
// claim is only worth as much as a test that actually exercises the real
// path: compiled configuration with "${cidr}" in it, through
// variables.Resolve, through expressions.Evaluate, into a rendered plan.
func TestPlanAnnotatesVarFileAndVarWithTheirOwnSource(t *testing.T) {
	const body = `
project: myapp
variables:
  cidr:
    type: string
resources:
  network:
    type: fake.network
    cidr: ${cidr}
`
	run := func(t *testing.T, opts *GlobalOptions) string {
		t.Helper()
		cmd := newPlanCommand(opts)
		cmd.SetArgs([]string{"dev"})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); !errors.Is(err, errChanges) {
			t.Fatalf("Execute() = %v (stderr: %s), want errChanges", err, stderr.String())
		}
		return stdout.String()
	}

	t.Run("--var alone names the flag", func(t *testing.T) {
		dir := projectDir(t, body)
		opts := &GlobalOptions{Dir: dir, Parallelism: 4, Vars: []string{"cidr=10.88.0.0/16"}}
		out := run(t, opts)
		if !strings.Contains(out, `cidr: "10.88.0.0/16" [variable, from --var]`) {
			t.Errorf("--var did not render annotated with its own flag:\n%s", out)
		}
	})

	t.Run("--var-file alone names the file as typed", func(t *testing.T) {
		dir := projectDir(t, body)
		writeFile(t, dir, "vars.yml", "cidr: 10.77.0.0/16\n")
		opts := &GlobalOptions{Dir: dir, Parallelism: 4, VarFiles: []string{"vars.yml"}}
		out := run(t, opts)
		if !strings.Contains(out, `cidr: "10.77.0.0/16" [variable, from vars.yml]`) {
			t.Errorf("--var-file did not render annotated with its own path:\n%s", out)
		}
		// The exact defect Amendment 5 shipped: naming the DECLARATION file
		// (infra.yml) instead of the file that actually supplied the value.
		if strings.Contains(out, "infra.yml]") {
			t.Errorf("annotation named the declaration file instead of the --var-file:\n%s", out)
		}
	})

	t.Run("both together: --var still outranks --var-file and names itself", func(t *testing.T) {
		dir := projectDir(t, body)
		writeFile(t, dir, "vars.yml", "cidr: 10.77.0.0/16\n")
		opts := &GlobalOptions{
			Dir: dir, Parallelism: 4,
			Vars:     []string{"cidr=10.99.0.0/16"},
			VarFiles: []string{"vars.yml"},
		}
		out := run(t, opts)
		if !strings.Contains(out, `cidr: "10.99.0.0/16" [variable, from --var]`) {
			t.Errorf("--var did not outrank --var-file, or was not annotated with --var:\n%s", out)
		}
		if strings.Contains(out, "10.77.0.0/16") {
			t.Errorf("the --var-file value leaked into the plan despite --var outranking it:\n%s", out)
		}
		if strings.Contains(out, "from vars.yml") {
			t.Errorf("annotation named vars.yml even though --var supplied the winning value:\n%s", out)
		}
	})
}

// TestPlanRendersVarFileWarningsEvenWithoutErrors guards against gating the
// --var-file diagnostic render on fds.HasErrors(): DecodeVariableFile can
// produce a WARNING (a --var-file key shadowing one of infra.yml's own block
// names) with no error at all, and a plan that only renders fds on the error
// path would run to completion and silently drop it.
func TestPlanRendersVarFileWarningsEvenWithoutErrors(t *testing.T) {
	dir := projectDir(t, `
project: myapp
resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
`)
	// "resources" collides with one of infra.yml's own top-level blocks —
	// legal (it decodes as an ordinary variable named "resources"), but
	// DecodeVariableFile warns about it.
	writeFile(t, dir, "vars.yml", "resources: 3\n")

	opts := &GlobalOptions{Dir: dir, Parallelism: 4, VarFiles: []string{"vars.yml"}}
	cmd := newPlanCommand(opts)
	cmd.SetArgs([]string{"dev"})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, errChanges) {
		t.Fatalf("Execute() = %v (stderr: %s), want errChanges — a warning must not fail the plan", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "resources") {
		t.Errorf("the --var-file warning was not rendered:\nstderr: %s", stderr.String())
	}
}

func TestLoadVarFilesReportsAMissingFile(t *testing.T) {
	ds := func() string {
		_, ds := loadVarFiles(t.TempDir(), []string{"nope.yml"})
		return renderToString(ds)
	}()
	if ds == "" {
		t.Fatal("a --var-file that does not exist must be an error, not an empty layer")
	}
	if !strings.Contains(ds, "nope.yml") {
		t.Errorf("the diagnostic must name the path the user typed:\n%s", ds)
	}
}
