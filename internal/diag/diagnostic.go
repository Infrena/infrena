// Package diag carries the errors and warnings produced by compilation and
// planning. Stages collect diagnostics rather than failing fast, so a single
// typo does not mask the rest of a file. Spec §7.4.
package diag

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"infra/pkg/address"
	"infra/pkg/value"
)

// Severity indicates whether a diagnostic is an error or warning.
type Severity uint8

const (
	// SeverityError indicates a problem that must be fixed.
	SeverityError Severity = iota
	// SeverityWarning indicates a potential issue that should be addressed.
	SeverityWarning
)

// String names a severity for display and for the plan artifact's wire form.
//
// It is a switch with an explicit default rather than
// `if s == SeverityWarning { return "Warning" }; return "Error"`. The if/else
// reads the same for the two defined values but collapses EVERY other value
// into "Error" — the most actionable string available — with nothing to say
// the value was not recognised. SeverityError is the zero value, so an unset
// Severity legitimately is an error and that case is correct; the problem is
// a value that is neither constant reporting as a real severity rather than
// as corruption.
//
// This is the same permissive-fallback shape that produced a secret leak in
// formatValue and a false equality in value.Equal. An out-of-range severity
// reaches the persisted plan artifact through planner's diagnosticWire, so
// mislabelling it there is durable.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "Error"
	case SeverityWarning:
		return "Warning"
	default:
		return "Severity(" + strconv.Itoa(int(s)) + ")"
	}
}

// Diagnostic follows the shape PLAN.md §44 requires: what is wrong, where, what
// was expected, and what to do about it.
type Diagnostic struct {
	Severity Severity
	Summary  string
	Detail   string
	Action   string
	Origin   value.Origin
	Related  []address.Address
}

// Diagnostics is a collection of diagnostic messages.
type Diagnostics []Diagnostic

// Add appends a single diagnostic to the collection.
func (ds *Diagnostics) Add(d Diagnostic) { *ds = append(*ds, d) }

// Extend appends all diagnostics from another collection.
func (ds *Diagnostics) Extend(other Diagnostics) { *ds = append(*ds, other...) }

// HasErrors returns true if any diagnostic has SeverityError.
func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Render writes a human-readable representation of all diagnostics to w.
func (ds Diagnostics) Render(w io.Writer) {
	for i, d := range ds {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s: %s\n", d.Severity, d.Summary)

		if loc := location(d.Origin); loc != "" {
			fmt.Fprintf(w, "  at %s\n", loc)
		}
		if d.Detail != "" {
			fmt.Fprintln(w)
			for _, line := range strings.Split(d.Detail, "\n") {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
		if len(d.Related) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Related resources:")
			for _, a := range d.Related {
				fmt.Fprintf(w, "    %s\n", a.String())
			}
		}
		if d.Action != "" {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Suggested action:")
			fmt.Fprintf(w, "    %s\n", d.Action)
		}
	}
}

func location(o value.Origin) string {
	pos := o.String()
	if len(o.Module) == 0 {
		if pos == "<generated>" {
			return ""
		}
		return pos
	}
	parts := make([]string, 0, len(o.Module)*2)
	for _, m := range o.Module {
		parts = append(parts, "module", m)
	}
	return fmt.Sprintf("%s, in %s", pos, strings.Join(parts, "."))
}
