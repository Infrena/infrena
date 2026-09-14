package modules

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/internal/modules/source"
	"github.com/infrena/infrena/internal/variables"
)

// Amendment 18: stage 5 COLLECTS resolutions and never writes them.
//
// fakeGit stands in for a resolved remote, mapping each source to a directory
// the fixture already wrote. It exists because `paths` deliberately refuses a
// non-path source, and because nothing about collection can be tested with
// local paths — a local path has no revision to pin and is skipped.
type fakeGit struct{ dirs map[string]string }

func (f fakeGit) Resolve(s source.Source, baseDir string) (source.Resolution, diag.Diagnostics) {
	var ds diag.Diagnostics
	dir, ok := f.dirs[s.String()]
	if !ok {
		ds.Add(diag.Diagnostic{Severity: diag.SeverityError, Summary: "no fixture for " + s.String()})
		return source.Resolution{}, ds
	}
	return source.Resolution{Dir: filepath.Join(baseDir, dir), Commit: "c-" + dir}, ds
}

func TestResolutionsAreCollectedDeduplicatedAndSorted(t *testing.T) {
	// The fixture CONTRADICTS the expected output twice over. `zeta` is
	// declared first and sorts last; and it is declared TWICE, under two names,
	// so an implementation that appended per Resolve call produces three
	// entries in declaration order. Only dedup-by-source plus one sort gives
	// [alpha, zeta].
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - name: z1
    source: https://github.com/acme/zeta:v1.0.0
  - name: a1
    source: https://github.com/acme/alpha:v1.0.0
  - name: z2
    source: https://github.com/acme/zeta:v1.0.0
resources:
  ca:
    type: module.a1
  cz:
    type: module.z1
  cz2:
    type: module.z2
`,
		"zeta/module.yml":  "resources:\n  z:\n    type: test.thing\n",
		"alpha/module.yml": "resources:\n  a:\n    type: test.thing\n",
	})

	git := fakeGit{dirs: map[string]string{
		"https://github.com/acme/zeta:v1.0.0":  "zeta",
		"https://github.com/acme/alpha:v1.0.0": "alpha",
	}}

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, git)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var got []string
	for _, r := range exp.Resolutions {
		got = append(got, r.Source.String()+"="+r.Resolution.Commit)
	}
	want := []string{
		"https://github.com/acme/alpha:v1.0.0=c-alpha",
		"https://github.com/acme/zeta:v1.0.0=c-zeta",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Resolutions = %v, want %v — keyed by SOURCE identity, so one source loaded "+
			"under two names is one entry, and sorted once because modules.lock is compared "+
			"against on every later run", got, want)
	}
}

func TestLocalPathsProduceNoResolutions(t *testing.T) {
	// A local path has no revision to pin, so it never reaches modules.lock —
	// the same fact 10a leans on when it requires a pin on remote sources and
	// not on these. Without this, the filter could be deleted and every
	// path-only project would write a lock file full of empty commits.
	decl, dir := fixture(t, map[string]string{
		"infra.yml":    "project: demo\nmodules:\n  - ./m\nresources:\n  c:\n    type: module.m\n",
		"m/module.yml": "resources:\n  thing:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(exp.Resolutions) != 0 {
		t.Errorf("Resolutions = %+v, want none", exp.Resolutions)
	}
}

func TestExpandRecursesThroughNestedInstantiations(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./modules/net
resources:
  gateway:
    type: test.thing
  netA:
    type: module.net
`,
		"modules/net/module.yml": `
modules:
  - ../deep
resources:
  subnet:
    type: test.thing
  inner:
    type: module.deep
`,
		"modules/deep/module.yml": `
resources:
  route:
    type: test.thing
`,
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	got := map[string]bool{}
	for _, inst := range exp.Instances {
		got[inst.Decl.Name] = true
		if strings.HasPrefix(inst.Decl.Type, TypePrefix) {
			t.Errorf("%q survived expansion as a %s resource; a module call must EXPAND, "+
				"never reach the planner", inst.Decl.Name, inst.Decl.Type)
		}
	}
	for _, want := range []string{"gateway", "subnet", "route"} {
		if !got[want] {
			t.Errorf("resource %q missing; stage 5 must recurse through every instantiation, got %v", want, got)
		}
	}
	if len(exp.Instances) != 3 {
		t.Errorf("got %d instances, want 3", len(exp.Instances))
	}
}

