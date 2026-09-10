package expressions

import (
	"strings"
	"testing"

	"infra/pkg/value"
)

func mustParse(t *testing.T, src string) *value.Expr {
	t.Helper()
	e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 3})
	if ds.HasErrors() {
		t.Fatalf("Parse(%q) reported errors: %+v", src, ds)
	}
	return e
}

func TestParsePlainTextIsALiteral(t *testing.T) {
	e := mustParse(t, "postgres")
	if e.Op != value.OpLiteral {
		t.Fatalf("Op = %v, want OpLiteral", e.Op)
	}
	if s, _ := e.Literal.AsString(); s != "postgres" {
		t.Errorf("Literal = %q, want \"postgres\"", s)
	}
}

func TestParseLoneInterpolationIsNotWrapped(t *testing.T) {
	// A string that is exactly one interpolation must not become a concat:
	// only the unwrapped form can carry a non-string kind to evaluation.
	e := mustParse(t, "${database.endpoint}")
	if e.Op != value.OpResourceRef {
		t.Fatalf("Op = %v, want OpResourceRef (not wrapped in OpConcat)", e.Op)
	}
	if e.Ref.Resource != "database" || e.Ref.Attribute != "endpoint" {
		t.Errorf("Ref = %+v, want database.endpoint", e.Ref)
	}
}

func TestParseMixedTextIsAConcat(t *testing.T) {
	e := mustParse(t, "${project}-db")
	if e.Op != value.OpConcat {
		t.Fatalf("Op = %v, want OpConcat", e.Op)
	}
	if len(e.Args) != 2 {
		t.Fatalf("Args = %d, want 2", len(e.Args))
	}
	if e.Args[0].Op != value.OpVarRef || e.Args[0].Ref.Resource != "project" {
		t.Errorf("first arg = %+v, want a var ref to project", e.Args[0])
	}
	if s, _ := e.Args[1].Literal.AsString(); s != "-db" {
		t.Errorf("second arg literal = %q, want \"-db\"", s)
	}
}

func TestParseBareNameIsAVarRefNotAResourceRef(t *testing.T) {
	// One segment is a variable; two or more is a resource attribute. The
	// compiler needs the distinction to know which scope to resolve against.
	e := mustParse(t, "${region}")
	if e.Op != value.OpVarRef {
		t.Errorf("Op = %v, want OpVarRef for a single-segment name", e.Op)
	}
}

func TestParseCall(t *testing.T) {
	e := mustParse(t, "${lower(database.engine)}")
	if e.Op != value.OpCall || e.Function != "lower" {
		t.Fatalf("Op = %v Function = %q, want OpCall lower", e.Op, e.Function)
	}
	if len(e.Args) != 1 || e.Args[0].Ref.Resource != "database" {
		t.Errorf("Args = %+v, want one ref to database.engine", e.Args)
	}
}

func TestParseCallWithMultipleArguments(t *testing.T) {
	e := mustParse(t, `${replace(database.engine, "sql", "SQL")}`)
	if len(e.Args) != 3 {
		t.Fatalf("Args = %d, want 3", len(e.Args))
	}
	if s, _ := e.Args[1].Literal.AsString(); s != "sql" {
		t.Errorf("second arg = %q, want the quoted literal \"sql\"", s)
	}
}

func TestParseNestedCall(t *testing.T) {
	e := mustParse(t, "${upper(lower(database.engine))}")
	if e.Function != "upper" || len(e.Args) != 1 {
		t.Fatalf("outer = %+v", e)
	}
	if e.Args[0].Op != value.OpCall || e.Args[0].Function != "lower" {
		t.Errorf("inner = %+v, want a nested lower() call", e.Args[0])
	}
}

func TestParseCollectsReferencesAcrossAConcat(t *testing.T) {
	e := mustParse(t, "${network.id}/${database.endpoint}")
	refs := e.References()
	if len(refs) != 2 {
		t.Fatalf("References() = %v, want 2", refs)
	}
}

func TestParseReportsUnclosedInterpolation(t *testing.T) {
	_, ds := Parse("${database.endpoint", value.Origin{File: "infra.yml", Line: 3})
	if !ds.HasErrors() {
		t.Fatal("an unclosed ${ must be an error, not silently treated as literal text")
	}
	if ds[0].Origin.Line != 3 {
		t.Errorf("diagnostic lost its origin: %+v", ds[0].Origin)
	}
}

func TestParseReportsEmptyInterpolation(t *testing.T) {
	if _, ds := Parse("${}", value.Origin{}); !ds.HasErrors() {
		t.Error("${} must be an error")
	}
}

func TestParseReportsUnclosedCall(t *testing.T) {
	if _, ds := Parse("${lower(database.engine}", value.Origin{}); !ds.HasErrors() {
		t.Error("an unclosed call must be an error")
	}
}

func TestParseReportsTrailingDot(t *testing.T) {
	if _, ds := Parse("${database.}", value.Origin{}); !ds.HasErrors() {
		t.Error("a trailing dot names no attribute and must be an error")
	}
}

func TestParseCollectsEveryError(t *testing.T) {
	_, ds := Parse("${} and ${database.}", value.Origin{})
	if len(ds) < 2 {
		t.Errorf("got %d diagnostics, want at least 2 — parsing reports every problem in one pass", len(ds))
	}
}

func TestParseDiagnosticsNameTheOffendingSource(t *testing.T) {
	_, ds := Parse("${lower(}", value.Origin{File: "infra.yml", Line: 7})
	if len(ds) == 0 {
		t.Fatal("expected a diagnostic")
	}
	var rendered strings.Builder
	ds.Render(&rendered)
	if !strings.Contains(rendered.String(), "infra.yml:7") {
		t.Errorf("diagnostic does not locate the error:\n%s", rendered.String())
	}
}

func TestParseEscapedDollarIsLiteral(t *testing.T) {
	// $${not_an_expression} is how a user writes a literal dollar-brace.
	e := mustParse(t, "$${literal}")
	if e.Op != value.OpLiteral {
		t.Fatalf("Op = %v, want OpLiteral", e.Op)
	}
	if s, _ := e.Literal.AsString(); s != "${literal}" {
		t.Errorf("Literal = %q, want \"${literal}\"", s)
	}
}

func TestParseBraceInQuotedLiteral(t *testing.T) {
	// A } inside a quoted literal must not close the interpolation.
	e := mustParse(t, `${replace("test}more", "x")}`)
	if e.Op != value.OpCall {
		t.Fatalf("Op = %v, want OpCall", e.Op)
	}
	if len(e.Args) != 2 {
		t.Fatalf("Args = %d, want 2", len(e.Args))
	}
	if s, _ := e.Args[0].Literal.AsString(); s != "test}more" {
		t.Errorf("first arg = %q, want \"test}more\" with brace intact", s)
	}
}

func TestParseEscapedQuoteInLiteral(t *testing.T) {
	// An escaped quote in a literal must not toggle the quote state.
	e := mustParse(t, `${replace("test\"more", "x")}`)
	if e.Op != value.OpCall {
		t.Fatalf("Op = %v, want OpCall", e.Op)
	}
	if len(e.Args) != 2 {
		t.Fatalf("Args = %d, want 2", len(e.Args))
	}
	if s, _ := e.Args[0].Literal.AsString(); s != `test"more` {
		t.Errorf("first arg = %q, want \"test\"more\" with escaped quote unescaped", s)
	}
}
