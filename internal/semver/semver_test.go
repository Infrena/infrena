package semver

import "testing"

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

// TestParseAcceptsTheFormsPeopleWrite. A floor is written by a person, in a YAML file,
// and `>= 0.4` is what they mean by it — refusing an omitted patch is pedantry over a
// form every other tool accepts.
func TestParseAcceptsTheFormsPeopleWrite(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.2.3", "1.2.3"},
		{"v1.2.3", "1.2.3"},
		{"0.4", "0.4.0"},
		{"2", "2.0.0"},
		{"  1.0.0  ", "1.0.0"},
		{"1.0.0-rc1", "1.0.0-rc1"},
		// Build metadata is not part of identity, so it is dropped rather than kept:
		// keeping it would make two equal versions print differently.
		{"1.0.0+build7", "1.0.0"},
	} {
		if got := mustParse(t, tc.in).String(); got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "  ", "x", "1.2.3.4", "1.x", "-1.0.0", "1..0"} {
		if v, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, v)
		}
	}
}

func TestCompareOrdersByEachFieldInTurn(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "1.0.1", -1},
		// Pre-release takes no part; see Compare's doc comment. Asserted so the
		// omission is pinned as a decision rather than left to be discovered.
		{"1.0.0-rc1", "1.0.0", 0},
	} {
		if got := Compare(mustParse(t, tc.a), mustParse(t, tc.b)); got != tc.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestABareVersionMeansExactly. `aws: "1.2.0"` pins; it does not mean `>= 1.2.0`.
// Guessing the looser reading would silently accept a version the user thought they
// had pinned, which is the direction that changes infrastructure.
func TestABareVersionMeansExactly(t *testing.T) {
	c, err := ParseConstraint("1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Allows(mustParse(t, "1.2.0")) {
		t.Error("1.2.0 must satisfy the constraint 1.2.0")
	}
	if c.Allows(mustParse(t, "1.2.1")) {
		t.Error("a bare version was read as a floor, so a pin does not pin")
	}
}

func TestConstraintTermsAreAnded(t *testing.T) {
	c, err := ParseConstraint(">= 0.3.0, < 0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"0.3.0", true}, // the floor is inclusive
		{"0.3.9", true},
		{"0.4.0", false}, // the ceiling is not
		{"0.2.9", false},
		{"1.0.0", false},
	} {
		if got := c.Allows(mustParse(t, tc.in)); got != tc.want {
			t.Errorf("%q allows %s = %v, want %v", c, tc.in, got, tc.want)
		}
	}
}

// TestEveryOperator, including the two that are easy to get wrong: `<=` must be tried
// before `<`, or `<=1.0` parses as `<` applied to `=1.0`.
func TestEveryOperator(t *testing.T) {
	for _, tc := range []struct {
		constraint, version string
		want                bool
	}{
		{">=1.0.0", "1.0.0", true},
		{">1.0.0", "1.0.0", false},
		{"<=1.0.0", "1.0.0", true},
		{"<=1.0.0", "1.0.1", false},
		{"<1.0.0", "1.0.0", false},
		{"!=1.0.0", "1.0.0", false},
		{"!=1.0.0", "1.0.1", true},
		{"=1.0.0", "1.0.0", true},
		{"==1.0.0", "1.0.0", true},
	} {
		c, err := ParseConstraint(tc.constraint)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", tc.constraint, err)
		}
		if got := c.Allows(mustParse(t, tc.version)); got != tc.want {
			t.Errorf("%q allows %s = %v, want %v", tc.constraint, tc.version, got, tc.want)
		}
	}
}

func TestParseConstraintRejectsNonsense(t *testing.T) {
	for _, in := range []string{"", "  ", ">=", ">= 1.0,", ",", ">= x", ">=1.0, , <2.0"} {
		if c, err := ParseConstraint(in); err == nil {
			t.Errorf("ParseConstraint(%q) = %v, want an error", in, c)
		}
	}
}

// TestAConstraintQuotesItselfAsWritten. A diagnostic saying "needs >= 0.4" has to show
// what the user wrote, not a normalised form they would have to translate back.
func TestAConstraintQuotesItselfAsWritten(t *testing.T) {
	c, err := ParseConstraint(">= 0.4")
	if err != nil {
		t.Fatal(err)
	}
	if c.String() != ">= 0.4" {
		t.Errorf("String() = %q, want the text as written", c)
	}
	if c.IsZero() {
		t.Error("a parsed constraint is not zero")
	}
	if !(Constraint{}).IsZero() {
		t.Error("the zero Constraint must report itself as unset")
	}
}
