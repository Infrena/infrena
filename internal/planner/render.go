package planner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"infra/pkg/value"
)

// RenderOptions controls how a Plan is rendered to text.
type RenderOptions struct {
	// Verbose additionally lists resources with no changes.
	Verbose bool
	// Color wraps operation markers in ANSI escape codes.
	Color bool
}

const (
	renderAnsiReset   = "\x1b[0m"
	renderAnsiGreen   = "\x1b[32m"
	renderAnsiYellow  = "\x1b[33m"
	renderAnsiBoldRed = "\x1b[1;31m"
	renderAnsiCyan    = "\x1b[36m"
)

// Render turns a Plan into the text a user reads before approving it.
//
// It is pure: identical plans render identical text, which is what makes
// invariant 6 testable at all (spec §12.1, §12.3) and is why every case in
// render_test.go is a golden-file comparison rather than a spot check.
func Render(p *Plan, opts RenderOptions) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Plan for project %q, environment %q:", p.Project, p.Environment))
	lines = append(lines, "")

	any := false
	for _, op := range p.Operations {
		if op.Kind == OpNoOp {
			if !opts.Verbose {
				continue
			}
			lines = append(lines, fmt.Sprintf("    %s.%s (no changes)", op.Type, op.Address.String()), "")
			any = true
			continue
		}
		lines = append(lines, renderOperationLines(op, opts)...)
		lines = append(lines, "")
		any = true
	}

	if !any {
		lines = append(lines, "No changes. Configuration matches the observed state.", "")
	}

	lines = append(lines, renderSummary(p))
	return strings.Join(lines, "\n") + "\n"
}

// renderOperationLines renders one changed operation: its header line, plus
// one line per attribute that differs.
func renderOperationLines(op Operation, opts RenderOptions) []string {
	header := "  " + renderMarker(op.Kind, opts.Color) + " " + op.Type + "." + op.Address.String()
	if op.Kind == OpReplace {
		if forced := renderForcedBy(op.Reasons); forced != "" {
			header += "  (replacement forced by: " + forced + ")"
		}
	}
	lines := []string{header}

	if op.Kind == OpDestroy || op.Kind == OpReplace {
		if warning := renderDependentsWarning(op); warning != "" {
			lines = append(lines, warning)
		}
	}

	switch op.Kind {
	case OpCreate:
		for _, k := range unionKeys(op.After) {
			lines = append(lines, "      "+k+": "+renderAnnotated(op.After[k]))
		}
	case OpDestroy, OpForget:
		for _, k := range unionKeys(op.Before) {
			lines = append(lines, "      "+k+": "+renderAnnotated(op.Before[k]))
		}
	case OpUpdate, OpReplace:
		for _, k := range unionKeys(op.Before, op.After) {
			before, hadBefore := op.Before[k]
			after, hasAfter := op.After[k]
			if hadBefore && hasAfter && before.Equal(after) {
				continue
			}
			lines = append(lines, "      "+k+": "+renderSide(before, hadBefore)+" -> "+renderSide(after, hasAfter))
		}
	}
	return lines
}

// renderDependentsWarning names how many resources depend on a destructive
// operation's resource, or the empty string when there are none — a "0
// dependent resources" line would be noise, not information.
func renderDependentsWarning(op Operation) string {
	n := len(op.Dependents)
	if n == 0 {
		return ""
	}
	noun := "resources"
	if n == 1 {
		noun = "resource"
	}
	return fmt.Sprintf("    ⚠ This resource has %d dependent %s.", n, noun)
}

// renderMarker returns an operation's symbol, optionally ANSI-colored.
// Destructive operations (Replace, Destroy) are bold red — the highlighting
// half of spec §12.3's bullet; renderDependentsWarning is the count half.
func renderMarker(k OpKind, color bool) string {
	s := k.Symbol()
	if !color {
		return s
	}
	switch k {
	case OpCreate:
		return renderAnsiGreen + s + renderAnsiReset
	case OpUpdate:
		return renderAnsiYellow + s + renderAnsiReset
	case OpReplace, OpDestroy:
		return renderAnsiBoldRed + s + renderAnsiReset
	case OpForget:
		return renderAnsiCyan + s + renderAnsiReset
	default:
		return s
	}
}

