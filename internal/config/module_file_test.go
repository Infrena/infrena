package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

// writeModule writes one module.yml into a temp directory and loads it, so every
// test below goes through the same LoadModule path stage 5 will.
func writeModule(t *testing.T, body string) File {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ModuleFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadModule(dir)
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	return f
}

// TestDecodeModuleFile is the happy path over PLAN.md §11.3's own example.
//
// Every block is written in the WRONG order — "replicas" before
// "application_name", "service" before "cache", "url" before "endpoint" — so an
// implementation that preserves document order, or leaves order to a map, fails.
// A fixture already sorted would pass against code that does no sorting and would
// look tidier, which is why it is not used.
func TestDecodeModuleFile(t *testing.T) {
	f := writeModule(t, `
inputs:
  replicas:
    type: integer
    default: 1
  application_name:
    type: string

resources:
  service:
    type: test.database
    engine: postgres
    count: ${replicas}
  cache:
    type: test.network
    cidr: 10.0.0.0/16

outputs:
  url:
    value: ${service.endpoint}
  endpoint:
    value: ${cache.id}
`)
	got, ds := DecodeModule(f)
	requireNoErrors(t, ds)

	if got.Origin.File != f.Path {
		t.Errorf("Origin.File = %q, want %q: stage 5 resolves a nested source against its directory",
			got.Origin.File, f.Path)
	}

	if len(got.Inputs) != 2 {
		t.Fatalf("decoded %d inputs, want 2", len(got.Inputs))
	}
	if got.Inputs[0].Name != "application_name" || got.Inputs[1].Name != "replicas" {
		t.Errorf("inputs are not sorted by name: %s, %s", got.Inputs[0].Name, got.Inputs[1].Name)
	}
	if got.Inputs[1].Type != value.KindInt || !got.Inputs[1].HasDefault {
		t.Errorf("replicas = %+v, want an integer with a default", got.Inputs[1])
	}
	if n, _ := got.Inputs[1].Default.AsInt(); n != 1 {
		t.Errorf("replicas default = %v, want 1", got.Inputs[1].Default.Raw)
	}
	if got.Inputs[0].Type != value.KindString {
		t.Errorf("application_name type = %v, want string", got.Inputs[0].Type)
	}

	if len(got.Resources) != 2 {
		t.Fatalf("decoded %d resources, want 2", len(got.Resources))
	}
	if got.Resources[0].Name != "cache" || got.Resources[1].Name != "service" {
		t.Errorf("resources are not sorted by name: %s, %s", got.Resources[0].Name, got.Resources[1].Name)
	}
	if got.Resources[1].Type != "test.database" {
		t.Errorf("service type = %q", got.Resources[1].Type)
	}
	if !got.Resources[1].Attributes["count"].HasExpressions {
		t.Error("${replicas} inside a module resource was not flagged as an expression")
	}

	if len(got.Outputs) != 2 {
		t.Fatalf("decoded %d outputs, want 2", len(got.Outputs))
	}
	if got.Outputs[0].Name != "endpoint" || got.Outputs[1].Name != "url" {
		t.Errorf("outputs are not sorted by name: %s, %s", got.Outputs[0].Name, got.Outputs[1].Name)
	}
	if !got.Outputs[1].HasExpressions {
		t.Error("url's ${service.endpoint} was not flagged as an expression")
	}
	if s, _ := got.Outputs[1].Value.AsString(); s != "${service.endpoint}" {
		t.Errorf("url value = %q, want the text kept verbatim for stage 5 to parse", s)
	}
	// The value's own Origin is what a diagnostic about the EXPRESSION points
	// at; OutputDecl.Origin points at the output's name. Both are needed and
	// they are different lines, which is why OutputDecl carries no third field.
	if got.Outputs[1].Value.Origin.Line == got.Outputs[1].Origin.Line {
		t.Errorf("the value's origin (line %d) is the name's origin; a diagnostic about the expression would point at the wrong line",
			got.Outputs[1].Value.Origin.Line)
	}
}

