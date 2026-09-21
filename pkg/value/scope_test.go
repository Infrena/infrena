package value

import (
	"encoding/json"
	"strings"
	"testing"
)

// allScopes is every defined Scope. Tests that must hold for all of them loop
// over this rather than sampling two or three: an exhaustive loop cannot be
// satisfied by the cases a test author happened to pick, and it fails loudly
// when a scope is added and the coverage is not extended with it.
var allScopes = []Scope{
	ScopeUnset,
	ScopeProviderDefault,
	ScopeBaseConfig,
	ScopeModuleDefault,
	ScopeEnvironmentInherit,
	ScopeEnvironmentVar,
	ScopeCLIOverride,
}

// TestEqualIgnoresScopeForEveryPairOfScopes pins that two values differing
// only in which scope supplied them are the same value, the same way Equal
// ignores provenance and sensitivity so a filled-in default does not read as a
// change.
//
// If Equal compared Scope, a no-op plan would stop being a no-op the moment a
// value moved between scopes: a `--var replicas=20` matching what
// variables.yml already said would plan as a change forever.
func TestEqualIgnoresScopeForEveryPairOfScopes(t *testing.T) {
	for _, a := range allScopes {
		for _, b := range allScopes {
			x := Int(20, SourceVariable).WithScope(a)
			y := Int(20, SourceVariable).WithScope(b)
			if !x.Equal(y) {
				t.Errorf("Int(20) at scope %s != Int(20) at scope %s: Equal is comparing Scope, "+
					"so a --var matching what variables.yml already said would plan as a change forever", a, b)
			}
		}
	}
}

// TestEqualIgnoresScopeAtEveryDepth is the composite half.
//
// Provenance is per-leaf, so an implementation could pass the scalar test
// above and still compare Scope inside the KindList and KindMap arms of Equal,
// which are separate code paths with their own recursion.
//
// Both the list element and the list value itself (held in the map) vary in
// scope between a and b. Varying only the element leaves both maps' "tags"
// entry at ScopeUnset, which the map arm compares equal either way — a test
// that cannot fail is not testing the map arm at all.
func TestEqualIgnoresScopeAtEveryDepth(t *testing.T) {
	for _, s := range allScopes {
		a := Map(map[string]Value{
			"tags": List([]Value{String("web", SourceVariable).WithScope(ScopeBaseConfig)}, SourceVariable).WithScope(ScopeBaseConfig),
		}, SourceVariable)
		b := Map(map[string]Value{
			"tags": List([]Value{String("web", SourceVariable).WithScope(s)}, SourceVariable).WithScope(s),
		}, SourceVariable)
		if !a.Equal(b) {
			t.Errorf("nested leaf at scope %s compared unequal to the same leaf at ScopeBaseConfig", s)
		}
	}
}

// TestEqualStillSeesADifferentDatumAtTheSameScope exercises the other answer.
// A predicate asserted in one direction only is not tested: `func Equal() bool
// { return true }` passes the two tests above.
func TestEqualStillSeesADifferentDatumAtTheSameScope(t *testing.T) {
	for _, s := range allScopes {
		if Int(20, SourceVariable).WithScope(s).Equal(Int(21, SourceVariable).WithScope(s)) {
			t.Errorf("Int(20) == Int(21) at scope %s: Equal has stopped comparing the datum", s)
		}
	}
}

// TestScopeRoundTripsThroughJSON pins that Scope survives state serialisation.
//
// Value marshals through an explicit wireValue struct (json.go), so Scope needs
// a deliberate field and tag — it will not round-trip by accident, and a
// missing tag fails silently: the value reads back as ScopeUnset, a confident
// wrong answer rather than an error, and `state show` and `explain` then report
// "no scope recorded" for values that have one.
//
// The loop covers every scope. ScopeUnset is the zero value, so a test that
// checked only it would pass against an implementation that persists nothing.
func TestScopeRoundTripsThroughJSON(t *testing.T) {
	for _, s := range allScopes {
		out := roundTrip(t, Int(20, SourceVariable).WithScope(s))
		if out.Scope != s {
			t.Errorf("Scope %s round-tripped as %s", s, out.Scope)
		}
		if out.Source != SourceVariable {
			t.Errorf("Source lost while adding Scope: got %v", out.Source)
		}
	}
}

