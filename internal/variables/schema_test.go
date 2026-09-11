package variables

import (
	"strings"
	"testing"

	"infra/internal/config"
	"infra/internal/diag"
	"infra/pkg/value"
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
