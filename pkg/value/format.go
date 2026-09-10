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