// TestModuleInputDiagnosticsSayInputNotVariable is the discriminating test for
// the noun threading.
//
// The fixture reaches decodeVariable's "must specify at least" guard: an empty
// mapping is a valid mapping, so the body check passes, the key loop runs zero
// times, no type and no default are recorded, and nothing has already been
// reported — exactly the state that guard fires on.
//
// The negative assertions are the point. Without them this passes against code
// that says "variable "replicas" ... input ..." in one sentence.
func TestModuleInputDiagnosticsSayInputNotVariable(t *testing.T) {
	f := writeModule(t, `
inputs:
  replicas: {}
`)
	_, ds := DecodeModule(f)
	d := requireErrorAbout(t, ds, `input "replicas" must specify at least a `+"`type`")
	text := d.Summary + " | " + d.Detail + " | " + d.Action
	if strings.Contains(text, "variable") || strings.Contains(text, "Variable") {
		t.Errorf("a module input's diagnostic calls it a variable: %s", text)
	}
	if strings.Contains(text, VariablesFileName) {
		t.Errorf("a module input's diagnostic sends the user to %s, which cannot supply it: %s", VariablesFileName, text)
	}
	if !strings.Contains(text, "instantiates") {
		t.Errorf("the suggested action does not say where an input's value comes from: %s", text)
	}
}

// TestModuleInputTypeErrorsSayInput covers the other two places the noun appears,
// each with a fragment unique to its own diagnostic.
func TestModuleInputTypeErrorsSayInput(t *testing.T) {
	t.Run("unknown type", func(t *testing.T) {
		f := writeModule(t, "inputs:\n  replicas:\n    type: intger\n")
		_, ds := DecodeModule(f)
		d := requireErrorAbout(t, ds, `unknown input type "intger"`)
		if strings.Contains(d.Detail, "Variable") {
			t.Errorf("detail calls a module input a variable: %s", d.Detail)
		}
	})

	t.Run("bound on a non-numeric type", func(t *testing.T) {
		f := writeModule(t, "inputs:\n  name:\n    type: string\n    min: 3\n")
		_, ds := DecodeModule(f)
		d := requireErrorAbout(t, ds, "`min` is not valid on input \"name\"")
		if strings.Contains(d.Detail, "Variable") {
			t.Errorf("detail calls a module input a variable: %s", d.Detail)
		}
	})
}

// TestProjectVariableWordingIsUnchanged is the other half of the noun threading:
// the existing call site must produce byte-identical text.
//
// Asserted here rather than left to the untouched tests, because
// decode_variables_test.go does not assert on every string the threading touches,
// so a regression in one of the others would go unnoticed.
func TestProjectVariableWordingIsUnchanged(t *testing.T) {
	files := writeConfig(t, "project: myapp\n\nvariables:\n  replicas: {}\n")
	_, ds := Decode(files)
	d := requireErrorAbout(t, ds, `variable "replicas" must specify at least a `+"`type`")
	if !strings.Contains(d.Action, VariablesFileName) {
		t.Errorf("a project variable's action no longer names %s: %s", VariablesFileName, d.Action)
	}
}

// TestModuleFileRejectsProjectLevelKeysAsUnknownKeys pins Amendment 1's
// structural argument and PLAN.md §11.3's rule: the module decoder has no case
// for `project`, `environments` or `variables`, so each falls out of the
// unknown-key diagnostic — which names the key and the line, and needs no bespoke
// rule.
//
// The fragment quotes the key, so no case can be satisfied by a sibling's
// diagnostic. The detail assertion is what makes the message useful rather than
// merely present.
func TestModuleFileRejectsProjectLevelKeysAsUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"project":      "project: myapp\n",
		"variables":    "variables:\n  region:\n    type: string\n",
		"environments": "environments:\n  dev: {}\n",
	}
	for key, body := range cases {
		t.Run(key, func(t *testing.T) {
			f := writeModule(t, body)
			_, ds := DecodeModule(f)
			d := requireErrorAbout(t, ds, "unknown key "+`"`+key+`"`+" in module file")
			if !strings.Contains(d.Detail, "`inputs`") || !strings.Contains(d.Detail, "`outputs`") {
				t.Errorf("detail does not name what a module file does declare: %s", d.Detail)
			}
		})
	}
}

