// Package semver parses the version constraints infrena's configuration language uses:
// a floor on infrena itself (`infrena: ">= 0.4"`), a range on a provider plugin
// (`plugins: {aws: ">= 0.3.0, < 0.4.0"}`), and the `infrena:` field of a plugin's own
// manifest. PLAN.md §61.2, §31.2.
//
// PUBLIC, and in pkg/ rather than internal/ for one reason: a plugin author needs to
// validate the constraint in their own plugin.yaml, and the alternative is a second
// implementation of this syntax that drifts from the first. Which means the exported
// surface here is API other people compile against, and §61.1's rules apply to changing
// it — additive is a minor, anything else is a major.
//
// BY HAND, not by a library. The whole syntax is comparison operators on
// MAJOR.MINOR.PATCH with comma meaning AND — a few dozen lines — and the third-party
// budget is two libraries for the entire product. A semver library would also bring
// pre-release and build-metadata ordering, which this deliberately does not have:
// every rule for what `1.0.0-rc1` sorts against is a rule a user has to know.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is MAJOR.MINOR.PATCH.
type Version struct {
	Major, Minor, Patch int
	// Pre is a pre-release suffix, kept only so a version carrying one can be
	// PRINTED as the user wrote it. It takes no part in comparison; see Compare.
	Pre string
}

// String renders a version as it would be written.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Parse reads a version, accepting a leading `v` and an omitted patch or minor.
//
// `0.4` means `0.4.0`, because that is what a person writing a floor means by it, and
// refusing it would be pedantry over a form every other tool accepts.
func Parse(s string) (Version, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Version{}, fmt.Errorf("a version is required")
	}
	text = strings.TrimPrefix(text, "v")

	var v Version
	if i := strings.IndexAny(text, "-+"); i >= 0 {
		// A build-metadata suffix (`+meta`) is dropped rather than kept: semver says
		// it is not part of identity, so keeping it would make two equal versions
		// print differently.
		if text[i] == '-' {
			v.Pre = text[i+1:]
		}
		text = text[:i]
	}

	parts := strings.Split(text, ".")
	if len(parts) > 3 {
		return Version{}, fmt.Errorf("%q has %d parts; a version is MAJOR.MINOR.PATCH", s, len(parts))
	}
	out := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%q is not a version: %q is not a number", s, part)
		}
		*out[i] = n
	}
	return v, nil
}

// Compare orders two versions: -1, 0 or +1.
//
// PRE-RELEASE IS IGNORED, deliberately. Semver orders `1.0.0-rc1` BEFORE `1.0.0`,
// which is correct and surprising: a user who pins `>= 1.0.0` and installs `1.0.0-rc1`
// is told it does not satisfy their own floor. Until this product ships pre-releases
// there is nothing to order, and inventing the rule early means inventing it without
// a case to check it against. Recorded so the omission is a decision.
func Compare(a, b Version) int {
	for _, pair := range [][2]int{
		{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch},
	} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return 0
}

// Constraint is a conjunction of comparisons: every term must hold.
type Constraint struct {
	terms []term
	text  string
}

type term struct {
	op      string
	version Version
}

// String returns the constraint as it was written, for a diagnostic to quote.
func (c Constraint) String() string { return c.text }

// IsZero reports whether no constraint was stated at all.
func (c Constraint) IsZero() bool { return len(c.terms) == 0 }

// operators, longest first: `<=` must be tried before `<`, or `<=1.0` parses as `<`
// applied to `=1.0` and fails confusingly.
var operators = []string{">=", "<=", "!=", "==", ">", "<", "="}

// ParseConstraint reads `>= 0.3.0, < 0.4.0`.
//
// A bare version means exactly that version — `aws: "1.2.0"` is `== 1.2.0`, not
// `>= 1.2.0`. Guessing the looser reading would silently accept a version the user
// thought they had pinned.
func ParseConstraint(s string) (Constraint, error) {
	c := Constraint{text: strings.TrimSpace(s)}
	if c.text == "" {
		return Constraint{}, fmt.Errorf("a version constraint is required")
	}

	for part := range strings.SplitSeq(c.text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return Constraint{}, fmt.Errorf("%q has an empty term; use `>= 1.0, < 2.0`", s)
		}
		t := term{op: "=="}
		for _, op := range operators {
			if strings.HasPrefix(part, op) {
				t.op = op
				if op == "=" {
					t.op = "=="
				}
				part = strings.TrimSpace(strings.TrimPrefix(part, op))
				break
			}
		}
		v, err := Parse(part)
		if err != nil {
			return Constraint{}, fmt.Errorf("%q: %w", s, err)
		}
		t.version = v
		c.terms = append(c.terms, t)
	}
	return c, nil
}

// Allows reports whether v satisfies every term.
func (c Constraint) Allows(v Version) bool {
	for _, t := range c.terms {
		if !t.allows(v) {
			return false
		}
	}
	return true
}

func (t term) allows(v Version) bool {
	cmp := Compare(v, t.version)
	switch t.op {
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	case "!=":
		return cmp != 0
	default: // ==
		return cmp == 0
	}
}
