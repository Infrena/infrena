package value

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Unrenderable stands in for a value that cannot be displayed safely. It is
// deliberately not empty: rendering nothing hides that data exists.
const Unrenderable = "<unrenderable>"

// Redacted is what a sensitive value renders as, at any nesting depth.
const Redacted = "<sensitive>"

// FormatOptions carries the two things callers legitimately disagree about.
// Everything else — redaction, fail-closed handling, key ordering — is fixed,
// because a caller that could vary those could get them wrong.
type FormatOptions struct {
	// Unknown is the text for a value whose result is not yet known. The
	// state inspector says "(unknown)"; a plan says "(known after apply)",
	// because in a plan it is a promise about what apply will do.
	Unknown string
	// QuoteStrings wraps string values in Go quotes. A plan quotes them so
	// leading spaces and empty strings are visible in a diff; `state show`
	// prints them bare for reading.
	QuoteStrings bool
}

// The three FormatOptions values below are the canonical answers to the two
// axes FormatOptions names — quoted or bare, and which Unknown text — so a new
// call site has a question with an answer ("which of these three contexts is
// this?") rather than a literal to copy from whichever site is nearest. Two
// literals that happened to read the same have already drifted apart once.
// A caller that finds none of the three fits should say why in its own comment
// before defining a fourth, rather than adjusting one of these and silently
// changing it for every existing caller.
var (
	// PlanFormatOptions is how a plan renders a value: a diff-like listing of
	// what apply would do, never mind what is true right now. Strings are
	// quoted so a leading space or an empty string is visible in the diff, and
	// an unknown reads as "(known after apply)" — a promise about the future,
	// because that is what a plan is.
	PlanFormatOptions = FormatOptions{Unknown: "(known after apply)", QuoteStrings: true}

	// ReportFormatOptions is how a report of what already happened renders a
	// value: what apply produced, or what refresh observed. It shares
	// PlanFormatOptions' quoting, since this is still a diff-like listing, but
	// not its Unknown text: after an apply or a refresh, a value that is still
	// not known is an anomaly being reported rather than a promise, so it reads
	// as "(unknown)".
	ReportFormatOptions = FormatOptions{Unknown: "(unknown)", QuoteStrings: true}

	// ProseFormatOptions is how a value renders standing on its own or embedded
	// in a sentence — a per-attribute listing, or a diagnostic's "the value
	// supplied by ... is ...". Strings are bare because this is for reading
	// rather than diffing, and a quoted value mid-sentence reads oddly. Unknown
	// is "(unknown)": nothing here is a promise about a future apply.
	ProseFormatOptions = FormatOptions{Unknown: "(unknown)"}
)

// Format renders one value for display, redacting sensitive data at every depth
// and refusing to render anything it cannot verify.
//
// Sensitivity is per-leaf: a non-sensitive map can hold a sensitive value, so
// this checks Sensitive before touching Raw at all and recurses into composites
// rather than formatting them whole.
//
// The rule that matters: Kind is a claim about Raw, and this function never
// takes that claim on trust. Every branch type-asserts Raw and returns
// Unrenderable when the assertion fails. Gating on Kind alone is not enough —
// a `%v` fallback anywhere, including inside a scalar arm, prints whatever Raw
// holds, and a Value whose Kind was never set or does not match its Raw then
// prints a nested secret in clear along with its own Sensitive flag.
//
// This is the only implementation, and must stay that way. Redaction spread
// across copies diverges, and then a fix applied to one leaves the other
// leaking.
func Format(v Value, opts FormatOptions) string {
	if v.Sensitive {
		return Redacted
	}
	if !v.Known {
		return opts.Unknown
	}

	switch v.Kind {
	case KindList:
		items, ok := v.Raw.([]Value)
		if !ok {
			return Unrenderable
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = Format(item, opts)
		}
		return "[" + strings.Join(parts, ", ") + "]"

	case KindMap:
		m, ok := v.Raw.(map[string]Value)
		if !ok {
			return Unrenderable
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + Format(m[k], opts)
		}
		return "{" + strings.Join(parts, ", ") + "}"

	case KindString:
		s, ok := v.Raw.(string)
		if !ok {
			return Unrenderable
		}
		if opts.QuoteStrings {
			return strconv.Quote(s)
		}
		return s

	case KindInt:
		n, ok := v.Raw.(int64)
		if !ok {
			return Unrenderable
		}
		return strconv.FormatInt(n, 10)

	case KindFloat:
		f, ok := v.Raw.(float64)
		if !ok {
			return Unrenderable
		}
		return fmt.Sprintf("%v", f)

	case KindBool:
		b, ok := v.Raw.(bool)
		if !ok {
			return Unrenderable
		}
		return strconv.FormatBool(b)

	default:
		// KindInvalid, or a kind added later that nobody taught this
		// function about. Both fail closed.
		return Unrenderable
	}
}

