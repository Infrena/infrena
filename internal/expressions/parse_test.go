package expressions

import (
	"reflect"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
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
	if e.Ref.Target.Name != "database" || e.Ref.Attribute != "endpoint" {
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
	if e.Args[0].Op != value.OpVarRef || e.Args[0].Ref.Target.Name != "project" {
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
	if len(e.Args) != 1 || e.Args[0].Ref.Target.Name != "database" {
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

// TestReferenceIntoAModuleIsRefused is contract Amendment 14a.
//
// ${module.prod.database.id} parses to the target name "module.prod.database",
// and address.Address{Name: "module.prod.database"}.String() returns that
// verbatim — byte for byte what Address{Module: ["prod"], Name: "database"}
// renders for the REAL database inside instance prod. Stage 6 keys its target
// map by canonical address, so without this guard the two collide and a user
// can reach inside a module. A module exposes outputs, not resources.
//
// The nested case is here because a two-level address has `module` at position
// 0 AND 2, so a guard that checked only the first segment would pass the first
// case and leak the second.
func TestReferenceIntoAModuleIsRefused(t *testing.T) {
	for _, src := range []string{
		"${module.prod.database.id}",
		"${module.prod.module.net.vpc.id}",
		"${prod.module.net.vpc.id}",
	} {
		t.Run(src, func(t *testing.T) {
			e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 3})
			if !ds.HasErrors() {
				t.Fatalf("Parse(%q) produced no error; a module's internals are addressable from outside it", src)
			}
			d := ds[0]
			text := d.Summary + " | " + d.Detail + " | " + d.Action
			if !strings.Contains(text, "names a module's internals") {
				t.Errorf("wrong diagnostic: %s", text)
			}
			if !strings.Contains(d.Action, "outputs") {
				t.Errorf("action does not name the alternative: %s", d.Action)
			}
			if e != nil && e.Op == value.OpResourceRef && len(e.Ref.Target.Module) == 0 {
				t.Errorf("a rejected reference still produced a resource target %q", e.Ref.Target.String())
			}
		})
	}
}

// TestReferenceNamingTheInstanceIsAccepted is the half that stops the guard
// being satisfied by rejecting everything.
//
// ${prod.endpoint} is how a caller reads a module instance's output, and it is
// the spelling PLAN.md §11.2 shows. A guard that refused it would make modules
// unusable while passing every assertion in the test above.
func TestReferenceNamingTheInstanceIsAccepted(t *testing.T) {
	for _, src := range []string{"${prod.endpoint}", "${database.connection_string}"} {
		t.Run(src, func(t *testing.T) {
			e, ds := Parse(src, value.Origin{File: "infra.yml", Line: 3})
			if ds.HasErrors() {
				t.Fatalf("Parse(%q) errored: %v", src, ds)
			}
			if e.Op != value.OpResourceRef {
				t.Fatalf("Op = %v, want OpResourceRef", e.Op)
			}
			if len(e.Ref.Target.Module) != 0 {
				t.Errorf("a parsed reference carries a module path %v; Qualify fills it, not the parser",
					e.Ref.Target.Module)
			}
		})
	}
}

// TestResourceNamedModuleIsRefusedWithItsOwnReason.
//
// ${module.id} is not someone reaching into a module — it is a resource
// literally named `module`, which address.Parse already cannot round-trip
// ("module" alone is "must be followed by a module name"). It gets its own
// action, because "reference one of the module's outputs instead" would be
// nonsense advice for it.
func TestResourceNamedModuleIsRefusedWithItsOwnReason(t *testing.T) {
	_, ds := Parse("${module.id}", value.Origin{File: "infra.yml", Line: 3})
	if !ds.HasErrors() {
		t.Fatal("a resource named `module` parsed cleanly; its address cannot round-trip")
	}
	if !strings.Contains(ds[0].Action, "Rename") {
		t.Errorf("action does not tell the user to rename the resource: %s", ds[0].Action)
	}
}

// TestQualifiedReferencesDoNotRouteThroughTheParser pins WHY the guard can live
// at parse time.
//
// Qualify sets Target.Module structurally on an already-parsed expression; it
// never re-parses a rendered address. If it did, every qualified reference would
// hit the guard above and modules would not work at all. This test states the
// property the guard depends on in the one place a reader will look for it.
func TestQualifiedReferencesDoNotRouteThroughTheParser(t *testing.T) {
	e, ds := Parse("${db.id}", value.Origin{File: "module.yml", Line: 2})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	qualified := e.Ref.InModule("net")
	if qualified.String() != "module.net.db.id" {
		t.Fatalf("InModule gave %q", qualified.String())
	}
	// The rendered form is exactly what the guard refuses as INPUT. That is the
	// point: it is produced, never typed.
	if _, ds := Parse("${"+qualified.String()+"}", value.Origin{File: "infra.yml", Line: 1}); !ds.HasErrors() {
		t.Error("the guard does not refuse a rendered qualified address; it must, or a user can type one")
	}
}

func TestVarPrefixParsesAsAVariable(t *testing.T) {
	e, ds := Parse("${var.region}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpVarRef {
		t.Fatalf("Op = %v, want OpVarRef", e.Op)
	}
	if got := e.Ref.VarName(); got != "region" {
		t.Errorf("VarName() = %q, want %q — the prefix is stripped at parse time", got, "region")
	}
}

func TestABareNameStillParsesAsAVariableDuringExpand(t *testing.T) {
	// Removed in the contract task. Here it pins that the expand step is
	// additive: nothing that worked stops working.
	e, ds := Parse("${region}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpVarRef || e.Ref.VarName() != "region" {
		t.Errorf("got %v/%q, want OpVarRef/region", e.Op, e.Ref.VarName())
	}
}

func TestAVariableReferenceRendersItsPrefix(t *testing.T) {
	e, _ := Parse("${var.region}", value.Origin{})
	if got := e.String(); got != "${var.region}" {
		t.Errorf("String() = %q, want %q — a diagnostic must echo what the user wrote", got, "${var.region}")
	}
}

func TestAVariablePathParsesToSteps(t *testing.T) {
	for _, tc := range []struct {
		src  string
		name string
		want []value.Step
	}{
		{"${var.tags.team}", "tags", []value.Step{{Kind: value.StepKey, Key: "team"}}},
		{"${var.azs[0]}", "azs", []value.Step{{Kind: value.StepIndex, Index: 0}}},
		{"${var.subnets[1].cidr}", "subnets", []value.Step{
			{Kind: value.StepIndex, Index: 1}, {Kind: value.StepKey, Key: "cidr"}}},
		{"${var.regions.us_east.azs[2]}", "regions", []value.Step{
			{Kind: value.StepKey, Key: "us_east"}, {Kind: value.StepKey, Key: "azs"},
			{Kind: value.StepIndex, Index: 2}}},
	} {
		e, ds := Parse(tc.src, value.Origin{})
		if ds.HasErrors() {
			t.Errorf("%s: unexpected errors: %v", tc.src, ds)
			continue
		}
		if e.Ref.VarName() != tc.name {
			t.Errorf("%s: VarName() = %q, want %q", tc.src, e.Ref.VarName(), tc.name)
		}
		if !reflect.DeepEqual(e.Ref.Path, tc.want) {
			t.Errorf("%s: Path = %+v, want %+v", tc.src, e.Ref.Path, tc.want)
		}
	}
}

func TestAnIndexMustBeALiteralInteger(t *testing.T) {
	_, ds := Parse("${var.azs[i]}", value.Origin{})
	if !ds.HasErrors() {
		t.Fatal("${var.azs[i]} must be refused: a varying index needs iteration, which this language does not have")
	}
	if got := ds[0].Summary; !strings.Contains(got, "literal") {
		t.Errorf("Summary = %q, want it to say the index must be a literal", got)
	}
}

func TestANegativeIndexIsRefused(t *testing.T) {
	_, ds := Parse("${var.azs[-1]}", value.Origin{})
	if !ds.HasErrors() {
		t.Fatal("${var.azs[-1]} must be refused: meaning would depend on a length the reader cannot see")
	}
}

func TestAPathRoundTripsThroughString(t *testing.T) {
	for _, src := range []string{"${var.tags.team}", "${var.azs[0]}", "${var.subnets[1].cidr}"} {
		e, ds := Parse(src, value.Origin{})
		if ds.HasErrors() {
			t.Fatalf("%s: %v", src, ds)
		}
		if got := e.String(); got != src {
			t.Errorf("String() = %q, want %q", got, src)
		}
	}
}

func TestAResourceReferenceTakesTheFirstSegmentAsItsTarget(t *testing.T) {
	// A resource name cannot contain a dot (config.checkResourceName), so the
	// target is always exactly one segment. The old rule took every segment
	// but the last, which could only ever build a target nothing is allowed
	// to declare.
	e, ds := Parse("${vpc.tags.Name}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Op != value.OpResourceRef {
		t.Fatalf("Op = %v, want OpResourceRef", e.Op)
	}
	if e.Ref.Target.Name != "vpc" {
		t.Errorf("Target.Name = %q, want %q", e.Ref.Target.Name, "vpc")
	}
	if e.Ref.Attribute != "tags" {
		t.Errorf("Attribute = %q, want %q", e.Ref.Attribute, "tags")
	}
	if len(e.Ref.Path) != 1 || e.Ref.Path[0].Key != "Name" {
		t.Errorf("Path = %+v, want one key step Name", e.Ref.Path)
	}
}

func TestATwoSegmentResourceReferenceIsUnchanged(t *testing.T) {
	e, ds := Parse("${vpc.id}", value.Origin{})
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", ds)
	}
	if e.Ref.Target.Name != "vpc" || e.Ref.Attribute != "id" || len(e.Ref.Path) != 0 {
		t.Errorf("got %q/%q/%+v, want vpc/id/no path", e.Ref.Target.Name, e.Ref.Attribute, e.Ref.Path)
	}
}
