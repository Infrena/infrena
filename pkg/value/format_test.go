package value

import (
	"strings"
	"testing"
)

// TestFormatNeverRendersRawItCannotVerify is the regression test for both
// measured leaks. The second one is the reason the Kind switch asserts Raw in
// EVERY arm rather than only in the composite ones: the first fix replaced a
// permissive default with an allowlist of scalar kinds and left
// fmt.Sprintf("%v", v.Raw) inside the scalar arm, so a Value claiming a scalar
// Kind while holding a composite Raw leaked exactly as before, one case label
// to the left of the fix.
func TestFormatNeverRendersRawItCannotVerify(t *testing.T) {
	secret := String("hunter2", SourceProvider).WithSensitive(true)
	composite := map[string]Value{"password": secret}

	cases := []struct {
		name string
		v    Value
	}{
		// The second leak: Kind claims a scalar, Raw holds a composite.
		{"int kind holding a composite", Value{Kind: KindInt, Known: true, Raw: composite}},
		{"float kind holding a composite", Value{Kind: KindFloat, Known: true, Raw: composite}},
		{"bool kind holding a composite", Value{Kind: KindBool, Known: true, Raw: composite}},
		{"string kind holding a composite", Value{Kind: KindString, Known: true, Raw: composite}},
		// The first leak: Kind never set at all.
		{"kind left at the zero value", Value{Known: true, Raw: composite}},
		// Composite kinds whose Raw is the wrong container type.
		{"map kind holding a foreign map", Value{Kind: KindMap, Known: true, Raw: map[string]any{"password": "hunter2"}}},
		{"list kind holding a foreign slice", Value{Kind: KindList, Known: true, Raw: []any{"hunter2"}}},
		// Scalar kinds whose Raw is merely the wrong scalar.
		{"int kind holding a string", Value{Kind: KindInt, Known: true, Raw: "hunter2"}},
		{"string kind holding an int", Value{Kind: KindString, Known: true, Raw: 12345}},
	}

	for _, tc := range cases {
		for _, opts := range []FormatOptions{
			{Unknown: "(unknown)"},
			{Unknown: "(known after apply)", QuoteStrings: true},
		} {
			got := Format(tc.v, opts)
			if strings.Contains(got, "hunter2") {
				t.Errorf("%s: secret rendered in clear text: %s", tc.name, got)
			}
			if got != Unrenderable {
				t.Errorf("%s: Format = %q, want %q — Kind is a claim about Raw and must never be trusted",
					tc.name, got, Unrenderable)
			}
		}
	}
}

// TestFormatRedactsAtEveryDepth pins that a sensitive leaf inside a
// well-formed composite is redacted, not just a sensitive value at the top.
func TestFormatRedactsAtEveryDepth(t *testing.T) {
	secret := String("hunter2", SourceProvider).WithSensitive(true)
	nested := Value{Kind: KindMap, Known: true, Raw: map[string]Value{
		"user": String("admin", SourceExplicit),
		"creds": {Kind: KindList, Known: true, Raw: []Value{
			{Kind: KindMap, Known: true, Raw: map[string]Value{"password": secret}},
		}},
	}}

	got := Format(nested, FormatOptions{Unknown: "(unknown)"})
	if strings.Contains(got, "hunter2") {
		t.Fatalf("a secret three levels down reached the page: %s", got)
	}
	if !strings.Contains(got, Redacted) {
		t.Errorf("expected a %s marker: %s", Redacted, got)
	}
	// Map keys sort, so output is stable across runs.
	if want := `{creds: [{password: <sensitive>}], user: admin}`; got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
}

// TestFormatOptionsAffectOnlyWhatTheyClaimTo guards the parameterisation.
// Redaction and fail-closed behaviour must not depend on either option — a
// secret hidden only under some options is a secret that leaks.
func TestFormatOptionsAffectOnlyWhatTheyClaimTo(t *testing.T) {
	bare := FormatOptions{Unknown: "(unknown)"}
	plan := FormatOptions{Unknown: "(known after apply)", QuoteStrings: true}

	if got := Format(String("eu-west-1", SourceExplicit), bare); got != "eu-west-1" {
		t.Errorf("bare string = %q, want unquoted", got)
	}
	if got := Format(String("eu-west-1", SourceExplicit), plan); got != `"eu-west-1"` {
		t.Errorf("plan string = %q, want quoted", got)
	}
	unknown := Unknown(KindString, SourceComputed)
	if got := Format(unknown, bare); got != "(unknown)" {
		t.Errorf("unknown under bare = %q", got)
	}
	if got := Format(unknown, plan); got != "(known after apply)" {
		t.Errorf("unknown under plan = %q", got)
	}
	// Sensitivity is not negotiable under either.
	secret := String("hunter2", SourceProvider).WithSensitive(true)
	for _, o := range []FormatOptions{bare, plan} {
		if got := Format(secret, o); got != Redacted {
			t.Errorf("secret rendered as %q, want %q", got, Redacted)
		}
	}
}

// TestFormatRendersOrdinaryValues guards the other direction: failing closed
// must not turn well-formed values into <unrenderable>.
func TestFormatRendersOrdinaryValues(t *testing.T) {
	o := FormatOptions{Unknown: "(unknown)"}
	cases := []struct {
		v    Value
		want string
	}{
		{String("eu-west-1", SourceExplicit), "eu-west-1"},
		{Int(20, SourceDefault), "20"},
		{Bool(true, SourceExplicit), "true"},
		{Float(1.5, SourceExplicit), "1.5"},
		{Value{Kind: KindList, Known: true, Raw: []Value{Int(1, SourceExplicit), Int(2, SourceExplicit)}}, "[1, 2]"},
	}
	for _, tc := range cases {
		if got := Format(tc.v, o); got != tc.want {
			t.Errorf("Format = %q, want %q", got, tc.want)
		}
	}
}
