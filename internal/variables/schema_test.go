package variables

import (
	"strings"
	"testing"

	"github.com/infrata/infrata/internal/config"
	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/pkg/value"
)

func decl(name string, kind value.Kind) config.VariableDecl {
	return config.VariableDecl{
		Name:   name,
		Type:   kind,
		Origin: value.Origin{File: "variables.yml", Line: 3, Column: 5},
	}
}

func withDefault(d config.VariableDecl, v value.Value) config.VariableDecl {
	d.Default, d.HasDefault = v, true
	return d
}

func TestSchemasCarriesEveryKindThrough(t *testing.T) {
	kinds := []value.Kind{
		value.KindString, value.KindInt, value.KindFloat,
		value.KindBool, value.KindList, value.KindMap,
	}
	var decls []config.VariableDecl
	for _, k := range kinds {
		decls = append(decls, decl("v_"+k.String(), k))
	}

	got, ds := Schemas(decls)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	for _, k := range kinds {
		s, ok := got["v_"+k.String()]
		if !ok {
			t.Fatalf("schema for kind %s is missing", k)
		}
		if s.Kind != k {
			t.Errorf("kind %s came through as %s", k, s.Kind)
		}
	}
}

func TestSchemasKeepsAnUntypedDeclarationThatHasADefault(t *testing.T) {
	// PLAN.md §9 makes schemas optional. A declaration with a default and no
	// type supplies a value and constrains nothing.
	d := decl("greeting", value.KindInvalid)
	d.Default, d.HasDefault = value.String("hello", value.SourceExplicit), true

	got, ds := Schemas([]config.VariableDecl{d})
	if ds.HasErrors() {
		t.Fatalf("an untyped declaration with a default is legal: %+v", ds)
	}
	s, ok := got["greeting"]
	if !ok {
		t.Fatal("greeting must stay in the table: it supplies a default")
	}
	if s.Kind != value.KindInvalid || !s.HasDefault {
		t.Errorf("schema = %+v, want an untyped schema carrying a default", s)
	}
}

func TestSchemasKeepsEveryDeclarationItIsGiven(t *testing.T) {
	// Nothing is dropped here. A declaration with neither a type nor a default
	// is stage 2's error, reported at the line the user wrote it; if one ever
	// reaches this table it must still come out the other side, because
	// discarding a declaration discards input the user supplied and the user
	// has no way to see that it happened.
	decls := []config.VariableDecl{
		decl("typed", value.KindInt),
		withDefault(decl("untyped", value.KindInvalid), value.String("x", value.SourceExplicit)),
	}
	got, ds := Schemas(decls)
	if ds.HasErrors() {
		t.Fatalf("both declarations are legal: %+v", ds)
	}
	if len(got) != len(decls) {
		t.Fatalf("Schemas returned %d schemas for %d declarations: %v", len(got), len(decls), got)
	}
}

func TestSchemasStoresTheDeclaredDefaultUnstamped(t *testing.T) {
	// Stage 4 decides what provenance a WINNING value carries, and it is the
	// only place that decides. A declaration is not a resolution.
	d := decl("replicas", value.KindInt)
	d.Default, d.HasDefault = value.Int(2, value.SourceExplicit), true

	got, _ := Schemas([]config.VariableDecl{d})
	if s := got["replicas"]; s.Default.Scope != value.ScopeUnset {
		t.Errorf("Default.Scope = %v, want ScopeUnset: only stage 4 may say which rung won", s.Default.Scope)
	}
}

