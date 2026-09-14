package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infrena/infrena/internal/diag"
	"github.com/infrena/infrena/pkg/value"
)

// decodeTree writes files to a temp dir, runs the REAL Load, and decodes the
// result.
//
// It deliberately goes through Load rather than hand-building []File. Whether
// stage 2 gets its files in the right order and tagged with the right FileKind
// is a precondition PRODUCTION establishes, and two M3 Criticals hid behind
// fixtures that manufactured preconditions production never established.
func decodeTree(t *testing.T, files map[string]string) (*ProjectDecl, diag.Diagnostics) {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return Decode(loaded)
}

// errorSummaries returns every error diagnostic's summary, for assertions that
// care that SOMETHING was reported and what it was about.
func errorSummaries(ds diag.Diagnostics) []string {
	var out []string
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			out = append(out, d.Summary)
		}
	}
	return out
}

func requireNoErrors(t *testing.T, ds diag.Diagnostics) {
	t.Helper()
	if ds.HasErrors() {
		t.Fatalf("unexpected errors: %v", errorSummaries(ds))
	}
}

// requireErrorAbout asserts exactly one error and that it mentions each
// fragment. Asserting the COUNT matters: a check that fires twice is a user
// reading the same problem described two different ways.
func requireErrorAbout(t *testing.T, ds diag.Diagnostics, fragments ...string) diag.Diagnostic {
	t.Helper()
	var errs []diag.Diagnostic
	for _, d := range ds {
		if d.Severity == diag.SeverityError {
			errs = append(errs, d)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("want exactly 1 error, got %d: %v", len(errs), errorSummaries(ds))
	}
	text := errs[0].Summary + " | " + errs[0].Detail + " | " + errs[0].Action
	for _, f := range fragments {
		if !strings.Contains(text, f) {
			t.Errorf("diagnostic does not mention %q: %s", f, text)
		}
	}
	if errs[0].Origin.Line == 0 {
		t.Errorf("diagnostic has no line number; stage 2 is the last stage that has one: %s", text)
	}
	return errs[0]
}

func findVariable(t *testing.T, p *ProjectDecl, name string) VariableDecl {
	t.Helper()
	for _, v := range p.Variables {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no variable %q in %v", name, p.Variables)
	return VariableDecl{}
}

const projectWithNoResources = "project: demo\nresources: {}\n"

// TestDecodeVariableDeclaration decodes PLAN.md §9's example verbatim.
func TestDecodeVariableDeclaration(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
variables:
  replicas:
    type: integer
    default: 2
    min: 1
    max: 100
`,
	})
	requireNoErrors(t, ds)

	v := findVariable(t, p, "replicas")
	if v.Type != value.KindInt {
		t.Errorf("Type = %v, want KindInt", v.Type)
	}
	if !v.HasDefault {
		t.Fatal("HasDefault = false")
	}
	// KindInt, not KindString: a default decoded as text would compare
	// against min and max by text, where "10" < "9".
	if got, ok := v.Default.AsInt(); !ok || got != 2 {
		t.Errorf("Default = %#v, want Int(2)", v.Default)
	}
	if !v.HasMin || !v.HasMax {
		t.Fatalf("HasMin=%v HasMax=%v", v.HasMin, v.HasMax)
	}
	// KindInt, not KindFloat and not KindString: bounds are stored as decoded,
	// so an integer bound stays an int64 and compares exactly.
	if got, ok := v.Min.AsInt(); !ok || got != 1 {
		t.Errorf("Min = %#v, want Int(1)", v.Min)
	}
	if got, ok := v.Max.AsInt(); !ok || got != 100 {
		t.Errorf("Max = %#v, want Int(100)", v.Max)
	}
	// A range diagnostic must be able to point at where the bound was
	// declared, which is the second reason these are Values and not floats.
	if v.Min.Origin.Line == 0 {
		t.Error("Min has no Origin; a range error could not name the line the bound is on")
	}
	if v.Origin.Line == 0 || !strings.HasSuffix(v.Origin.File, ProjectFileName) {
		t.Errorf("Origin = %v, want a line in %s", v.Origin, ProjectFileName)
	}
	if v.Default.Scope != value.ScopeUnset {
		t.Errorf("Default.Scope = %v; stage 2 declares, it does not resolve — Scope is stages 3 and 4's answer", v.Default.Scope)
	}
}

// TestEveryDocumentedVariableTypeIsAccepted and TestUnknownVariableTypeIsRejected
// are the two directions of one predicate. Either alone is satisfied by a
// constant function.
func TestEveryDocumentedVariableTypeIsAccepted(t *testing.T) {
	want := map[string]value.Kind{
		"string":  value.KindString,
		"integer": value.KindInt,
		"float":   value.KindFloat,
		"boolean": value.KindBool,
		"list":    value.KindList,
		"map":     value.KindMap,
	}
	for spelling, kind := range want {
		t.Run(spelling, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + spelling + "\n",
			})
			requireNoErrors(t, ds)
			if got := findVariable(t, p, "v").Type; got != kind {
				t.Errorf("type %q decoded as %v, want %v", spelling, got, kind)
			}
		})
	}
}

func TestUnknownVariableTypeIsRejected(t *testing.T) {
	// "int" and "bool" are the near-misses a user actually types; "widget" is
	// the far miss. All must be refused, and the message must list what IS
	// available (PLAN.md §44: say what was expected and what to do).
	for _, spelling := range []string{"int", "bool", "str", "number", "widget", "Integer"} {
		t.Run(spelling, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + spelling + "\n",
			})
			requireErrorAbout(t, ds, spelling, "integer", "string", "boolean")
		})
	}
}

// TestUnknownTypeDiagnosticOffersEveryRealType. The message must list what IS
// available, or a user who typed `int` learns only that it is wrong.
//
// The list is driven from value.KindNames rather than a copy here, so a type
// added to value.ParseKind cannot start working while the diagnostic keeps
// advertising the old set. The frozen spelling of each type is pinned in
// pkg/value, beside ParseKind and Kind.String(), which own it in both
// directions; this package has no table of its own to freeze.
func TestUnknownTypeDiagnosticOffersEveryRealType(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  v:\n    type: widget\n",
	})
	d := requireErrorAbout(t, ds, "widget")
	text := d.Summary + " | " + d.Detail + " | " + d.Action
	for _, name := range value.KindNames() {
		if !strings.Contains(text, name) {
			t.Errorf("diagnostic does not offer %q: %s", name, text)
		}
	}
}

// TestBoundsAreAcceptedOnNumericTypes is the accepting direction of the
// min/max predicate.
func TestBoundsAreAcceptedOnNumericTypes(t *testing.T) {
	for _, spelling := range []string{"integer", "float"} {
		t.Run(spelling, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + spelling + "\n    min: 1\n    max: 10\n",
			})
			requireNoErrors(t, ds)
			v := findVariable(t, p, "v")
			if !v.HasMin || !v.HasMax {
				t.Errorf("HasMin=%v HasMax=%v on type %s", v.HasMin, v.HasMax, spelling)
			}
		})
	}
}

// TestBoundsAreRejectedOnNonNumericTypes is the refusing direction, over every
// type that is not orderable plus the untyped case.
func TestBoundsAreRejectedOnNonNumericTypes(t *testing.T) {
	for _, spelling := range []string{"string", "boolean", "list", "map"} {
		for _, bound := range []string{"min", "max"} {
			t.Run(spelling+"/"+bound, func(t *testing.T) {
				_, ds := decodeTree(t, map[string]string{
					ProjectFileName: projectWithNoResources +
						"variables:\n  v:\n    type: " + spelling + "\n    " + bound + ": 1\n",
				})
				requireErrorAbout(t, ds, bound, "v", spelling)
			})
		}
	}
	// "type" alone does not discriminate here: decodeBound's generic
	// non-numeric-type message ALSO contains "type" (it reads "has type
	// invalid" when kind is KindInvalid and the KindInvalid-specific branch
	// is skipped), so a fixture using only that fragment would pass whether
	// or not the untyped-specific wording actually ran. "declares no" is
	// unique to the untyped-specific Detail across the whole package
	// (grepped decode.go before trusting it).
	t.Run("untyped", func(t *testing.T) {
		_, ds := decodeTree(t, map[string]string{
			ProjectFileName: projectWithNoResources +
				"variables:\n  v:\n    min: 1\n",
		})
		requireErrorAbout(t, ds, "min", "v", "declares no")
	})
}

// TestUnusableTypeSuppressesTheBoundDiagnostic covers two audit gaps with one
// fixture: decodeBound's `typeReported` suppression, and a non-scalar
// `type:` (as opposed to an unknown spelling like `type: widget`, already
// covered elsewhere). A non-scalar `type:` is the only route that sets
// `typeReported = true` through requireScalar's OWN failure rather than
// ParseKind's, so combining it with a `min` is what exercises decodeBound's
// suppression specifically: without it, a variable whose `type:` is already
// reported as unusable would ALSO get a second, misleading "`min` is not
// valid on variable..." diagnostic about the same declaration.
// requireErrorAbout's exactly-one-error assertion is what fails if that
// suppression is skipped.
func TestUnusableTypeSuppressesTheBoundDiagnostic(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type:\n      - a\n      - b\n    min: 1\n",
	})
	requireErrorAbout(t, ds, "v", "type")
}

// TestBoundIsCheckedEvenWhenTypeIsWrittenAfterIt is the ordering fixture, and
// it is built so the natural order CONTRADICTS the required behaviour: `min`
// appears BEFORE `type`, so an implementation that checks each key as it walks
// the mapping sees no type yet and lets the bound through.
func TestBoundIsCheckedEvenWhenTypeIsWrittenAfterIt(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    min: 1\n    max: 10\n    type: string\n",
	})
	// Two errors here — one per bound — so requireErrorAbout's exactly-one
	// rule does not apply.
	got := errorSummaries(ds)
	if len(got) != 2 {
		t.Fatalf("want 2 errors (one per bound), got %d: %v", len(got), got)
	}
	joined := strings.Join(got, " | ")
	for _, f := range []string{"min", "max"} {
		if !strings.Contains(joined, f) {
			t.Errorf("no diagnostic for %q: %s", f, joined)
		}
	}
}

// TestBoundIsCoercedToTheDeclaredType is the invariant Task 4 relies on: after
// stage 2, a bound's Kind ALWAYS equals its variable's declared Type, so a
// consumer switches on one Kind rather than a cross product — and value.AsFloat,
// which does not coerce an int64, is safe to use directly.
//
// The fixture is the one that would otherwise slip through: YAML tags `min: 1`
// as !!int even under `type: float`, so an implementation that stores the
// bound as decoded produces a KindInt bound on a float variable and AsFloat
// reads false.
func TestBoundIsCoercedToTheDeclaredType(t *testing.T) {
	cases := []struct {
		name, decl string
		wantKind   value.Kind
	}{
		{"int written as int", "type: integer\n    min: 1\n    max: 10\n", value.KindInt},
		{"float written as int", "type: float\n    min: 1\n    max: 10\n", value.KindFloat},
		{"float written as float", "type: float\n    min: 1.5\n    max: 9.5\n", value.KindFloat},
		{"int written as whole float", "type: integer\n    min: 1.0\n    max: 10.0\n", value.KindInt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + "variables:\n  v:\n    " + tc.decl,
			})
			requireNoErrors(t, ds)
			v := findVariable(t, p, "v")
			if !v.HasMin || !v.HasMax {
				t.Fatalf("HasMin=%v HasMax=%v", v.HasMin, v.HasMax)
			}
			if v.Min.Kind != tc.wantKind || v.Max.Kind != tc.wantKind {
				t.Fatalf("Min.Kind=%v Max.Kind=%v, want %v (Type=%v)", v.Min.Kind, v.Max.Kind, tc.wantKind, v.Type)
			}
			if v.Min.Kind != v.Type {
				t.Errorf("bound Kind %v != declared Type %v; Task 4's Kind switch breaks", v.Min.Kind, v.Type)
			}
			// The accessor for the declared kind must actually read it. This
			// is the assertion that fails if coercion is skipped.
			switch tc.wantKind {
			case value.KindFloat:
				if _, ok := v.Min.AsFloat(); !ok {
					t.Error("AsFloat cannot read a float variable's min; the bound was not coerced")
				}
			case value.KindInt:
				if _, ok := v.Min.AsInt(); !ok {
					t.Error("AsInt cannot read an integer variable's min")
				}
			}
			// Origin must survive the coercion, or a range diagnostic cannot
			// name the line the bound is on.
			if v.Min.Origin.Line == 0 {
				t.Error("coercion dropped the bound's Origin")
			}
		})
	}
}

// TestDefaultIsCoercedToTheDeclaredTypeRegardlessOfKeyOrder pins M4 contract
// Amendment 4: a numeric `default:` coerces to its variable's declared type
// exactly like `min`/`max` already do. YAML tags `default: 1` as !!int even
// under `type: float`, so `type: float` with `default: 1` must store 1.0,
// not an int64 masquerading under a float-typed field.
//
// Variable "b" writes `default:` BEFORE `type:` — the order that would defeat
// an implementation that coerces inside decodeVariable's key-walking loop,
// since `v.Type` is not yet known when that line is reached. "a" writes the
// natural order and would pass even against that broken implementation; "b"
// is the fixture that actually discriminates, which is why both must be
// present and both must produce the same result.
func TestDefaultIsCoercedToTheDeclaredTypeRegardlessOfKeyOrder(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + `
variables:
  a:
    type: float
    default: 1
  b:
    default: 1
    type: float
`,
	})
	requireNoErrors(t, ds)
	for _, name := range []string{"a", "b"} {
		v := findVariable(t, p, name)
		if v.Default.Kind != value.KindFloat {
			t.Errorf("%s: Default.Kind = %v, want KindFloat (Type=%v)", name, v.Default.Kind, v.Type)
			continue
		}
		if got, ok := v.Default.AsFloat(); !ok || got != 1.0 {
			t.Errorf("%s: Default = %#v, want Float(1.0)", name, v.Default)
		}
		if v.Default.Origin.Line == 0 {
			t.Errorf("%s: Default has no Origin; coercion dropped it", name)
		}
	}
}

// TestDefaultThatCannotBeCoercedIsRejected is Amendment 4's refusing
// direction on the same rule TestBoundThatCannotBeCoercedIsRejected pins for
// `min`/`max`: `type: integer` with `default: 1.5` quietly becoming 1 would
// let a plan start from a value the user never wrote, with nothing printed.
func TestDefaultThatCannotBeCoercedIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    default: 1.5\n",
	})
	requireErrorAbout(t, ds, "default", "v", "integer")
}

// TestDefaultThatOverflowsFloatIsRejected is Amendment 4's counterpart to
// TestBoundThatOverflowsFloatIsRejected: an int64 above 2^53 does not survive
// conversion to float64 and back, so it must be a diagnostic here too, for
// the identical reason.
func TestDefaultThatOverflowsFloatIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: float\n    default: 9007199254740993\n",
	})
	requireErrorAbout(t, ds, "default", "v", "float")
}

// TestUntypedDefaultIsNotCoerced pins Amendment 4's boundary: an untyped
// declaration has no declared kind to coerce toward, so its default is
// stored exactly as decoded. A coercion attempt here would have nothing to
// coerce TO, and value.KindInt would be an arbitrary choice no more correct
// than any other.
func TestUntypedDefaultIsNotCoerced(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  v:\n    default: 1\n",
	})
	requireNoErrors(t, ds)
	v := findVariable(t, p, "v")
	if v.Default.Kind != value.KindInt {
		t.Errorf("Default.Kind = %v, want KindInt: an untyped variable's default must not be coerced", v.Default.Kind)
	}
	if got, ok := v.Default.AsInt(); !ok || got != 1 {
		t.Errorf("Default = %#v, want Int(1)", v.Default)
	}
}

// TestBoundThatCannotBeCoercedIsRejected pins that a lossy conversion is a
// diagnostic, never a silent truncation. `type: integer` with `min: 1.5`
// quietly becoming 1 would accept values below the stated minimum with nothing
// printed.
func TestBoundThatCannotBeCoercedIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    min: 1.5\n",
	})
	d := requireErrorAbout(t, ds, "min", "v", "integer")
	if strings.Contains(d.Summary+d.Detail, "must be a number") {
		t.Error("1.5 IS a number; the diagnostic should say it is not a whole one")
	}
}

// TestBoundThatOverflowsFloatIsRejected pins coerceBound's float-overflow
// path: an int64 above 2^53 does not survive being converted to float64 and
// back, so declaring `type: float` with such a `min` must be a diagnostic —
// otherwise the stored bound silently is not the number the user wrote.
// float64 can represent every integer up to 2^53 exactly; 2^53+1 is exactly
// halfway between the two representable floats on either side of it, and
// round-half-to-even rounds it down to 2^53, so this specific value is what
// makes the conversion lossy rather than merely large.
func TestBoundThatOverflowsFloatIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: float\n    min: 9007199254740993\n",
	})
	requireErrorAbout(t, ds, "min", "v", "float")
}

// TestQuotedBoundIsRejected: "10" is a string, and string bounds compare by
// text, so `max: "9"` would silently reject 10.
func TestQuotedBoundIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n    max: \"9\"\n",
	})
	requireErrorAbout(t, ds, "max", "number")
}

// TestImpossibleBoundsAreRejected: no value can satisfy min > max, and every
// plan would fail with a range error naming the VALUE rather than the
// declaration that is actually wrong.
//
// Both numeric types are exercised. boundGreater switches on Kind and calls
// AsInt for one and AsFloat for the other — two independent code paths, not
// one path coerced twice — so a fixture using only "integer" would leave a
// broken float comparison silently accepting an impossible declaration.
func TestImpossibleBoundsAreRejected(t *testing.T) {
	for _, tc := range []struct{ spelling, min, max string }{
		{"integer", "100", "1"},
		{"float", "5.0", "1.0"},
	} {
		t.Run(tc.spelling, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + tc.spelling + "\n    min: " + tc.min + "\n    max: " + tc.max + "\n",
			})
			requireErrorAbout(t, ds, "min", "max", "v")
		})
	}
}

// TestBoundsAtTheSameNumberAreAllowed is the boundary: min == max pins a
// variable to one value, which is unusual but not wrong. A `>=` comparison
// instead of `>` fails here — on both numeric types, for the same reason
// TestImpossibleBoundsAreRejected checks both.
func TestBoundsAtTheSameNumberAreAllowed(t *testing.T) {
	for _, tc := range []struct{ spelling, bound string }{
		{"integer", "5"},
		{"float", "1.5"},
	} {
		t.Run(tc.spelling, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources +
					"variables:\n  v:\n    type: " + tc.spelling + "\n    min: " + tc.bound + "\n    max: " + tc.bound + "\n",
			})
			requireNoErrors(t, ds)
		})
	}
}

// TestVariablesFileValueWithInterpolationIsRejected: variables.yml is
// resolved before any expression scope exists (PLAN.md §8), so an unresolved
// `${...}` there must be REJECTED, not stored as the variable's literal value
// and carried into stage 4 as if it had been written that way on purpose.
// The sibling check on infra.yml's `default:` is covered elsewhere; this is
// the same rule in the other file that can declare a variable's value.
func TestVariablesFileValueWithInterpolationIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:   projectWithNoResources,
		VariablesFileName: "region: ${var.other}\n",
	})
	requireErrorAbout(t, ds, "region", "interpolation")
}

// TestDuplicateVariableNameIsRejected. yaml.v3's Node decoding does not
// deduplicate mapping keys, so both entries arrive here and the last would
// silently win — discarding a type or a bound the user wrote.
func TestDuplicateVariableNameIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  v:\n    type: integer\n  v:\n    type: string\n",
	})
	d := requireErrorAbout(t, ds, "v", "more than once")
	if !strings.Contains(d.Detail, "line") {
		t.Errorf("duplicate diagnostic must point at the other declaration: %s", d.Detail)
	}
}

// TestEmptyVariableDeclarationIsRejected. A declaration with neither a type
// nor a default says nothing about the variable.
//
// It is an ERROR, not a dropped declaration with a warning: dropping it
// discards something the user wrote and carries on as though they had not,
// which is the silent-loss shape with a log line in front of it.
func TestEmptyVariableDeclarationIsRejected(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  replicas: {}\n",
	})
	requireErrorAbout(t, ds, "replicas", "type", "default")
}

// TestAVariableWithEitherHalfIsAccepted is the other direction, and it is the
// half that catches an over-eager check. A type alone is a complete
// declaration (the variable is required, and stage 4 will say so if it is
// unset); a default alone is a complete declaration (untyped with a fallback,
// which PLAN.md §9 permits by calling schemas optional).
func TestAVariableWithEitherHalfIsAccepted(t *testing.T) {
	for name, body := range map[string]string{
		"type only":    "variables:\n  v:\n    type: string\n",
		"default only": "variables:\n  v:\n    default: 2\n",
		"both":         "variables:\n  v:\n    type: integer\n    default: 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + body,
			})
			requireNoErrors(t, ds)
		})
	}
}

// TestEmptyDeclarationSaysTheRootCauseOnce. Each of these is also an empty
// declaration, but each already has a diagnostic naming the real problem —
// so exactly one error, not two describing the same line.
//
// requireErrorAbout asserts the count, which is what makes this test do work:
// without the suppressions it sees two and fails.
func TestEmptyDeclarationSaysTheRootCauseOnce(t *testing.T) {
	cases := map[string][]string{
		// `type` present but unusable.
		"variables:\n  v:\n    type: widget\n": {"widget"},
		// `default` present but unusable.
		"variables:\n  v:\n    default: ${var.other}\n": {"interpolation"},
		// A bound with no type: decodeBound already says to add one.
		"variables:\n  v:\n    min: 1\n": {"min", "type"},
	}
	for body, fragments := range cases {
		t.Run(strings.ReplaceAll(body, "\n", " "), func(t *testing.T) {
			_, ds := decodeTree(t, map[string]string{
				ProjectFileName: projectWithNoResources + body,
			})
			requireErrorAbout(t, ds, fragments...)
		})
	}
}

// TestVariableBodyMustBeAMapping pins ruling 1, and the Action must tell the
// user where a bare value actually goes.
//
// The fragments matter here: `"v"` and VariablesFileName both also appear in
// decodeVariable's "must specify at least a `type` or a `default`"
// diagnostic — the one that fires INSTEAD when this guard is disabled, since
// a scalar body has no Content for the key-walking loop to see, so nothing
// sets Type or HasDefault and the declaration looks empty rather than
// malformed. `"schema"` is unique to this diagnostic across the whole
// package (grepped decode.go for every other "must be a mapping" and every
// other use of the word "schema" before trusting it), so it is the fragment
// that actually distinguishes "the body is the wrong shape" from "the body
// says nothing".
func TestVariableBodyMustBeAMapping(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources + "variables:\n  v: 2\n",
	})
	requireErrorAbout(t, ds, "v", "must be a mapping", "schema")
}

// TestVariablesFileValuesAreTaggedVariableAtEveryDepth pins per-leaf
// provenance (spec §5.1). decodeValue marks everything SourceExplicit because
// that is what an attribute in infra.yml is; a value from variables.yml is a
// variable, and setting only the top level leaves every list element claiming
// to be explicit configuration — which `explain` and minimal generation both
// read.
//
// `limits.ports` is a LIST NESTED INSIDE A MAP, deliberately, not a second
// list of scalars alongside `tags`. retagSource(item, src) and
// item.WithSource(src) are indistinguishable whenever `item` is itself a
// scalar — recursing and not recursing look identical when there is nothing
// underneath to leave untagged — so a fixture whose composites hold only
// scalar leaves (the original version of this fixture: `tags` a list of
// strings, `limits` a map of one int) cannot tell a genuinely recursive
// retagSource from `v.WithSource(src)` called once at the top. `ports`
// forces at least one level of real recursion: `limits` is a map holding a
// list, and that list holds scalars, so a shallow retag would leave
// `limits.ports[]` at SourceExplicit while `limits.cpu` and the top-level
// `limits` itself both read SourceVariable — a result this test's per-leaf
// walk would report at the `limits.ports[]` path specifically.
func TestVariablesFileValuesAreTaggedVariableAtEveryDepth(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName:   projectWithNoResources,
		VariablesFileName: "region: us-east-1\ntags:\n  - web\n  - api\nlimits:\n  cpu: 2\n  ports:\n    - 80\n    - 443\n",
	})
	requireNoErrors(t, ds)

	if got, ok := p.VariableValues["region"].AsString(); !ok || got != "us-east-1" {
		t.Fatalf("region = %#v", p.VariableValues["region"])
	}
	var check func(path string, v value.Value)
	check = func(path string, v value.Value) {
		if v.Source != value.SourceVariable {
			t.Errorf("%s: Source = %v, want SourceVariable", path, v.Source)
		}
		if v.Scope != value.ScopeUnset {
			t.Errorf("%s: Scope = %v; stage 2 declares, it does not resolve", path, v.Scope)
		}
		switch items := v.Raw.(type) {
		case []value.Value:
			for i, item := range items {
				check(path+"[]", item)
				_ = i
			}
		case map[string]value.Value:
			for k, item := range items {
				check(path+"."+k, item)
			}
		}
	}
	for name, v := range p.VariableValues {
		check(name, v)
	}
	if len(p.VariableValues) != 3 {
		t.Fatalf("VariableValues has %d entries, want 3: %v", len(p.VariableValues), p.VariableValues)
	}
}

// TestEmptyOptionalFilesAreNotErrors pins ruling 10 in both directions: an
// empty variables.yml is fine, an empty infra.yml is not.
func TestEmptyOptionalFilesAreNotErrors(t *testing.T) {
	_, ds := decodeTree(t, map[string]string{
		ProjectFileName:               projectWithNoResources,
		VariablesFileName:             "",
		"environments/production.yml": "",
	})
	requireNoErrors(t, ds)

	_, ds = decodeTree(t, map[string]string{ProjectFileName: ""})
	if !ds.HasErrors() {
		t.Fatal("an empty infra.yml must still be an error")
	}
}

// TestVariablesAreSortedByName. The insertion order CONTRADICTS the sorted
// order, and the assertion loops rather than sampling.
func TestVariablesAreSortedByName(t *testing.T) {
	p, ds := decodeTree(t, map[string]string{
		ProjectFileName: projectWithNoResources +
			"variables:\n  zulu:\n    type: string\n  mike:\n    type: string\n" +
			"  alpha:\n    type: string\n  yankee:\n    type: string\n" +
			"  bravo:\n    type: string\n  november:\n    type: string\n",
	})
	requireNoErrors(t, ds)
	for i := 1; i < len(p.Variables); i++ {
		if p.Variables[i-1].Name >= p.Variables[i].Name {
			t.Fatalf("Variables not sorted: %q before %q", p.Variables[i-1].Name, p.Variables[i].Name)
		}
	}
	if len(p.Variables) != 6 {
		t.Fatalf("got %d variables, want 6", len(p.Variables))
	}
}
