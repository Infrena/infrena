package expressions

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/value"
)

// TestParseShapesAreUnchangedByTheScannerRewrite is a CHARACTERISATION test
// (PLAN.md §10.4, Task 1). It records what matchBrace and splitArgs do TODAY, so
// unifying them into one walk can be shown not to alter it.
//
// It must be green BEFORE the refactor. A characterisation test that fails first
// is recording a bug rather than the behaviour.
//
// Each case is named for its SHAPE rather than its expectation, so a wrong
// expectation shows up as a wrong name.
func TestParseShapesAreUnchangedByTheScannerRewrite(t *testing.T) {
	for _, tc := range []struct {
		shape string
		src   string
		// want is the first diagnostic summary, or "" for no diagnostics.
		want string
		// refs is the RESOURCE reference targets parsed, joined by ",". A bare
		// name with no dot is a VARIABLE and does not appear here — which is
		// itself behaviour worth recording, and it is why the fixtures below use
		// dotted targets wherever the extraction is the thing being pinned.
		refs string
		// args is the number of arguments the top-level call parsed to, or 0 when
		// the expression is not a call.
		//
		// Without this the test cannot see splitArgs at all: a quoted comma
		// mis-split into three arguments leaves the REFERENCES identical, and
		// arity is not checked until evaluation. A sabotage proved exactly that.
		args int
	}{
		{shape: "a bare variable is not a resource reference", src: "${a}", refs: ""},
		{shape: "a resource reference", src: "${db.id}", refs: "db.id"},
		{shape: "two references and literal text", src: "${db.id}-${net.id}", refs: "db.id,net.id"},
		{shape: "a call with three arguments", src: "${replace(db.id, \"x\", \"y\")}", refs: "db.id", args: 3},
		{shape: "a nested call", src: "${lower(trim(db.name))}", refs: "db.name", args: 1},
		// A comma inside a NESTED call's parens is not an argument separator.
		// Nothing else here has one, and without it splitArgs' depth counter can
		// be deleted with every test still green — a sabotage proved that.
		{shape: "a comma inside a nested call", src: `${replace(join(db.id, "-"), "x", "y")}`,
			refs: "db.id", args: 3},
		{shape: "a quoted brace inside an argument", src: "${join(db.id, \"}\")}", refs: "db.id", args: 2},
		{shape: "a quoted comma inside an argument", src: "${join(db.id, \",\")}", refs: "db.id", args: 2},
		{shape: "an escaped quote inside an argument", src: `${join(db.id, "\"")}`, refs: "db.id", args: 2},
		{shape: "a quoted paren inside an argument", src: "${join(db.id, \")\")}", refs: "db.id", args: 2},
		{shape: "no interpolation at all", src: "plain text", refs: ""},
		{shape: "a literal dollar without a brace", src: "costs $5", refs: ""},

		{shape: "an empty interpolation", src: "${}", want: "empty interpolation"},
		{shape: "an unclosed interpolation", src: "${a", want: "unclosed interpolation"},
		{shape: "an unclosed call", src: "${lower(a}", want: `unclosed call to "lower"`},
		{shape: "a call with no name", src: "${(a)}", want: "call with no function name"},
		{shape: "a reference with an empty segment", src: "${a..b}", want: `malformed reference "a..b"`},

		// An unknown function parses cleanly and is refused at EVALUATION, not
		// here. Recorded because it is surprising, and because a rewrite that
		// started reporting it at parse time would be a behaviour change.
		{shape: "an unknown function parses without complaint", src: "${nosuch(db.id)}", refs: "db.id", args: 1},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			e, ds := Parse(tc.src, value.Origin{File: "f.yml", Line: 1, Column: 1})

			var got string
			if len(ds) > 0 {
				got = ds[0].Summary
			}
			if tc.want == "" && got != "" {
				t.Fatalf("Parse(%q) reported %q, want no diagnostic", tc.src, got)
			}
			if tc.want != "" {
				if !strings.Contains(got, tc.want) {
					t.Fatalf("Parse(%q) reported %q, want something containing %q", tc.src, got, tc.want)
				}
				return
			}

			var names []string
			if e != nil {
				for _, r := range e.References() {
					names = append(names, r.String())
				}
			}
			if strings.Join(names, ",") != tc.refs {
				t.Errorf("Parse(%q) referenced %q, want %q", tc.src, strings.Join(names, ","), tc.refs)
			}
			if tc.args > 0 {
				if e == nil || e.Op != value.OpCall {
					t.Fatalf("Parse(%q) did not produce a call", tc.src)
				}
				if len(e.Args) != tc.args {
					t.Errorf("Parse(%q) split into %d arguments, want %d — a delimiter inside a "+
						"quoted literal was treated as significant", tc.src, len(e.Args), tc.args)
				}
			}
		})
	}
}