func TestSchemasReportsEveryBadDeclarationNotJustTheFirst(t *testing.T) {
	// spec §7.4, and Schemas' own doc comment: "a declaration with a bad
	// bound keeps its type and loses the bound rather than stopping the
	// walk, so a second bad declaration is still reported." That claim was
	// unpinned — every other bad-declaration test in this file calls Schemas
	// with exactly one bad declaration, so nothing distinguishes "collect
	// every problem" from "stop at the first". Two declarations here, in two
	// DIFFERENT failure modes, so the test cannot pass by reporting the same
	// problem twice: one default has the wrong kind, the other is out of its
	// own bounds.
	wrongKind := decl("wrongkind", value.KindInt)
	wrongKind.Default, wrongKind.HasDefault = value.String("nope", value.SourceExplicit), true

	outOfBounds := withDefault(
		numDecl("toolarge", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
		value.Int(500, value.SourceExplicit))

	_, ds := Schemas([]config.VariableDecl{wrongKind, outOfBounds})
	if !ds.HasErrors() {
		t.Fatal("both declarations are individually bad and must both be reported")
	}
	var sb strings.Builder
	ds.Render(&sb)
	out := sb.String()
	for _, want := range []string{`"wrongkind"`, `"toolarge"`} {
		if !strings.Contains(out, want) {
			t.Errorf("both bad declarations must be reported, not just the first; %q is missing:\n%s", want, out)
		}
	}
}

func numDecl(name string, kind value.Kind, min, max value.Value, hasMin, hasMax bool) config.VariableDecl {
	d := decl(name, kind)
	d.Min, d.HasMin = min, hasMin
	d.Max, d.HasMax = max, hasMax
	return d
}

func TestSchemasCarriesBoundsThrough(t *testing.T) {
	decls := []config.VariableDecl{
		numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
		numDecl("ratio", value.KindFloat, value.Float(0.5, value.SourceExplicit), value.Float(1.5, value.SourceExplicit), true, true),
	}
	got, ds := Schemas(decls)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if s := got["replicas"]; !s.HasMin || !s.HasMax {
		t.Fatalf("integer bounds were dropped: %+v", s)
	}
	if n, _ := got["replicas"].Min.AsInt(); n != 1 {
		t.Errorf("min = %d, want 1", n)
	}
	if f, _ := got["ratio"].Max.AsFloat(); f != 1.5 {
		t.Errorf("max = %v, want 1.5", f)
	}
}

func TestSchemasCarriesBoundsWithoutFlatteningThem(t *testing.T) {
	// The reason bounds are value.Value and not float64. 2^53+1 is the
	// smallest integer float64 cannot represent; through a float64 it becomes
	// 2^53, and a value of exactly 2^53+1 would then validate as "at the
	// minimum" when it is in fact below it. This test fails against the
	// rejected DESIGN, not merely against a broken implementation.
	const tooBigForFloat64 = int64(1)<<53 + 1
	d := numDecl("big", value.KindInt, value.Int(tooBigForFloat64, value.SourceExplicit), value.Value{}, true, false)

	got, ds := Schemas([]config.VariableDecl{d})
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if n, _ := got["big"].Min.AsInt(); n != tooBigForFloat64 {
		t.Errorf("min = %d, want %d — the bound must survive exactly", n, tooBigForFloat64)
	}
}

func TestSchemasKeepsTheBoundsOrigin(t *testing.T) {
	// PLAN.md §44: a range error must be able to say WHERE the bound was
	// declared. Stage 2 put an Origin on the bound; losing it here would make
	// that impossible without any test failing elsewhere.
	d := numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Value{}, true, false)
	d.Min = d.Min.WithOrigin(value.Origin{File: "variables.yml", Line: 5, Column: 7})

	got, _ := Schemas([]config.VariableDecl{d})
	if got["replicas"].Min.Origin.Line != 5 {
		t.Errorf("Min.Origin = %+v, want variables.yml:5:7", got["replicas"].Min.Origin)
	}
}

func TestSchemasChecksTheDeclaredDefaultAgainstItsOwnConstraints(t *testing.T) {
	// The one declaration-level check stage 2 does not perform: a `default`
	// outside its own `min`/`max` is a declaration nothing can satisfy, and
	// reporting it once here beats reporting it every time the default wins.
	build := func(def int64) diag.Diagnostics {
		d := withDefault(
			numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
			value.Int(def, value.SourceExplicit))
		_, ds := Schemas([]config.VariableDecl{d})
		return ds
	}
	if !build(500).HasErrors() {
		t.Error("`default: 500` with `max: 100` can never be satisfied and must be rejected")
	}
	if build(2).HasErrors() {
		t.Error("a default inside its own bounds must be accepted")
	}
}

