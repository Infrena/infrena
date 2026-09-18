package modules

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/variables"
	"github.com/infrena/infrena/pkg/value"
)

// scopeOf returns the scope the named resource was instantiated at.
func scopeOf(t *testing.T, exp *Expansion, name string) *Scope {
	t.Helper()
	for _, inst := range exp.Instances {
		if inst.Decl.Name == name {
			return inst.Scope
		}
	}
	t.Fatalf("no instance named %q in %d instances", name, len(exp.Instances))
	return nil
}

// callerVars builds a root variable scope with one variable resolved, standing
// in for what stage 4 hands stage 5.
func callerVars(name string, v value.Value) variables.Scope {
	var s variables.Scope
	s.Override(name, v)
	s.Override("environment", value.String("dev", value.SourceEnvironment).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("the environment argument"))
	return s
}

const moduleWithReplicas = `
inputs:
  replicas:
    type: integer
    default: 1
resources:
  worker:
    type: test.thing
`

func TestCallersInputBeatsTheModulesDeclaredDefault(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicas: 7
`,
		"m/module.yml": moduleWithReplicas,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, ok := scopeOf(t, exp, "worker").Variable("replicas")
	if !ok {
		t.Fatal("the module's input must be visible as a variable inside it")
	}
	if n, _ := got.AsInt(); n != 7 {
		t.Errorf("replicas = %v, want 7 — the caller's explicit value, not the default", got.Raw)
	}
	if got.Scope == value.ScopeModuleDefault {
		t.Error("a caller's explicit input must NOT be stamped ScopeModuleDefault; that rung " +
			"is the module's own default, and inverting the two inverts \"explicit config " +
			"always wins over an implicit default\"")
	}
}

func TestUnsuppliedInputTakesTheDeclaredDefaultAtScopeModuleDefault(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":    "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": moduleWithReplicas,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, ok := scopeOf(t, exp, "worker").Variable("replicas")
	if !ok {
		t.Fatal("an unsupplied input with a default must still be in scope")
	}
	if n, _ := got.AsInt(); n != 1 {
		t.Errorf("replicas = %v, want the declared default 1", got.Raw)
	}
	// The rung reserved since M4 Task 1 and unpopulated until now.
	if got.Scope != value.ScopeModuleDefault {
		t.Errorf("replicas scope = %v, want ScopeModuleDefault — the module's own default is "+
			"the ONLY thing that fills that rung", got.Scope)
	}
	if got.Source != value.SourceDefault {
		t.Errorf("replicas source = %v, want SourceDefault", got.Source)
	}
}

// Amendment 8b: stage 5 keeps whatever provenance evaluation produced. Deleted
// from 5b was the re-stamping, not the evaluation.
func TestCallerSuppliedInputKeepsTheProvenanceOfWhatSuppliedIt(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicas: ${var.count}
`,
		"m/module.yml": moduleWithReplicas,
	})

	vars := callerVars("count", value.Int(9, value.SourceVariable).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("--var"))

	exp, ds := Expand(decl, vars, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, _ := scopeOf(t, exp, "worker").Variable("replicas")
	if n, _ := got.AsInt(); n != 9 {
		t.Errorf("replicas = %v, want 9", got.Raw)
	}
	if got.Scope != value.ScopeCLIOverride {
		t.Errorf("replicas scope = %v, want ScopeCLIOverride — a module boundary is a point "+
			"where it is tempting to re-derive provenance, and the rule is that provenance "+
			"is recorded where a value ENTERS, not where it is passed along", got.Scope)
	}
	if value.ScopeLabel(got) != "--var" {
		t.Errorf("ScopeLabel = %q, want %q", value.ScopeLabel(got), "--var")
	}
}

// The ordering fixture for this task. `size` is bound in BOTH scopes, to
// different values, so the two candidate implementations disagree and the
// assertion can tell them apart. A fixture where the name existed in only one
// scope would pass against either.
func TestInputIsEvaluatedInTheCallersScopeNotTheModules(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    chosen: ${var.size}
`,
		"m/module.yml": `
inputs:
  size:
    type: string
    default: from-the-module
  chosen:
    type: string
resources:
  worker:
    type: test.thing
`,
	})

	vars := callerVars("size", value.String("from-the-caller", value.SourceVariable).
		WithScope(value.ScopeBaseConfig))

	exp, ds := Expand(decl, vars, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got, _ := scopeOf(t, exp, "worker").Variable("chosen")
	s, _ := got.AsString()
	if s == "from-the-module" {
		t.Fatal("the call's attributes were evaluated in the MODULE's scope; they are written " +
			"at the call site and name things at the call site")
	}
	if s != "from-the-caller" {
		t.Fatalf("chosen = %q, want %q", s, "from-the-caller")
	}
}

func TestModuleDoesNotSeeTheCallersVariables(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":    "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": moduleWithReplicas,
	})

	vars := callerVars("caller_only", value.String("x", value.SourceVariable))

	exp, ds := Expand(decl, vars, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	scope := scopeOf(t, exp, "worker")

	if _, ok := scope.Variable("caller_only"); ok {
		t.Error("a module must not see the caller's variables; its requirements would then be " +
			"invisible at the call site and it would break when moved to another project")
	}
	if _, ok := scope.Variable("environment"); !ok {
		t.Error("a module must see `environment`: it is a fact about the invocation, not the " +
			"caller's configuration, and a --var cannot even change it (§11.3)")
	}
}

func TestUndeclaredAttributeOnAModuleCallIsRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicase: 3
`,
		"m/module.yml": moduleWithReplicas,
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if !hasFragment(ds, "module \"m\" has no input \"replicase\"") {
		t.Errorf("a typo'd input must be refused, not silently dropped in favour of the "+
			"default; got %+v", ds)
	}
	if !hasFragment(ds, "replicas") {
		t.Errorf("the diagnostic must list the inputs the module does declare; got %+v", ds)
	}
}

