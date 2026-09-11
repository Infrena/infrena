package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrata/infrata/pkg/value"
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

	moves := moveCandidates(p)
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
		lines = append(lines, renderOperationLines(op, moves[op.Address.String()], opts)...)
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
//
// moveCandidates is the addresses this same plan creates for a resource of
// the same type and logical name as op, at a different module path — see
// moveCandidates. It is only ever non-empty for op.Kind == OpDestroy.
func renderOperationLines(op Operation, moveCandidates []string, opts RenderOptions) []string {
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
	if op.Kind == OpDestroy {
		if warning := renderMoveWarning(moveCandidates); warning != "" {
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

// moveCandidates maps a destroy operation's address to the addresses this same
// plan CREATES for a resource of the same type and the same logical name at a
// different module path.
//
// Spec §7.2: after stage 5 an address embeds its module path, so moving a
// resource between modules renames it, and a rename is a destroy plus a
// create. `state mv` is deferred past Phase 1 (§5.2), which makes the plan the
// only place this is visible before it happens.
//
// It is a heuristic and the rendered note says so. Nothing here can know
// whether two resources sharing a type and a logical name are the same
// resource; the note reports what the plan contains and what a rename does,
// and leaves the judgement to the reader. The cost of a false positive is one
// line of reading. The cost of a false negative is a database.
//
// Deterministic by construction: p.Operations is already sorted by address
// (spec §12.1), so the candidate lists come out sorted without a sort here.
// M3 measured eleven redundant sorts whose only job was undoing map iteration;
// this is not the twelfth.
func moveCandidates(p *Plan) map[string][]string {
	var creates []Operation
	for _, op := range p.Operations {
		if op.Kind == OpCreate {
			creates = append(creates, op)
		}
	}
	if len(creates) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, op := range p.Operations {
		if op.Kind != OpDestroy {
			continue
		}
		for _, c := range creates {
			if c.Type == op.Type && c.Address.Name == op.Address.Name &&
				c.Address.String() != op.Address.String() {
				out[op.Address.String()] = append(out[op.Address.String()], c.Address.String())
			}
		}
	}
	return out
}

// renderMoveWarning renders the note moveCandidates found, or "".
func renderMoveWarning(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	return fmt.Sprintf("    ⚠ Also created in this plan as %s. A resource's address includes its module "+
		"path, so moving one between modules renames it — and a renamed resource is destroyed and "+
		"recreated, not moved.", strings.Join(candidates, ", "))
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
// field is false, and value.Format renders any unknown value as "(known after
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

// renderAnnotated renders one value plus, when it applies, the note saying
// which precedence level supplied it.
//
// The entire implementation — the redaction, the fail-closed handling and the
// annotation — lives in value.Annotate, which is the ONLY copy in the tree.
// This function is the wiring and nothing else. Do not reimplement the
// annotation here, or add a label table: value.Scope.String() is the one label
// table, and a second one in this package would drift silently because nothing
// would compare them.
//
// value.PlanFormatOptions, not a package-local copy: this package is exactly
// the caller its own doc comment names ("Used by internal/planner's renderer
// for Before/After") — see that comment for the two axes it fixes and why a
// second, slightly different copy here is how the plan renderer and
// internal/cli's state inspector drifted apart in M2.
func renderAnnotated(v value.Value) string {
	return value.Annotate(v, value.PlanFormatOptions)
}

// renderSummary is the "N to create, N to update, ..." line spec §12.3 asks
// for. A map lookup for a Counts() key with no entries is Go's zero value, so
// a plan with no operations of some kind needs no special case.
func renderSummary(p *Plan) string {
	counts := p.Counts()
	return fmt.Sprintf("Plan: %d to create, %d to update, %d to replace, %d to destroy, %d to forget.",
		counts[OpCreate], counts[OpUpdate], counts[OpReplace], counts[OpDestroy], counts[OpForget])
}
