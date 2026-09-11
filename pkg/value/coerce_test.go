package value

import "testing"

// TestCoerceExactConversionsSucceed pins the accepting direction of the ONE
// exactness rule Coerce enforces, reused unchanged by internal/config's
// stage 2 (on a variable's bounds and its default) and, from M4 Task 6,
// stage 4 (on a value resolved against a schema).
func TestCoerceExactConversionsSucceed(t *testing.T) {
	cases := []struct {
		name     string
		in       Value
		to       Kind
		wantKind Kind
	}{
		{"int to int (already the declared kind)", Int(5, SourceExplicit), KindInt, KindInt},
		{"float to float (already the declared kind)", Float(1.5, SourceExplicit), KindFloat, KindFloat},
		{"int to float, exact", Int(1, SourceExplicit), KindFloat, KindFloat},
		{"float to int, whole number", Float(1.0, SourceExplicit), KindInt, KindInt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Coerce(tc.in, tc.to)
			if !ok {
				t.Fatalf("Coerce(%#v, %v) failed, want success", tc.in, tc.to)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
		})
	}
}

// TestCoerceLossyConversionsFail is the refusing direction: a conversion
// that would change the value is never applied, and the value comes back
// with its ORIGINAL Kind and Raw rather than a half-converted one — a caller
// that forgot to check ok must not silently receive a wrong value.
//
// The two cases are the two directions coerceBound and decodeDefault already
// test through internal/config, now pinned once at the source: a fractional
// part rounded away converting float to int, and an int64 above 2^53 that
// does not survive round-tripping through float64.
func TestCoerceLossyConversionsFail(t *testing.T) {
	t.Run("fractional float under int", func(t *testing.T) {
		in := Float(1.5, SourceExplicit)
		got, ok := Coerce(in, KindInt)
		if ok {
			t.Fatalf("Coerce(1.5, KindInt) succeeded, want failure: %#v", got)
		}
		if got.Kind != KindFloat {
			t.Errorf("on failure, Kind must be unchanged: got %v, want KindFloat", got.Kind)
		}
		if f, ok := got.AsFloat(); !ok || f != 1.5 {
			t.Errorf("on failure, Raw must be unchanged: got %v (ok=%v), want 1.5", f, ok)
		}
	})

	// 2^53+1 is exactly representable as int64 but rounds under
	// round-half-to-even when converted to float64 and back — the same value
	// Task 3's FIX 5 used for the bound equivalent of this case, reused here
	// because it is the value that makes the conversion lossy rather than
	// merely large.
	t.Run("int above 2^53 under float", func(t *testing.T) {
		in := Int(9007199254740993, SourceExplicit)
		got, ok := Coerce(in, KindFloat)
		if ok {
			t.Fatalf("Coerce(2^53+1, KindFloat) succeeded, want failure: %#v", got)
		}
		if got.Kind != KindInt {
			t.Errorf("on failure, Kind must be unchanged: got %v, want KindInt", got.Kind)
		}
		if n, ok := got.AsInt(); !ok || n != 9007199254740993 {
			t.Errorf("on failure, Raw must be unchanged: got %v (ok=%v), want 9007199254740993", n, ok)
		}
	})
}

// TestCoerceDoesNotReportATypeMismatch pins that a non-numeric value under a
// numeric Kind fails WITHOUT Coerce attempting or judging a conversion: it
// is a type error for the CALLER's own diagnostic to name ("must be a
// float"), not a lossy-conversion failure Coerce has no vocabulary to
// describe. Coerce normalises; it does not judge.
func TestCoerceDoesNotReportATypeMismatch(t *testing.T) {
	in := String("hello", SourceExplicit)
	got, ok := Coerce(in, KindFloat)
	if ok {
		t.Fatalf("Coerce(string, KindFloat) succeeded, want failure: %#v", got)
	}
	if got.Kind != KindString {
		t.Errorf("Kind must be unchanged: got %v, want KindString", got.Kind)
	}
	if s, ok := got.AsString(); !ok || s != "hello" {
		t.Errorf("Raw must be unchanged: got %v (ok=%v), want \"hello\"", s, ok)
	}
}

// TestCoercePreservesEveryOtherField pins that Coerce changes ONLY Kind and
// Raw. A coercion that silently cleared Sensitive would put a secret in a
// plan's output with nothing marking it as one — the shape of the M3
// Critical again, this time on the way into a plan instead of out of a
// provider.
func TestCoercePreservesEveryOtherField(t *testing.T) {
	origin := Origin{File: "infra.yml", Line: 7, Column: 3}
	sentinelExpr := &Expr{Op: OpLiteral, Literal: Int(1, SourceExplicit)}

	in := Int(1, SourceDefault).WithScope(ScopeBaseConfig).WithSensitive(true).WithOrigin(origin)
	in.Expr = sentinelExpr

	got, ok := Coerce(in, KindFloat)
	if !ok {
		t.Fatalf("Coerce failed: %#v", got)
	}
	if got.Source != SourceDefault {
		t.Errorf("Source = %v, want SourceDefault", got.Source)
	}
	if got.Scope != ScopeBaseConfig {
		t.Errorf("Scope = %v, want ScopeBaseConfig", got.Scope)
	}
	if !got.Sensitive {
		t.Error("Sensitive was dropped by Coerce")
	}
	if got.Origin.File != origin.File || got.Origin.Line != origin.Line || got.Origin.Column != origin.Column {
		t.Errorf("Origin = %v, want %v", got.Origin, origin)
	}
	if got.Expr != sentinelExpr {
		t.Error("Expr was dropped by Coerce")
	}
}

// TestCoerceOnAlreadyDeclaredKindIsANoOpSuccess: a value already at the
// target Kind must succeed, not merely be left alone by a caller's own
// same-kind check — a caller that always calls Coerce, unconditionally, must
// not have to special-case "already correct" to avoid a false failure.
func TestCoerceOnAlreadyDeclaredKindIsANoOpSuccess(t *testing.T) {
	in := Float(2.5, SourceExplicit)
	got, ok := Coerce(in, KindFloat)
	if !ok {
		t.Fatal("Coerce on an already-matching Kind must succeed")
	}
	if f, ok := got.AsFloat(); !ok || f != 2.5 {
		t.Errorf("Raw changed on a no-op coercion: got %v (ok=%v), want 2.5", f, ok)
	}
}
