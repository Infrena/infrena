// Package diag carries the errors and warnings produced by compilation and
// planning. Stages collect diagnostics rather than failing fast, so a single
// typo does not mask the rest of a file. Spec §7.4.
package diag

import (
	"fmt"
	"io"
	"strings"

	"infra/pkg/address"
	"infra/pkg/value"
)

type Severity uint8

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "Warning"
	}
	return "Error"
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

type Diagnostics []Diagnostic

func (ds *Diagnostics) Add(d Diagnostic) { *ds = append(*ds, d) }

func (ds *Diagnostics) Extend(other Diagnostics) { *ds = append(*ds, other...) }

func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

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
