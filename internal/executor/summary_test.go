package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
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
//
// Failed is a map[string]error, and Go randomises range order per call —
// not once per map instance — so with only 2 keys a single render has a
// 1-in-2 chance of coming out already sorted even with no sort.Strings
// call in sortedFailedIDs at all. Rendering the same Result repeatedly and
// requiring every call to match makes accidental agreement (1/2)^n rather
// than a single coin flip: measured at n=20 against a deliberately
// unsorted sortedFailedIDs, this fails all 20 times (see task-12-report.md
// round 2). Skipped and Applied need no such loop — Skipped is a plain
// slice this test orders itself, and Applied here has one element — so
// this only re-derives the same map-order risk Failed has.
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

	for i := 0; i < 20; i++ {
		got := Render(r, RenderOptions{})
		if got != want {
			t.Fatalf("Render() iteration %d =\n%q\nwant\n%q", i, got, want)
		}
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
// often than not (map iteration only has to land "e before p" by chance).
//
// Three names alone are still not enough, though: Go randomises map range
// order per CALL, not once per map instance, so with 3 keys (3! = 6
// orderings, one of them already sorted) a single render still has a
// 1-in-6 chance of passing with the sort deleted — measured at ~17% (2/10)
// against a version of this package with sort.Strings(names) removed.
// Rendering the same Result repeatedly and requiring every call to match
// draws a fresh random order each time, so accidental agreement across n
// renders is (1/6)^n: measured at n=20 against the same deliberately
// unsorted build, this fails all 20 times (see task-12-report.md round 2).
// This also pins something worth asserting in its own right — the Global
// Constraint that Render is deterministic, same Result in, byte-identical
// string out, on every call, not just the first.
func TestRenderSortsAttributeNamesRegardlessOfMapOrder(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("host", map[string]value.Value{
		"zone":   value.String("us-east-1a", value.SourceExplicit),
		"ami":    value.String("ami-1234", value.SourceExplicit),
		"region": value.String("us-east-1", value.SourceExplicit),
	}))
	r := Result{Applied: []address.Address{{Name: "host"}}, Failed: map[string]error{}, State: st}

	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + host\n" +
		"      ami: \"ami-1234\"\n" +
		"      region: \"us-east-1\"\n" +
		"      zone: \"us-east-1a\"\n"

	for i := 0; i < 20; i++ {
		got := Render(r, RenderOptions{})
		if got != want {
			t.Fatalf("Render() iteration %d =\n%q\nwant\n%q", i, got, want)
		}
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
	// "-", not "+": applied-but-absent-from-state IS the definition of a
	// removal, so this is the destroy/forget rendering. Marking it "+"
	// told the user the opposite of what happened — after `infra destroy`
	// every deleted resource read as created.
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  - gone\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
	// Pin the distinction itself, not just this branch: the SAME address,
	// present in state, must render "+". Without this, swapping both
	// markers would leave the test above green.
	present := &state.State{}
	present.Set(summaryRS("gone", nil))
	r2 := Result{Applied: []address.Address{{Name: "gone"}}, Failed: map[string]error{}, State: present}
	if got2 := Render(r2, RenderOptions{}); !strings.Contains(got2, "  + gone") {
		t.Fatalf("a resource present in state must render \"+\", got:\n%q", got2)
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

// summaryRSWithProvider is summaryRS plus the provider identity fields,
// which only RenderOptions.Verbose renders.
func summaryRSWithProvider(name, provider, providerID string, attrs map[string]value.Value) *resource.ResourceState {
	return &resource.ResourceState{
		Address:    address.Address{Name: name},
		Provider:   provider,
		ProviderID: providerID,
		Attributes: attrs,
	}
}

// TestRenderVerboseAddsProviderIdentityAndDefaultDoesNot pins BOTH halves of
// the flag in one exact-output comparison each: that Verbose adds the
// provider line, and that the default output is byte-identical to what it
// was before Verbose existed. Asserting only the first would let a Verbose
// that always rendered — i.e. a flag that is once again not a flag — pass.
func TestRenderVerboseAddsProviderIdentityAndDefaultDoesNot(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRSWithProvider("db", "test", "db-7f3a", map[string]value.Value{
		"engine": value.String("postgres", value.SourceExplicit),
	}))
	r := Result{Applied: []address.Address{{Name: "db"}}, Failed: map[string]error{}, State: st}

	quiet := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + db\n" +
		"      engine: \"postgres\"\n"
	if got := Render(r, RenderOptions{}); got != quiet {
		t.Fatalf("default Render() =\n%q\nwant\n%q", got, quiet)
	}

	loud := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + db\n" +
		"      (provider test, id db-7f3a)\n" +
		"      engine: \"postgres\"\n"
	if got := Render(r, RenderOptions{Verbose: true}); got != loud {
		t.Fatalf("verbose Render() =\n%q\nwant\n%q", got, loud)
	}
}

// TestRenderVerboseShowsProviderIdentityForAResourceWithNoAttributes guards
// the early return that the Verbose change had to move. Before Verbose,
// renderAppliedLines returned the bare header when a resource had no
// attributes; if that early return is restored above the Verbose block, this
// resource loses its provider line while the one above keeps its own, so a
// test using only the attributes-present case cannot catch it.
func TestRenderVerboseShowsProviderIdentityForAResourceWithNoAttributes(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRSWithProvider("empty", "test", "empty-01", nil))
	r := Result{Applied: []address.Address{{Name: "empty"}}, Failed: map[string]error{}, State: st}

	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + empty\n" +
		"      (provider test, id empty-01)\n"
	if got := Render(r, RenderOptions{Verbose: true}); got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderVerboseOmitsTheLineWhenThereIsNoProviderIdentity covers the
// destroyed/forgotten shape and any state entry recording neither field:
// printing "()" would be worse than printing nothing.
func TestRenderVerboseOmitsTheLineWhenThereIsNoProviderIdentity(t *testing.T) {
	st := &state.State{}
	st.Set(summaryRS("bare", map[string]value.Value{"a": value.String("1", value.SourceExplicit)}))
	r := Result{Applied: []address.Address{{Name: "bare"}}, Failed: map[string]error{}, State: st}

	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + bare\n" +
		"      a: \"1\"\n"
	if got := Render(r, RenderOptions{Verbose: true}); got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderVerboseHandlesEitherIdentityFieldAlone pins the two partial
// cases separately, because a single implementation that concatenated both
// unconditionally would produce "(provider test, id )" and still satisfy a
// test that only ever supplies both.
func TestRenderVerboseHandlesEitherIdentityFieldAlone(t *testing.T) {
	for _, tc := range []struct {
		name, provider, providerID, wantLine string
	}{
		{"providerOnly", "test", "", "      (provider test)\n"},
		{"idOnly", "", "id-9", "      (id id-9)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{}
			st.Set(summaryRSWithProvider("r", tc.provider, tc.providerID, nil))
			res := Result{Applied: []address.Address{{Name: "r"}}, Failed: map[string]error{}, State: st}
			want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n\nApplied:\n  + r\n" + tc.wantLine
			if got := Render(res, RenderOptions{Verbose: true}); got != want {
				t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// TestRenderVerboseNeverRendersAnAttributeOutsideValueFormat is the §36
// guard for the new code path: verboseProvenance writes strings directly
// rather than through value.Format, so this proves it cannot become a second
// rendering path for attribute data. A sensitive attribute named "provider"
// must still be redacted, and must not be confusable with the metadata line.
func TestRenderVerboseNeverRendersAnAttributeOutsideValueFormat(t *testing.T) {
	secret := value.Value{Kind: value.KindString, Raw: "hunter2", Source: value.SourceProvider, Sensitive: true}
	st := &state.State{}
	st.Set(summaryRSWithProvider("db", "test", "db-1", map[string]value.Value{"provider": secret}))
	r := Result{Applied: []address.Address{{Name: "db"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{Verbose: true})
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Render() leaked the raw secret:\n%q", got)
	}
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  + db\n" +
		"      (provider test, id db-1)\n" +
		"      provider: <sensitive>\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
}
