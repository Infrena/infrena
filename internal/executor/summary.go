package executor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

const (
	summaryAnsiReset   = "\x1b[0m"
	summaryAnsiGreen   = "\x1b[32m"
	summaryAnsiBoldRed = "\x1b[1;31m"
	summaryAnsiCyan    = "\x1b[36m"
)

// RenderOptions controls how a Result is rendered after an apply. It mirrors
// planner.RenderOptions so plan and apply output read as one family.
type RenderOptions struct {
	// Verbose adds each applied resource's provider and provider-assigned
	// ID: the question an apply leaves a person with that a plan cannot
	// answer.
	Verbose bool
	// Color wraps the markers in ANSI escapes.
	Color bool
}

// Render turns a Result into the text a person reads after an apply: what
// was applied, including the attributes the provider returned that a plan
// could only show as "known after apply"; what failed and why; and what was
// skipped. It does not say why something was skipped — that explanation is
// already a diagnostic, rendered separately on stderr.
func Render(r Result, opts RenderOptions) string {
	var lines []string

	applied := append([]address.Address(nil), r.Applied...)
	address.Sort(applied)

	lines = append(lines, fmt.Sprintf("Apply complete: %d applied, %d failed, %d skipped.",
		len(applied), len(r.Failed), len(r.Skipped)))

	if len(applied) > 0 {
		forgotten := make(map[string]bool, len(r.Forgotten))
		for _, a := range r.Forgotten {
			forgotten[a.String()] = true
		}
		lines = append(lines, "", "Applied:")
		for _, addr := range applied {
			lines = append(lines, renderAppliedLines(addr, r.State, forgotten, opts)...)
		}
	}

	if len(r.Failed) > 0 {
		lines = append(lines, "", "Failed:")
		for _, id := range sortedFailedIDs(r.Failed) {
			verb, addr := splitOpID(id)
			lines = append(lines, fmt.Sprintf("  %s %s %s: %s", failedMarker(opts.Color), verb, addr, r.Failed[id]))
		}
	}

	if len(r.Skipped) > 0 {
		lines = append(lines, "", "Skipped (see the diagnostics for which dependency failed):")
		skipped := append([]string(nil), r.Skipped...)
		sort.Strings(skipped)
		for _, id := range skipped {
			verb, addr := splitOpID(id)
			lines = append(lines, fmt.Sprintf("  %s %s %s", skippedMarker(opts.Color), verb, addr))
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

// renderAppliedLines renders one applied resource and the attributes the
// provider returned for it, redacted through renderValue.
//
// Apply output carries no provenance annotation, so a line reads "size: 7"
// and never "size: 7 [variable, from --var]". Whether it should is an open
// question.
//
// A resource can be applied with nothing in state when it was destroyed or
// forgotten, so a missing entry is not treated as a bug.
func renderAppliedLines(addr address.Address, st *state.State, forgotten map[string]bool, opts RenderOptions) []string {
	if st == nil {
		// Nothing to consult, so nothing to distinguish: a bare applied
		// marker is the only honest answer.
		return []string{"  " + appliedMarker(opts.Color) + " " + addr.String()}
	}
	rs, ok := st.Get(addr)
	if !ok {
		if forgotten[addr.String()] {
			// Absent from state but not deleted: dropped from management
			// with the real resource left standing. State absence alone
			// cannot tell this from a destroy, which is why
			// Result.Forgotten exists.
			return []string{"  " + forgetMarker(opts.Color) + " " + addr.String()}
		}
		// Applied but absent from the state this run produced: the
		// operation was a removal. Marking it "+" would tell the user the
		// opposite of what happened, so after `infrena destroy` every
		// deleted resource would read as created. Read from state rather
		// than the operation kind, because a Result carries addresses.
		return []string{"  " + removedMarker(opts.Color) + " " + addr.String()}
	}

	header := "  " + appliedMarker(opts.Color) + " " + addr.String()

	lines := []string{header}
	if opts.Verbose {
		lines = append(lines, verboseProvenance(rs)...)
	}
	if len(rs.Attributes) == 0 {
		return lines
	}

	names := make([]string, 0, len(rs.Attributes))
	for name := range rs.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, "      "+name+": "+renderValue(rs.Attributes[name]))
	}
	return lines
}

// verboseProvenance renders the provider and provider-assigned ID of an
// applied resource, for RenderOptions.Verbose.
//
// Parenthesised rather than written as "name: value", because every other
// indented line under an applied resource is an attribute and an attribute
// may legitimately be called "provider" or "id". Nobody reading the output
// should have to guess which lines the renderer added.
//
// Neither field is a value.Value, so neither is redacted; both are already
// plain strings. Nothing here may render an attribute.
//
// Returns nothing when both fields are empty, rather than empty parentheses.
func verboseProvenance(rs *resource.ResourceState) []string {
	switch {
	case rs.Provider == "" && rs.ProviderID == "":
		return nil
	case rs.ProviderID == "":
		return []string{"      (provider " + rs.Provider + ")"}
	case rs.Provider == "":
		return []string{"      (id " + rs.ProviderID + ")"}
	default:
		return []string{"      (provider " + rs.Provider + ", id " + rs.ProviderID + ")"}
	}
}

// renderValue is this package's only call into value.Format. It uses the
// report options rather than the plan renderer's, because after an apply
// every attribute should be known: an unknown one is an anomaly, not a
// promise about the future.
func renderValue(v value.Value) string {
	return value.Format(v, value.ReportFormatOptions)
}

func sortedFailedIDs(failed map[string]error) []string {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// splitOpID recovers the verb and address from an OpNode.ID(), which is
// exactly "<verb>:<address>".
//
// The limit of 2 is load-bearing. An address may itself contain a colon,
// since address parsing splits only on ".". What is guaranteed is that the
// verb never does, so splitting on the first colon always lands on the
// separator. An unlimited Split, or a split on the last colon, would break
// on such an address.
func splitOpID(id string) (verb, addr string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "operation", id
	}
	return parts[0], parts[1]
}

// removedMarker marks a resource that was applied by being deleted. It
// mirrors the plan renderer's "-" so a plan and the summary of applying it
// read the same way round.
func removedMarker(color bool) string {
	if !color {
		return "-"
	}
	return summaryAnsiBoldRed + "-" + summaryAnsiReset
}

// forgetMarker marks a resource dropped from management that still exists at
// the provider, matching the plan renderer's "=".
//
// The distinction from removedMarker is not cosmetic: "-" says the resource
// was deleted, and for a retained resource that is false in the direction
// that matters — it reassures a user something is gone when it is still
// running and still costing money.
func forgetMarker(color bool) string {
	if !color {
		return "="
	}
	return summaryAnsiCyan + "=" + summaryAnsiReset
}

func appliedMarker(color bool) string {
	if !color {
		return "+"
	}
	return summaryAnsiGreen + "+" + summaryAnsiReset
}

func failedMarker(color bool) string {
	if !color {
		return "x"
	}
	return summaryAnsiBoldRed + "x" + summaryAnsiReset
}

func skippedMarker(color bool) string {
	if !color {
		return "-"
	}
	return summaryAnsiCyan + "-" + summaryAnsiReset
}