func TestRequiredInputWithNoValueAndNoDefaultIsRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": `
inputs:
  image:
    type: string
resources:
  worker:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if !hasFragment(ds, "module \"app\" requires input \"image\"") {
		t.Errorf("an input with no default and no value must be refused; got %+v", ds)
	}

	// Reported AND resolved to a poison value (§7.4), so that every expression
	// reading it does not ALSO report "undefined variable".
	got, ok := scopeOf(t, exp, "worker").Variable("image")
	if !ok {
		t.Fatal("a missing required input must still leave a value of the right shape in " +
			"scope, or one mistake is reported once per use site")
	}
	if got.Known {
		t.Error("nothing supplied it, so there is nothing to know")
	}
	if got.Kind != value.KindString {
		t.Errorf("kind = %v, want the DECLARED kind", got.Kind)
	}
}

// Contract Amendment 2. The declared kind is the whole point: it makes
// value.Coerce's numeric cross-kind branch reachable, and it keeps an unset
// input behaving like an unset variable.
func TestUnsetInputCarriesItsDeclaredKindNotKindString(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m\nresources:\n  app:\n    type: module.m\n",
		"m/module.yml": `
inputs:
  count:
    type: integer
resources:
  worker:
    type: test.thing
`,
	})

	exp, _ := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	got, ok := scopeOf(t, exp, "worker").Variable("count")
	if !ok {
		t.Fatal("count is not in scope")
	}
	if got.Kind != value.KindInt {
		t.Errorf("kind = %v, want KindInt. An unknown STRING here would discard the one piece "+
			"of type information `type: integer` exists to carry, and would leave "+
			"value.Coerce's unknown branch unreachable — it is guarded by a numeric "+
			"cross-kind case", got.Kind)
	}
	if got.Raw != nil {
		t.Error("Value's contract is that Raw is nil when Known is false")
	}
}

func TestInputIsTypeCheckedThroughTheVariableSchema(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  app:
    type: module.m
    replicas: plenty
`,
		"m/module.yml": moduleWithReplicas,
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if !ds.HasErrors() {
		t.Fatal("a string supplied for an integer input must be refused")
	}
	// variables.Schema.Validate's own wording, with the noun threaded. The
	// fragment asserts both halves at once: that no second type checker was
	// built (the sentence is Validate's), and that the message names what the
	// user actually wrote.
	if !hasFragment(ds, `input "replicas" must be an integer`) {
		t.Errorf("the input must be checked by variables.Schema and reported as an INPUT; got %+v", ds)
	}
	for _, d := range ds {
		if strings.Contains(d.Summary, `variable "replicas"`) {
			t.Errorf("the diagnostic calls it a variable (%q). The user wrote an attribute on a "+
				"module call; naming the wrong construct is spec §44 failing at the last hop, "+
				"after every stage upstream got it right", d.Summary)
		}
		// A module input literal carries Scope=unset, and this validator renders
		// ScopeLabel into a sentence. Step 5.3 is what stops "The value supplied
		// by unset is a string" — a clause naming the ABSENCE of provenance as
		// though it were a source the user could act on.
		if strings.Contains(d.Detail, "unset") {
			t.Errorf("Detail = %q names an unset scope; a module input is the first value to "+
				"reach this validator without a stamped scope", d.Detail)
		}
	}
}

// TestAWholeResourceReferenceAsAModuleInputIsRefused pins fix round 1's
// Critical: a module input declares a TYPE (§9), not a relationship, so there
// is no attribute declaration to project a bare ${net} against the way
// stage 6's bindAttribute does. Before this fix, evaluateCall handed the
// unresolved, empty-attribute expression straight to expressions.Evaluate,
// which returned an Unknown carrying it and no diagnostic — the module's
// input, and everything inside it built from that input, would be created
// with the attribute silently unset.
func TestAWholeResourceReferenceAsAModuleInputIsRefused(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  net:
    type: fake.vpc
  app:
    type: module.m
    vpc: ${net}
`,
		"m/module.yml": `
inputs:
  vpc:
    type: string
resources:
  sub:
    type: fake.subnet
`,
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if !ds.HasErrors() {
		t.Fatal("a whole-resource reference passed as a module input must be refused: a module " +
			"input declares a type, not a relationship, so there is nothing to project against")
	}
	var out strings.Builder
	ds.Render(&out)
	if !strings.Contains(out.String(), "${net.") {
		t.Errorf("diagnostic must show naming an attribute explicitly as the fix:\n%s", out.String())
	}
}

// TestAModuleInputNamingAnAttributeStillWorks pins the cost of the refusal
// above at nil: a module input naming an attribute explicitly, which is the
// only form that ever worked, keeps working.
func TestAModuleInputNamingAnAttributeStillWorks(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  net:
    type: fake.vpc
  app:
    type: module.m
    vpc: ${net.id}
`,
		"m/module.yml": `
inputs:
  vpc:
    type: string
resources:
  sub:
    type: fake.subnet
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{}, nil, nil)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	got, ok := scopeOf(t, exp, "sub").Variable("vpc")
	if !ok {
		t.Fatal("the module's input must be visible as a variable inside it")
	}
	if got.Known {
		t.Error("a reference to a not-yet-created resource's attribute must be unknown, not known")
	}
}
