package planner

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
)

var update = flag.Bool("update", false, "update golden files")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file %s: %v (run with -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("Render() does not match %s\n--- got ---\n%s--- want ---\n%s", path, got, string(want))
	}
}

func mixedPlan() *Plan {
	return &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "app"}, Type: "fake.application", Kind: OpDestroy,
				Before: map[string]value.Value{
					"password": value.String("hunter2", value.SourceProvider).WithSensitive(true),
				},
				Dependents: []address.Address{{Name: "cdn"}, {Name: "monitor"}},
			},
			{
				Address: address.Address{Name: "cache"}, Type: "test.cache", Kind: OpForget,
				Before: map[string]value.Value{
					"id": value.String("cache-1", value.SourceProvider),
				},
			},
			{
				Address: address.Address{Name: "database"}, Type: "fake.database", Kind: OpReplace,
				Before:     map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
				After:      map[string]value.Value{"engine": value.String("mysql", value.SourceExplicit)},
				Reasons:    []ChangeReason{{Attribute: "engine", ForceNew: true, Note: "forces replacement"}},
				Dependents: []address.Address{{Name: "reports"}},
			},
			{
				Address: address.Address{Name: "loadbalancer"}, Type: "test.loadbalancer", Kind: OpUpdate,
				Before: map[string]value.Value{"size": value.Int(10, value.SourceDefault)},
				After:  map[string]value.Value{"size": value.Int(50, value.SourceExplicit)},
			},
			{
				Address: address.Address{Name: "network"}, Type: "fake.network", Kind: OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
}

func TestRenderCreateOnly(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "network"}, Type: "fake.network", Kind: OpCreate,
				After: map[string]value.Value{
					"cidr": value.String("10.20.0.0/16", value.SourceExplicit),
					"id":   value.Unknown(value.KindString, value.SourceComputed),
				},
			},
		},
	}
	checkGolden(t, "create.golden", Render(p, RenderOptions{}))
}

func TestRenderMixedOperations(t *testing.T) {
	checkGolden(t, "mixed.golden", Render(mixedPlan(), RenderOptions{}))
}

// scopedPlan hand-builds its values, like mixedPlan, rather than routing
// through the compiler, so this golden depends on no other task's stamping.
// ScopeProviderDefault is Task 7's to stamp onto a real plan (step 9.3); here
// it is asserted directly against the renderer regardless of whether Task 7
// has landed.
func scopedPlan() *Plan {
	return &Plan{
		Project:     "myapp",
		Environment: "production",
		Operations: []Operation{
			{
				Address: address.Address{Name: "db"}, Type: "fake.database", Kind: OpCreate,
				After: map[string]value.Value{
					// Explicit at base config: rule 2 suppresses the
					// annotation entirely.
					"engine": value.String("postgres", value.SourceExplicit).WithScope(value.ScopeBaseConfig),
					// Variable at base config: annotated.
					"network": value.String("net-1", value.SourceVariable).WithScope(value.ScopeBaseConfig),
					// The floor of the chain: a provider default.
					"size": value.Int(100, value.SourceDefault).WithScope(value.ScopeProviderDefault),
					// Won by --var-file: Amendment 6 (contract.md). The
					// annotation names the file itself, not the generic
					// "--var" Scope.String() would otherwise give every
					// ScopeCLIOverride value — the fix Amendment 5 tried and
					// had reverted.
					"zone": value.String("us-east-1a", value.SourceVariable).
						WithScope(value.ScopeCLIOverride).WithSuppliedBy("prod.yml"),
				},
			},
			{
				Address: address.Address{Name: "net"}, Type: "fake.network", Kind: OpUpdate,
				Before: map[string]value.Value{
					"cidr": value.String("10.0.0.0/16", value.SourceVariable).WithScope(value.ScopeBaseConfig),
				},
				After: map[string]value.Value{
					// Won by --var: the shape the user fixed.
					"cidr": value.String("10.9.0.0/16", value.SourceVariable).WithScope(value.ScopeCLIOverride),
				},
			},
		},
	}
}

