package planner

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/infrena/infrena/pkg/schema"
	"github.com/infrena/infrena/pkg/value"
)

// RenderOptions controls how a Plan is rendered to text.
type RenderOptions struct {
	// Verbose additionally lists resources with no changes, and notes an
	// attribute whose value the provider chose.
	Verbose bool
	// Color wraps operation markers in ANSI escape codes.
	Color bool

	// Definition looks up a resource type's schema, or nil for none.
	//
	// A lookup rather than the registry itself, so rendering stays pure and
	// this package depends on nothing but the schema and value packages; a
	// registry parameter would invite a renderer that consults providers.
	//
	// Optional. A nil lookup falls back to canonical attribute names with no
	// notes.
	Definition func(resourceType string) (*schema.ResourceDefinition, bool)
}

// definitionFor is the nil-safe form of the lookup.
func (o RenderOptions) definitionFor(resourceType string) *schema.ResourceDefinition {
	if o.Definition == nil {
		return nil
	}
	if def, ok := o.Definition(resourceType); ok {
		return def
	}
	return nil
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
// determinism testable against golden files.
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
// moveCandidates holds the addresses this same plan creates for a resource of
// the same type and logical name at a different module path. It is only ever
// non-empty for a destroy.
func renderOperationLines(op Operation, moveCandidates []string, opts RenderOptions) []string {
	header := "  " + renderMarker(op.Kind, opts.Color) + " " + op.Type + "." + op.Address.String()
	if op.Kind == OpReplace {
		if forced := renderForcedBy(op.Reasons); forced != "" {
			header += "  (replacement forced by: " + forced + ")"
		}
	}
	lines := []string{header}

	if op.Kind == OpCreate {
		lines = append(lines, renderCreateNotes(op.Reasons)...)
	}
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

	def := opts.definitionFor(op.Type)

	// An ignored attribute produces no change, so it appears in no operation
	// line. It is listed once under the resource, because silence is what the
	// user asked for about the value, not about the rule.
	//
	// Not on a create or a replace, where it would be a lie: both build the
	// resource afresh from configuration, so an ignored attribute is reset by
	// them and the ordinary attribute line below shows the value changing.
	if op.Kind != OpCreate && op.Kind != OpReplace {
		for _, name := range op.Lifecycle.IgnoreChanges {
			shown := name
			if def != nil {
				shown = def.Display(name)
			}
			lines = append(lines, "      "+shown+": [change ignored]")
		}
	}

	switch op.Kind {
	case OpCreate:
		for _, k := range unionKeys(op.After) {
			lines = append(lines, "      "+renderAttributeName(def, k, op.After[k], opts)+": "+
				renderAnnotated(op.After[k]))
		}
	case OpDestroy, OpForget:
		for _, k := range unionKeys(op.Before) {
			lines = append(lines, "      "+renderAttributeName(def, k, op.Before[k], opts)+": "+
				renderAnnotated(op.Before[k]))
		}
	case OpUpdate, OpReplace:
		for _, k := range unionKeys(op.Before, op.After) {
			before, hadBefore := op.Before[k]
			after, hasAfter := op.After[k]
			if hadBefore && hasAfter && before.Equal(after) {
				continue
			}
			shown := after
			if !hasAfter {
				shown = before
			}
			line := "      " + renderAttributeName(def, k, shown, opts) + ": " +
				renderSide(before, hadBefore) + " -> " + renderSide(after, hasAfter)
			// A user who set ignore_changes to stop a pipeline's value being
			// reverted needs to know that a replacement resets it anyway, the
			// new resource being built from configuration.
			if op.Kind == OpReplace && slices.Contains(op.Lifecycle.IgnoreChanges, k) {
				line += "   [ignored, but a replacement resets it]"
			}
			lines = append(lines, line)
		}
		lines = append(lines, renderMetadataLines(op.Reasons)...)
	}
	return lines
}

// renderAttributeName renders an attribute's key: the name a user should see, plus the
// note that says a value is the provider's rather than theirs.
//
// Two things, both needing the schema, which is why RenderOptions carries a
// lookup. Plans show the friendly alias where a plugin declares one, because
// the provider's own spelling is rarely what the user wrote; and an attribute
// the provider chose is marked, because a user who has just deleted that line
// from configuration and re-planned would otherwise see nothing happen and
// conclude the edit failed.
//
// The note deliberately does not say "no longer set in configuration".
// infrena cannot know that: state records what the provider returned, so
// every attribute in it carries the provider as its source, explicit ones
// included.
//
// Shown under --verbose, and always when the attribute forces replacement,
// since a later explicit value would replace the resource and a reader needs
// to know the current one is the provider's before typing over it.
func renderAttributeName(
	def *schema.ResourceDefinition, name string, v value.Value, opts RenderOptions,
) string {
	if def == nil {
		return name
	}
	shown := def.Display(name)

	attr, known := def.Attribute(name)
	if !known || !attr.Computed || !attr.Optional {
		return shown
	}
	// Configuration's values reach a plan as explicit or defaulted; a value
	// carried over from the observed resource keeps the provider's own
	// provenance. Inside one plan that is what separates "the user set this"
	// from "the provider chose it".
	if v.Source != value.SourceProvider {
		return shown
	}
	if !opts.Verbose && !attr.ForceNew {
		return shown
	}
	return shown + "   [provider-chosen, not in configuration]"
}

// renderMetadataLines prints the changes an update makes to the metadata
// infrena records about a resource rather than to a provider attribute:
// lifecycle settings and dependency edges.
//
// Neither is an attribute, so neither appears in Before or After and the loop
// above cannot reach them. Without this, an update whose only change is one
// of them renders as a bare header with nothing under it.
//
// Reasons carry no type tag beyond their name, so this is where the two kinds
// are recognised. Both are safe to print literally, where a ChangeReason
// otherwise carries no values: these are booleans the user typed, and
// addresses.
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

// renderCreateNotes says why a create is happening, for the one case where
// the marker alone is a lie by omission.
//
// A "+" usually means what it looks like: the resource is new and nothing
// existed. But the same marker covers a resource that is in state and that
// the provider no longer reports — drift, where something outside infrena
// deleted live infrastructure and the plan is offering to build it back. The
// two are indistinguishable on the header line, and the second is the one a
// reader must not skim past.
//
// Only reasons with no attribute name are printed: an attribute-scoped reason
// on a create is about a value, and every value already has its own line.
func renderCreateNotes(reasons []ChangeReason) []string {
	var lines []string
	for _, r := range reasons {
		if r.Attribute != "" || r.Note == "" {
			continue
		}
		lines = append(lines, "    ⚠ "+r.Note)
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
// An address embeds its module path, so moving a resource between modules
// renames it, and a rename is a destroy plus a create. There is no command to
// move a resource within state, which makes the plan the only place this is
// visible before it happens.
//
// It is a heuristic and the rendered note says so: nothing here can know
// whether two resources sharing a type and a logical name are the same
// resource. The cost of a false positive is one line of reading; the cost of
// a false negative is a database.
//
// Deterministic by construction, since the operations are already sorted by
// address, so no sort is needed here.
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

// renderMarker returns an operation's symbol, optionally ANSI-coloured.
// Destructive operations are bold red.
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
// Redundant with the planner's own sort of the reasons. Kept because Render
// is pure in its argument: a plan from any other producer, such as one read
// back from disk, gets the same output.
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
// A missing key looks up as the zero Value, which is not known, and an
// unknown value renders as "known after apply". So without this an attribute
// the user removed from configuration reads as a value that will be computed
// during apply, when the truth is the opposite: it is going away.
//
// present is the map lookup's comma-ok, the only thing separating the two.
func renderSide(v value.Value, present bool) string {
	if !present {
		return "(absent)"
	}
	return renderAnnotated(v)
}

// renderAnnotated renders one value plus, when it applies, the note saying
// which precedence level supplied it.
//
// The redaction, the fail-closed handling and the annotation all live in
// value.Annotate, the only copy in the tree; this function is the wiring.
// Do not reimplement the annotation here or add a label table of your own —
// a second one would drift silently, because nothing compares them.
//
// The shared plan format options are used for the same reason.
func renderAnnotated(v value.Value) string {
	return value.Annotate(v, value.PlanFormatOptions)
}

// renderSummary is the "N to create, N to update, ..." line. A missing key in
// the counts reads as zero, so a plan with no operations of some kind needs
// no special case.
func renderSummary(p *Plan) string {
	counts := p.Counts()
	line := fmt.Sprintf("Plan: %d to create, %d to update, %d to replace, %d to destroy, %d to forget.",
		counts[OpCreate], counts[OpUpdate], counts[OpReplace], counts[OpDestroy], counts[OpForget])
	// Appended rather than made a sixth number: it is not a change to
	// infrastructure in the sense the other five are, and it is rare enough
	// that a reader meeting it for the first time should notice rather than
	// scan past a zero.
	if n := counts[OpDestroyDeposed]; n > 0 {
		line += fmt.Sprintf(" %d left over from an interrupted replacement to clean up.", n)
	}
	return line
}