// TestModuleFileAcceptsNestedModules. Spec §7.2 bounds recursion at 32
// instantiations rather than forbidding it and §11.3 says a module may declare
// `modules:`, so a module file carries a list of its own, decoded by Task 2's
// decoder rather than a second one. Without this, depth is always 1 and Ruling 6's
// depth bound and cycle detector have nothing to detect.
//
// The fixture writes "zeta" before "alpha" so the sort is exercised rather than
// assumed.
func TestModuleFileAcceptsNestedModules(t *testing.T) {
	f := writeModule(t, `
resources:
  outer:
    type: test.network
    cidr: 10.0.0.0/16

modules:
  - ./zeta
  - name: alpha
    source: ./inner/app-stack
`)
	got, ds := DecodeModule(f)
	requireNoErrors(t, ds)

	if len(got.Modules) != 2 {
		t.Fatalf("decoded %d nested modules, want 2", len(got.Modules))
	}
	if got.Modules[0].Name != "alpha" || got.Modules[1].Name != "zeta" {
		t.Errorf("nested modules are not sorted by name: %s, %s", got.Modules[0].Name, got.Modules[1].Name)
	}
	if got.Modules[0].Source.Location != "./inner/app-stack" {
		t.Errorf("nested location = %q", got.Modules[0].Source.Location)
	}
}

// TestNestedModuleNameCollisionIsAnError. The collision rule applies at every
// level of nesting, which is why DecodeModule keeps its own `seen` map rather
// than sharing the project's.
func TestNestedModuleNameCollisionIsAnError(t *testing.T) {
	f := writeModule(t, `
modules:
  - ./one/net
  - ./two/net
`)
	_, ds := DecodeModule(f)
	requireErrorAbout(t, ds, `two modules are both named "net"`, "./one/net", "./two/net")
}

// TestOutputShapeErrors. Each fragment is unique to its diagnostic.
func TestOutputShapeErrors(t *testing.T) {
	t.Run("outputs is a list", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  - endpoint\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, "`outputs` must be a mapping of output name to declaration")
	})

	t.Run("output body is a scalar", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint: ${service.endpoint}\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, `output "endpoint" must be a mapping`)
	})

	t.Run("no value", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    description: the endpoint\n")
		_, ds := DecodeModule(f)
		// TWO errors here — the unknown key and the missing value — so assert on
		// the set rather than through requireErrorAbout, which insists on
		// exactly one. Both are wanted: silencing the second would leave a user
		// who typo'd `value` as `description` with no statement that the output
		// publishes nothing.
		var summaries []string
		for _, d := range ds {
			summaries = append(summaries, d.Summary)
		}
		joined := strings.Join(summaries, " | ")
		for _, want := range []string{
			`output "endpoint" has no ` + "`value`",
			`unknown key "description" in output "endpoint"`,
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("diagnostics do not mention %q: %s", want, joined)
			}
		}
	})

	t.Run("duplicate output", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    value: ${a.b}\n  endpoint:\n    value: ${c.d}\n")
		_, ds := DecodeModule(f)
		// The body has no leading newline, so the first `endpoint:` is line 2.
		requireErrorAbout(t, ds, `output "endpoint" is declared more than once`, "line 2")
	})
}