// TestRenderScopedValuesShowPrecedence is the golden covering a whole plan
// with mixed scopes: spec §12.3's "values sourced from defaults annotated
// [default]" plus M4's full ladder, in one artifact a person would actually
// read.
func TestRenderScopedValuesShowPrecedence(t *testing.T) {
	checkGolden(t, "scopes.golden", Render(scopedPlan(), RenderOptions{}))
}

// TestRenderScopedPlanIsDeterministicAcrossRepeatedCalls extends the
// determinism guarantee to scopedPlan specifically: the golden above proves
// one render is right, this proves 100 of them agree byte for byte, so a
// map-order leak in value.Annotate's call path would not hide behind a
// single lucky render.
func TestRenderScopedPlanIsDeterministicAcrossRepeatedCalls(t *testing.T) {
	want := Render(scopedPlan(), RenderOptions{})
	for i := range 100 {
		if got := Render(scopedPlan(), RenderOptions{}); got != want {
			t.Fatalf("Render(scopedPlan()) is not deterministic; iteration %d differs.\n--- first ---\n%s--- iteration %d ---\n%s",
				i, want, i, got)
		}
	}
}

func TestRenderVerboseListsUnchangedResources(t *testing.T) {
	p := mixedPlan()
	p.Operations = append(p.Operations, Operation{
		Address: address.Address{Name: "zzz"}, Type: "fake.network", Kind: OpNoOp,
		Before: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
		After:  map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
	})
	checkGolden(t, "verbose.golden", Render(p, RenderOptions{Verbose: true}))
}

func TestRenderNoChanges(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "network"}, Type: "fake.network", Kind: OpNoOp,
				Before: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				After:  map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
	checkGolden(t, "nochanges.golden", Render(p, RenderOptions{}))
}

// TestRenderNeverLeaksASecretForAnyOperationKind is the test Task 12's plan
// text specifically asks for: sensitivity is per-leaf (a lesson M1 shipped a
// leak over), so a secret nested inside a list or a map must be redacted just
// as surely as one at the top level, for every operation kind and under both
// RenderOptions.
func TestRenderNeverLeaksASecretForAnyOperationKind(t *testing.T) {
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)
	nested := value.Map(map[string]value.Value{
		"user": value.String("admin", value.SourceProvider),
		"pass": secret,
	}, value.SourceProvider)
	inList := value.List([]value.Value{
		value.String("public", value.SourceProvider),
		secret,
	}, value.SourceProvider)

	var ops []Operation
	for i, kind := range []OpKind{OpCreate, OpUpdate, OpReplace, OpDestroy, OpForget} {
		op := Operation{
			Address: address.Address{Name: fmt.Sprintf("r%d", i)},
			Type:    "test.secret",
			Kind:    kind,
		}
		leaves := map[string]value.Value{"scalar": secret, "nested": nested, "list": inList}
		switch kind {
		case OpCreate:
			op.After = leaves
		case OpDestroy, OpForget:
			op.Before = leaves
		case OpUpdate, OpReplace:
			op.Before = map[string]value.Value{"scalar": value.String("was", value.SourceProvider)}
			op.After = leaves
			if kind == OpReplace {
				op.Reasons = []ChangeReason{{Attribute: "scalar", ForceNew: true}}
			}
		}
		ops = append(ops, op)
	}
	p := &Plan{Project: "myapp", Environment: "dev", Operations: ops}

	for _, verbose := range []bool{false, true} {
		for _, color := range []bool{false, true} {
			got := Render(p, RenderOptions{Verbose: verbose, Color: color})
			if strings.Contains(got, "hunter2") {
				t.Errorf("verbose=%v color=%v: rendered output leaks the secret:\n%s", verbose, color, got)
			}
			if !strings.Contains(got, "<sensitive>") {
				t.Errorf("verbose=%v color=%v: expected a <sensitive> marker somewhere", verbose, color)
			}
		}
	}
}

