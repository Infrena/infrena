package cli

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
)

func res(addr, typ string, deps ...string) (string, *resource.ResolvedResource) {
	a, _ := address.Parse(addr)
	r := &resource.ResolvedResource{Address: a, Type: typ}
	for _, d := range deps {
		p, _ := address.Parse(d)
		r.DependsOn = append(r.DependsOn, p)
	}
	return addr, r
}

// TestGraphDrawsDependentsBeneathTheirDependency. Edges point from a resource to
// what it needs; the tree is drawn the other way, because the question a reader
// is asking is "what gets created after this", which is the executor's order.
func TestGraphDrawsDependentsBeneathTheirDependency(t *testing.T) {
	m := map[string]*resource.ResolvedResource{}
	for _, f := range []func() (string, *resource.ResolvedResource){
		func() (string, *resource.ResolvedResource) { return res("net", "fake.network") },
		func() (string, *resource.ResolvedResource) { return res("db", "fake.database", "net") },
		func() (string, *resource.ResolvedResource) { return res("app", "fake.application", "db") },
	} {
		k, v := f()
		m[k] = v
	}

	var sb strings.Builder
	renderGraph(&sb, m)
	got := sb.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), got)
	}
	// The root is the thing nothing depends on, and it is unindented.
	if lines[0] != "fake.network.net" {
		t.Errorf("first line = %q, want the root unindented", lines[0])
	}
	// Depth must increase: a flat list would satisfy "contains all three".
	if !strings.Contains(lines[1], "└── fake.database.db") {
		t.Errorf("second line = %q, want db one level in", lines[1])
	}
	if !strings.HasPrefix(lines[2], "     ") || !strings.Contains(lines[2], "fake.application.app") {
		t.Errorf("third line = %q, want app indented BENEATH db, not beside it", lines[2])
	}
}

// TestGraphShowsASharedDependencyUnderEachDependent. Not a duplicate to
// deduplicate: the tree answers "what must exist before this", and a shared
// dependency genuinely sits beneath both.
func TestGraphShowsASharedDependencyUnderEachDependent(t *testing.T) {
	m := map[string]*resource.ResolvedResource{}
	for _, f := range []func() (string, *resource.ResolvedResource){
		func() (string, *resource.ResolvedResource) { return res("net", "fake.network") },
		func() (string, *resource.ResolvedResource) { return res("a", "fake.database", "net") },
		func() (string, *resource.ResolvedResource) { return res("b", "fake.database", "net") },
		func() (string, *resource.ResolvedResource) { return res("z", "fake.application", "a", "b") },
	} {
		k, v := f()
		m[k] = v
	}
	var sb strings.Builder
	renderGraph(&sb, m)
	if n := strings.Count(sb.String(), "fake.application.z"); n != 2 {
		t.Errorf("z appears %d times, want 2 — it depends on both a and b:\n%s", n, sb.String())
	}
}

// TestGraphIsDeterministic. The tree is read by a human and diffed by a script;
// map order must never reach it. The fixture declares roots in reverse order so
// a missing sort is visible rather than lucky.
func TestGraphIsDeterministic(t *testing.T) {
	m := map[string]*resource.ResolvedResource{}
	for _, n := range []string{"zeta", "mid", "alpha"} {
		k, v := res(n, "fake.network")
		m[k] = v
	}
	var first string
	for i := range 20 {
		var sb strings.Builder
		renderGraph(&sb, m)
		if i == 0 {
			first = sb.String()
			continue
		}
		if sb.String() != first {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, first, sb.String())
		}
	}
	if !strings.HasPrefix(first, "fake.network.alpha") {
		t.Errorf("roots are not sorted; got:\n%s", first)
	}
}

// TestGraphOnAnEmptyConfigurationSaysSo, rather than printing nothing and
// leaving a user unsure whether the command ran.
func TestGraphOnAnEmptyConfigurationSaysSo(t *testing.T) {
	var sb strings.Builder
	renderGraph(&sb, map[string]*resource.ResolvedResource{})
	if !strings.Contains(sb.String(), "No resources") {
		t.Errorf("empty output = %q", sb.String())
	}
}