// TestBareOutputReferenceIsRefused checks all THREE answers, because a guard that
// only ever rejects is as wrong as one that never does.
//
// Contract Amendment 4e: `${...}` is this language's only reference syntax, with
// `$${` as its escape, so a bare scalar is a literal everywhere else. Decoded as
// an ordinary value, `service.endpoint` is that literal string and the module
// would publish it to its caller with nothing printed; read as a reference, every
// dotted literal in an output becomes ambiguous.
func TestBareOutputReferenceIsRefused(t *testing.T) {
	t.Run("bare dotted name is refused", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    value: service.endpoint\n")
		_, ds := DecodeModule(f)
		d := requireErrorAbout(t, ds, `output "endpoint" names "service.endpoint" without `+"`${...}`")
		if !strings.Contains(d.Action, "${service.endpoint}") {
			t.Errorf("action does not give the reference spelling: %s", d.Action)
		}
		if !strings.Contains(d.Action, `"service.endpoint"`) {
			t.Errorf("action does not give the literal spelling: %s", d.Action)
		}
	})

	t.Run("quoted means the literal", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  endpoint:\n    value: \"service.endpoint\"\n")
		got, ds := DecodeModule(f)
		requireNoErrors(t, ds)
		if s, _ := got.Outputs[0].Value.AsString(); s != "service.endpoint" {
			t.Errorf("quoted output = %q", s)
		}
	})

	t.Run("an undotted literal is fine", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  tier:\n    value: production\n")
		got, ds := DecodeModule(f)
		requireNoErrors(t, ds)
		if s, _ := got.Outputs[0].Value.AsString(); s != "production" {
			t.Errorf("literal output = %q", s)
		}
	})

	t.Run("a non-string literal is fine", func(t *testing.T) {
		f := writeModule(t, "outputs:\n  replicas:\n    value: 3\n")
		got, ds := DecodeModule(f)
		requireNoErrors(t, ds)
		if n, _ := got.Outputs[0].Value.AsInt(); n != 3 {
			t.Errorf("integer output = %v", got.Outputs[0].Value.Raw)
		}
	})
}

// TestEmptyAndMalformedModuleFiles.
func TestEmptyAndMalformedModuleFiles(t *testing.T) {
	// The empty case deliberately does NOT use requireErrorAbout: that helper
	// also asserts the diagnostic has a line number, and an empty file has no
	// node tree to take one from — Decode's own "configuration file is empty" has
	// the same shape.
	t.Run("empty", func(t *testing.T) {
		f := writeModule(t, "")
		_, ds := DecodeModule(f)
		if !ds.HasErrors() {
			t.Fatal("an empty module file produced no error")
		}
		var found bool
		for _, d := range ds {
			if strings.Contains(d.Summary, "module file is empty") {
				found = true
				if d.Origin.File != f.Path {
					t.Errorf("diagnostic names %q, want %q", d.Origin.File, f.Path)
				}
			}
		}
		if !found {
			t.Errorf("no \"module file is empty\" diagnostic: %v", errorSummaries(ds))
		}
	})

	t.Run("top level is a list", func(t *testing.T) {
		f := writeModule(t, "- inputs\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, "module file must be a mapping", ModuleFileName)
	})

	t.Run("a block declared twice", func(t *testing.T) {
		f := writeModule(t, "resources:\n  a:\n    type: test.network\noutputs:\n  x:\n    value: ${a.id}\nresources:\n  b:\n    type: test.network\n")
		_, ds := DecodeModule(f)
		requireErrorAbout(t, ds, "`resources` is declared more than once in this module file", "line 1")
	})
}

// TestLoadModuleDiagnoses covers the three ways a source directory fails to name
// a module, each of which reads identically as a bare "no such file" and needs
// telling apart. These are errors rather than diagnostics for Load's own reason:
// a file that did not load has no node tree, so there is no Origin to point at.
func TestLoadModuleDiagnoses(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		_, err := LoadModule(filepath.Join(t.TempDir(), "absent"))
		if err == nil {
			t.Fatal("want an error for a source that does not exist")
		}
		if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), ModuleFileName) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("source is a file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "net.yml")
		if err := os.WriteFile(path, []byte("resources: {}\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadModule(path)
		if err == nil {
			t.Fatal("want an error for a source that is a file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("infra.yml where module.yml was expected", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ProjectFileName), []byte("project: x\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadModule(dir)
		if err == nil {
			t.Fatal("want an error when the directory holds infra.yml instead")
		}
		if !strings.Contains(err.Error(), "Rename") || !strings.Contains(err.Error(), ModuleFileName) {
			t.Errorf("error = %v; it must say how to turn this into a module", err)
		}
	})
}
