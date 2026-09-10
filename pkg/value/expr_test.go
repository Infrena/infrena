package value

import (
	"strings"
	"testing"
)

func TestReferenceString(t *testing.T) {
	r := Reference{Resource: "database", Attribute: "endpoint"}
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
				{Op: OpResourceRef, Ref: Reference{Resource: "database", Attribute: "endpoint"}},
			}},
			{Op: OpLiteral, Literal: String("-", SourceExplicit)},
			{Op: OpResourceRef, Ref: Reference{Resource: "network", Attribute: "id"}},
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
		{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "id"}},
		{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "id"}},
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
		return &Expr{Op: OpResourceRef, Ref: Reference{Resource: res, Attribute: attr}}
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
		{Op: OpResourceRef, Ref: Reference{Resource: "network", Attribute: "id"}},
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
	e := &Expr{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "endpoint"}}
	v := Unknown(KindString, SourceComputed)
	v.Expr = e

	if v.Known {
		t.Error("a value awaiting an expression must not be Known")
	}
	if v.Expr.Ref.Resource != "db" {
		t.Error("Value must carry the expression that will produce it")
	}
}

func TestExprIsNotSerialised(t *testing.T) {
	// State on disk records what a provider reported, never a pending
	// expression. Persisting one would resurrect a dangling reference on load.
	v := Unknown(KindString, SourceComputed)
	v.Expr = &Expr{Op: OpResourceRef, Ref: Reference{Resource: "db", Attribute: "endpoint"}}

	data, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	for _, leak := range []string{"expr", "Expr", "db.endpoint"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("serialised form leaks %q: %s", leak, data)
		}
	}
}
