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

// Format renders one value for display, redacting sensitive data at every
// depth and refusing to render anything it cannot verify.
//
// Sensitivity is per-leaf: a non-sensitive map can hold a sensitive value, so
// this checks Sensitive before touching Raw at all and recurses into
// composites rather than formatting them whole.
//
// THE RULE THAT MATTERS: Kind is a claim about Raw, and this function never
// takes that claim on trust. Every branch type-asserts Raw and returns
// Unrenderable when the assertion fails.
//
// That rule exists because the obvious weaker versions have both shipped here
// and both leaked. The first ended in `default: fmt.Sprintf("%v", v.Raw)`,
// which looks safe because sensitivity is checked at the top — but that check
// covers the value in hand, not the leaves inside it, and KindInvalid is the
// zero value of Kind, so any Value whose Kind was never set carried its Raw
// into %v. Measured, it printed:
//
//	map[password:{string true hunter2 provider true  <generated>}]
//
// the secret in clear text with its own Sensitive flag beside it. The second
// version replaced that default with an allowlist of scalar kinds — and kept
// `fmt.Sprintf("%v", v.Raw)` inside the scalar arm. A Value claiming KindInt
// while holding a map produced character-for-character the same leak, one
// case label to the left of the fix. Gating on Kind is not enough; Raw has to
// be checked.
//
// This lives in pkg/value, and is the only implementation, because the
// previous two copies lived in different packages and had already diverged in
// three ways — which is how the second leak survived a fix applied to the
// first.
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

// Annotate renders one value and appends the provenance annotation a plan
// shows beside it — today "[default]", and with M4's scopes
// "[variable, from --var]".
//
// It lives here, beside Format, and delegates the rendering to Format
// unchanged. The rendering must never be reimplemented: Format is the ONLY
// redaction path in the tree, and the two measured leaks documented on it both
// came from a second copy that had drifted. Annotate adds a SUFFIX to whatever
// Format returned and touches Raw not at all, so a sensitive value is
// "<sensitive> [variable, from --var]" — the annotation describes where a
// value came from, which is not itself secret, and hiding it would remove the
// only clue a user has for finding the secret they need to change.
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
//     that will produce it does — and "(known after apply) [explicit, from
//     base config]" describes the attribute rather than the value.
//
//   - Ordinary explicit configuration is not annotated. Annotating it would
//     put "[explicit, from base config]" on nearly every line of every plan,
//     which buries the [default] and [variable] markers that actually carry
//     information. PLAN.md §19 wants a plan a human reads.
//
//   - A value with no Scope recorded falls back to M2's behaviour exactly:
//     "[default]" for a default and nothing otherwise. Every Value in the tree
//     has ScopeUnset until stage 4 exists, so this function is output-identical
//     to M3's renderAnnotated today. That is deliberate — a rendering change
//     and a provenance change landing in the same commit would make it
//     impossible to tell which one moved a golden test.
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
	return "[" + string(v.Source) + ", from " + v.Scope.String() + "]"
}
