package expressions

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/pkg/value"
)

// PLAN.md §10.3 — a map or list literal may appear as a function ARGUMENT and
// nowhere else.

func parseArgs(t *testing.T, src string) (*value.Expr, string) {
	t.Helper()
	e, ds := Parse(src, value.Origin{File: "f.yml", Line: 1, Column: 1})
	var sb strings.Builder
	ds.Render(&sb)
	return e, sb.String()
}

// TestAMapLiteralIsOneArgument. The comma INSIDE the braces is the whole point:
// before the scanners were unified this split into three arguments.
func TestAMapLiteralIsOneArgument(t *testing.T) {
	e, out := parseArgs(t, `${merge(tags, {team: payments, project: billing})}`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if e == nil || e.Op != value.OpCall || len(e.Args) != 2 {
		t.Fatalf("want a call with 2 arguments, got %#v", e)
	}
	lit := e.Args[1]
	if lit.Op != value.OpLiteral || lit.Literal.Kind != value.KindMap {
		t.Fatalf("the second argument is not a map literal: %#v", lit)
	}
	m, _ := lit.Literal.Raw.(map[string]value.Value)
	if len(m) != 2 {
		t.Fatalf("the literal holds %d entries, want 2: %#v", len(m), m)
	}
	// A bare word is a STRING, not a variable. Anyone writing
	// `{team: payments}` means the string, which is what the YAML around it
	// would mean; resolving it as a reference would make the obvious spelling
	// silently mean something else.
	if got, _ := m["team"].AsString(); got != "payments" {
		t.Errorf("team = %v, want the literal string", m["team"])
	}
	if m["team"].Kind != value.KindString {
		t.Errorf("team is %v, want a string", m["team"].Kind)
	}
}

// TestAListLiteralIsOneArgument, and a NESTED one, because the depth counter is
// the mechanism and one level cannot prove a counter.
func TestAListLiteralIsOneArgument(t *testing.T) {
	e, out := parseArgs(t, `${join([a, b, c], "-")}`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	if len(e.Args) != 2 {
		t.Fatalf("want 2 arguments, got %d", len(e.Args))
	}
	items, _ := e.Args[0].Literal.Raw.([]value.Value)
	if len(items) != 3 {
		t.Fatalf("the list holds %d items, want 3", len(items))
	}

	// Nested: a map inside a call inside a call, with commas at two depths.
	e, out = parseArgs(t, `${merge(default(tags, {a: 1, b: 2}), {c: 3})}`)
	if out != "" {
		t.Fatalf("nested literals: %s", out)
	}
	if len(e.Args) != 2 {
		t.Errorf("the outer call split into %d arguments, want 2 — a comma inside a nested "+
			"literal is not an argument separator", len(e.Args))
	}
}

// TestLiteralScalarsTakeTheShapeYamlWouldGiveThem, within the small set this
// reads. Anything unreadable stays text, which is the answer that cannot
// silently change a value's meaning.
func TestLiteralScalarsTakeTheShapeYamlWouldGiveThem(t *testing.T) {
	e, out := parseArgs(t, `${merge(tags, {n: 3, f: 1.5, yes: true, no: false, s: plain, q: "quoted"})}`)
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}
	m, _ := e.Args[1].Literal.Raw.(map[string]value.Value)
	for _, tc := range []struct {
		key  string
		kind value.Kind
	}{
		{"n", value.KindInt}, {"f", value.KindFloat},
		{"yes", value.KindBool}, {"no", value.KindBool},
		{"s", value.KindString}, {"q", value.KindString},
	} {
		if m[tc.key].Kind != tc.kind {
			t.Errorf("%s is %v, want %v", tc.key, m[tc.key].Kind, tc.kind)
		}
	}
	if got, _ := m["q"].AsString(); got != "quoted" {
		t.Errorf("a quoted scalar kept its quotes: %q", got)
	}
}

// TestALiteralOutsideAnArgumentIsRefused is the BOUND, and the reason this task
// is not "the language gains literals".
func TestALiteralOutsideAnArgumentIsRefused(t *testing.T) {
	for _, src := range []string{`${{a: b}}`, `${[a, b]}`} {
		_, out := parseArgs(t, src)
		if out == "" {
			t.Errorf("%s must be refused: YAML already expresses a map or list everywhere "+
				"except a function argument", src)
			continue
		}
		if !strings.Contains(out, "function argument") {
			t.Errorf("%s: the diagnostic does not say where a literal may appear:\n%s", src, out)
		}
	}
}

// TestAnUnclosedLiteralSaysWhatIsMissing.
func TestAnUnclosedLiteralSaysWhatIsMissing(t *testing.T) {
	// The call's own closer is present, so the missing brace is the literal's.
	_, out := parseArgs(t, `${merge(tags, {a: b)}`)
	if out == "" {
		t.Fatal("an unclosed map literal must be reported")
	}
}

// TestAMapLiteralEntryWithoutAColonIsReported — `{a b}` is a mistake, not an
// entry keyed by the empty string.
func TestAMapLiteralEntryWithoutAColonIsReported(t *testing.T) {
	_, out := parseArgs(t, `${merge(tags, {a b})}`)
	if out == "" {
		t.Fatal("a map literal entry with no `:` must be reported")
	}
	if !strings.Contains(out, ":") {
		t.Errorf("the diagnostic does not mention the missing separator:\n%s", out)
	}
}