// Annotate renders one value and appends the provenance annotation a plan shows
// beside it, such as "[default]" or "[variable, from --var]".
//
// The rendering is delegated to Format unchanged and must never be
// reimplemented: Format is the only redaction path. Annotate adds a suffix to
// whatever Format returned and touches Raw not at all, so a sensitive value is
// "<sensitive> [variable, from --var]" — where a value came from is not itself
// secret, and hiding it would remove the only clue a user has for finding the
// secret they need to change.
func Annotate(v Value, opts FormatOptions) string {
	s := Format(v, opts)
	if a := annotation(v); a != "" {
		return s + " " + a
	}
	return s
}

// annotation returns the bracketed provenance suffix, or "" when there is
// nothing worth saying.
//
// The suppression rules, and why each exists:
//
//   - An unknown value is not annotated. It has no origin yet — the expression
//     that will produce it does — and "(known after apply) [explicit, from base
//     config]" describes the attribute rather than the value.
//
//   - Ordinary explicit configuration is not annotated. Doing so would put
//     "[explicit, from base config]" on nearly every line of every plan, burying
//     the [default] and [variable] markers that actually carry information.
//
//   - A value with no Scope recorded is annotated "[default]" if it is a default
//     and not at all otherwise.
//
//   - A scoped value with no Source recorded is not annotated either, even
//     though Scope.String() has an answer. This is deliberately the opposite of
//     how Format treats a value it cannot verify, for the opposite reason.
//     Format must produce the value itself, so a silent wrong answer would be
//     worse than a visible refusal. An annotation is optional decoration and
//     nothing downstream depends on it, so saying nothing is the fail-closed
//     answer here, and "[, from --var]" — the empty ValueSource zero value —
//     would be the silent corruption. Do not "fix" one of these two functions to
//     match the other.
//
//   - At ScopeCLIOverride only, the location named prefers v.SuppliedBy over
//     Scope.String() when it is set. It is scoped to that one rung so that the
//     day another rung starts stamping SuppliedBy, a value from a vars file does
//     not silently start rendering "from variables.yml" instead of "from base
//     config".
func annotation(v Value) string {
	if !v.Known {
		return ""
	}
	if v.Source == SourceExplicit && (v.Scope == ScopeUnset || v.Scope == ScopeBaseConfig) {
		return ""
	}
	if v.Scope == ScopeUnset {
		if v.Source == SourceDefault {
			return "[default]"
		}
		return ""
	}
	if v.Source == "" {
		return ""
	}
	return "[" + string(v.Source) + ", from " + ScopeLabel(v) + "]"
}

// ScopeLabel names the input that supplied v, for a plan annotation and for a
// diagnostic alike — which is why it is exported rather than private to
// annotation.
//
// At ScopeCLIOverride it prefers v.SuppliedBy over Scope.String() when
// SuppliedBy is set: "--var" is not specific enough to tell a --var-file value
// from an actual --var. Diagnostics must use this rather than building the label
// by hand from v.Scope.String(), which always says "--var" even for a
// --var-file. Every other scope is exactly Scope.String().
func ScopeLabel(v Value) string {
	if v.Scope == ScopeCLIOverride && v.SuppliedBy != "" {
		return v.SuppliedBy
	}
	return v.Scope.String()
}
