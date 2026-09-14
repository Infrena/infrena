package value

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/address"
)

func TestAStepRendersAsItWasWritten(t *testing.T) {
	if got := (Step{Kind: StepKey, Key: "team"}).String(); got != ".team" {
		t.Errorf("Step.String() = %q, want %q", got, ".team")
	}
	if got := (Step{Kind: StepIndex, Index: 0}).String(); got != "[0]" {
		t.Errorf("Step.String() = %q, want %q", got, "[0]")
	}
}

func TestAReferenceRendersItsPath(t *testing.T) {
	// A resource attribute with a path: the form ${vpc.tags.Name}.
	r := Reference{
		Target:    address.Address{Name: "vpc"},
		Attribute: "tags",
		Path:      []Step{{Kind: StepKey, Key: "Name"}},
	}
	if got := r.String(); got != "vpc.tags.Name" {
		t.Errorf("String() = %q, want %q", got, "vpc.tags.Name")
	}

	// A variable with a mixed path: the form ${var.subnets[0].cidr}. Under
	// OpVarRef the Attribute is empty and the path carries everything.
	v := Reference{
		Target: address.Address{Name: "subnets"},
		Path:   []Step{{Kind: StepIndex, Index: 0}, {Kind: StepKey, Key: "cidr"}},
	}
	if got := v.String(); got != "subnets[0].cidr" {
		t.Errorf("String() = %q, want %q", got, "subnets[0].cidr")
	}
}

func TestTwoPathsIntoOneAttributeAreTwoReferences(t *testing.T) {
	// References() dedups on String(), so a path must be part of the key or
	// ${vpc.tags.Name} and ${vpc.tags.Env} collapse into one and the second
	// silently resolves to the first.
	e := &Expr{Op: OpConcat, Args: []*Expr{
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Name: "vpc"}, Attribute: "tags",
			Path: []Step{{Kind: StepKey, Key: "Name"}}}},
		{Op: OpResourceRef, Ref: Reference{Target: address.Address{Name: "vpc"}, Attribute: "tags",
			Path: []Step{{Kind: StepKey, Key: "Env"}}}},
	}}
	if got := len(e.References()); got != 2 {
		t.Errorf("References() returned %d, want 2 — a path must be part of the dedup key", got)
	}
}

func TestInModuleCarriesThePath(t *testing.T) {
	// InModule re-roots a reference written inside a module. If it dropped
	// Path, a module containing ${vpc.tags.Name} would silently resolve to
	// the whole tags map instead of the "Name" key — a silently wrong plan,
	// not an error.
	r := Reference{
		Target:    address.Address{Name: "vpc"},
		Attribute: "tags",
		Path:      []Step{{Kind: StepKey, Key: "Name"}},
	}
	got := r.InModule("net")
	if len(got.Path) != 1 || got.Path[0].Key != "Name" {
		t.Errorf("InModule dropped the path: Path = %+v, want one key step Name", got.Path)
	}
}

func TestReferenceString(t *testing.T) {
	r := LocalRef("database", "endpoint")
	if got := r.String(); got != "database.endpoint" {
		t.Errorf("String() = %q, want \"database.endpoint\"", got)
	}
}

func TestReferencesCollectsFromNestedExpr(t *testing.T) {
	// ${lower(database.endpoint)}-${network.id}
	e := &Expr{
		Op: OpConcat,
		Args: []*Expr{
			{Op: OpCall, Function: "lower", Args: []*Expr{
				{Op: OpResourceRef, Ref: LocalRef("database", "endpoint")},
			}},
			{Op: OpLiteral, Literal: String("-", SourceExplicit)},
			{Op: OpResourceRef, Ref: LocalRef("network", "id")},
		},
	}

	got := e.References()
	if len(got) != 2 {
		t.Fatalf("References() returned %d, want 2: %v", len(got), got)
	}
	if got[0].String() != "database.endpoint" || got[1].String() != "network.id" {
		t.Errorf("References() = %v, want [database.endpoint network.id] in source order", got)
	}
}

func TestReferencesDeduplicates(t *testing.T) {
	e := &Expr{Op: OpConcat, Args: []*Expr{
		{Op: OpResourceRef, Ref: LocalRef("db", "id")},
		{Op: OpResourceRef, Ref: LocalRef("db", "id")},
	}}
	if got := e.References(); len(got) != 1 {
		t.Errorf("References() = %v, want one entry — a reference used twice is one dependency edge", got)
	}
}