func TestExpandRefusesACycleAndShowsIt(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./modules/a
resources:
  alpha:
    type: module.a
`,
		"modules/a/module.yml": "modules:\n  - ../b\nresources:\n  beta:\n    type: module.b\n",
		"modules/b/module.yml": "modules:\n  - ../a\nresources:\n  gamma:\n    type: module.a\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatal("a module instantiating itself transitively must be a diagnostic, not a stack overflow")
	}
	if !hasFragment(ds, "never terminates") {
		t.Errorf("the diagnostic must say the expansion never terminates; got %+v", ds)
	}
	// Spec §7.4: name the full cycle, not one participant.
	if !hasFragment(ds, "alpha (./modules/a) -> beta (../b) -> gamma (../a)") {
		t.Errorf("the diagnostic must render the whole cycle in participation order; got %+v", ds)
	}
}

// The guard-ordering test. A cycle is infinitely deep, so a depth check placed
// first would swallow every cycle and make the cycle diagnostic unreachable.
// Nothing about either check, read alone, reveals that.
func TestExpandReportsACycleAsACycleAndNotAsExcessiveDepth(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":            "project: demo\nmodules:\n  - ./modules/a\nresources:\n  alpha:\n    type: module.a\n",
		"modules/a/module.yml": "modules:\n  - ../b\nresources:\n  beta:\n    type: module.b\n",
		"modules/b/module.yml": "modules:\n  - ../a\nresources:\n  gamma:\n    type: module.a\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	for _, s := range summaries(ds) {
		if strings.Contains(s, "deeper than") {
			t.Fatalf("a 3-deep cycle was reported as excessive nesting (%q); the cycle check must run first", s)
		}
	}
}

// chain builds MaxDepth+extra nested modules in DISTINCT directories, so the
// fixture reaches the depth guard rather than tripping the cycle guard on the
// way there.
func chain(depth int) map[string]string {
	files := map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m0\nresources:\n  top:\n    type: module.m0\n",
	}
	for i := range depth {
		if i < depth-1 {
			files[fmt.Sprintf("m%d/module.yml", i)] = fmt.Sprintf(
				"modules:\n  - ../m%d\nresources:\n  step:\n    type: module.m%d\n", i+1, i+1)
			continue
		}
		files[fmt.Sprintf("m%d/module.yml", i)] = "resources:\n  leaf:\n    type: test.thing\n"
	}
	return files
}

func TestExpandRefusesNestingDeeperThanMaxDepth(t *testing.T) {
	decl, dir := fixture(t, chain(MaxDepth+1))

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatalf("nesting %d deep must be refused", MaxDepth+1)
	}
	if !hasFragment(ds, "deeper than 32 instantiations") {
		t.Errorf("the diagnostic must say the nesting is too deep; got %+v", ds)
	}
	for _, s := range summaries(ds) {
		if strings.Contains(s, "never terminates") || strings.Contains(s, "cycle") {
			t.Fatalf("an acyclic deep tree was reported as a cycle (%q); they are different failures", s)
		}
	}
}

func TestExpandAcceptsNestingExactlyAtMaxDepth(t *testing.T) {
	// The boundary the guard must not move. Without this, `>` and `>=` are
	// indistinguishable and the limit silently becomes 31 or 33.
	decl, dir := fixture(t, chain(MaxDepth))

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("nesting exactly %d deep is within the limit: %+v", MaxDepth, ds)
	}
	if len(exp.Instances) != 1 {
		t.Errorf("got %d instances, want 1", len(exp.Instances))
	}
}

// Amendment 12b: the bound counts INSTANTIATIONS — module boundaries crossed to
// reach a resource — never loads. §8e auto-loads every directory under the
// project root holding a module.yml, so a project with more modules than
// MaxDepth sitting side by side is ordinary and must compile.
//
// This is the discriminating half of the depth pair: chain(MaxDepth+1) above
// must FAIL and this must PASS. An implementation that counted loads passes the
// first and fails this one, which is the only way to tell the two apart — and a
// bound on the module COUNT would be a ceiling no user could predict from their
// nesting.
func TestManySiblingModulesNeverTripTheDepthBound(t *testing.T) {
	const n = MaxDepth + 9
	files := map[string]string{}
	var res strings.Builder
	res.WriteString("project: demo\nresources:\n")
	for i := range n {
		files[fmt.Sprintf("m%d/module.yml", i)] = "resources:\n  leaf:\n    type: test.thing\n"
		fmt.Fprintf(&res, "  c%02d:\n    type: module.m%d\n", i, i)
	}
	files["infra.yml"] = res.String()
	decl, dir := fixture(t, files)

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("%d sibling modules are all at depth 1; the bound counts module boundaries "+
			"crossed, not modules loaded: %+v", n, ds)
	}
	if len(exp.Instances) != n {
		t.Errorf("got %d instances, want %d", len(exp.Instances), n)
	}
}

func TestExpandAcceptsADiamondNothingLikeACycle(t *testing.T) {
	// One source instantiated twice on two branches. A visited-set that never
	// popped would call this a cycle.
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./shared
resources:
  left:
    type: module.shared
  right:
    type: module.shared
`,
		"shared/module.yml": "resources:\n  thing:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("instantiating one source twice is not a cycle: %+v", ds)
	}
	if len(exp.Instances) != 2 {
		t.Errorf("got %d instances, want 2 — one per instantiation", len(exp.Instances))
	}
}

