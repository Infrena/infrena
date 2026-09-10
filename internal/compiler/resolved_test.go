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
