package value

import (
	"testing"

	"github.com/infrena/infrena/pkg/address"
)

// TestReferenceStringIncludesTheModulePath pins the whole point of Ruling 1:
// a reference renders as the address it names, module path and all.
//
// It discriminates against the shape this task replaces and against any
// implementation that carries a module path but drops it when rendering —
// which would be worse than not carrying one, because References() and
// ConfigHash both read a reference through String().
func TestReferenceStringIncludesTheModulePath(t *testing.T) {
	r := Reference{Target: address.Address{Module: []string{"net"}, Name: "db"}, Attribute: "endpoint"}
	if got := r.String(); got != "module.net.db.endpoint" {
		t.Errorf("String() = %q, want \"module.net.db.endpoint\"", got)
	}
	nested := Reference{Target: address.Address{Module: []string{"net", "inner"}, Name: "db"}, Attribute: "endpoint"}
	if got := nested.String(); got != "module.net.module.inner.db.endpoint" {
		t.Errorf("nested String() = %q", got)
	}
}

// TestReferencesDoesNotCollapseSameNamedResourcesInDifferentModules is the
// dependency-graph half of the bug, and it is the test that would have caught
// the bare-name shape.
//
// References() deduplicates on Ref.String(). Two modules each declaring a `db`
// produce two references that are identical as bare names, so a bare-name
// Reference yields ONE edge where two are required — a missing dependency
// edge, which is acceptance invariant 4, from an expression that looks
// entirely ordinary.
func TestReferencesDoesNotCollapseSameNamedResourcesInDifferentModules(t *testing.T) {
	e := &Expr{Op: OpConcat, Args: []*Expr{
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Module: []string{"net"}, Name: "db"}, Attribute: "id"}},
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Module: []string{"app"}, Name: "db"}, Attribute: "id"}},
	}}
	got := e.References()
	if len(got) != 2 {
		t.Fatalf("References() returned %d references, want 2: two modules each declaring a \"db\" "+
			"must produce two dependency edges, not one; got %v", len(got), got)
	}
	if got[0].String() == got[1].String() {
		t.Errorf("both references render as %q", got[0].String())
	}
}

// TestLocalRefIsScopeRelative pins the constructor every existing call site
// uses: a reference as WRITTEN, with no module path, which at the root is also
// the absolute one.
func TestLocalRefIsScopeRelative(t *testing.T) {
	r := LocalRef("db", "id")
	if len(r.Target.Module) != 0 {
		t.Errorf("LocalRef gave a module path %v; a reference as written has none until it is resolved into a scope", r.Target.Module)
	}
	if got := r.String(); got != "db.id" {
		t.Errorf("String() = %q, want \"db.id\"", got)
	}
}

// TestInModuleReRootsWithoutAliasing pins that Reference.InModule DELEGATES to
// Address.InModule rather than appending to the receiver's slice.
//
// An implementation that did `r.Target.Module = append(r.Target.Module, ...)`
// would pass the rendering assertion and corrupt the caller's reference, which
// during expansion means one instantiation's references leaking into another's.
func TestInModuleReRootsWithoutAliasing(t *testing.T) {
	base := LocalRef("db", "id")
	inner := base.InModule("net")
	if got := inner.String(); got != "module.net.db.id" {
		t.Errorf("InModule gave %q, want \"module.net.db.id\"", got)
	}
	if len(base.Target.Module) != 0 {
		t.Error("InModule mutated the receiver's module path")
	}
	outer := inner.InModule("app")
	if got := outer.String(); got != "module.app.module.net.db.id" {
		t.Errorf("nested InModule gave %q", got)
	}
	if got := inner.String(); got != "module.net.db.id" {
		t.Errorf("the second InModule mutated the first result: %q", got)
	}
}

// TestVarRefCarriesABareName pins the OpVarRef case. A variable has no module
// path of its own — it is resolved in the scope the expression was written in,
// before stage 5 re-roots anything — so VarRef sets only the name, and String()
// must not render a trailing dot for the empty Attribute.
func TestVarRefCarriesABareName(t *testing.T) {
	r := VarRef("region")
	if got := r.VarName(); got != "region" {
		t.Errorf("VarName() = %q", got)
	}
	if got := r.String(); got != "region" {
		t.Errorf("String() = %q, want \"region\" with no trailing dot", got)
	}
}
