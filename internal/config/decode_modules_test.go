package config

import (
	"strconv"
	"strings"
	"testing"
)

// TestDecodeModulesList is the happy path.
//
// The entries are written "zeta" before "alpha" on purpose. A fixture already in
// sorted order passes against code that does no sorting at all — and it looks
// tidier, which is exactly why it gets written by mistake.
func TestDecodeModulesList(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - ./modules/zeta
  - ./modules/alpha
  - https://github.com/acme/infra-app-stack:v1.2.0
  - name: app_stack_v2
    source: https://github.com/other/infra-app-stack:v2.0.0
`)
	got, ds := Decode(files)
	requireNoErrors(t, ds)

	if len(got.Modules) != 4 {
		t.Fatalf("decoded %d modules, want 4", len(got.Modules))
	}
	names := make([]string, len(got.Modules))
	for i, m := range got.Modules {
		names[i] = m.Name
	}
	want := []string{"alpha", "app_stack_v2", "infra_app_stack", "zeta"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("modules are not sorted by name: got %v, want %v", names, want)
		}
	}

	byName := map[string]ModuleLoadDecl{}
	for _, m := range got.Modules {
		byName[m.Name] = m
	}
	if byName["alpha"].Source.Location != "./modules/alpha" {
		t.Errorf("alpha location = %q", byName["alpha"].Source.Location)
	}
	// The pin is kept SEPARATELY from the location. A parsed source whose Ref
	// were folded back into Location would send stage 5's cache the tag as part
	// of the path, and .infrena/modules keys on the two independently.
	stack := byName["infra_app_stack"]
	if stack.Source.Location != "https://github.com/acme/infra-app-stack" || stack.Source.Ref != "v1.2.0" {
		t.Errorf("derived-name entry parsed to %+v, want location without the pin and Ref \"v1.2.0\"", stack.Source)
	}
	if byName["app_stack_v2"].Source.Ref != "v2.0.0" {
		t.Errorf("named entry ref = %q", byName["app_stack_v2"].Source.Ref)
	}
	for _, m := range got.Modules {
		// ModuleLoadDecl has no SourceOrigin: source.Source carries its own,
		// stamped from the origin stage 2 passed to Parse. If this fails,
		// Parse is not stamping it and the field cannot be dropped — say so
		// rather than adding a second copy back.
		if m.Source.Origin.Line == 0 {
			t.Errorf("module %q: source.Source.Origin has no line; stage 5's fetch diagnostics point at it", m.Name)
		}
		if m.Origin.Line == 0 {
			t.Errorf("module %q has no Origin line", m.Name)
		}
	}

	// Loading is not instantiating. A `modules:` entry must not appear as a
	// resource, or the plan would propose a resource with no type.
	if len(got.Resources) != 0 {
		t.Errorf("decoded %d resources from a file with none", len(got.Resources))
	}
}

// TestUndeivableNameIsStage2sHalfOfTheDerivationRule.
//
// The derivation rule itself, and its table, belong to source.DeriveName and
// must not be re-tested here: a second table is how two implementations of one
// rule stay green while drifting apart.
//
// What IS stage 2's is the plumbing: that DeriveName's refusal reaches the user
// as a diagnostic naming the `name:` form, and that the entry is DROPPED. The
// second assertion is the load-bearing one. A refusal test that checks only the
// message passes against code that prints the message and keeps the entry
// anyway, leaving a module with an empty or mangled name in ProjectDecl for
// anything downstream that does not check errors first.
//
// The fixture has to REACH derivation. A source that source.Parse rejects never
// gets there — parseSource returns early so Parse's own diagnostic is not
// followed by a second one about the same entry — so this uses a path, which
// Parse accepts, whose last segment is not an identifier.
func TestUndeivableNameIsStage2sHalfOfTheDerivationRule(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - ./modules/my.module
`)
	got, ds := Decode(files)
	requireErrorAbout(t, ds, "name:")
	if len(got.Modules) != 0 {
		t.Errorf("an entry whose name could not be derived was still decoded: %+v", got.Modules[0])
	}
}

