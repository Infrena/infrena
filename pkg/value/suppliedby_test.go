package value

import "testing"

// TestEqualIgnoresSuppliedByForEveryScope is invariant 1 (Amendment 6,
// contract.md): a --var and a --var-file entry supplying the same datum are
// the same input, so Equal must not distinguish them by SuppliedBy any more
// than it does by Scope (TestEqualIgnoresScopeForEveryPairOfScopes,
// scope_test.go). Without this, a --var whose value matches what a
// --var-file already said would plan as a change forever.
func TestEqualIgnoresSuppliedByForEveryScope(t *testing.T) {
	suppliedBys := []string{"", "--var", "vars.yml", "shared/prod.yml"}
	for _, a := range suppliedBys {
		for _, b := range suppliedBys {
			x := Int(20, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy(a)
			y := Int(20, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy(b)
			if !x.Equal(y) {
				t.Errorf("Int(20) supplied by %q != Int(20) supplied by %q: Equal is comparing "+
					"SuppliedBy, so a --var matching a --var-file entry would plan as a change forever", a, b)
			}
		}
	}
}

// TestEqualIgnoresSuppliedByAtEveryDepth is the composite half, mirroring
// TestEqualIgnoresScopeAtEveryDepth: Equal's KindList and KindMap arms are
// separate code paths with their own recursion, so a scalar-only test could
// pass while a nested leaf's SuppliedBy still broke equality. Both the list
// ELEMENT and the list VALUE ITSELF vary, for the same reason the Scope
// version varies both: a test that only varies one leaves the other
// unexercised regardless of whether that arm ignores SuppliedBy or not.
func TestEqualIgnoresSuppliedByAtEveryDepth(t *testing.T) {
	a := Map(map[string]Value{
		"tags": List([]Value{
			String("web", SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("--var"),
		}, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("--var"),
	}, SourceVariable)
	b := Map(map[string]Value{
		"tags": List([]Value{
			String("web", SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("vars.yml"),
		}, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("vars.yml"),
	}, SourceVariable)
	if !a.Equal(b) {
		t.Error("nested leaf differing only in SuppliedBy compared unequal")
	}
}

// TestEqualStillSeesADifferentDatumWithSuppliedBySet exercises the other
// answer. A predicate asserted in one direction only is not tested.
func TestEqualStillSeesADifferentDatumWithSuppliedBySet(t *testing.T) {
	x := Int(20, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("--var")
	y := Int(21, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("--var")
	if x.Equal(y) {
		t.Error("Int(20) == Int(21) with the same SuppliedBy: Equal has stopped comparing the datum")
	}
}

// TestSuppliedByRoundTripsThroughJSON is invariant 3. Value marshals through
// an explicit wireValue struct (pkg/value/json.go), so SuppliedBy needs its
// own field and tag — it will NOT round-trip by accident, and a missing tag
// fails SILENTLY: the value reads back with SuppliedBy "", which folds
// straight into annotation()'s fallback rather than erroring, so a saved
// plan (M6) would render "from --var" for a value that said "from f.yml"
// before it was saved.
func TestSuppliedByRoundTripsThroughJSON(t *testing.T) {
	cases := []string{"--var", "vars.yml", "shared/prod.yml"}
	for _, s := range cases {
		out := roundTrip(t, Int(20, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy(s))
		if out.SuppliedBy != s {
			t.Errorf("SuppliedBy %q round-tripped as %q", s, out.SuppliedBy)
		}
		if out.Scope != ScopeCLIOverride {
			t.Errorf("Scope lost while adding SuppliedBy: got %v", out.Scope)
		}
	}
}

// TestSuppliedByOmittedWhenEmptyRoundTripsAsEmpty pins the zero-value case:
// a value with no SuppliedBy (every rung but ScopeCLIOverride) must read
// back with SuppliedBy still "", not some encoded placeholder that
// annotation() would mistake for a real one.
func TestSuppliedByOmittedWhenEmptyRoundTripsAsEmpty(t *testing.T) {
	out := roundTrip(t, Int(10, SourceDefault).WithScope(ScopeProviderDefault))
	if out.SuppliedBy != "" {
		t.Errorf("SuppliedBy = %q, want empty", out.SuppliedBy)
	}
}

// TestSuppliedByRoundTripsAtEveryDepth mirrors TestScopeRoundTripsAtEveryDepth:
// the wireValue path recurses through json.Marshal on []Value and
// map[string]Value, a different code path from the scalar case above.
func TestSuppliedByRoundTripsAtEveryDepth(t *testing.T) {
	in := Map(map[string]Value{
		"replicas": Int(20, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("vars.yml"),
		"tags": List([]Value{
			String("web", SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("--var"),
		}, SourceVariable).WithScope(ScopeBaseConfig),
	}, SourceExplicit).WithScope(ScopeBaseConfig)

	out := roundTrip(t, in)
	m, ok := out.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Raw is %T, want map[string]Value", out.Raw)
	}
	if m["replicas"].SuppliedBy != "vars.yml" {
		t.Errorf("nested scalar SuppliedBy = %q, want %q", m["replicas"].SuppliedBy, "vars.yml")
	}
	items, ok := m["tags"].Raw.([]Value)
	if !ok || len(items) != 1 {
		t.Fatalf("tags Raw is %#v", m["tags"].Raw)
	}
	if items[0].SuppliedBy != "--var" {
		t.Errorf("list item SuppliedBy = %q, want %q", items[0].SuppliedBy, "--var")
	}
}

// TestAnnotatePrefersSuppliedByOnlyAtCLIOverride pins Amendment 6's exact
// scope restriction: SuppliedBy wins over Scope.String() ONLY at
// ScopeCLIOverride. At every other scope it must be IGNORED even when set —
// generalising the preference would mean the day any other rung starts
// stamping it, "variables.yml" starts rendering "from variables.yml" instead
// of "from base config", a label change the owner did not choose (see
// annotation()'s doc comment). The last two cases are the restriction
// itself, not the happy path: they set SuppliedBy at a non-CLI-override
// scope and assert it is ignored.
func TestAnnotatePrefersSuppliedByOnlyAtCLIOverride(t *testing.T) {
	cases := []struct {
		name string
		in   Value
		want string
	}{
		{
			name: "CLI override with SuppliedBy names the file",
			in:   Int(7, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("f.yml"),
			want: "7 [variable, from f.yml]",
		},
		{
			name: `CLI override with SuppliedBy "--var" names the flag`,
			in:   Int(42, SourceVariable).WithScope(ScopeCLIOverride).WithSuppliedBy("--var"),
			want: "42 [variable, from --var]",
		},
		{
			name: "CLI override with no SuppliedBy falls back to the scope label",
			in:   Int(20, SourceVariable).WithScope(ScopeCLIOverride),
			want: "20 [variable, from --var]",
		},
		{
			name: "base config with SuppliedBy set is still labelled by scope, not SuppliedBy",
			in:   String("10.0.0.0/16", SourceVariable).WithScope(ScopeBaseConfig).WithSuppliedBy("vars.yml"),
			want: `"10.0.0.0/16" [variable, from base config]`,
		},
		{
			name: "environment variable with SuppliedBy set is still labelled by scope",
			in:   String("large", SourceEnvironment).WithScope(ScopeEnvironmentVar).WithSuppliedBy("vars.yml"),
			want: `"large" [environment, from environment variable]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Annotate(tc.in, planOpts); got != tc.want {
				t.Errorf("Annotate = %q, want %q", got, tc.want)
			}
		})
	}
}
