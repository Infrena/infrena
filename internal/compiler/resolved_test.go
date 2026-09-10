package compiler

import (
	"testing"

	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func cfg(resources ...*resource.ResolvedResource) ResolvedConfig {
	c := ResolvedConfig{Project: "myapp", Environment: "dev", Resources: map[string]*resource.ResolvedResource{}}
	for _, r := range resources {
		c.Resources[r.Address.String()] = r
	}
	return c
}

func res(name, typ string, attrs map[string]value.Value) *resource.ResolvedResource {
	return &resource.ResolvedResource{
		Address: address.Address{Name: name},
		Type:    typ,
		Attrs:   attrs,
	}
}

func TestAddressesAreSorted(t *testing.T) {
	c := cfg(res("zebra", "test.network", nil), res("alpha", "test.network", nil))
	got := c.Addresses()
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zebra" {
		t.Errorf("Addresses() = %v, want sorted", got)
	}
}

func TestHashIsStableAcrossRuns(t *testing.T) {
	c := cfg(
		res("a", "test.network", map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)}),
		res("b", "test.database", map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)}),
	)
	first, err := c.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := c.Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if next != first {
			t.Fatal("Hash must be stable across runs; Go map iteration is randomised")
		}
	}
}

func TestHashIgnoresOrigin(t *testing.T) {
	// Moving a resource between lines is not a change in desired state.
	a := res("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithOrigin(value.Origin{File: "infra.yml", Line: 3}),
	})
	b := res("db", "test.database", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit).WithOrigin(value.Origin{File: "infra.yml", Line: 99}),
	})
	b.Origin = value.Origin{Line: 42}

	ha, _ := cfg(a).Hash()
	hb, _ := cfg(b).Hash()
	if ha != hb {
		t.Error("Hash must ignore Origin — a line number is not desired state")
	}
}

func TestHashIncludesProvenance(t *testing.T) {
	// A value that arrived as an explicit setting is not the same desired
	// state as the identical value arriving from a default.
	explicit := res("db", "test.database", map[string]value.Value{
		"size": value.Int(10, value.SourceExplicit),
	})
	defaulted := res("db", "test.database", map[string]value.Value{
		"size": value.Int(10, value.SourceDefault),
	})
	ha, _ := cfg(explicit).Hash()
	hb, _ := cfg(defaulted).Hash()
	if ha == hb {
		t.Error("Hash must include provenance — spec §12.1")
	}
}

func TestHashIncludesLifecycle(t *testing.T) {
	plain := res("db", "test.database", nil)
	guarded := res("db", "test.database", nil)
	guarded.Lifecycle = resource.Lifecycle{PreventDestroy: true}

	ha, _ := cfg(plain).Hash()
	hb, _ := cfg(guarded).Hash()
	if ha == hb {
		t.Error("Hash must include lifecycle — turning on prevent_destroy changes desired state")
	}
}

func TestHashDistinguishesDifferentUnresolvedReferences(t *testing.T) {
	// Both values are unknown at compile time, so everything Hash() looked at
	// before — kind, source, known, sensitive — is identical. What differs is
	// which resource the attribute will resolve from, which is the whole point
	// of a reference.
	unknownRef := func(res, attr string) value.Value {
		v := value.Unknown(value.KindString, value.SourceComputed)
		v.Expr = &value.Expr{
			Op:  value.OpResourceRef,
			Ref: value.Reference{Resource: res, Attribute: attr},
		}
		return v
	}

	a := cfg(res("db", "test.database", map[string]value.Value{"network": unknownRef("network_a", "id")}))
	b := cfg(res("db", "test.database", map[string]value.Value{"network": unknownRef("network_b", "id")}))

	ha, _ := a.Hash()
	hb, _ := b.Hash()
	if ha == hb {
		t.Error("two unresolved values referencing different resources must not hash alike — M6 staleness would miss a changed dependency")
	}
}

func TestHashDistinguishesDifferentCalls(t *testing.T) {
	call := func(fn string) value.Value {
		v := value.Unknown(value.KindString, value.SourceComputed)
		v.Expr = &value.Expr{
			Op:       value.OpCall,
			Function: fn,
			Args:     []*value.Expr{{Op: value.OpResourceRef, Ref: value.Reference{Resource: "db", Attribute: "engine"}}},
		}
		return v
	}
	ha, _ := cfg(res("r", "test.network", map[string]value.Value{"cidr": call("lower")})).Hash()
	hb, _ := cfg(res("r", "test.network", map[string]value.Value{"cidr": call("upper")})).Hash()
	if ha == hb {
		t.Error("lower() and upper() over the same reference are different desired states")
	}
}

func TestHashChangesWithAValue(t *testing.T) {
	a := cfg(res("db", "test.database", map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)}))
	b := cfg(res("db", "test.database", map[string]value.Value{"engine": value.String("mysql", value.SourceExplicit)}))
	ha, _ := a.Hash()
	hb, _ := b.Hash()
	if ha == hb {
		t.Error("a changed attribute must change the hash")
	}
}

func TestHashDistinguishesUnknownFromEmpty(t *testing.T) {
	unknown := cfg(res("db", "test.database", map[string]value.Value{
		"endpoint": value.Unknown(value.KindString, value.SourceComputed),
	}))
	empty := cfg(res("db", "test.database", map[string]value.Value{
		"endpoint": value.String("", value.SourceExplicit),
	}))
	hu, _ := unknown.Hash()
	he, _ := empty.Hash()
	if hu == he {
		t.Error("an unknown value and an empty string are different desired states")
	}
}

// TestConfigHashSeesAVariableFeedingADeferredExpression is the reason Task 3
// exists. hashExpr folds an unresolved expression by op name, function, ref
// NAME and argument count — never a resolved value, because before folding
// there was none to write. Two configurations differing only in a --var that
// feeds a deferred expression therefore hashed identically, and M6's staleness
// refusal compares exactly this hash to decide whether a saved plan still
// describes the configuration.
func TestConfigHashSeesAVariableFeedingADeferredExpression(t *testing.T) {
	body := `
project: myapp
resources:
  network:
    type: test.network
    cidr: 10.0.0.0/16
  database:
    type: test.database
    engine: postgres
    network: ${prefix}-${network.id}
`
	hashWith := func(prefix string) string {
		t.Helper()
		cfg, ds := Compile(loadFiles(t, body), testRegistry(t), Options{
			Vars: map[string]string{"prefix": prefix},
		})
		if ds.HasErrors() {
			t.Fatalf("compile with prefix=%q: %+v", prefix, ds)
		}
		h, err := cfg.Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		return h
	}

	if a, b := hashWith("acme"), hashWith("totally-different"); a == b {
		t.Errorf("ConfigHash is identical for two different --var values feeding a deferred "+
			"expression (%s); a saved plan would be accepted against configuration it was not computed from", a)
	}
}