func TestSchemasDefaultDiagnosticNamesTheLineTheDefaultIsOn(t *testing.T) {
	// The check runs in stage 4, and PLAN.md §44 still gets its location: the
	// Origin travels with the datum because Default is a value.Value. This
	// test is what stops someone concluding the origin is lost and moving the
	// check into stage 2 to recover it.
	d := withDefault(
		numDecl("replicas", value.KindInt, value.Int(1, value.SourceExplicit), value.Int(100, value.SourceExplicit), true, true),
		value.Int(500, value.SourceExplicit).WithOrigin(value.Origin{File: "variables.yml", Line: 9, Column: 11}))

	_, ds := Schemas([]config.VariableDecl{d})
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "variables.yml:9:11") {
		t.Errorf("the diagnostic must point at the default, not just name the variable:\n%s", sb.String())
	}
}

func intSchema(t *testing.T, min, max int64) Schema {
	t.Helper()
	d := numDecl("replicas", value.KindInt, value.Int(min, value.SourceExplicit), value.Int(max, value.SourceExplicit), true, true)
	// TestValidatePointsAtWhereTheBoundWasDeclared checks with a value of 500,
	// which is above max (not below min) for every schema this fixture
	// builds — so the origin that must survive to the diagnostic is Max's, not
	// Min's. The brief's draft put WithOrigin on d.Min, which the offending
	// value never violates; deviated here so the test actually exercises
	// "the diagnostic locates the VIOLATED bound's own declared origin" per
	// PLAN.md §44, rather than passing by coincidence.
	d.Max = d.Max.WithOrigin(value.Origin{File: "variables.yml", Line: 5, Column: 7})
	got, ds := Schemas([]config.VariableDecl{d})
	if ds.HasErrors() {
		t.Fatalf("fixture schema is itself invalid: %+v", ds)
	}
	return got["replicas"]
}

func TestValidateRejectsTheWrongKind(t *testing.T) {
	s := intSchema(t, 1, 100)
	v := value.String("many", value.SourceVariable).WithScope(value.ScopeCLIOverride)

	ds := s.Validate(v)
	if !ds.HasErrors() {
		t.Fatal("a string supplied for an integer variable must be rejected")
	}
	var sb strings.Builder
	ds.Render(&sb)
	out := sb.String()
	for _, want := range []string{"replicas", "integer", "string", "--var"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic must name the variable, what was expected, what was found, and WHICH SCOPE supplied it; %q is missing:\n%s", want, out)
		}
	}
}

// TestValidateKindMismatchNamesTheVarFileNotDashDashVar pins Validate's
// kind-mismatch diagnostic to value.ScopeLabel — the second of MAJOR 1's
// three sites, and the one the M4 re-review found still unpinned:
// TestValidateRejectsTheWrongKind above only ever supplies a value with no
// SuppliedBy set, so it can only observe the "--var" FALLBACK and cannot
// tell "ScopeLabel, falling back correctly" apart from "v.Scope.String()
// hardcoded" — reverting this site's ScopeLabel call broke nothing in the
// suite before this test existed. This one supplies SuppliedBy exactly as
// config.DecodeVariableFile stamps a --var-file entry, so only a real
// ScopeLabel call can produce the right text.
func TestValidateKindMismatchNamesTheVarFileNotDashDashVar(t *testing.T) {
	s := intSchema(t, 1, 100)
	v := value.String("nope", value.SourceVariable).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("conf/vars.yml")

	got := renderOne(t, s.Validate(v))
	// "supplied by conf/vars.yml is a string." is unique to this template:
	// Coerce's and boundDiag's Details both continue past the value with
	// more text ("which cannot be converted...", ". The bound is..."), so
	// this exact fragment cannot be satisfied by either of the other two
	// sites firing instead — see schema_test.go's TestCoerceDiagnosticNamesTheVarFileNotDashDashVar.
	if !strings.Contains(got, "supplied by conf/vars.yml is a string.") {
		t.Errorf("Validate's kind-mismatch diagnostic must name the --var-file path that actually supplied the value:\n%s", got)
	}
	if strings.Contains(got, "supplied by --var ") {
		t.Errorf("must not blame --var for a --var-file value:\n%s", got)
	}
}

func TestValidateAcceptsTheRightKind(t *testing.T) {
	if ds := intSchema(t, 1, 100).Validate(value.Int(4, value.SourceVariable)); ds.HasErrors() {
		t.Fatalf("a valid integer must produce no diagnostics: %+v", ds)
	}
}