func TestReferencesOnNilIsEmptyNotPanic(t *testing.T) {
	var e *Expr
	if got := e.References(); len(got) != 0 {
		t.Errorf("References() on nil = %v, want empty", got)
	}
}

func TestStringRendersPasteableSource(t *testing.T) {
	// A diagnostic that shows source a user cannot paste back is a trap. The
	// delimiters belong at the top level only: rendering a call's arguments
	// through String() would give ${lower(${db.endpoint})}.
	ref := func(res, attr string) *Expr {
		return &Expr{Op: OpResourceRef, Ref: LocalRef(res, attr)}
	}

	cases := []struct {
		name string
		in   *Expr
		want string
	}{
		{
			name: "lone reference",
			in:   ref("database", "endpoint"),
			want: "${database.endpoint}",
		},
		{
			name: "call over a reference",
			in:   &Expr{Op: OpCall, Function: "lower", Args: []*Expr{ref("database", "engine")}},
			want: "${lower(database.engine)}",
		},
		{
			name: "nested call",
			in: &Expr{Op: OpCall, Function: "upper", Args: []*Expr{
				{Op: OpCall, Function: "lower", Args: []*Expr{ref("database", "engine")}},
			}},
			want: "${upper(lower(database.engine))}",
		},
		{
			name: "call with quoted literals",
			in: &Expr{Op: OpCall, Function: "replace", Args: []*Expr{
				ref("database", "engine"),
				{Op: OpLiteral, Literal: String("sql", SourceExplicit)},
				{Op: OpLiteral, Literal: String("SQL", SourceExplicit)},
			}},
			want: `${replace(database.engine, "sql", "SQL")}`,
		},
		{
			name: "concat of literal and reference",
			in: &Expr{Op: OpConcat, Args: []*Expr{
				{Op: OpLiteral, Literal: String("prefix-", SourceExplicit)},
				ref("network", "id"),
			}},
			want: "prefix-${network.id}",
		},
	}

	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%s: String() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStringRedactsASensitiveLiteral pins that a literal carrying a sensitive
// value never renders its raw contents through String() or inner(). A
// deferred expression can fold a resolved SENSITIVE value into an OpLiteral
// (internal/expressions' residual does exactly this), and String() is not the
// sanctioned redaction path — value.Format is — so it must defer to the same
// Redacted marker rather than growing a second one.
func TestStringRedactsASensitiveLiteral(t *testing.T) {
	secret := &Expr{Op: OpLiteral, Literal: String("hunter2", SourceVariable).WithSensitive(true)}

	// Top level, via String(): the literal sits directly in a concat, the
	// same shape residual produces for a folded sensitive part.
	concat := &Expr{Op: OpConcat, Args: []*Expr{
		secret,
		{Op: OpLiteral, Literal: String("-", SourceExplicit)},
		{Op: OpResourceRef, Ref: LocalRef("network", "id")},
	}}
	if got := concat.String(); strings.Contains(got, "hunter2") {
		t.Errorf("String() = %q leaks the sensitive literal", got)
	} else if !strings.Contains(got, Redacted) {
		t.Errorf("String() = %q, want it to contain %q", got, Redacted)
	}

	// Nested inside a call, via inner(): a quoted-literal argument position.
	call := &Expr{Op: OpCall, Function: "lower", Args: []*Expr{secret}}
	if got := call.String(); strings.Contains(got, "hunter2") {
		t.Errorf("String() = %q leaks the sensitive literal via inner()", got)
	} else if !strings.Contains(got, Redacted) {
		t.Errorf("String() = %q, want it to contain %q", got, Redacted)
	}
}

func TestStringOnNilIsEmptyNotPanic(t *testing.T) {
	var e *Expr
	if got := e.String(); got != "" {
		t.Errorf("String() on nil = %q, want empty", got)
	}
}

func TestValueCarriesExpr(t *testing.T) {
	e := &Expr{Op: OpResourceRef, Ref: LocalRef("db", "endpoint")}
	v := Unknown(KindString, SourceComputed)
	v.Expr = e

	if v.Known {
		t.Error("a value awaiting an expression must not be Known")
	}
	if v.Expr.Ref.Target.Name != "db" {
		t.Error("Value must carry the expression that will produce it")
	}
}

// TestAnExpressionSurvivesTheWire.
//
// This test used to assert the OPPOSITE — that Expr is never serialised — on the
// grounds that "state on disk records what a provider reported, never a pending
// expression; persisting one would resurrect a dangling reference on load". The concern
// is real and has NOT been dropped; it has moved to where it belongs. Refusing to encode
// an expression anywhere made it impossible for state to hold one, and also impossible
// for a PLAN ARTIFACT to hold one — and a plan's whole job is to record work not yet
// done, including the values that will only be known once part of it has run.
//
// The cost of the old rule was silent: a saved plan containing `network: ${net.id}`
// decoded to an unknown with no expression, which is indistinguishable from a computed
// attribute, so the executor dropped it. The apply reported success and left the
// attribute unset.
//
// The state invariant is now enforced by internal/state, which refuses to encode a
// resource attribute that is unknown or carries an expression — a checked rule rather
// than an emergent property of what Value declines to write.
func TestAnExpressionSurvivesTheWire(t *testing.T) {
	v := Unknown(KindString, SourceComputed)
	v.Expr = &Expr{Op: OpResourceRef, Ref: LocalRef("db", "endpoint")}

	data, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var back Value
	if err := back.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if back.Known {
		t.Error("a value awaiting an expression must not come back Known")
	}
	if back.Expr == nil {
		t.Fatalf("the expression did not survive: %s", data)
	}
	if back.Expr.Op != OpResourceRef {
		t.Errorf("Op = %v, want OpResourceRef — an operation read back wrongly changes what the expression means", back.Expr.Op)
	}
	if got := back.Expr.Ref.Target.Name; got != "db" {
		t.Errorf("reference target = %q, want db", got)
	}
	if got := back.Expr.Ref.Attribute; got != "endpoint" {
		t.Errorf("reference attribute = %q, want endpoint", got)
	}
}

// TestAPathIntoAResourceAttributeSurvivesTheWire guards exprwire.go's
// wireReference specifically for Path, which — until this test — it dropped
// silently in both directions. A saved plan containing ${vpc.tags.Name} would
// round-trip to ${vpc.tags}: apply would then resolve the WHOLE tags map
// instead of the Name key, with no error, which is PLAN.md §37's exact
// "carries the expression that reproduces it" guarantee broken silently.
//
// A MIXED path (a key then an index), because a fixture with only one kind of
// step could pass against a wire step that hard-codes StepKey and ignores
// Index — or the reverse.
func TestAPathIntoAResourceAttributeSurvivesTheWire(t *testing.T) {
	original := &Expr{
		Op: OpResourceRef,
		Ref: Reference{
			Target:    address.Address{Name: "vpc"},
			Attribute: "subnets",
			Path:      []Step{{Kind: StepIndex, Index: 0}, {Kind: StepKey, Key: "cidr"}},
		},
	}

	v := Unknown(KindString, SourceComputed)
	v.Expr = original

	data, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var back Value
	if err := back.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if back.Expr == nil {
		t.Fatalf("the expression did not survive: %s", data)
	}
	if got, want := back.Expr.String(), original.String(); got != want {
		t.Errorf("String() = %q after the wire, want %q — the path did not round-trip", got, want)
	}
	if len(back.Expr.Ref.Path) != 2 {
		t.Fatalf("Path = %+v, want 2 steps", back.Expr.Ref.Path)
	}
	if back.Expr.Ref.Path[0].Kind != StepIndex || back.Expr.Ref.Path[0].Index != 0 {
		t.Errorf("Path[0] = %+v, want the index step", back.Expr.Ref.Path[0])
	}
	if back.Expr.Ref.Path[1].Kind != StepKey || back.Expr.Ref.Path[1].Key != "cidr" {
		t.Errorf("Path[1] = %+v, want the key step \"cidr\"", back.Expr.Ref.Path[1])
	}
}

// TestAKnownValueWritesNoExpressionKey.
//
// The additivity claim, asserted rather than assumed. Every value in a state file and
// every value a provider plugin is sent or returns is KNOWN, so if a known value's bytes
// are unchanged, none of those formats changed at all.
func TestAKnownValueWritesNoExpressionKey(t *testing.T) {
	data, err := json.Marshal(String("10.0.0.0/16", SourceExplicit))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "expr") {
		t.Errorf("a known value gained an expr key: %s", data)
	}
}
