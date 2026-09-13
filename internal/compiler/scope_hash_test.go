package compiler

import (
	"testing"

	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

var allScopes = []value.Scope{
	value.ScopeUnset,
	value.ScopeProviderDefault,
	value.ScopeBaseConfig,
	value.ScopeModuleDefault,
	value.ScopeEnvironmentInherit,
	value.ScopeEnvironmentVar,
	value.ScopeCLIOverride,
}

// configAtScope builds a one-resource config whose single attribute carries the
// given scope and the given datum.
func configAtScope(s value.Scope, replicas int64) ResolvedConfig {
	addr := address.Address{Name: "app"}
	return ResolvedConfig{
		Project:     "demo",
		Environment: "production",
		Resources: map[string]*resource.ResolvedResource{
			addr.String(): {
				Address: addr,
				Type:    "fake.database",
				Attrs: map[string]value.Value{
					"replicas": value.Int(replicas, value.SourceVariable).WithScope(s),
				},
			},
		},
	}
}

// TestHashIgnoresScope is invariant 2.
//
// ConfigHash answers "was this plan computed against this configuration". M3
// proved the hash must capture the RESOLVED VALUE: two different --var values
// once hashed identically, which would have let a plan computed with one be
// applied with another.
//
// A value of 20 is the same input whether it came from variables.yml or from
// --var, so hashing Scope would make an UNCHANGED configuration look stale in
// M6 — a saved plan would refuse to apply because the user happened to pass
// the same number a different way.
//
// hashValue in resolved.go writes `write("kind", ..., "source", ...)`. Adding
// "scope" beside them is the single-line change this test exists to catch.
func TestHashIgnoresScope(t *testing.T) {
	want, err := configAtScope(value.ScopeUnset, 20).Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for _, s := range allScopes {
		got, err := configAtScope(s, 20).Hash()
		if err != nil {
			t.Fatalf("Hash at scope %s: %v", s, err)
		}
		if got != want {
			t.Errorf("hash differs at scope %s: %s != %s; ConfigHash is covering Scope, "+
				"so an unchanged configuration reads as stale in M6", s, got, want)
		}
	}
}

// TestHashStillSeesADifferentValueAtTheSameScope exercises the other answer.
// Without it, `func Hash() { return "", nil }` passes the test above.
func TestHashStillSeesADifferentValueAtTheSameScope(t *testing.T) {
	for _, s := range allScopes {
		a, err := configAtScope(s, 20).Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		b, err := configAtScope(s, 21).Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if a == b {
			t.Errorf("replicas=20 and replicas=21 hash identically at scope %s; "+
				"ConfigHash has stopped covering the resolved value", s)
		}
	}
}