// TestScopeRoundTripsAtEveryDepth pins that composites keep per-leaf scope.
// The wireValue path recurses through json.Marshal on []Value and
// map[string]Value, so this covers a different code path from the scalar case.
func TestScopeRoundTripsAtEveryDepth(t *testing.T) {
	in := Map(map[string]Value{
		"replicas": Int(20, SourceVariable).WithScope(ScopeCLIOverride),
		"tags": List([]Value{
			String("web", SourceVariable).WithScope(ScopeEnvironmentVar),
		}, SourceVariable).WithScope(ScopeBaseConfig),
	}, SourceExplicit).WithScope(ScopeBaseConfig)

	out := roundTrip(t, in)
	m, ok := out.Raw.(map[string]Value)
	if !ok {
		t.Fatalf("Raw is %T, want map[string]Value", out.Raw)
	}
	if m["replicas"].Scope != ScopeCLIOverride {
		t.Errorf("nested scalar Scope = %s, want ScopeCLIOverride", m["replicas"].Scope)
	}
	items, ok := m["tags"].Raw.([]Value)
	if !ok || len(items) != 1 {
		t.Fatalf("tags Raw is %#v", m["tags"].Raw)
	}
	if items[0].Scope != ScopeEnvironmentVar {
		t.Errorf("list element Scope = %s, want ScopeEnvironmentVar", items[0].Scope)
	}
}

// TestScopeWireNamesAreFrozen pins the persisted spelling of every Scope.
//
// Same contract as TestKindWireNamesAreFrozen: these strings are on disk. Scope
// is a uint8 whose iota ordering has been inserted into more than once, so
// persisting the number would silently reinterpret every state file written
// before the insertion.
//
// scoped_vars and instance_default stay at state version 1: ScopeUnset is
// omitted from the wire, so no file written before either of them holds a scope
// that needs translating. A Scope added from here should come with a version
// bump, which is an ordinary change now that state.Decode reads the migration
// path with json.Decoder.UseNumber() and no longer loses integer precision on
// the way through.
//
// The literals below are duplicated deliberately — deriving them from
// scopeWireNames would assert nothing.
func TestScopeWireNamesAreFrozen(t *testing.T) {
	frozen := map[Scope]string{
		ScopeProviderDefault:    "provider_default",
		ScopeInstanceDefault:    "instance_default",
		ScopeBaseConfig:         "base_config",
		ScopeScopedVars:         "scoped_vars",
		ScopeModuleDefault:      "module_default",
		ScopeEnvironmentInherit: "environment_inherit",
		ScopeEnvironmentVar:     "environment_var",
		ScopeCLIOverride:        "cli_override",
	}
	if len(scopeWireNames) != len(frozen) {
		t.Fatalf("scopeWireNames has %d entries, frozen contract has %d; a new Scope needs a state version bump",
			len(scopeWireNames), len(frozen))
	}
	for scope, want := range frozen {
		got, err := scopeToWireName(scope)
		if err != nil {
			t.Errorf("scopeToWireName(%v): %v", scope, err)
			continue
		}
		if got != want {
			t.Errorf("scope %v persists as %q, want %q", scope, got, want)
		}
		back, err := scopeFromWireName(want)
		if err != nil || back != scope {
			t.Errorf("scopeFromWireName(%q) = %v, %v; want %v, nil", want, back, err, scope)
		}
	}
}

// TestUnsetScopeIsOmittedFromTheWire pins that adding Scope does not rewrite
// every state file already on disk.
//
// ScopeUnset is the zero value precisely so that Values constructed before
// Scope existed stay valid. Persisting it as an explicit "scope":"unset" would
// churn every state file on the next apply for no information gain, and would
// make two files with identical content compare unequal.
func TestUnsetScopeIsOmittedFromTheWire(t *testing.T) {
	data, err := json.Marshal(Int(20, SourceVariable))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "scope") {
		t.Errorf("ScopeUnset was written to the wire: %s", data)
	}
}