func TestValidateUsesTheGrammaticallyCorrectArticle(t *testing.T) {
	// article() is a plain decision (KindInt -> "an", everything else -> "a")
	// with no test in the brief pinning either branch: a version that always
	// returned "a" passed the whole suite, since every other assertion here
	// only checks that the kind NAME is present, not the word before it.
	render := func(ds diag.Diagnostics) string {
		var sb strings.Builder
		ds.Render(&sb)
		return sb.String()
	}

	got, ds := Schemas([]config.VariableDecl{decl("v", value.KindInt)})
	if ds.HasErrors() {
		t.Fatalf("fixture: %+v", ds)
	}
	out := render(got["v"].Validate(value.String("x", value.SourceVariable)))
	if !strings.Contains(out, "an integer") {
		t.Errorf("want %q in the diagnostic for a KindInt mismatch, got:\n%s", "an integer", out)
	}
	if strings.Contains(out, "a integer") {
		t.Errorf("diagnostic uses the wrong article for KindInt:\n%s", out)
	}

	got, ds = Schemas([]config.VariableDecl{decl("v", value.KindString)})
	if ds.HasErrors() {
		t.Fatalf("fixture: %+v", ds)
	}
	out = render(got["v"].Validate(value.Int(1, value.SourceVariable)))
	if !strings.Contains(out, "a string") {
		t.Errorf("want %q in the diagnostic for a KindString mismatch, got:\n%s", "a string", out)
	}
	if strings.Contains(out, "an string") {
		t.Errorf("diagnostic uses the wrong article for KindString:\n%s", out)
	}
}

func TestValidateEnforcesBoundsInclusively(t *testing.T) {
	s := intSchema(t, 1, 100)
	for _, tc := range []struct {
		n       int64
		wantErr bool
	}{
		{0, true},  // below min
		{1, false}, // ON min: inclusive
		{50, false},
		{100, false}, // ON max: inclusive
		{101, true},  // above max
	} {
		ds := s.Validate(value.Int(tc.n, value.SourceVariable))
		if got := ds.HasErrors(); got != tc.wantErr {
			t.Errorf("Validate(%d) errored = %v, want %v (bounds are inclusive: 1 and 100 are permitted, 0 and 101 are not)", tc.n, got, tc.wantErr)
		}
	}
}

func floatSchema(t *testing.T, min, max float64) Schema {
	t.Helper()
	d := numDecl("ratio", value.KindFloat, value.Float(min, value.SourceExplicit), value.Float(max, value.SourceExplicit), true, true)
	got, ds := Schemas([]config.VariableDecl{d})
	if ds.HasErrors() {
		t.Fatalf("fixture schema is itself invalid: %+v", ds)
	}
	return got["ratio"]
}

func TestValidateEnforcesFloatBoundsInclusively(t *testing.T) {
	// The int-kind sibling of this test (TestValidateEnforcesBoundsInclusively)
	// does not exercise compareBounds' KindFloat arm at all: mutating its `<`
	// to `<=` left the whole suite green. Comparison happens in the declared
	// kind (ruling 2), so the float side needs its own boundary coverage, not
	// just its own carries-through coverage (TestSchemasCarriesBoundsThrough).
	s := floatSchema(t, 0.5, 1.5)
	for _, tc := range []struct {
		f       float64
		wantErr bool
	}{
		{0.4, true},  // below min
		{0.5, false}, // ON min: inclusive
		{1.0, false},
		{1.5, false}, // ON max: inclusive
		{1.6, true},  // above max
	} {
		ds := s.Validate(value.Float(tc.f, value.SourceVariable))
		if got := ds.HasErrors(); got != tc.wantErr {
			t.Errorf("Validate(%v) errored = %v, want %v (bounds are inclusive: 0.5 and 1.5 are permitted, 0.4 and 1.6 are not)", tc.f, got, tc.wantErr)
		}
	}
}

func TestValidateComparesIntegersThatFloat64WouldConflate(t *testing.T) {
	// 2^53 and 2^53+1 are the same float64. Comparison must happen in int64 or
	// a value exactly one below the minimum validates as being on it.
	const min = int64(1)<<53 + 1
	s := intSchema(t, min, min+1000)
	if ds := s.Validate(value.Int(min-1, value.SourceVariable)); !ds.HasErrors() {
		t.Errorf("%d is below the minimum of %d and must be rejected; through float64 the two are indistinguishable", min-1, min)
	}
	if ds := s.Validate(value.Int(min, value.SourceVariable)); ds.HasErrors() {
		t.Errorf("%d is exactly the minimum and must be accepted: %+v", min, ds)
	}
}