func TestRenderColorWrapsMarkersInANSIAndPlainDoesNot(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "network"}, Type: "fake.network", Kind: OpCreate,
				After: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}

	plain := Render(p, RenderOptions{})
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("Color: false must not emit ANSI escape codes:\n%s", plain)
	}

	colored := Render(p, RenderOptions{Color: true})
	if !strings.Contains(colored, "\x1b[32m+\x1b[0m") {
		t.Errorf("Color: true must wrap the create marker in ANSI, got:\n%s", colored)
	}
}

func TestRenderDestructiveWithoutDependentsShowsNoWarning(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "solo"}, Type: "fake.network", Kind: OpDestroy,
				Before: map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
			},
		},
	}
	got := Render(p, RenderOptions{})
	if strings.Contains(got, "dependent") {
		t.Errorf("a destroy with no dependents must not render a dependents warning:\n%s", got)
	}
}

func TestRenderDestructiveWithDependentsShowsCountSingularAndPlural(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Name: "one"}, Type: "fake.network", Kind: OpDestroy,
				Before:     map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				Dependents: []address.Address{{Name: "app"}},
			},
			{
				Address: address.Address{Name: "two"}, Type: "fake.network", Kind: OpDestroy,
				Before:     map[string]value.Value{"cidr": value.String("10.1.0.0/16", value.SourceExplicit)},
				Dependents: []address.Address{{Name: "app"}, {Name: "worker"}},
			},
		},
	}
	got := Render(p, RenderOptions{})
	if !strings.Contains(got, "1 dependent resource.") || strings.Contains(got, "1 dependent resources.") {
		t.Errorf("singular count rendered wrong:\n%s", got)
	}
	if !strings.Contains(got, "2 dependent resources.") {
		t.Errorf("plural count rendered wrong:\n%s", got)
	}
}

// TestRenderAnnotatedFailsClosedOnUnexpectedShapes covers the branch a golden
// file cannot: values whose Kind and Raw disagree, or whose Kind was never
// set. It drives renderAnnotated rather than renderLeaf (deleted in Task 9,
// folded into value.Annotate) — which makes it strictly stronger than before,
// since it now also covers the annotation path: a malformed value must not
// leak through that either. See value.Format's doc comment for the two
// measured leaks this prevents.
func TestRenderAnnotatedFailsClosedOnUnexpectedShapes(t *testing.T) {
	secret := value.String("hunter2", value.SourceProvider).WithSensitive(true)

	cases := []struct {
		name string
		v    value.Value
	}{
		{"kind left at the zero value with a composite Raw",
			value.Value{Known: true, Raw: map[string]value.Value{"password": secret}}},
		{"kind says map, Raw is a different map type",
			value.Value{Kind: value.KindMap, Known: true, Raw: map[string]any{"password": "hunter2"}}},
		{"kind says list, Raw is a different slice type",
			value.Value{Kind: value.KindList, Known: true, Raw: []any{"hunter2"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAnnotated(tc.v)
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret rendered in clear text: %s", got)
			}
			if got != "<unrenderable>" {
				t.Errorf("renderAnnotated = %q, want %q", got, "<unrenderable>")
			}
		})
	}
}

