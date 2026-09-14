package modules

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/variables"
)

func addresses(exp *Expansion) []string {
	out := make([]string, 0, len(exp.Instances))
	for _, inst := range exp.Instances {
		out = append(out, inst.Address.String())
	}
	return out
}

func TestResourcesInAModuleCarryTheModuleQualifiedAddress(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./net
resources:
  gateway:
    type: test.thing
  net:
    type: module.net
`,
		"net/module.yml": `
modules:
  - ../deep
resources:
  subnet:
    type: test.thing
  inner:
    type: module.deep
`,
		"deep/module.yml": "resources:\n  route:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	want := []string{"gateway", "module.net.module.inner.route", "module.net.subnet"}
	got := addresses(exp)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

// The ordering fixture. The expected output CONTRADICTS every walk order:
// `module.mid.x` sorts BETWEEN the two root resources, so no traversal — plain
// resources then calls, or calls then plain resources — produces it. A fixture
// already in address order would pass against an implementation that never
// sorted, and would look tidier.
func TestExpansionIsSortedByAddress(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./mid
resources:
  alpha:
    type: test.thing
  zeta:
    type: test.thing
  mid:
    type: module.mid
`,
		"mid/module.yml": "resources:\n  x:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	want := []string{"alpha", "module.mid.x", "zeta"}
	got := addresses(exp)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v — invariant 6 requires one deterministic order, and "+
			"the module's resource sorts BETWEEN the two root ones, which no walk order "+
			"produces", got, want)
	}
}

func TestOriginNamesTheInstantiationAResourceCameFrom(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml":      "project: demo\nmodules:\n  - ./net\nresources:\n  net1:\n    type: module.net\n",
		"net/module.yml": "resources:\n  subnet:\n    type: test.thing\n    tag: production\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	inst := exp.Instances[0]

	if strings.Join(inst.Decl.Origin.Module, ".") != "net1" {
		t.Errorf("Origin.Module = %v, want [net1] — Ruling 7 makes this the ONLY way an error "+
			"message can say which instantiation a problem came from", inst.Decl.Origin.Module)
	}
	attr, ok := inst.Decl.Attributes["tag"]
	if !ok {
		t.Fatal("attribute `tag` missing from the copied declaration")
	}
	if strings.Join(attr.Origin.Module, ".") != "net1" {
		t.Errorf("attribute Origin.Module = %v, want [net1] — a diagnostic about one ATTRIBUTE "+
			"points at the attribute's origin, not the resource's", attr.Origin.Module)
	}
}

func TestTwoInstantiationsOfOneSourceDoNotShareADeclaration(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./m
resources:
  left:
    type: module.m
  right:
    type: module.m
`,
		"m/module.yml": "resources:\n  thing:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if len(exp.Instances) != 2 {
		t.Fatalf("got %d instances, want 2", len(exp.Instances))
	}

	a, b := exp.Instances[0], exp.Instances[1]
	if a.Decl == b.Decl {
		t.Fatal("two instantiations share one *config.ResourceDecl; stamping Origin.Module on " +
			"it gives BOTH the path of whichever was expanded last")
	}
	if strings.Join(a.Decl.Origin.Module, ".") == strings.Join(b.Decl.Origin.Module, ".") {
		t.Errorf("both instantiations report Origin.Module %v", a.Decl.Origin.Module)
	}
	if a.Address.String() != "module.left.thing" || b.Address.String() != "module.right.thing" {
		t.Errorf("addresses = %v, want [module.left.thing module.right.thing]", addresses(exp))
	}
}

// Amendment 8's new hazard. `prod` is a resource NAME the user wrote, and after
// expansion nothing is addressed `prod`.
func TestDependsOnAModuleCallFansOutToEveryResourceItProduced(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./net
resources:
  netA:
    type: module.net
  web:
    type: test.thing
    depends_on: [netA]
`,
		"net/module.yml": "resources:\n  subnet:\n    type: test.thing\n  gateway:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var web Instance
	for _, inst := range exp.Instances {
		if inst.Address.String() == "web" {
			web = inst
		}
	}
	var got []string
	for _, a := range web.ExtraDeps {
		got = append(got, a.String())
	}
	want := []string{"module.netA.gateway", "module.netA.subnet"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("web ExtraDeps = %v, want %v — nothing is addressed `netA` after expansion, "+
			"so an edge naming it must fan out or it names nothing at all", got, want)
	}
}

