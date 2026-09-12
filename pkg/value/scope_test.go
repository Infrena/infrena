package value

import (
	"encoding/json"
	"strings"
	"testing"
)

// allScopes is every defined Scope. Tests that must hold for all of them LOOP
// over this rather than sampling two or three.
//
// Sampling is not a style question here. Measured on this project: an
// assertion over two map keys inserted in sorted order passed ~88% of the time
// against deliberately broken code, and six keys still passed 38%. An
// exhaustive loop over a fixed slice has no such failure mode, and it also
// fails loudly when M5 adds a scope and forgets to extend the coverage.
var allScopes = []Scope{
	ScopeUnset,
	ScopeProviderDefault,
	ScopeBaseConfig,
	ScopeModuleDefault,
	ScopeEnvironmentInherit,
	ScopeEnvironmentVar,
	ScopeCLIOverride,
}

// TestEqualIgnoresScopeForEveryPairOfScopes is invariant 1.
//
// M2 established that value.Equal ignores provenance and sensitivity so that a
// filled-in default does not read as a change. Scope joins that list: two
// values that differ only in WHICH SCOPE supplied them are the same value.
//
// If Equal compares Scope, acceptance invariant 2 (no-op plan) breaks the
// moment a value moves between scopes — a `--var replicas=20` that matches
// what variables.yml already said would plan as a change forever. That is the
// exact phantom-diff shape M3 spent a Critical fixing.
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

// TestEqualIgnoresScopeAtEveryDepth is the composite half of invariant 1.
//
// Provenance is per-leaf (spec §5.1), so an implementation could pass the
// scalar test above and still compare Scope inside the KindList and KindMap
// arms of Equal — which are separate code paths with their own recursion.
//
// Both the list ELEMENT and the list VALUE ITSELF (held in the map) vary in
// scope between a and b. Varying only the element leaves both maps' "tags"
// entry at ScopeUnset, which the map arm would then compare equal on scope
// regardless of whether Equal's map arm ignores Scope or not — a test that
// cannot fail is not testing the map arm at all.
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

// TestEqualStillSeesADifferentDatumAtTheSameScope exercises the OTHER answer.
//
// A predicate asserted in one direction only is not tested: `func Equal() bool
// { return true }` passes the two tests above. Every predicate in this project
// that survived mutation testing survived in exactly one direction.
func TestEqualStillSeesADifferentDatumAtTheSameScope(t *testing.T) {
	for _, s := range allScopes {
		if Int(20, SourceVariable).WithScope(s).Equal(Int(21, SourceVariable).WithScope(s)) {
			t.Errorf("Int(20) == Int(21) at scope %s: Equal has stopped comparing the datum", s)
		}
	}
}

// TestScopeRoundTripsThroughJSON is invariant 3.
//
// M1 requires Value to survive state serialisation losslessly. Value marshals
// through an explicit wireValue struct (pkg/value/json.go), so Scope needs a
// deliberate field and tag — it will NOT round-trip by accident, and a missing
// tag fails SILENTLY: the value reads back as ScopeUnset, which is a confident
// wrong answer rather than an error. `state show` and `explain` would then
// report "no scope recorded" for values that have one.
//
// The loop deliberately covers every scope. ScopeUnset is the zero value, so a
// test that checked only it would pass against an implementation that persists
// nothing at all.
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
// Same contract as TestKindWireNamesAreFrozen: these strings are on disk.
// Scope is a uint8 whose iota ordering M5 and M7 both inserted into, so
// persisting the NUMBER would silently reinterpret every state file written
// before them.
//
// M7 added scoped_vars and did NOT bump state.CurrentVersion, against this
// test's original advice, having tried it. Bumping requires a no-op migration
// from 1 to 2, and Decode routes every non-current file through
// json.Unmarshal into map[string]any — where a number becomes a float64 and an
// integer beyond 2^53 comes back WRONG. internal/state's own golden test
// catches it: 9007199254740993 read back as 9007199254740992.
//
// So the bump buys a better message for a version 1 build reading a version 2
// file ("written by a newer version" rather than "unknown scope"), and costs
// silent precision loss when THIS build reads any existing file. That trade is
// the wrong way round for a file whose whole job is fidelity, and the message
// only matters once more than one build exists in the world.
//
// Revisit when the state format changes for a real reason: fixing the
// migration path to use json.Decoder.UseNumber() removes the cost, and is
// recorded as a follow-up. The
// literals below are duplicated deliberately — deriving them from
// scopeWireNames would assert nothing.
func TestScopeWireNamesAreFrozen(t *testing.T) {
	frozen := map[Scope]string{
		ScopeProviderDefault:    "provider_default",
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
// ScopeUnset is the zero value precisely so that Values constructed before M4
// stay valid. Persisting it as an explicit "scope":"unset" would churn every
// state file on the next apply for no information gain, and would make a
// pre-M4 file and a post-M4 file with identical content compare unequal.
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

// TestDecodingAnUnrecognisedScopeErrors is FIX 4: a state file written by a
// binary that knows a scope this binary does not (e.g. a pre-M5 binary
// reading state written after M5 adds an eighth precedence level) must fail
// to decode rather than silently read back as ScopeUnset. Silently degrading
// provenance in a state file is the wrong-answer shape this project has paid
// for before — scopeFromWireName already refuses it; this test is the only
// thing exercising that branch.
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
// what a user writes in infra.yml, so deriving them from the implementation
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

// planOpts is how a plan will render values. Duplicated here rather than
// imported, because internal/planner may not import test helpers from
// pkg/value and this task must not depend on the renderer at all.
var planOpts = FormatOptions{Unknown: "(known after apply)", QuoteStrings: true}

// TestAnnotateMatchesTodaysPlanOutputForUnscopedValues pins the compatibility
// half of the contract with Task 9.
//
// Every Value in the tree has ScopeUnset until stage 4 exists, so when Task 9
// points renderAnnotated at Annotate, plan output must not move. These are the
// exact strings internal/planner produces today; if this table is wrong, Task 9
// discovers it as a wall of moved golden tests with no idea which change caused
// them.
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
// unreachable the moment stage 4 exists, which is exactly what this task
// enables and does not itself build.
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
// itself. Format is the ONLY redaction path in the tree; a second one is how
// a leak fixed in one place survived in the other. The annotation is a SUFFIX
// on whatever Format returned — where a value came from is not itself secret,
// and hiding it would remove the only clue a user has for finding the secret
// they need to change.
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