func TestExpandRefusesAnInstantiationOfAnUnloadedModule(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": "project: demo\nresources:\n  prod:\n    type: module.app_stack\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if !hasFragment(ds, "no module named \"app_stack\" is loaded") {
		t.Errorf("instantiating a module that no `modules:` entry loads must be refused; got %+v", ds)
	}
}

// A syntax error inside a module file must say WHICH instantiation it came
// from. Ruling 7 makes Origin.Module the only way an error message can, and one
// module instantiated twice would otherwise produce two identical diagnostics a
// reader cannot tell apart.
//
// The fixture's defect is `project:` inside module.yml, not a bad `type:`: a
// bare scalar `type: 17` decodes fine as the literal string "17" — nothing in
// stage 2 requires a `type:` scalar to be written as quoted text — so it
// produces no diagnostic at all and this test would vacuously pass regardless
// of whether stamping works. `project:` is a key ModuleFile's decoder has no
// case for (§11.3: a module has no project name), so it reliably fails.
func TestADiagnosticFromInsideAModuleNamesTheInstantiation(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./modules/net
resources:
  net1:
    type: module.net
`,
		"modules/net/module.yml": "project: prod\nresources:\n  subnet:\n    type: test.thing\n",
	})

	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatal("a module file declaring `project:` must be refused by the module decoder (§11.3: a module has no project name)")
	}
	var stamped bool
	for _, d := range ds {
		if strings.Join(d.Origin.Module, ".") == "net1" {
			stamped = true
		}
	}
	if !stamped {
		t.Error("every diagnostic from a nested load must be stamped with the INSTANTIATION " +
			"it came from — `net1`, the resource name, not `net`, the loaded module's name")
	}
}

// deepCycle builds MaxDepth distinct nested modules whose last one points back
// at the first. At the moment expand considers re-entering m0 the path already
// holds MaxDepth entries, so the cycle guard and the depth guard are BOTH
// satisfied — which is the only shape in which the order of the two is
// observable at all. chain() deliberately avoids this; this fixture seeks it.
func deepCycle() map[string]string {
	files := map[string]string{
		"infra.yml": "project: demo\nmodules:\n  - ./m0\nresources:\n  top:\n    type: module.m0\n",
	}
	for i := range MaxDepth {
		next := fmt.Sprintf("m%d", i+1)
		if i+1 == MaxDepth {
			next = "m0" // close the loop instead of terminating
		}
		files[fmt.Sprintf("m%d/module.yml", i)] = fmt.Sprintf(
			"modules:\n  - ../%s\nresources:\n  step:\n    type: module.%s\n", next, next)
	}
	return files
}

// TestADeepCycleIsReportedAsACycleNotAsDepth pins the ORDER of the two guards,
// which nothing else reaches.
//
// A simple a->b->a cycle repeats at depth 2, far below MaxDepth, so it is
// reported as a cycle whichever guard runs first — which is why swapping them
// leaves every other test in this package green. The order is observable only
// when a cycle first repeats AT the bound, and then it decides whether the user
// is told "you have a loop, here it is" or "your nesting is too deep". The
// second sends someone looking for nesting they do not have.
//
// Swap the two blocks in expand and this fails; nothing else does.
func TestADeepCycleIsReportedAsACycleNotAsDepth(t *testing.T) {
	decl, dir := fixture(t, deepCycle())
	_, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if !ds.HasErrors() {
		t.Fatalf("a %d-deep cycle must be refused", MaxDepth)
	}
	if !hasFragment(ds, "forms a cycle") {
		t.Errorf("a cycle at the depth bound was not reported as a cycle; got %+v", ds)
	}
	if hasFragment(ds, "deeper than") {
		t.Errorf("a cycle was reported as excessive nesting, which sends the user looking for nesting they do not have; got %+v", ds)
	}
}