// TestRenderAnnotatesEveryScope drives renderAnnotated, not value.Annotate
// directly. Task 1 already proves Annotate itself works; what is unproven
// until this test exists is the WIRING — that the plan renderer actually
// reaches Annotate, with the plan's own FormatOptions, rather than a
// planner-local copy. A test that called value.Annotate would pass even
// against a renderer that never delegates to it.
//
// The expected strings duplicate Scope.String()'s wording, by design: this
// asserts the user-visible result, Scope.String() produces the wording, and
// a change to either that the other does not expect is exactly what this
// table is meant to catch.
func TestRenderAnnotatesEveryScope(t *testing.T) {
	cases := []struct {
		name string
		v    value.Value
		want string
	}{
		{
			name: "unset scope keeps the M2 annotation",
			v:    value.Int(10, value.SourceDefault),
			want: "10 [default]",
		},
		{
			name: "explicit at base config is not annotated",
			v:    value.String("web", value.SourceExplicit).WithScope(value.ScopeBaseConfig),
			want: `"web"`,
		},
		{
			// The case an unconditional "explicit is never annotated" rule
			// would hide: an attribute written explicitly and then won by a
			// higher layer. Unreachable in M4, reachable in M5.
			name: "explicit won by a higher layer IS annotated",
			v:    value.String("web", value.SourceExplicit).WithScope(value.ScopeEnvironmentVar),
			want: `"web" [explicit, from environment config]`,
		},
		{
			name: "provider default",
			v:    value.Int(10, value.SourceDefault).WithScope(value.ScopeProviderDefault),
			want: "10 [default, from provider default]",
		},
		{
			name: "base config",
			v:    value.String("10.0.0.0/16", value.SourceVariable).WithScope(value.ScopeBaseConfig),
			want: `"10.0.0.0/16" [variable, from base config]`,
		},
		{
			name: "module default",
			v:    value.Int(2, value.SourceModule).WithScope(value.ScopeModuleDefault),
			want: "2 [module, from module default]",
		},
		{
			name: "environment inheritance",
			v:    value.String("small", value.SourceEnvironment).WithScope(value.ScopeEnvironmentInherit),
			want: `"small" [environment, from environment inheritance]`,
		},
		{
			name: "environment variables",
			v:    value.String("large", value.SourceEnvironment).WithScope(value.ScopeEnvironmentVar),
			want: `"large" [environment, from environment config]`,
		},
		{
			name: "cli override — the shape the user fixed",
			v:    value.Int(20, value.SourceVariable).WithScope(value.ScopeCLIOverride),
			want: "20 [variable, from --var]",
		},
		{
			name: "unknown values are not annotated",
			v:    value.Unknown(value.KindString, value.SourceComputed).WithScope(value.ScopeCLIOverride),
			want: "(known after apply)",
		},
		{
			name: "a sensitive value redacts regardless of scope",
			v:    value.String("hunter2", value.SourceVariable).WithScope(value.ScopeCLIOverride).WithSensitive(true),
			want: "<sensitive> [variable, from --var]",
		},
		{
			// Scope.String()'s default branch: an unrecognised level reports
			// as unrecognised rather than collapsing into a real one, because
			// a value attributed to the WRONG level is worse than one
			// attributed to none — a user would act on it.
			name: "a scope outside the enum reports itself",
			v:    value.Int(1, value.SourceDefault).WithScope(value.Scope(200)),
			want: "1 [default, from Scope(200)]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAnnotated(tc.v)
			if got != tc.want {
				t.Errorf("renderAnnotated = %q, want %q", got, tc.want)
			}
			if tc.v.Sensitive && strings.Contains(got, "hunter2") {
				t.Errorf("the datum leaked through the annotation path: %q", got)
			}
		})
	}
}

