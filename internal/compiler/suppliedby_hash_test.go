package compiler

import (
	"testing"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// configWithSuppliedBy builds a one-resource config whose single attribute
// carries the given SuppliedBy and datum, at ScopeCLIOverride — the only
// scope SuppliedBy is meaningful at (see value.Annotate's doc comment).
func configWithSuppliedBy(suppliedBy string, replicas int64) ResolvedConfig {
	addr := address.Address{Name: "app"}
	return ResolvedConfig{
		Project:     "demo",
		Environment: "production",
		Resources: map[string]*resource.ResolvedResource{
			addr.String(): {
				Address: addr,
				Type:    "fake.database",
				Attrs: map[string]value.Value{
					"replicas": value.Int(replicas, value.SourceVariable).
						WithScope(value.ScopeCLIOverride).WithSuppliedBy(suppliedBy),
				},
			},
		},
	}
}

// TestHashIgnoresSuppliedBy is invariant 2 (Amendment 6, contract.md),
// mirroring TestHashIgnoresScope. A value of 20 is the same input whether
// --var or --var-file supplied it, so hashing SuppliedBy would make an
// unchanged configuration look stale in M6 — a saved plan would refuse to
// apply because the user happened to pass the same number through a
// different flag than the one recorded in the saved plan.
//
// hashValue in resolved.go writes `write("kind", ..., "source", ...)` and
// touches nothing else about provenance (Scope is excluded the same way).
// Adding "supplied_by" beside them is the single-line change this test
// exists to catch.
func TestHashIgnoresSuppliedBy(t *testing.T) {
	want, err := configWithSuppliedBy("", 20).Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for _, s := range []string{"--var", "vars.yml", "shared/prod.yml"} {
		got, err := configWithSuppliedBy(s, 20).Hash()
		if err != nil {
			t.Fatalf("Hash with SuppliedBy %q: %v", s, err)
		}
		if got != want {
			t.Errorf("hash differs with SuppliedBy %q: %s != %s; ConfigHash is covering SuppliedBy, "+
				"so an unchanged configuration reads as stale in M6", s, got, want)
		}
	}
}

// TestHashStillSeesADifferentValueWithSuppliedBySet exercises the other
// answer. Without it, `func Hash() { return "", nil }` passes the test above.
func TestHashStillSeesADifferentValueWithSuppliedBySet(t *testing.T) {
	for _, s := range []string{"--var", "vars.yml"} {
		a, err := configWithSuppliedBy(s, 20).Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		b, err := configWithSuppliedBy(s, 21).Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if a == b {
			t.Errorf("replicas=20 and replicas=21 hash identically with SuppliedBy %q; "+
				"ConfigHash has stopped covering the resolved value", s)
		}
	}
}
