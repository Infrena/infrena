package planner

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infra/pkg/address"
	"infra/pkg/value"
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
				Address: address.Address{Name: "app"}, Type: "test.application", Kind: OpDestroy,
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
				Address: address.Address{Name: "database"}, Type: "test.database", Kind: OpReplace,
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
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpCreate,
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
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpCreate,
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

func TestRenderVerboseListsUnchangedResources(t *testing.T) {
	p := mixedPlan()
	p.Operations = append(p.Operations, Operation{
		Address: address.Address{Name: "zzz"}, Type: "test.network", Kind: OpNoOp,
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
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpNoOp,
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
				Address: address.Address{Name: "network"}, Type: "test.network", Kind: OpCreate,
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
				Address: address.Address{Name: "solo"}, Type: "test.network", Kind: OpDestroy,
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
				Address: address.Address{Name: "one"}, Type: "test.network", Kind: OpDestroy,
				Before:     map[string]value.Value{"cidr": value.String("10.0.0.0/16", value.SourceExplicit)},
				Dependents: []address.Address{{Name: "app"}},
			},
			{
				Address: address.Address{Name: "two"}, Type: "test.network", Kind: OpDestroy,
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

// TestRenderLeafFailsClosedOnUnexpectedShapes covers the branch a golden file
// cannot: values whose Kind and Raw disagree, or whose Kind was never set.
// See renderLeaf's doc comment for the measured leak this prevents.
func TestRenderLeafFailsClosedOnUnexpectedShapes(t *testing.T) {
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
			got := renderLeaf(tc.v)
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret rendered in clear text: %s", got)
			}
			if got != "<unrenderable>" {
				t.Errorf("renderLeaf = %q, want %q", got, "<unrenderable>")
			}
		})
	}
}