// TestRenderAnnotatesCLIOverrideWithSuppliedByOnly pins Amendment 6 (owner
// ruling, contract.md): at ScopeCLIOverride, renderAnnotated names the
// value's own SuppliedBy in preference to the scope's generic "--var" label,
// and nowhere else.
//
// This replaced Amendment 5, which tried the same idea by reusing Origin and
// was reverted: internal/expressions/eval.go's OpVarRef case re-origins
// every "${var.var}" reference to the referencing expression's site, so Origin
// does not survive to the renderer for any value that reaches a resource
// attribute the normal way. SuppliedBy is a dedicated field that
// WithOrigin's overwrite cannot touch, which is what makes this fix
// different in kind from the one that failed — but this test alone, like
// Amendment 5's, constructs a Value directly and cannot see whether that
// survival claim actually holds through real expression evaluation. See
// internal/cli's TestPlanAnnotatesVarFileAndVarWithTheirOwnSource for the
// end-to-end proof; this test proves only that the renderer's wiring reaches
// value.Annotate correctly, and the scope restriction Amendment 6 requires
// (last case).
func TestRenderAnnotatesCLIOverrideWithSuppliedByOnly(t *testing.T) {
	cases := []struct {
		name string
		v    value.Value
		want string
	}{
		{
			name: "--var-file names the file as typed",
			v:    value.Int(7, value.SourceVariable).WithScope(value.ScopeCLIOverride).WithSuppliedBy("f.yml"),
			want: "7 [variable, from f.yml]",
		},
		{
			name: "--var names the literal flag",
			v:    value.Int(42, value.SourceVariable).WithScope(value.ScopeCLIOverride).WithSuppliedBy("--var"),
			want: "42 [variable, from --var]",
		},
		{
			name: "no SuppliedBy falls back to the scope label",
			v:    value.Int(20, value.SourceVariable).WithScope(value.ScopeCLIOverride),
			want: "20 [variable, from --var]",
		},
		{
			// The restriction: SuppliedBy set at a scope OTHER than
			// ScopeCLIOverride must be ignored, not preferred.
			name: "SuppliedBy at base config is ignored",
			v:    value.String("web", value.SourceVariable).WithScope(value.ScopeBaseConfig).WithSuppliedBy("vars.yml"),
			want: `"web" [variable, from base config]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAnnotated(tc.v)
			if got != tc.want {
				t.Errorf("renderAnnotated = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderDistinguishesRemovedFromUnknown covers the one case a golden
// cannot: an attribute present on one side of a diff and absent from the
// other. Before renderSide existed, a missing key's zero Value rendered as
// "(known after apply)", so a REMOVED attribute claimed it would be computed
// during apply — the opposite of the truth, on the surface a person reads
// before agreeing to change infrastructure.
func TestRenderDistinguishesRemovedFromUnknown(t *testing.T) {
	p := &Plan{
		Version: PlanVersion, Project: "myapp", Environment: "dev",
		Operations: []Operation{{
			Address: addr("database"),
			Type:    "fake.database",
			Kind:    OpUpdate,
			Before: map[string]value.Value{
				"size": value.Int(10, value.SourceExplicit),
				"tags": value.String("old", value.SourceExplicit),
			},
			After: map[string]value.Value{
				"size": value.Int(20, value.SourceExplicit),
				// tags removed from configuration entirely.
				"endpoint": value.Unknown(value.KindString, value.SourceComputed),
			},
		}},
	}

	out := Render(p, RenderOptions{})

	// The removed attribute must not claim it will be computed.
	if strings.Contains(out, `tags: "old" -> (known after apply)`) {
		t.Errorf("a removed attribute must not render as pending computation:\n%s", out)
	}
	if !strings.Contains(out, `tags: "old" -> (absent)`) {
		t.Errorf("a removed attribute must render as absent:\n%s", out)
	}
	// An attribute added on the after side reads the other way round.
	if !strings.Contains(out, "endpoint: (absent) -> (known after apply)") {
		t.Errorf("an added computed attribute must show absent on the before side:\n%s", out)
	}
	// A genuine value change is unaffected.
	if !strings.Contains(out, "size: 10 -> 20") {
		t.Errorf("an ordinary change must still render normally:\n%s", out)
	}
}

// TestRenderForcedByIsSortedRegardlessOfReasonOrder pins render.go's
// sort.Strings in renderForcedBy.
//
// Reasons normally arrive sorted (diff.go's sortReasons, itself fed by
// unionKeys), which is why removing this sort fails nothing: the sort is
// defence in depth against a Plan built by some other producer — a plan read
// back from disk in M4, or a second planner. Render's documented contract is
// that identical plans render identical text, and the input here is the
// adversarial one that contract has to survive, so the fixture hands it
// reasons in an order Compute would never produce.
//
// Four forcing attributes, not two: with two, the wrong order is still one of
// only two strings and a reader can talk themselves into either.
func TestRenderForcedByIsSortedRegardlessOfReasonOrder(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{{
			Address: address.Address{Name: "db"}, Type: "fake.database", Kind: OpReplace,
			Before: map[string]value.Value{"engine": value.String("postgres", value.SourceProvider)},
			After:  map[string]value.Value{"engine": value.String("mysql", value.SourceExplicit)},
			Reasons: []ChangeReason{
				{Attribute: "zone", ForceNew: true},
				{Attribute: "engine", ForceNew: true},
				{Attribute: "size", ForceNew: false}, // not forcing: must not appear
				{Attribute: "region", ForceNew: true},
				{Attribute: "account", ForceNew: true},
			},
		}},
	}

	got := Render(p, RenderOptions{})
	const want = "-/+ fake.database.db  (replacement forced by: account, engine, region, zone)"
	if !strings.Contains(got, want) {
		t.Errorf("replacement header is not canonically ordered.\n--- want line ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestRenderIsDeterministicAcrossRepeatedCalls pins the plan-text half of
// invariant 6 (spec §12.1): the same plan must render the same bytes every
// time. Render walks Before and After as maps, so the ordering comes from
// unionKeys and nothing else; without it Go's randomised range order leaks
// into the artifact a user reads before approving a change.
//
// The loop is the measurement. A single render cannot distinguish "sorted" from
// "whatever order this run produced", and Go's small-map range is skewed toward
// insertion order, so a one-shot comparison against a hand-written expectation
// passes most of the time even when the sort is gone. Five attributes give
// enough orderings that 30 renders effectively never all agree by accident.
//
// Two of the After attributes carry a Scope (Task 9), so this also covers
// that value.Annotate's output is as deterministic as value.Format's — the
// same reason this is an extension of the existing fixture rather than a
// second determinism test.
func TestRenderIsDeterministicAcrossRepeatedCalls(t *testing.T) {
	plan := func() *Plan {
		return &Plan{
			Project:     "myapp",
			Environment: "dev",
			Operations: []Operation{{
				Address: address.Address{Name: "db"}, Type: "fake.database", Kind: OpReplace,
				Before: map[string]value.Value{
					"engine":   value.String("postgres", value.SourceProvider),
					"size":     value.Int(10, value.SourceProvider),
					"network":  value.String("net-1", value.SourceProvider),
					"zone":     value.String("a", value.SourceProvider),
					"endpoint": value.String("db-1.test", value.SourceProvider),
				},
				After: map[string]value.Value{
					"engine":   value.String("mysql", value.SourceExplicit),
					"size":     value.Int(50, value.SourceExplicit).WithScope(value.ScopeCLIOverride),
					"network":  value.String("net-2", value.SourceExplicit).WithScope(value.ScopeEnvironmentVar),
					"zone":     value.String("b", value.SourceExplicit),
					"endpoint": value.Unknown(value.KindString, value.SourceComputed),
				},
				// Already in canonical order, so renderForcedBy's own sort
				// is a no-op here: this test is about the attribute maps,
				// and TestRenderForcedByIsSortedRegardlessOfReasonOrder is
				// about the reasons. Keeping them separate is what lets each
				// name which decision it pins.
				Reasons: []ChangeReason{
					{Attribute: "engine", ForceNew: true},
					{Attribute: "network", ForceNew: true},
					{Attribute: "size"},
					{Attribute: "zone", ForceNew: true},
				},
			}},
		}
	}

	want := Render(plan(), RenderOptions{})
	if !strings.Contains(want, `network: "net-1" -> "net-2"`) {
		t.Fatalf("fixture did not reach the attribute-diff loop:\n%s", want)
	}
	for i := range 30 {
		if got := Render(plan(), RenderOptions{}); got != want {
			t.Fatalf("Render is not deterministic; iteration %d differs.\n--- first ---\n%s--- iteration %d ---\n%s",
				i, want, i, got)
		}
	}
}

// TestRenderNotesAResourceThatMayHaveMovedBetweenModules is the second cost of
// flattening (spec §7.2), rendered where a user meets it.
//
// Addresses embed the module path, so moving a resource from one module to
// another renames it, and a rename is a destroy plus a create. The plan is the
// last thing a person reads before agreeing to that, and a destroy of
// `module.old.store` sitting next to a create of `module.new.store` with no
// connection drawn between them is how someone loses a database.
//
// The note is phrased as a possibility, not a claim: nothing here can know
// whether two resources sharing a type and a logical name are the same
// resource.
func TestRenderNotesAResourceThatMayHaveMovedBetweenModules(t *testing.T) {
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Module: []string{"new"}, Name: "store"},
				Type:    "fake.database",
				Kind:    OpCreate,
				After:   map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
			{
				Address: address.Address{Module: []string{"old"}, Name: "store"},
				Type:    "fake.database",
				Kind:    OpDestroy,
				Before:  map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
		},
	}

	out := Render(p, RenderOptions{})
	// Not "module.new.store" alone: the create operation's own header line
	// ("+ fake.database.module.new.store") also contains that substring, so a
	// needle of just the address finds two lines rather than the one note.
	// "destroyed and recreated" appears only in the note.
	note := lineContainingInRender(t, out, "destroyed and recreated")
	if !strings.Contains(note, "module.new.store") {
		t.Errorf("the note %q does not name the address the resource is reappearing at:\n%s", note, out)
	}
	// On the destroy, not on the create: the create is not the dangerous half.
	destroyAt := strings.Index(out, "- fake.database.module.old.store")
	if destroyAt < 0 || strings.Index(out, note) < destroyAt {
		t.Errorf("the note is not attached to the destroy operation:\n%s", out)
	}
}

func TestRenderDoesNotNoteUnrelatedDestroysAndCreates(t *testing.T) {
	// Different logical names, so nothing was moved. A note here would appear
	// on most plans that destroy anything, which is how a warning stops being
	// read.
	p := &Plan{
		Project:     "myapp",
		Environment: "dev",
		Operations: []Operation{
			{
				Address: address.Address{Module: []string{"new"}, Name: "cache"},
				Type:    "fake.database",
				Kind:    OpCreate,
				After:   map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
			{
				Address: address.Address{Module: []string{"old"}, Name: "store"},
				Type:    "fake.database",
				Kind:    OpDestroy,
				Before:  map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
			},
		},
	}
	if out := Render(p, RenderOptions{}); strings.Contains(out, "destroyed and recreated") {
		t.Errorf("two unrelated resources were reported as a move:\n%s", out)
	}
}

// TestRenderDoesNotNoteADestroyWithNoMatchingCreate covers the two remaining
// shapes that RESEMBLE a move without being one. A warning that fires on
// ordinary destroys teaches people to skip it, and then it is not there for the
// one destroy that matters — which is a worse outcome than not having the
// warning at all.
func TestRenderDoesNotNoteADestroyWithNoMatchingCreate(t *testing.T) {
	destroy := Operation{
		Address: address.Address{Module: []string{"old"}, Name: "store"},
		Type:    "fake.database",
		Kind:    OpDestroy,
		Before:  map[string]value.Value{"engine": value.String("postgres", value.SourceExplicit)},
	}

	for _, tc := range []struct {
		name string
		ops  []Operation
	}{
		{
			// The ordinary case: a plain destroy, nothing created at all.
			name: "nothing is created",
			ops:  []Operation{destroy},
		},
		{
			// Same logical name, same module move, DIFFERENT type. Two
			// resources of different types are not one resource that moved,
			// whatever they are called.
			name: "the created resource is a different type",
			ops: []Operation{
				{
					Address: address.Address{Module: []string{"new"}, Name: "store"},
					Type:    "fake.network",
					Kind:    OpCreate,
					After:   map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				},
				destroy,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Render(&Plan{Project: "myapp", Environment: "dev", Operations: tc.ops}, RenderOptions{})
			if strings.Contains(out, "destroyed and recreated") {
				t.Errorf("the move note fired on a destroy that only resembles a move:\n%s", out)
			}
		})
	}
}

// lineContainingInRender returns the single rendered line containing needle,
// failing if there is not exactly one. The count is the point: a note that
// appears twice is as wrong as one that never appears, and an assertion on
// "contains" alone would pass for both.
//
// This duplicates tests/integration's lineContaining (Task 10.1) because a
// test helper does not cross packages. If either grows a behaviour the other
// lacks, that is a signal the assertion moved, not that they should be merged.
func lineContainingInRender(t *testing.T, out, needle string) string {
	t.Helper()
	var found []string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, needle) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line containing %q, found %d:\n%s", needle, len(found), out)
	}
	return found[0]
}
