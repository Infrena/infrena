// Package diag carries the errors and warnings produced by compilation and
// planning. Stages collect diagnostics rather than failing fast, so a single
// typo does not mask the rest of a file.
package diag

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/infrena/infrena/pkg/address"
	"github.com/infrena/infrena/pkg/value"
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
// The default case reports an unrecognised value as itself rather than folding
// it into "Error". A severity reaches the persisted plan artifact, so a value
// that is neither constant must read as corruption rather than as a real
// severity.
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

// Diagnostic is one problem: what is wrong, where, what was expected, and what
// to do about it.
type Diagnostic struct {
	Severity Severity
	// Summary is the one-line headline.
	Summary string
	// Detail explains what was expected and what was found.
	Detail string
	// Action is what the user should do, rendered as a block they can paste.
	Action string
	Origin value.Origin
	// Related names other resources implicated in the problem.
	Related []address.Address
}

// Diagnostics is a collection of diagnostic messages.
type Diagnostics []Diagnostic

// Add appends a single diagnostic to the collection.
func (ds *Diagnostics) Add(d Diagnostic) { *ds = append(*ds, d) }

// InModule returns a copy of these diagnostics with every Origin re-rooted into
// the named module instantiation.
//
// The earlier compile stages are re-entered for each module source and know
// nothing about instantiations, so a malformed module file yields a diagnostic
// naming only the file. Stamping on the way out is what turns three identical
// copies of that diagnostic, one per instantiation, into three that can be told
// apart.
func (ds Diagnostics) InModule(name string) Diagnostics {
	if len(ds) == 0 {
		return nil
	}
	out := make(Diagnostics, len(ds))
	for i, d := range ds {
		d.Origin = d.Origin.InModule(name)
		out[i] = d
	}
	return out
}

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
			writeIndented(w, "  ", d.Detail)
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
			// Indented line by line, like Detail above. Prefixing once would
			// indent only the first line of a pasteable block, which reads as
			// though the message had ended.
			writeIndented(w, "    ", d.Action)
		}
	}
}

// writeIndented writes text with every line prefixed, leaving a blank line
// blank rather than emitting the prefix as trailing whitespace: a diagnostic is
// compared in tests and pasted into issues, and invisible trailing spaces are a
// nuisance in both.
func writeIndented(w io.Writer, prefix, text string) {
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "%s%s\n", prefix, line)
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
