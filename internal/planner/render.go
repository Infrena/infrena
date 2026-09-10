package planner

import (
	"fmt"
	"sort"
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
		lines = append(lines, renderMetadataLines(op.Reasons)...)
	}
	return lines
}

// renderMetadataLines prints the changes an update makes to metadata infra
// records about a resource rather than to a provider attribute: lifecycle
// settings, and dependency edges.
//
// They cannot come out of the attribute loop above: neither is an attribute,
// so neither appears in Before or After, and without this an update whose
// only change is one of them renders as a bare header with nothing under it —
// a change the user is asked to approve without being told what it is.
//
// Reasons carry no type tag beyond their Attribute name, so this is where the
// two kinds are recognised. Both are safe to print literally, where
// ChangeReason's doc otherwise forbids values: lifecycle reasons carry two
// booleans the user typed, and a dependency reason carries addresses.
func renderMetadataLines(reasons []ChangeReason) []string {
	var lines []string
	for _, r := range reasons {
		if r.Note == "" {
			continue
		}
		if !strings.HasPrefix(r.Attribute, lifecyclePrefix) && r.Attribute != dependsOnAttribute {
			continue
		}
		lines = append(lines, "      "+r.Attribute+": "+r.Note)
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
//
// Redundant with diff.go's sortReasons, which sorts Reasons before Compute
// ever returns them, which is in turn redundant with unionKeys building them
// in sorted order in the first place — three sorts, one property. Kept
// because Render is documented as pure in its argument: a Plan from any
// other producer (M4 reads one back from disk) gets the same output. Pinned
// by TestRenderForcedByIsSortedRegardlessOfReasonOrder, which hands it the
// unsorted Reasons Compute would never produce.
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

// renderLeaf renders one value for a plan, redacting sensitive data.
//
// The whole implementation lives in value.Format, which is the ONLY copy in
// the tree. It used to be duplicated here and in internal/cli's state
// inspector, and the two had already diverged — which is how a leak fixed in
// one survived in the other. See value.Format's comment for the two measured
// leaks that produced its fail-closed rule.
func renderLeaf(v value.Value) string {
	return value.Format(v, value.FormatOptions{
		// A plan promises what apply will do, so an unknown says so.
		Unknown: "(known after apply)",
		// Quoted, so a leading space or an empty string is visible in a diff.
		QuoteStrings: true,
	})
}

// renderSummary is the "N to create, N to update, ..." line spec §12.3 asks
// for. A map lookup for a Counts() key with no entries is Go's zero value, so
// a plan with no operations of some kind needs no special case.
func renderSummary(p *Plan) string {
	counts := p.Counts()
	return fmt.Sprintf("Plan: %d to create, %d to update, %d to replace, %d to destroy, %d to forget.",
		counts[OpCreate], counts[OpUpdate], counts[OpReplace], counts[OpDestroy], counts[OpForget])
}