// renderForcedBy names the attributes whose change forced a replacement,
// sorted so Render's own output does not depend on the order Compute
// produced Reasons in.
func renderForcedBy(reasons []ChangeReason) string {
	var names []string
	for _, r := range reasons {
		if r.ForceNew {
			names = append(names, r.Attribute)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// renderSide renders one side of an attribute diff, distinguishing an
// attribute that is ABSENT from that side from one that is merely not yet
// known.
//
// Looking a missing key up in a map yields the zero value.Value, whose Known
// field is false, and renderLeaf renders any unknown value as "(known after
// apply)". So an attribute the user REMOVED from configuration rendered as
//
//	tags: {"env": "dev"} -> (known after apply)
//
// which tells the reader the value will be computed during apply. The truth
// is the opposite: it is going away. Two different facts had collapsed into
// one string because both arrive as a Value with Known false, and the plan is
// the artifact a person reads before agreeing to change infrastructure — a
// wrong tense there is the failure this whole task exists to avoid.
//
// present is the map lookup's comma-ok, which is the only thing that
// separates the two cases.
func renderSide(v value.Value, present bool) string {
	if !present {
		return "(absent)"
	}
	return renderAnnotated(v)
}

// renderAnnotated renders one value plus, when it applies, the "[default]"
// annotation. Unlike the replacement reason, this is read straight off the
// Value — Source and Known already say everything Render needs.
func renderAnnotated(v value.Value) string {
	s := renderLeaf(v)
	if v.Known && v.Source == value.SourceDefault {
		s += " [default]"
	}
	return s
}

// unrenderable stands in for a value this function cannot safely display.
// It is deliberately not empty: rendering nothing would hide the existence of
// data, and the reader needs to know something is there.
const unrenderable = "<unrenderable>"

// renderLeaf renders one value for display, redacting sensitive data.
//
// Sensitivity is per-leaf: a non-sensitive list or map can hold a sensitive
// element. This checks Sensitive before descending into Raw at all, and
// recurses into List and Map so nothing buried inside a composite reaches the
// page in clear text. Map keys are sorted so output is stable across runs.
//
// The kind switch is an ALLOWLIST and must stay one. The obvious shape —
// ending in `default: fmt.Sprintf("%v", v.Raw)` — looks safe because
// sensitivity is checked at the top, but that check only covers the value in
// hand, not the leaves inside it. value.KindInvalid is the ZERO VALUE of
// value.Kind, so any Value whose Kind was never set carries its Raw straight
// into %v, and %v on a map[string]value.Value prints every field of every
// leaf. This was measured against internal/cli/state.go's formatValue, which
// had exactly that default branch; it rendered
//
//	map[password:{string true hunter2 provider true  <generated>}]
//
// printing the secret in clear text with its own Sensitive flag beside it,
// ignored. formatValue was hardened the same way in the same commit that
// wrote this comment, and its regression test is
// TestFormatValueFailsClosedOnUnexpectedShapes.
//
// The composite branches fail closed for the same reason: a failed type
// assertion used to yield an empty {} or [], which claims a composite was
// empty when it was really unreadable.
func renderLeaf(v value.Value) string {
	if v.Sensitive {
		return "<sensitive>"
	}
	if !v.Known {
		return "(known after apply)"
	}
	switch v.Kind {
	case value.KindList:
		items, ok := v.Raw.([]value.Value)
		if !ok {
			return unrenderable
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = renderLeaf(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case value.KindMap:
		m, ok := v.Raw.(map[string]value.Value)
		if !ok {
			return unrenderable
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + renderLeaf(m[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case value.KindString:
		s, _ := v.AsString()
		return strconv.Quote(s)
	case value.KindInt, value.KindFloat, value.KindBool:
		return fmt.Sprintf("%v", v.Raw)
	default:
		// KindInvalid, or a kind added later that nobody taught this
		// function about. Both fail closed. See the comment above.
		return unrenderable
	}
}

// renderSummary is the "N to create, N to update, ..." line spec §12.3 asks
// for. A map lookup for a Counts() key with no entries is Go's zero value, so
// a plan with no operations of some kind needs no special case.
func renderSummary(p *Plan) string {
	counts := p.Counts()
	return fmt.Sprintf("Plan: %d to create, %d to update, %d to replace, %d to destroy, %d to forget.",
		counts[OpCreate], counts[OpUpdate], counts[OpReplace], counts[OpDestroy], counts[OpForget])
}