// TestSetScopeReachesTheWireUnderItsOwnKey catches a field that is marshalled
// under the wrong key but symmetrically unmarshalled — which round-trips
// perfectly and is unreadable by anything else that parses the state file.
func TestSetScopeReachesTheWireUnderItsOwnKey(t *testing.T) {
	data, err := json.Marshal(Int(20, SourceVariable).WithScope(ScopeCLIOverride))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"scope":"cli_override"`) {
		t.Errorf("marshalled form lacks \"scope\":\"cli_override\": %s", data)
	}
}

// TestDecodingAnUnrecognisedScopeErrors pins that a state file written by a
// binary knowing a scope this one does not fails to decode, rather than
// silently reading back as ScopeUnset and degrading provenance without a word.
// scopeFromWireName already refuses it; this test is the only thing exercising
// that branch.
func TestDecodingAnUnrecognisedScopeErrors(t *testing.T) {
	data := []byte(`{"kind":"integer","known":true,"raw":20,"source":"variable","scope":"not_a_scope"}`)
	var v Value
	err := json.Unmarshal(data, &v)
	if err == nil {
		t.Fatal("Unmarshal accepted an unrecognised scope name; it should have failed rather than silently producing ScopeUnset")
	}
	if !strings.Contains(err.Error(), "not_a_scope") {
		t.Errorf("error %q does not name the offending scope", err)
	}
}

// allKinds is every Kind ParseKind is expected to handle, plus the zero value.
var allKinds = []Kind{KindInvalid, KindString, KindInt, KindFloat, KindBool, KindList, KindMap}

// TestKindStringAndParseKindAreInverses is what makes two switches safe to keep
// as two switches. It loops every Kind rather than sampling: an exhaustive
// round trip cannot be satisfied by an implementation that happens to agree on
// the two cases a test author picked.
func TestKindStringAndParseKindAreInverses(t *testing.T) {
	for _, k := range allKinds {
		name := k.String()
		got, ok := ParseKind(name)
		if k == KindInvalid {
			// The one deliberate asymmetry: String() answers "invalid" so a
			// diagnostic can name an unset Kind, but `type: invalid` in a
			// configuration file must reach the unknown-type diagnostic.
			if ok {
				t.Errorf("ParseKind(%q) accepted the KindInvalid spelling; `type: invalid` would silently become the zero Kind", name)
			}
			continue
		}
		if !ok || got != k {
			t.Errorf("ParseKind(%q) = %v, %v; want %v, true — String and ParseKind have drifted", name, got, ok, k)
		}
	}
}

// TestKindNamesMatchesParseKindExactly stops the diagnostic list from
// advertising a name that does not work, or omitting one that does. Both
// directions, because either alone is satisfied by an empty list.
func TestKindNamesMatchesParseKindExactly(t *testing.T) {
	names := KindNames()
	for _, name := range names {
		if _, ok := ParseKind(name); !ok {
			t.Errorf("KindNames lists %q, which ParseKind rejects", name)
		}
	}
	for _, k := range allKinds {
		if k == KindInvalid {
			continue
		}
		found := false
		for _, name := range names {
			if name == k.String() {
				found = true
			}
		}
		if !found {
			t.Errorf("ParseKind accepts %q but KindNames omits it, so no diagnostic offers it", k.String())
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("KindNames is not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// TestKindSpellingsAreFrozen duplicates the six literals deliberately. They are
// what a user writes in infrena.yml, so deriving them from the implementation
// would assert nothing about the product API they now are.
func TestKindSpellingsAreFrozen(t *testing.T) {
	frozen := map[string]Kind{
		"string":  KindString,
		"integer": KindInt,
		"float":   KindFloat,
		"boolean": KindBool,
		"list":    KindList,
		"map":     KindMap,
	}
	if len(KindNames()) != len(frozen) {
		t.Fatalf("KindNames has %d entries, the frozen language contract has %d", len(KindNames()), len(frozen))
	}
	for name, want := range frozen {
		if got, ok := ParseKind(name); !ok || got != want {
			t.Errorf("ParseKind(%q) = %v, %v; want %v, true", name, got, ok, want)
		}
		if got := want.String(); got != name {
			t.Errorf("%v.String() = %q, want %q", want, got, name)
		}
	}
	// The near-misses a user actually types must NOT resolve.
	for _, wrong := range []string{"int", "bool", "str", "number", "Integer", "invalid", ""} {
		if _, ok := ParseKind(wrong); ok {
			t.Errorf("ParseKind accepted %q", wrong)
		}
	}
}

// TestAsFloatDoesNotCoerceIntegers pins the trap in the doc comment, in both
// directions. If this ever starts passing for the KindInt case, a caller that
// relied on the distinction to tell 1 from 1.0 has silently changed behaviour.
func TestAsFloatDoesNotCoerceIntegers(t *testing.T) {
	if f, ok := Float(1.5, SourceVariable).AsFloat(); !ok || f != 1.5 {
		t.Errorf("AsFloat on a float = %v, %v; want 1.5, true", f, ok)
	}
	if _, ok := Int(1, SourceVariable).AsFloat(); ok {
		t.Error("AsFloat coerced a KindInt value; callers must handle int64 explicitly")
	}
	if _, ok := Unknown(KindFloat, SourceVariable).AsFloat(); ok {
		t.Error("AsFloat answered true for an unknown value")
	}
}

// planOpts is how a plan renders values, duplicated here rather than imported
// so that pkg/value does not depend on the renderer.
var planOpts = FormatOptions{Unknown: "(known after apply)", QuoteStrings: true}

// TestAnnotateMatchesTodaysPlanOutputForUnscopedValues pins the compatibility
// half: an unscoped value must annotate exactly as internal/planner renders it
// today, so pointing the renderer at Annotate cannot move plan output. If this
// table is wrong, the move surfaces as a wall of shifted golden tests with no
// clue which change caused them.
func TestAnnotateMatchesTodaysPlanOutputForUnscopedValues(t *testing.T) {
	cases := []struct {
		name string
		in   Value
		want string
	}{
		{"default", Int(100, SourceDefault), "100 [default]"},
		{"explicit", Int(100, SourceExplicit), "100"},
		{"variable", Int(100, SourceVariable), "100"},
		{"provider", String("x", SourceProvider), `"x"`},
		{"sensitive default", String("s", SourceDefault).WithSensitive(true), "<sensitive> [default]"},
		{"unknown", Unknown(KindString, SourceComputed), "(known after apply)"},
	}
	for _, tc := range cases {
		if got := Annotate(tc.in, planOpts); got != tc.want {
			t.Errorf("%s: Annotate = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAnnotateNamesTheScopeWhenThereIsOne is the forward half: the annotation
// must actually change once a scope is recorded, or stage 4 could stamp every
// value and no plan would ever say so.
func TestAnnotateNamesTheScopeWhenThereIsOne(t *testing.T) {
	cases := []struct {
		in   Value
		want string
	}{
		{Int(20, SourceVariable).WithScope(ScopeCLIOverride), "20 [variable, from --var]"},
		{Int(2, SourceDefault).WithScope(ScopeProviderDefault), "2 [default, from provider default]"},
		{Int(10, SourceEnvironment).WithScope(ScopeEnvironmentVar), "10 [environment, from environment config]"},
		{Int(1, SourceEnvironment).WithScope(ScopeEnvironmentInherit), "1 [environment, from environment inheritance]"},
	}
	for _, tc := range cases {
		if got := Annotate(tc.in, planOpts); got != tc.want {
			t.Errorf("Annotate = %q, want %q", got, tc.want)
		}
	}
}

// TestAnnotateFailsClosedOnAnUnsetSource pins the fail-closed guard for a
// scoped value whose Source was never set.
//
// ValueSource is a string whose zero value is "". Without the guard,
// annotation's final line builds "[" + string(v.Source) + ", from " +
// v.Scope.String() + "]" unconditionally once Scope is non-unset, so an empty
// Source renders as "[, from --var]" — a confident, wrong-looking annotation
// rather than no annotation. Nothing stamps a Scope without also stamping a
// Source today, so this is unreachable in the current tree; it stops being
// unreachable the moment stage 4 exists.
func TestAnnotateFailsClosedOnAnUnsetSource(t *testing.T) {
	v := Value{Kind: KindInt, Known: true, Raw: int64(5)}.WithScope(ScopeCLIOverride)
	if got := Annotate(v, planOpts); got != "5" {
		t.Errorf("Annotate on a scoped value with unset Source = %q, want \"5\" (no annotation)", got)
	}
}

// TestAnnotateStaysQuietForOrdinaryConfiguration pins the suppression rule in
// the direction that would otherwise go unnoticed: explicit base configuration
// gets NO annotation, at either ScopeUnset or ScopeBaseConfig. Annotating it
// would put "[explicit, from base config]" on nearly every line of every plan
// and bury the markers that carry information.
func TestAnnotateStaysQuietForOrdinaryConfiguration(t *testing.T) {
	for _, s := range []Scope{ScopeUnset, ScopeBaseConfig} {
		if got := Annotate(String("web", SourceExplicit).WithScope(s), planOpts); got != `"web"` {
			t.Errorf("explicit value at scope %s annotated as %q, want bare", s, got)
		}
	}
	// An unknown value has no origin yet — the expression that will produce it
	// does — so it is never annotated, whatever scope it carries.
	for _, s := range allScopes {
		got := Annotate(Unknown(KindString, SourceVariable).WithScope(s), planOpts)
		if got != "(known after apply)" {
			t.Errorf("unknown value at scope %s annotated as %q", s, got)
		}
	}
}

// TestAnnotateRedactsThroughFormat pins that Annotate never renders anything
// itself. Format is the only redaction path in the tree, and a second one
// leaks whenever only the first is fixed. The annotation is a suffix on
// whatever Format returned — where a value came from is not itself secret, and
// hiding it would remove the only clue a user has for finding the secret they
// need to change.
func TestAnnotateRedactsThroughFormat(t *testing.T) {
	secret := String("hunter2", SourceVariable).WithSensitive(true).WithScope(ScopeCLIOverride)
	got := Annotate(secret, planOpts)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Annotate leaked a sensitive value: %q", got)
	}
	if got != "<sensitive> [variable, from --var]" {
		t.Errorf("Annotate = %q, want %q", got, "<sensitive> [variable, from --var]")
	}
	// Per-leaf: a non-sensitive composite holding a sensitive leaf.
	nested := Map(map[string]Value{
		"password": String("hunter2", SourceVariable).WithSensitive(true),
	}, SourceExplicit)
	if strings.Contains(Annotate(nested, planOpts), "hunter2") {
		t.Error("Annotate leaked a nested sensitive value")
	}
}
