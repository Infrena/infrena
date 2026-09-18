package expressions

import (
	"slices"
	"testing"
)

// TestTemplateFuncNamesIsPinned, the same way the expression language's own six
// built-ins are.
//
// The set is OURS rather than sprig's, and deliberately tiny. sprig ships 211
// functions across 26 modules, sixteen of which contradict guarantees this
// project asserts: env and expandenv read the environment, so a secret would
// reach rendered text as a literal with its sensitivity stripped; uuidv4, now
// and the rand family break plan determinism; getHostByName does a DNS lookup
// while rendering. A denylist against an API that grows on someone else's
// schedule fails silently, so the set is built up instead of cut down.
//
// EVERY FUNCTION HERE MUST BE PURE. Adding one is a deliberate act, which is
// what this test makes it.
func TestTemplateFuncNamesIsPinned(t *testing.T) {
	want := []string{"indent", "join", "lower", "quote", "seq", "sortAlpha", "trim", "until", "upper"}
	got := TemplateFuncNames()
	if !slices.Equal(got, want) {
		t.Errorf("TemplateFuncNames() = %v, want %v.\nAdding a function is a decision: it must be "+
			"pure — same inputs, same output, no I/O, no clock, no randomness — or a plan stops "+
			"being a deterministic function of the configuration.", got, want)
	}
}

func TestUntilAndSeq(t *testing.T) {
	until := templateFuncs["until"].(func(int) []int)
	if got := until(3); !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("until(3) = %v, want [0 1 2]", got)
	}
	// Negative and zero yield nothing rather than panicking or looping.
	if got := until(-1); len(got) != 0 {
		t.Errorf("until(-1) = %v, want empty", got)
	}
	seq := templateFuncs["seq"].(func(int, int) []int)
	if got := seq(2, 4); !slices.Equal(got, []int{2, 3, 4}) {
		t.Errorf("seq(2,4) = %v, want [2 3 4]", got)
	}
	if got := seq(4, 2); len(got) != 0 {
		t.Errorf("seq(4,2) = %v, want empty — a backwards range is not an infinite one", got)
	}
}

// TestIndentLeavesBlankLinesAlone. Padding an empty line turns it into trailing
// whitespace, which YAML tolerates and reviewers do not.
func TestIndentLeavesBlankLinesAlone(t *testing.T) {
	indent := templateFuncs["indent"].(func(int, string) string)
	if got := indent(2, "a\n\nb"); got != "  a\n\n  b" {
		t.Errorf("indent(2, %q) = %q", "a\n\nb", got)
	}
}