func TestValidatePointsAtWhereTheBoundWasDeclared(t *testing.T) {
	// PLAN.md §44: an error says what was expected AND where. A bound
	// diagnostic that names only the offending value leaves the reader hunting
	// for the constraint.
	ds := intSchema(t, 1, 100).Validate(value.Int(500, value.SourceVariable))
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "variables.yml:5:7") {
		t.Errorf("the diagnostic must locate the declared bound:\n%s", sb.String())
	}
}

func TestValidateNamesTheCorrectRelationForEachBound(t *testing.T) {
	// checkBounds passes "at least" for the min violation and "at most" for
	// the max violation. No other test distinguishes the two: swapping them
	// (telling a user who is BELOW the minimum that they must be "at most" X)
	// left the rest of this suite green, because every other assertion here
	// only checks HasErrors() or a location string, never which relation word
	// was used. That swap would send a user fixing the error in the wrong
	// direction, so it is a correctness gap, not merely a wording one.
	s := intSchema(t, 1, 100)

	belowMin := renderOne(t, s.Validate(value.Int(0, value.SourceVariable)))
	if !strings.Contains(belowMin, "at least 1") {
		t.Errorf("a value below the minimum must be told \"at least 1\", got:\n%s", belowMin)
	}
	if strings.Contains(belowMin, "at most") {
		t.Errorf("a value below the minimum must not be told \"at most\", got:\n%s", belowMin)
	}

	aboveMax := renderOne(t, s.Validate(value.Int(500, value.SourceVariable)))
	if !strings.Contains(aboveMax, "at most 100") {
		t.Errorf("a value above the maximum must be told \"at most 100\", got:\n%s", aboveMax)
	}
	if strings.Contains(aboveMax, "at least") {
		t.Errorf("a value above the maximum must not be told \"at least\", got:\n%s", aboveMax)
	}
}

func renderOne(t *testing.T, ds diag.Diagnostics) string {
	t.Helper()
	if !ds.HasErrors() {
		t.Fatal("expected at least one diagnostic")
	}
	var sb strings.Builder
	ds.Render(&sb)
	return sb.String()
}

func TestValidateRejectsANumericValueWhoseDatumDoesNotMatchItsClaimedKind(t *testing.T) {
	// numericDatumMatchesKind exists because value.Equal and value.Format each
	// shipped a version that trusted Kind as a claim about Raw, and each
	// leaked because of it (one a false equality, one a printed secret). This
	// pins the same defence here: a Value claiming KindInt while holding a
	// non-int64 Raw — state or a provider is the realistic source, a test
	// fixture stands in for it — must be reported, not silently treated as
	// valid. Mutating numericDatumMatchesKind to `return true` passed the rest
	// of this suite; only this test catches it.
	s := intSchema(t, 1, 100)
	malformed := value.Value{Kind: value.KindInt, Known: true, Raw: "not an int", Source: value.SourceVariable}

	ds := s.Validate(malformed)
	if !ds.HasErrors() {
		t.Fatal("a Value claiming KindInt whose Raw is not an int64 must be rejected, not silently accepted")
	}
	var sb strings.Builder
	ds.Render(&sb)
	if !strings.Contains(sb.String(), "malformed") {
		t.Errorf("want the internal-error diagnostic, got:\n%s", sb.String())
	}
}

func TestValidateFailsClosedWhenABoundDoesNotMatchItsDeclaredKind(t *testing.T) {
	// The reviewer's exact repro. Schema is an exported struct with exported
	// fields, so anything can build one without going through Schemas() — the
	// tests in this file do exactly that. Before this fix, a KindInt schema
	// whose Min was hand-built as a KindFloat value made compareBounds unable
	// to read the bound, and it answered "no violation" for a pair it
	// couldn't compare — silently ACCEPTING a value that was actually below
	// the minimum. A validator that says "no problems" when it could not
	// perform the comparison is worse than one that errors: the caller has
	// no way to tell "checked and fine" from "could not check". This must
	// now fail closed: report a diagnostic instead of passing silently.
	s := Schema{Name: "replicas", Kind: value.KindInt, Origin: value.Origin{File: "variables.yml", Line: 3, Column: 5},
		Min: value.Float(1.5, value.SourceExplicit), HasMin: true}

	ds := s.Validate(value.Int(0, value.SourceVariable))
	if !ds.HasErrors() {
		t.Fatal("a bound that does not match its schema's declared kind must be reported, not silently treated as satisfied")
	}
	var sb strings.Builder
	ds.Render(&sb)
	out := sb.String()
	for _, want := range []string{"replicas", "min"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic must name the variable and which bound is inconsistent; %q is missing:\n%s", want, out)
		}
	}
}