func TestAModuleCallsOwnDependsOnReachesEveryResourceItProduced(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./app
resources:
  base:
    type: test.thing
  appA:
    type: module.app
    depends_on: [base]
`,
		"app/module.yml": "resources:\n  server:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var server Instance
	for _, inst := range exp.Instances {
		if inst.Address.String() == "module.appA.server" {
			server = inst
		}
	}
	if len(server.ExtraDeps) != 1 || server.ExtraDeps[0].String() != "base" {
		t.Fatalf("module.appA.server ExtraDeps = %v, want [base] — a depends_on written on the "+
			"CALL belongs to everything the call expands into", server.ExtraDeps)
	}
}

// TestFanOutEdgesAreSortedAcrossTwoCalls pins sortAddresses in fanOut, which
// nothing else reaches.
//
// The existing fan-out tests have ONE module call, so `edges` is a single
// slice read out of `calls` and is already ordered however expansion produced
// it — deleting the sort changes nothing and the tests pass twenty times out of
// twenty. That was measured, not assumed.
//
// Two calls named in an order that is NOT their sorted order is the shape that
// discriminates: without the sort the edges come back in depends_on order, and
// depends_on order is the user's, so the plan a user reads would reorder itself
// when they reorder a list that is meant to be a set. Invariant 6.
func TestFanOutEdgesAreSortedAcrossTwoCalls(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./net
resources:
  zeta:
    type: module.net
  alpha:
    type: module.net
  web:
    type: test.thing
    depends_on: [zeta, alpha]
`,
		"net/module.yml": "resources:\n  subnet:\n    type: test.thing\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var web Instance
	for _, inst := range exp.Instances {
		if inst.Address.String() == "web" {
			web = inst
		}
	}
	var got []string
	for _, a := range web.ExtraDeps {
		got = append(got, a.String())
	}
	want := []string{"module.alpha.subnet", "module.zeta.subnet"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("web ExtraDeps = %v, want %v — depends_on is a set, so the edges it produces "+
			"must not carry the order the user happened to list it in", got, want)
	}
}

// TestACallsOwnReferenceBecomesAnEdgeOnWhatItProduced is the canonical module
// pattern — `network: ${net.id}` on the call — and the edge it needs exists
// nowhere else.
//
// The call is expanded away, so stage 6 never sees its attributes. Inside the
// module the value arrives as an INPUT, which is a bare `${var.network}` with no
// dot, and stage 6 walks only resource references for edges. Before this edge
// was recorded the plan was CLEAN and the apply failed after the outer resource
// had already been created:
//
//	create module.primary.store failed: network is still unknown after its
//	dependencies were applied
//
// A plan-only assertion cannot catch that, which is why this asserts the edge
// rather than the rendered plan.
func TestACallsOwnReferenceBecomesAnEdgeOnWhatItProduced(t *testing.T) {
	decl, dir := fixture(t, map[string]string{
		"infra.yml": `
project: demo
modules:
  - ./db
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  primary:
    type: module.db
    network: ${net.id}
`,
		"db/module.yml": "inputs:\n  network:\n    type: string\nresources:\n  store:\n    type: fake.database\n    engine: postgres\n    network: ${var.network}\n",
	})

	exp, ds := Expand(decl, variables.Scope{}, nil, Env{Name: "dev"}, dir, paths{})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}

	var store Instance
	for _, inst := range exp.Instances {
		if inst.Address.String() == "module.primary.store" {
			store = inst
		}
	}
	if store.Decl == nil {
		t.Fatal("no module.primary.store")
	}
	var deps []string
	for _, a := range store.ExtraDeps {
		deps = append(deps, a.String())
	}
	if strings.Join(deps, ",") != "net" {
		t.Errorf("module.primary.store ExtraDeps = %v, want [net] — without this edge the "+
			"executor schedules both in the same wave and the input is still unknown", deps)
	}
}
