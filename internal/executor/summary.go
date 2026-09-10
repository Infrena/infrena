package executor

import (
	"fmt"
	"sort"
	"strings"

	"infra/internal/state"
	"infra/pkg/address"
	"infra/pkg/resource"
	"infra/pkg/value"
)

const (
	summaryAnsiReset   = "\x1b[0m"
	summaryAnsiGreen   = "\x1b[32m"
	summaryAnsiBoldRed = "\x1b[1;31m"
	summaryAnsiCyan    = "\x1b[36m"
)

// RenderOptions controls how a Result is rendered after an apply. It
// mirrors planner.RenderOptions in shape and meaning — Verbose adds detail,
// Color wraps markers in ANSI — so the two renderers read as one family of
// output, not two unrelated ones.
//
// Verbose adds each applied resource's provider and provider-assigned ID.
// That is the question an apply leaves a person with that a plan cannot
// answer — "what is the real thing that now exists?" — and it is the
// executor's analogue of planner.RenderOptions.Verbose additionally listing
// no-change resources: both show more about what the command touched.
//
// The flag must do something. --verbose is registered globally (root.go) and
// forwarded to planner.Render by plan.go, so apply and destroy will forward
// it here symmetrically; a Verbose that changed nothing would be a silently
// ignored flag, which this project treats as a defect rather than a
// harmless no-op (see --var-file, which errors rather than being ignored).
type RenderOptions struct {
	Verbose bool
	Color   bool
}

// Render turns a Result into the text a person reads after an apply: what
// was applied — including the attributes the provider returned, the values
// a plan could only ever show as "(known after apply)" — what failed and
// why, and what was skipped. It never re-derives WHY something was
// skipped beyond the bare operation: that explanation is a diag.Diagnostic
// task 9 already produced, rendered separately on stderr (spec §16).
func Render(r Result, opts RenderOptions) string {
	var lines []string

	applied := append([]address.Address(nil), r.Applied...)
	address.Sort(applied)

	lines = append(lines, fmt.Sprintf("Apply complete: %d applied, %d failed, %d skipped.",
		len(applied), len(r.Failed), len(r.Skipped)))

	if len(applied) > 0 {
		lines = append(lines, "", "Applied:")
		for _, addr := range applied {
			lines = append(lines, renderAppliedLines(addr, r.State, opts)...)
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
// provider returned for it, redacting through value.Format exactly as
// planner.Render's renderLeaf does — the same, and only, redaction path
// (see value.Format's own comment for the two leaks that made that rule).
// A resource can be Applied with nothing in st when it was destroyed or
// forgotten: State.Get's comma-ok reports that plainly rather than this
// treating a missing entry as a bug.
func renderAppliedLines(addr address.Address, st *state.State, opts RenderOptions) []string {
	if st == nil {
		// Nothing to consult, so nothing to distinguish: a bare applied
		// marker is the only honest answer.
		return []string{"  " + appliedMarker(opts.Color) + " " + addr.String()}
	}
	rs, ok := st.Get(addr)
	if !ok {
		// Applied, but absent from the state this run produced: the
		// operation was a removal — a destroy, a forget, or the destroy
		// half of a replace whose create did not land. Marking it "+"
		// tells the user the opposite of what happened, which after
		// `infra destroy` means every deleted resource reads as created.
		// This is the same "applied means absent" rule the tracker uses to
		// decide Result.Applied membership (isRemoval, isolation.go), read
		// here from the state rather than the operation kind because
		// Result carries addresses, not kinds.
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
// Deliberately parenthesised rather than written as "name: value": every
// other indented line under an applied resource IS an attribute, and an
// attribute can legitimately be called "provider" or "id". Parentheses keep
// metadata unambiguous from data — a person (or a script) reading this
// output must not have to guess which lines came from the provider's
// attributes and which the renderer added.
//
// Neither field is a value.Value, so neither goes through value.Format:
// Provider is a provider name and ProviderID is an opaque identifier the
// provider chose, and both are already strings. Nothing here may render an
// attribute — that stays in renderAppliedLines, through renderValue.
//
// Returns nothing when both fields are empty, which is the honest answer
// for a resource state that records neither rather than printing empty
// parentheses.
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

// renderValue is this package's one call site into value.Format, sibling to
// internal/planner/render.go's renderLeaf. Unknown differs deliberately
// from the plan renderer's "(known after apply)": after an apply every
// applied attribute should genuinely be known, so an unknown one here is an
// anomaly, not a promise about the future.
func renderValue(v value.Value) string {
	return value.Format(v, value.FormatOptions{Unknown: "(unknown)", QuoteStrings: true})
}

func sortedFailedIDs(failed map[string]error) []string {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// splitOpID recovers the verb and address from an OpNode.ID() string,
// documented as exactly "<verb>:<address>".
//
// SplitN with n=2 is what makes this safe, and the reason is NOT that an
// address cannot contain ":" — it can. address.Parse only splits on ".",
// so Parse("alpha:beta") succeeds and yields Name "alpha:beta" (verified
// directly: Parse("alpha:beta") -> alpha:beta, err=<nil>). What is
// actually guaranteed is that the VERB never contains ":" — it is one of
// a fixed set (create/update/destroy/forget/replace/noop) written by
// OpNode.ID() — so splitting on the FIRST colon always lands on the
// separator, and everything after it is the address however many colons
// it holds.
//
// This matters if anyone later "simplifies" it: strings.Split without a
// limit, or a LastIndex-based split, would break on such an address while
// this does not.
func splitOpID(id string) (verb, addr string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return "operation", id
	}
	return parts[0], parts[1]
}

// removedMarker marks a resource that was applied by ceasing to exist —
// destroyed or forgotten. It mirrors planner.Render's "-" for a destroy so a
// plan and the summary of applying it read the same way round.
func removedMarker(color bool) string {
	if !color {
		return "-"
	}
	return summaryAnsiBoldRed + "-" + summaryAnsiReset
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