func TestValidateIgnoresAnUnknownValue(t *testing.T) {
	// `infra validate` with no environment selected resolves environment-scoped
	// variables to unknowns (task 6). An unknown carries its Kind but has no
	// datum, so a bound check on it would be an invented answer.
	if ds := intSchema(t, 1, 100).Validate(value.Unknown(value.KindInt, value.SourceVariable)); ds.HasErrors() {
		t.Fatalf("an unknown value has no datum to bound-check: %+v", ds)
	}
}

func TestValidateRejectsAnUnknownOfTheWrongKind(t *testing.T) {
	if ds := intSchema(t, 1, 100).Validate(value.Unknown(value.KindString, value.SourceVariable)); !ds.HasErrors() {
		t.Fatal("an unknown still carries its Kind, so a kind mismatch is catchable even when the datum is not")
	}
}

func TestValidateAcceptsAnythingForAnUntypedDeclaration(t *testing.T) {
	d := decl("anything", value.KindInvalid)
	d.Default, d.HasDefault = value.String("x", value.SourceExplicit), true
	got, _ := Schemas([]config.VariableDecl{d})
	s := got["anything"]

	for _, v := range []value.Value{
		value.String("x", value.SourceVariable),
		value.Int(1, value.SourceVariable),
		value.Bool(true, value.SourceVariable),
	} {
		if ds := s.Validate(v); ds.HasErrors() {
			t.Errorf("an untyped declaration constrains nothing, but %s was rejected: %+v", v.Kind, ds)
		}
	}
}

func TestCoerceLeavesAnUntypedDeclarationUnconstrained(t *testing.T) {
	// Coerce's first guard is an OR of two conditions: s.Kind == KindInvalid,
	// or v.Kind == s.Kind. The rest of this package's Resolve-level coercion
	// tests all use a typed schema, so this is the only place the untyped
	// half of that OR is exercised: an untyped declaration has declared no
	// kind to normalise to, so a numeric literal must pass through exactly
	// as supplied, unconverted.
	d := decl("anything", value.KindInvalid)
	d.Default, d.HasDefault = value.String("x", value.SourceExplicit), true
	got, _ := Schemas([]config.VariableDecl{d})
	s := got["anything"]

	in := value.Int(1, value.SourceVariable)
	out, ds := s.Coerce(in)
	if ds.HasErrors() {
		t.Fatalf("an untyped declaration coerces nothing: %+v", ds)
	}
	if out.Kind != value.KindInt {
		t.Errorf("Kind = %v, want KindInt unchanged — nothing declares a target kind to coerce to", out.Kind)
	}
}

// TestCoerceDiagnosticNamesTheVarFileNotDashDashVar pins Coerce's
// lossy-conversion diagnostic to value.ScopeLabel — the first of MAJOR 1's
// three sites, and (with TestValidateKindMismatchNamesTheVarFileNotDashDashVar)
// the second the M4 re-review found unpinned: TestVarFileBoundViolationNamesTheFileNotDashDashVar
// in resolve_test.go only exercises boundDiag, so reverting ONLY this site's
// ScopeLabel call broke nothing in the suite before this test existed.
func TestCoerceDiagnosticNamesTheVarFileNotDashDashVar(t *testing.T) {
	s := intSchema(t, 1, 100)
	// A --var-file entry, exactly as config.DecodeVariableFile stamps one:
	// ScopeCLIOverride, SuppliedBy the path as typed. 1.5 cannot become an
	// integer without changing it, so Coerce reports it rather than
	// silently rounding.
	v := value.Float(1.5, value.SourceVariable).
		WithScope(value.ScopeCLIOverride).WithSuppliedBy("conf/big.yml")

	_, ds := s.Coerce(v)
	got := renderOne(t, ds)
	// "supplied by conf/big.yml is 1.5, which cannot be converted" is
	// unique to this template — "which cannot be converted" appears
	// nowhere else in this package (grepped), so this fragment cannot be
	// satisfied by Validate's or boundDiag's Detail firing instead.
	if !strings.Contains(got, "supplied by conf/big.yml is 1.5, which cannot be converted") {
		t.Errorf("Coerce's diagnostic must name the --var-file path that actually supplied the value:\n%s", got)
	}
	if strings.Contains(got, "supplied by --var ") {
		t.Errorf("must not blame --var for a --var-file value:\n%s", got)
	}
}

