package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

func summaryRS(name string, attrs map[string]value.Value) *resource.ResourceState {
	return &resource.ResourceState{Address: address.Address{Name: name}, Attributes: attrs}
}

// The entire output is compared exactly rather than with strings.Contains: a
// contains-only check on "<sensitive>" cannot tell a correctly redacted output
// apart from one that also leaked the secret beside it. The negative check
// below sits on top of that comparison, it does not replace it.
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

// Failed and Skipped are inserted in an order that contradicts the expected
// sorted order, so a missing sort fails this test rather than passing by coincidence.
//
// Failed is a map[string]error, and Go randomises range order per call rather
// than once per map instance, so with only two keys a single render has a
// 1-in-2 chance of coming out sorted with no sort at all. Requiring every one
// of many renders to match makes accidental agreement (1/2)^n. Skipped needs
// no such loop: it is a plain slice this test orders itself.
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

	for i := range 20 {
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

// splitOpID relies on the verb never containing ":", not on the address never
// containing one. address.Parse only splits on ".", so a Name of "alpha:beta"
// parses and "create:alpha:beta" is a real input: the split must stop at the
// first colon so the address is returned whole rather than truncated.
func TestSplitOpIDStopsAtFirstColon(t *testing.T) {
	verb, addr := splitOpID("create:alpha:beta")
	if verb != "create" || addr != "alpha:beta" {
		t.Fatalf("splitOpID(%q) = (%q, %q), want (%q, %q)", "create:alpha:beta", verb, addr, "create", "alpha:beta")
	}
}

// Applied is inserted in an order that contradicts the expected sorted order,
// as the Failed and Skipped test above does. Every other test passes a single
// Applied address, which cannot tell a real address.Sort call apart from one
// deleted entirely.
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

// Three attribute names, because a two-attribute case survives a deleted sort
// most of the time — map iteration only has to land "e before p" by chance.
//
// Three is still not enough on its own: Go randomises map range order per
// call, so with three keys one render in six comes out sorted anyway.
// Requiring every one of many renders to match draws a fresh order each time,
// making accidental agreement (1/6)^n, and pins that Render is deterministic —
// same Result in, byte-identical string out, on every call and not just the
// first.
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

	for i := range 20 {
		got := Render(r, RenderOptions{})
		if got != want {
			t.Fatalf("Render() iteration %d =\n%q\nwant\n%q", i, got, want)
		}
	}
}

// An Applied address that Result.State has no entry for: destroy and forget
// both remove the state entry while the address still belongs in Applied.
// Every other test sidesteps it by storing the resource it also lists as
// Applied, which leaves half of the "!ok || len(rs.Attributes) == 0" guard
// unexercised.
func TestRenderAppliedResourceMissingFromStateShowsHeaderOnly(t *testing.T) {
	st := &state.State{}
	r := Result{Applied: []address.Address{{Name: "gone"}}, Failed: map[string]error{}, State: st}

	got := Render(r, RenderOptions{})
	// "-", not "+": applied-but-absent-from-state is the definition of a
	// removal, so this is the destroy/forget rendering. Marking it "+" tells the
	// user the opposite of what happened — after `infrena destroy` every deleted
	// resource reads as created.
	want := "Apply complete: 1 applied, 0 failed, 0 skipped.\n" +
		"\n" +
		"Applied:\n" +
		"  - gone\n"
	if got != want {
		t.Fatalf("Render() =\n%q\nwant\n%q", got, want)
	}
	// Pin the distinction itself, not just this branch: the same address,
	// present in state, must render "+". Without this, swapping both markers
	// would leave the assertion above green.
	present := &state.State{}
	present.Set(summaryRS("gone", nil))
	r2 := Result{Applied: []address.Address{{Name: "gone"}}, Failed: map[string]error{}, State: present}
	if got2 := Render(r2, RenderOptions{}); !strings.Contains(got2, "  + gone") {
		t.Fatalf("a resource present in state must render \"+\", got:\n%q", got2)
	}
}

// The other half of the same guard: a resource present in state whose
// Attributes map is empty must render identically to the missing-from-state
// case, not as an empty attribute block.
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

// Result.State itself is nil, which every other test avoids because they all
// populate State to exercise attribute rendering.
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

// The only test exercising RenderOptions.Color; without it the marker
// functions' color branches go unverified. Exact full-string comparison rather
// than a substring check, so a wrong escape code or a marker in the wrong
// color is caught.
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

// Both halves of the flag, one exact-output comparison each: Verbose adds the
// provider line, and the default output is byte-identical without it.
// Asserting only the first would let a Verbose that always renders — a flag
// that is not a flag — pass.
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

// renderAppliedLines must not return the bare header for an attribute-less
// resource before the Verbose block runs: moved back above it, this resource
// loses its provider line while an attributes-present one keeps its own, which
// no test using only the attributes-present case can catch.
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

// The destroyed/forgotten shape, and any state entry recording neither field:
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

// The two partial cases are pinned separately, because an implementation that
// concatenated both fields unconditionally would produce "(provider test, id )"
// and still satisfy a test that always supplies both.
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

// verboseProvenance writes strings directly rather than through value.Format,
// so this pins that it cannot become a second rendering path for attribute
// data. A sensitive attribute named "provider" must still be redacted, and
// must not be confusable with the metadata line.
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
