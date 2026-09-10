package executor

import (
	"errors"
	"strings"
	"testing"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

func summaryRS(name string, attrs map[string]value.Value) *resource.ResourceState {
	return &resource.ResourceState{Address: address.Address{Name: name}, Attributes: attrs}
}

// TestRenderRedactsSensitiveAttributes compares the ENTIRE output exactly,
// not with strings.Contains: a contains-only check on "<sensitive>" cannot
// tell a correctly redacted output apart from one that ALSO leaked
// "hunter2" beside it. The negative check below is belt-and-suspenders on
// top of the exact comparison, not a substitute for it.
func TestRenderRedactsSensitiveAttributes(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("db", map[string]value.Value{
		"engine":   value.String("postgres", value.SourceExplicit),
		"password": value.String("hunter2", value.SourceProvider).WithSensitive(true),
	}))
	r := Result{Applied: []address.Address{{Name: "db"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + db\n" +
		"      engine: \"postgres\"\n" +
		"      password: <sensitive>\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "hunter2") {
		t.Fatal("Render() leaked the raw secret")
	}
}

// TestRenderSortsFailedAndSkippedRegardlessOfInputOrder inserts both
// Failed and Skipped in an order that CONTRADICTS the expected sorted
// order (zulu before alpha, zebra/alpha-dep/mango not alphabetical), so a
// missing sort.Strings/address.Sort call fails this test rather than
// passing by coincidence.
func TestRenderSortsFailedAndSkippedRegardlessOfInputOrder(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("network", map[string]value.Value{
		"cidr": value.String("10.0.0.0/16", value.SourceExplicit),
	}))
	r := Result{
		Applied: []address.Address{{Name: "network"}},
		Failed: map[string]error{
			"update:zulu":  errors.New("connection refused"),
			"create:alpha": errors.New("quota exceeded"),
		},
		Skipped: []string{"update:zebra", "create:alpha-dep", "destroy:mango"},
		State:   st,
	}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 2 failed, 3 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + network\n" +
		"      cidr: \"10.0.0.0/16\"\n" +
		"\n" +
		"Failed:\n" +
		"  x create alpha: quota exceeded\n" +
		"  x update zulu: connection refused\n" +
		"\n" +
		"Skipped (see the diagnostics for which dependency failed):\n" +
		"  - create alpha-dep\n" +
		"  - destroy mango\n" +
		"  - update zebra\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderEmptyResultShowsZeroCounts(t *testing.T) {
	got := Render(Result{Failed: map[string]error{}}, RenderOptions{})
	want := "Apply complete: 0 applied, 0 failed, 0 skipped.\n"
	if got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

// TestSplitOpIDStopsAtFirstColon pins the real guarantee splitOpID relies
// on: the VERB never contains ":", not that an address never does.
// address.Parse only splits on ".", so an address whose Name is
// "alpha:beta" parses successfully and its String() is "alpha:beta" — an
// id of "create:alpha:beta" is therefore a real input this function must
// handle, and SplitN(id, ":", 2) must stop at the FIRST colon so the
// second half is returned whole rather than truncated at the second colon.
func TestSplitOpIDStopsAtFirstColon(t *testing.T) {
	verb, addr := splitOpID("create:alpha:beta")
	if verb != "create" || addr != "alpha:beta" {
		t.Fatalf("splitOpID(%q) = (%q, %q), want (%q, %q)", "create:alpha:beta", verb, addr, "create", "alpha:beta")
	}
}

// TestRenderSortsAppliedRegardlessOfInputOrder inserts Applied in an order
// (zulu, alpha, mango) that CONTRADICTS the expected sorted order, mirroring
// how TestRenderSortsFailedAndSkippedRegardlessOfInputOrder pins Failed and
// Skipped. Every prior test happens to pass a single Applied address, which
// cannot distinguish a real address.Sort call from one deleted entirely —
// mutation testing confirmed removing that call leaves this package green
// without this test.
func TestRenderSortsAppliedRegardlessOfInputOrder(t *testing.T) {
	r := Result{
		Applied: []address.Address{{Name: "zulu"}, {Name: "alpha"}, {Name: "mango"}},
		Failed:  map[string]error{},
	}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 3 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + alpha\n" +
		"  + mango\n" +
		"  + zulu\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderSortsAttributeNamesRegardlessOfMapOrder uses three attribute
// names whose natural (alphabetical) order does not match Go's arbitrary
// map iteration order often enough to pass by coincidence: mutation testing
// showed that deleting the sort.Strings call over attribute names left the
// two-attribute case in TestRenderRedactsSensitiveAttributes passing more
// often than not (map iteration only has to land "e before p" by chance),
// making that test a weak, flaky guard. Three names spread across the
// alphabet make an unsorted pass far less likely to slip through.
func TestRenderSortsAttributeNamesRegardlessOfMapOrder(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("host", map[string]value.Value{
		"zone":   value.String("us-east-1a", value.SourceExplicit),
		"ami":    value.String("ami-1234", value.SourceExplicit),
		"region": value.String("us-east-1", value.SourceExplicit),
	}))
	r := Result{Applied: []address.Address{{Name: "host"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + host\n" +
		"      ami: \"ami-1234\"\n" +
		"      region: \"us-east-1\"\n" +
		"      zone: \"us-east-1a\"\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderAppliedResourceMissingFromStateShowsHeaderOnly covers an
// Applied address that Result.State does not have an entry for — a real
// case (destroy and forget both remove the state entry, yet the address
// still belongs in Applied) that every other test sidesteps by always
// storing the resource it also lists as Applied. Mutation testing showed
// collapsing the "!ok || len(rs.Attributes) == 0" guard down to "!ok" alone
// left this package green with no test noticing the len(...) == 0 half was
// never exercised at all.
func TestRenderAppliedResourceMissingFromStateShowsHeaderOnly(t *testing.T) {
	st := &state.State{}
	r := Result{Applied: []address.Address{{Name: "gone"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + gone\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderAppliedResourceWithNoAttributesShowsHeaderOnly covers the other
// half of the same guard: a resource IS present in state but its
// Attributes map is empty, which must render identically to the
// missing-from-state case rather than an empty attribute block.
func TestRenderAppliedResourceWithNoAttributesShowsHeaderOnly(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("empty", nil))
	r := Result{Applied: []address.Address{{Name: "empty"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + empty\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderNilStateShowsHeaderOnly covers Result.State itself being nil,
// which every other test avoids since they all populate State to exercise
// attribute rendering. Never observed by any test before this one.
func TestRenderNilStateShowsHeaderOnly(t *testing.T) {
	r := Result{Applied: []address.Address{{Name: "solo"}}, Failed: map[string]error{}}

	got := Render(r, RenderOptions{})
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + solo\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderColorWrapsMarkersInANSI is the only test exercising
// RenderOptions.Color; without it, appliedMarker/failedMarker/skippedMarker's
// color branches are unverified dead code from a coverage standpoint. Exact
// full-string comparison, not a substring check, so a wrong escape code or
// a marker using the wrong color would be caught.
func TestRenderColorWrapsMarkersInANSI(t *testing.T) {
	r := Result{
		Applied: []address.Address{{Name: "a"}},
		Failed:  map[string]error{"create:b": errors.New("boom")},
		Skipped: []string{"update:c"},
	}

	got := Render(r, RenderOptions{Color: true})
	want := "Apply complete: 1 applied, 1 failed, 1 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  \x1b[32m+\x1b[0m a\n" +
		"\n" +
		"Failed:\n" +
		"  \x1b[1;31mx\x1b[0m create b: boom\n" +
		"\n" +
		"Skipped (see the diagnostics for which dependency failed):\n" +
		"  \x1b[36m-\x1b[0m update c\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}
