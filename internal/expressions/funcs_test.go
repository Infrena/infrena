package expressions

import (
	"testing"

	"github.com/infrata/infrata/pkg/value"
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

func TestSensitivityUnionsEveryArgumentPosition(t *testing.T) {
	// Both leaks this project shipped were an argument position omitted from a
	// hand-written union: join's separator, then replace's search string. A
	// secret search term reveals its own position through an unclassified
	// result, which is a side channel on the secret's content.
	secret := str("hunter2").WithSensitive(true)
	plain := str("plain")

	cases := []struct {
		name string
		fn   string
		args []value.Value
	}{
		{"replace: sensitive subject", "replace", []value.Value{secret, plain, plain}},
		{"replace: sensitive search term", "replace", []value.Value{plain, secret, plain}},
		{"replace: sensitive replacement", "replace", []value.Value{plain, plain, secret}},
		{"join: sensitive separator", "join", []value.Value{secret, value.List([]value.Value{plain}, value.SourceExplicit)}},
		{"join: sensitive element", "join", []value.Value{plain, value.List([]value.Value{plain, secret}, value.SourceExplicit)}},
		{"lower: sensitive subject", "lower", []value.Value{secret}},
		{"upper: sensitive subject", "upper", []value.Value{secret}},
		{"trim: sensitive subject", "trim", []value.Value{secret}},
	}

	for _, tc := range cases {
		if got := call(t, tc.fn, tc.args...); !got.Sensitive {
			t.Errorf("%s: result is not sensitive — a secret in any argument position classifies the result", tc.name)
		}
	}
}

func TestMalformedCompositeIsTreatedAsSensitive(t *testing.T) {
	// A value whose Kind claims list but whose Raw is not one cannot be
	// inspected. A security check that cannot verify safety must deny, not
	// assume: over-redacting is recoverable, leaking is not.
	malformed := value.Value{Kind: value.KindList, Known: true, Raw: "not a list", Source: value.SourceExplicit}
	if !sensitiveAnywhere(malformed) {
		t.Error("a malformed composite must be treated as sensitive — the check could not inspect it")
	}

	malformedMap := value.Value{Kind: value.KindMap, Known: true, Raw: 42, Source: value.SourceExplicit}
	if !sensitiveAnywhere(malformedMap) {
		t.Error("a malformed map must be treated as sensitive")
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

// TestFunctionsDoNotClassifyValuesThatAreNotSensitive is the deliberate
// inverse of TestSensitivityUnionsEveryArgumentPosition above, and it exists
// because that test — and every other redaction test in this project —
// asserts only that secrets are NOT shown. Nothing asserted that non-secrets
// ARE, so sensitivity was pinned in one direction only.
//
// Measured before this test existed: forcing sensitiveAnywhere to return true
// left the ENTIRE suite green, while the real binary rendered
// `cidr: ${upper("10.0.0.0/16")}` as `cidr: <sensitive>` instead of
// `cidr: "10.0.0.0/16"`. Over-classification is not a security bug — it is the
// fail-safe direction — which is exactly why every instinct guarding this area
// points away from it. Its cost is different in kind: every transformed value
// in every plan prints <sensitive>, plans stop being readable, and the
// product's headline feature quietly dies with a green suite.
//
// The two tests together are what discriminate: this one fails if
// classification becomes too broad, its sibling fails if it becomes too
// narrow. Either alone can be satisfied by a constant.
func TestFunctionsDoNotClassifyValuesThatAreNotSensitive(t *testing.T) {
	plain := str("plain")
	list := value.List([]value.Value{plain, str("other")}, value.SourceExplicit)

	cases := []struct {
		name string
		fn   string
		args []value.Value
	}{
		{"replace: nothing sensitive", "replace", []value.Value{plain, plain, plain}},
		{"join: nothing sensitive", "join", []value.Value{plain, list}},
		{"lower: nothing sensitive", "lower", []value.Value{plain}},
		{"upper: nothing sensitive", "upper", []value.Value{plain}},
		{"trim: nothing sensitive", "trim", []value.Value{plain}},
	}

	for _, tc := range cases {
		if got := call(t, tc.fn, tc.args...); got.Sensitive {
			t.Errorf("%s: result was classified sensitive, but no argument was — "+
				"over-classification renders every transformed value as <sensitive> "+
				"and makes plans unreadable", tc.name)
		}
	}
}

// TestSensitivityIsPerLeafNotWholeCollection pins the boundary the two tests
// above straddle: a list containing one sensitive element classifies, while a
// list containing none does not. A single-element check cannot tell those
// apart, and a constant satisfies either one alone.
func TestSensitivityIsPerLeafNotWholeCollection(t *testing.T) {
	plain := str("plain")
	secret := str("hunter2").WithSensitive(true)

	clean := value.List([]value.Value{plain, str("other")}, value.SourceExplicit)
	if got := call(t, "join", str("-"), clean); got.Sensitive {
		t.Error("a list with no sensitive element must not classify the joined result")
	}

	tainted := value.List([]value.Value{plain, secret}, value.SourceExplicit)
	if got := call(t, "join", str("-"), tainted); !got.Sensitive {
		t.Error("a list with one sensitive element must classify the joined result")
	}
}