func TestValidateChecksListAndMapByKindOnly(t *testing.T) {
	got, ds := Schemas([]config.VariableDecl{decl("tags", value.KindList)})
	if ds.HasErrors() {
		t.Fatalf("fixture: %+v", ds)
	}
	s := got["tags"]

	mixed := value.List([]value.Value{
		value.String("a", value.SourceVariable),
		value.Int(2, value.SourceVariable),
	}, value.SourceVariable)
	if ds := s.Validate(mixed); ds.HasErrors() {
		t.Fatalf("PLAN.md §9 has no element-type syntax, so a list's elements are unconstrained: %+v", ds)
	}
	if ds := s.Validate(value.String("a,b", value.SourceVariable)); !ds.HasErrors() {
		t.Fatal("a string supplied for a list variable is still a kind mismatch")
	}
}

func schemaFor(t *testing.T, kind value.Kind) Schema {
	t.Helper()
	got, ds := Schemas([]config.VariableDecl{decl("v", kind)})
	if ds.HasErrors() {
		t.Fatalf("fixture schema %s is invalid: %+v", kind, ds)
	}
	return got["v"]
}

func TestParseTextConvertsToTheDeclaredKind(t *testing.T) {
	origin := value.Origin{File: "--var"}
	for _, tc := range []struct {
		kind value.Kind
		text string
		want any
	}{
		{value.KindString, "20", "20"},
		{value.KindInt, "20", int64(20)},
		{value.KindFloat, "2.5", 2.5},
		{value.KindBool, "true", true},
	} {
		got, ds := schemaFor(t, tc.kind).ParseText(tc.text, origin)
		if ds.HasErrors() {
			t.Fatalf("%s: unexpected diagnostics: %+v", tc.kind, ds)
		}
		if got.Kind != tc.kind || got.Raw != tc.want {
			t.Errorf("%s: ParseText(%q) = %v/%v, want %v/%v", tc.kind, tc.text, got.Kind, got.Raw, tc.kind, tc.want)
		}
		if got.Source != value.SourceVariable || got.Scope != value.ScopeCLIOverride {
			t.Errorf("%s: Source/Scope = %v/%v, want SourceVariable/ScopeCLIOverride", tc.kind, got.Source, got.Scope)
		}
		if got.SuppliedBy != "--var" {
			t.Errorf("%s: SuppliedBy = %q, want %q", tc.kind, got.SuppliedBy, "--var")
		}
	}
}

// TestParseTextCoercesAnExactFloatLiteralToInt reproduces M4 final review's
// MINOR 1: "--var size=42.0" against `type: integer` was rejected while a
// --var-file entry of `size: 42.0` was silently coerced — Amendment 4
// (contract.md) says a numeric literal coerces to the declared kind wherever
// it appears, exactly or not at all, and names a SUPPLIED value including
// --var explicitly, not only a declared default or --var-file. "42.0" is an
// exact int conversion and must now be accepted the same way.
func TestParseTextCoercesAnExactFloatLiteralToInt(t *testing.T) {
	got, ds := schemaFor(t, value.KindInt).ParseText("42.0", value.Origin{File: "--var"})
	if ds.HasErrors() {
		t.Fatalf("42.0 is an exact integer and must be accepted: %+v", ds)
	}
	if got.Kind != value.KindInt || got.Raw != int64(42) {
		t.Errorf("ParseText(%q) = %v/%v, want KindInt/42", "42.0", got.Kind, got.Raw)
	}
}

