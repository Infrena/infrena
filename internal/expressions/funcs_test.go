package expressions

import (
	"testing"

	"infra/pkg/value"
)

func call(t *testing.T, name string, args ...value.Value) value.Value {
	t.Helper()
	fn, _, ok := Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q) not found", name)
	}
	got, err := fn(args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

func str(s string) value.Value { return value.String(s, value.SourceExplicit) }

func TestStringFunctions(t *testing.T) {
	cases := []struct {
		name string
		args []value.Value
		want string
	}{
		{"lower", []value.Value{str("PostGres")}, "postgres"},
		{"upper", []value.Value{str("postgres")}, "POSTGRES"},
		{"trim", []value.Value{str("  padded  ")}, "padded"},
		{"replace", []value.Value{str("my-sql-db"), str("sql"), str("SQL")}, "my-SQL-db"},
		{"join", []value.Value{str("-"), value.List([]value.Value{str("a"), str("b")}, value.SourceExplicit)}, "a-b"},
	}
	for _, tc := range cases {
		got, ok := call(t, tc.name, tc.args...).AsString()
		if !ok || got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDefaultFallsBackOnUnknown(t *testing.T) {
	got := call(t, "default", value.Unknown(value.KindString, value.SourceComputed), str("fallback"))
	if s, _ := got.AsString(); s != "fallback" {
		t.Errorf("default(unknown, fallback) = %q, want \"fallback\"", s)
	}
}

func TestDefaultFallsBackOnEmptyString(t *testing.T) {
	got := call(t, "default", str(""), str("fallback"))
	if s, _ := got.AsString(); s != "fallback" {
		t.Errorf("default(\"\", fallback) = %q, want \"fallback\"", s)
	}
}

func TestDefaultKeepsAPresentValue(t *testing.T) {
	got := call(t, "default", str("present"), str("fallback"))
	if s, _ := got.AsString(); s != "present" {
		t.Errorf("default(present, fallback) = %q, want \"present\"", s)
	}
}

func TestFunctionsPreserveSensitivity(t *testing.T) {
	// Transforming a secret does not declassify it.
	secret := str("hunter2").WithSensitive(true)
	if got := call(t, "upper", secret); !got.Sensitive {
		t.Error("upper() of a sensitive value must stay sensitive")
	}
	if got := call(t, "replace", secret, str("h"), str("H")); !got.Sensitive {
		t.Error("replace() of a sensitive value must stay sensitive")
	}
}

func TestWrongArityIsAnError(t *testing.T) {
	fn, _, _ := Lookup("replace")
	if _, err := fn([]value.Value{str("only-one")}); err == nil {
		t.Error("replace with one argument must error, not silently do nothing")
	}
}

func TestWrongKindIsAnError(t *testing.T) {
	fn, _, _ := Lookup("lower")
	if _, err := fn([]value.Value{value.Int(42, value.SourceExplicit)}); err == nil {
		t.Error("lower(42) must error rather than coercing")
	}
}

func TestUnknownFunctionIsNotFound(t *testing.T) {
	if _, _, ok := Lookup("md5"); ok {
		t.Error("only the six specified functions may exist; adding one is a spec change")
	}
}

func TestNamesIsSortedAndComplete(t *testing.T) {
	want := []string{"default", "join", "lower", "replace", "trim", "upper"}
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v — sorted, for stable diagnostics", got, want)
		}
	}
}