// TestModulesMustBeAList pins the shape asymmetry, and the diagnostic has to
// EXPLAIN it.
//
// `resources:`, `variables:` and `environments:` are all mappings, so the
// obvious reading of a `modules:` mapping error is "I mistyped something". The
// detail says why this one is a list: a mapping's key would be the name, and the
// name is optional. The fixture is the shape every other IaC tool uses, so it
// is the mistake a real user makes.
func TestModulesMustBeAList(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  networking:
    source: ./modules/networking
`)
	_, ds := Decode(files)
	d := requireErrorAbout(t, ds, "`modules` must be a list of module sources")
	if !strings.Contains(d.Detail, "derived") {
		t.Errorf("detail does not say why `modules` is a list rather than a mapping: %s", d.Detail)
	}
	if !strings.Contains(d.Action, "- ./modules/") {
		t.Errorf("action does not show the list spelling: %s", d.Action)
	}
}

// TestTwoEntriesDerivingTheSameNameIsAnError is the collision rule, and the
// assertions are what stop it being resolved by order.
//
// Both entries must be named — a diagnostic that names only the second tells the
// user half of what they need — and NEITHER may survive into got.Modules. A
// "last one wins" implementation passes an error-count assertion and still
// silently instantiates the wrong module, so the count of decoded modules is the
// assertion that catches it.
func TestTwoEntriesDerivingTheSameNameIsAnError(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - https://github.com/acme/infra-app-stack:v1.2.0
  - https://github.com/other/infra-app-stack:v2.0.0
`)
	got, ds := Decode(files)
	d := requireErrorAbout(t, ds,
		`two modules are both named "infra_app_stack"`,
		"github.com/acme/infra-app-stack:v1.2.0",
		"github.com/other/infra-app-stack:v2.0.0",
		"name:")
	if d.Origin.Line != 6 {
		t.Errorf("diagnostic points at line %d, want the SECOND entry at line 6", d.Origin.Line)
	}
	for _, m := range got.Modules {
		if m.Name == "infra_app_stack" {
			t.Errorf("a colliding module survived decoding as %+v; "+
				"resolving a collision by order is what the `name:` form exists to prevent", m)
		}
	}
}

// TestModuleEntryUnknownKeyIsAnError. `inputs` gets its own assertion because it
// is the migration trap: loading no longer takes inputs, and a user moving from
// the older spelling will write them here. The action has to say where they go.
func TestModuleEntryUnknownKeyIsAnError(t *testing.T) {
	t.Run("inputs", func(t *testing.T) {
		files := writeConfig(t, `
project: myapp

modules:
  - name: app
    source: ./modules/app
    inputs:
      replicas: 3
`)
		_, ds := Decode(files)
		d := requireErrorAbout(t, ds, `unknown key "inputs" in `+"`modules`"+`[0]`)
		if !strings.Contains(d.Action, "module.app") {
			t.Errorf("action does not say where inputs go — on the resource that instantiates the module: %s", d.Action)
		}
	})

	t.Run("typo", func(t *testing.T) {
		files := writeConfig(t, `
project: myapp

modules:
  - name: app
    source: ./modules/app
    sorce: ./oops
`)
		_, ds := Decode(files)
		requireErrorAbout(t, ds, `unknown key "sorce" in `+"`modules`"+`[0]`, "`name` and `source`")
	})
}

// TestModuleEntryWithoutSourceIsAnError.
func TestModuleEntryWithoutSourceIsAnError(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - name: app
`)
	_, ds := Decode(files)
	requireErrorAbout(t, ds, "`modules`[0] has no `source`", "source: ./modules/app")
}

// TestExplicitNameMustBeAnIdentifier. A module's name becomes half of a resource
// type, `module.<name>`, so a name containing a dot would produce a type stage 5
// reads as a path into a module.
func TestExplicitNameMustBeAnIdentifier(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - name: app.stack
    source: ./modules/app
`)
	_, ds := Decode(files)
	requireErrorAbout(t, ds, `module name "app.stack" is not a valid identifier`, "module.app.stack")
}

// TestModuleTypeSpelling covers both malformed spellings AND the accepted one.
//
// The accepted case is what stops this being a guard that rejects everything: a
// check refusing both malformed spellings and also refusing `module.app_stack`
// passes both rejection assertions and makes modules uninstantiable.
func TestModuleTypeSpelling(t *testing.T) {
	t.Run("no module name", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: module.\n")
		_, ds := Decode(files)
		requireErrorAbout(t, ds, `resource "prod" has type "module." with no module name`, "module.app_stack")
	})

	t.Run("path into a module", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: module.app.database\n")
		_, ds := Decode(files)
		d := requireErrorAbout(t, ds, `resource "prod" names a path into a module, not a module`)
		if !strings.Contains(d.Action, "type: module.app") {
			t.Errorf("action does not give the corrected type: %s", d.Action)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: module.app_stack\n    replicas: 3\n")
		got, ds := Decode(files)
		requireNoErrors(t, ds)
		if got.Resources[0].Type != "module.app_stack" {
			t.Errorf("Type = %q", got.Resources[0].Type)
		}
		// A caller's input is an ordinary attribute. If it were anything else,
		// stage 6's bindAttribute would not see it and its provenance would have
		// to be re-derived.
		attr, ok := got.Resources[0].Attributes["replicas"]
		if !ok {
			t.Fatalf("replicas is not an attribute: %+v", got.Resources[0].Attributes)
		}
		if n, _ := attr.Value.AsInt(); n != 3 {
			t.Errorf("replicas = %v", attr.Value.Raw)
		}
	})

	t.Run("a provider type containing module is untouched", func(t *testing.T) {
		// "modulefoo.thing" does not begin with "module." and must not be
		// caught by a prefix check written as strings.Contains or as a check on
		// the first segment alone.
		files := writeConfig(t, "project: myapp\n\nresources:\n  prod:\n    type: modulefoo.thing\n")
		got, ds := Decode(files)
		requireNoErrors(t, ds)
		if got.Resources[0].Type != "modulefoo.thing" {
			t.Errorf("Type = %q", got.Resources[0].Type)
		}
	})
}