// TestParseTextStillRejectsALossyFloatLiteralForInt is the sibling check:
// coercion must stay "exact or not at all" — a fractional part is still a
// genuine error, not silently rounded away. TestParseTextRejectsTextThatIsNotTheDeclaredKind
// already pins "2.5" via the table above; this one is the same claim, named
// for the specific fix so a regression here fails with an obvious title.
func TestParseTextStillRejectsALossyFloatLiteralForInt(t *testing.T) {
	if _, ds := schemaFor(t, value.KindInt).ParseText("42.5", value.Origin{File: "--var"}); !ds.HasErrors() {
		t.Error("42.5 cannot become an integer without changing it and must stay rejected")
	}
}

// TestParseTextStampsSuppliedByFromOrigin pins that ParseText's SuppliedBy
// stamp reuses origin.File exactly, rather than hardcoding the literal
// "--var". Its one caller (resolve.go's rung 6) always builds origin from
// that same literal, so a mutation that drops the SuppliedBy stamp entirely
// is INVISIBLE to a rendered plan for --var specifically: --var's SuppliedBy
// ("--var") is byte-identical to Scope.String()'s ScopeCLIOverride fallback,
// so "stamped as --var" and "never stamped" render the same string. A
// distinctive origin, checked against the field directly rather than through
// rendering, is the only way to tell the two apart.
func TestParseTextStampsSuppliedByFromOrigin(t *testing.T) {
	origin := value.Origin{File: "a-distinctive-origin-name.yml"}
	got, ds := schemaFor(t, value.KindInt).ParseText("20", origin)
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %+v", ds)
	}
	if got.SuppliedBy != origin.File {
		t.Errorf("SuppliedBy = %q, want %q", got.SuppliedBy, origin.File)
	}
}

func TestParseTextRejectsTextThatIsNotTheDeclaredKind(t *testing.T) {
	for _, tc := range []struct {
		kind value.Kind
		text string
	}{
		{value.KindInt, "many"},
		{value.KindInt, "2.5"},
		{value.KindFloat, "many"},
		{value.KindBool, "maybe"},
	} {
		if _, ds := schemaFor(t, tc.kind).ParseText(tc.text, value.Origin{File: "--var"}); !ds.HasErrors() {
			t.Errorf("--var for a %s variable given %q must be rejected", tc.kind, tc.text)
		}
	}
}

func TestParseTextRefusesListAndMapVariables(t *testing.T) {
	for _, k := range []value.Kind{value.KindList, value.KindMap} {
		if _, ds := schemaFor(t, k).ParseText("a,b,c", value.Origin{File: "--var"}); !ds.HasErrors() {
			t.Errorf("--var cannot supply a %s: parsing one needs a mini-language, and PLAN.md §9 forbids building one", k)
		}
	}
}

func TestParseTextLeavesAnUntypedDeclarationAsText(t *testing.T) {
	d := decl("anything", value.KindInvalid)
	d.Default, d.HasDefault = value.String("x", value.SourceExplicit), true
	got, _ := Schemas([]config.VariableDecl{d})

	v, ds := got["anything"].ParseText("20", value.Origin{File: "--var"})
	if ds.HasErrors() {
		t.Fatalf("an untyped declaration constrains nothing: %+v", ds)
	}
	if v.Kind != value.KindString {
		t.Errorf("Kind = %v, want KindString — guessing a type from the spelling would make `--var version=1.10` the float 1.1", v.Kind)
	}
}

// TestShowRendersStringsBareNotQuoted pins show's use of
// value.ProseFormatOptions rather than value.ReportFormatOptions or
// value.PlanFormatOptions: a diagnostic's "the value supplied by ... is ..."
// reads as a sentence, not a diff, so a string value must appear bare — the
// same distinction ProseFormatOptions' own doc comment names. A version of
// show that quoted strings (either of the other two named option sets) would
// pass every other test in this package, because none of them happens to
// assert on the quoting of a string bound violation's Detail text — this
// test exists specifically to close that gap.
func TestShowRendersStringsBareNotQuoted(t *testing.T) {
	got := show(value.String("staging", value.SourceExplicit))
	if got != "staging" {
		t.Errorf(`show(String("staging")) = %q, want the bare word "staging" — ProseFormatOptions does not quote strings`, got)
	}
}