// TestResourceNameWithADotIsRefused. A resource literally named `prod.database`
// writes the state key "module.prod.database", byte for byte what the resource
// `database` inside module instance `prod` writes, because Address.String()
// returns Name verbatim when Module is empty. The assertion that the resource is NOT decoded is the
// one that matters: a diagnostic alone would still leave the colliding decl in
// the set for anything downstream that does not check for errors first.
func TestResourceNameWithADotIsRefused(t *testing.T) {
	files := writeConfig(t, `
project: myapp

resources:
  module.prod.database:
    type: fake.network
    cidr: 10.0.0.0/16
`)
	got, ds := Decode(files)
	d := requireErrorAbout(t, ds, `resource name "module.prod.database" contains a dot`, "state")
	if !strings.Contains(d.Action, "module_prod_database") {
		t.Errorf("action does not offer a usable replacement: %s", d.Action)
	}
	if len(got.Resources) != 0 {
		t.Errorf("a resource with a colliding address name was still decoded: %+v", got.Resources[0])
	}
}

// TestResourceNameMustBeAnIdentifier checks BOTH answers.
//
// The accepted half is not decoration: this guard runs on every resource in
// every configuration, so one that is even slightly too strict breaks real
// projects. Hyphens and leading underscores are in the accept list because both
// are ordinary in real names and neither can collide with an address.
func TestResourceNameMustBeAnIdentifier(t *testing.T) {
	for _, name := range []string{"9lives", "my name", "web!", ""} {
		t.Run("rejected/"+name, func(t *testing.T) {
			files := writeConfig(t, "project: myapp\n\nresources:\n  "+strconv.Quote(name)+":\n    type: fake.network\n")
			_, ds := Decode(files)
			requireErrorAbout(t, ds, "is not a valid identifier")
		})
	}

	for _, name := range []string{"web", "web_2", "my-app", "_internal", "A"} {
		t.Run("accepted/"+name, func(t *testing.T) {
			files := writeConfig(t, "project: myapp\n\nresources:\n  "+name+":\n    type: fake.network\n")
			got, ds := Decode(files)
			requireNoErrors(t, ds)
			if len(got.Resources) != 1 || got.Resources[0].Name != name {
				t.Errorf("decoded %+v, want one resource named %q", got.Resources, name)
			}
		})
	}
}

// TestModulesIsNoLongerAnUnrecognisedTopLevelKey. The fragment quotes the key,
// because decodeDocument's default arm emits the same sentence for every
// unrecognised key.
func TestModulesIsNoLongerAnUnrecognisedTopLevelKey(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - ./modules/networking
`)
	got, ds := Decode(files)
	for _, d := range ds {
		if strings.Contains(d.Summary, `unrecognised top-level key "modules"`) {
			t.Fatalf("`modules:` is still being ignored with a warning: %s", d.Summary)
		}
	}
	requireNoErrors(t, ds)
	if len(got.Modules) != 1 || got.Modules[0].Name != "networking" {
		t.Fatalf("decoded %+v, want one module named networking", got.Modules)
	}
}

// TestModuleEntryShapeErrors covers the entry shapes requireScalar handles, so
// an alias or a null does not silently become a module named after an anchor.
func TestModuleEntryShapeErrors(t *testing.T) {
	t.Run("null entry", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nmodules:\n  -\n")
		_, ds := Decode(files)
		requireErrorAbout(t, ds, "`modules`[0] has no value")
	})

	t.Run("nested list", func(t *testing.T) {
		files := writeConfig(t, "project: myapp\n\nmodules:\n  - - ./modules/net\n")
		_, ds := Decode(files)
		requireErrorAbout(t, ds, "`modules`[0] must be a single value, not a list or a mapping")
	})
}

// TestDuplicateExplicitNameIsAnError — the collision rule applies to names
// written with `name:` just as it does to derived ones.
func TestDuplicateExplicitNameIsAnError(t *testing.T) {
	files := writeConfig(t, `
project: myapp

modules:
  - name: app
    source: ./modules/one
  - name: app
    source: ./modules/two
`)
	_, ds := Decode(files)
	requireErrorAbout(t, ds, `two modules are both named "app"`, "./modules/one", "./modules/two", "name:")
}
