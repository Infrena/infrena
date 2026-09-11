package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/infrata/infrata/internal/diag"
	"github.com/infrata/infrata/internal/executor"
	"github.com/infrata/infrata/internal/refresh"
	"github.com/infrata/infrata/internal/state"
	"github.com/infrata/infrata/pkg/address"
	"github.com/infrata/infrata/pkg/report"
	"github.com/infrata/infrata/pkg/resource"
	"github.com/infrata/infrata/pkg/value"
)

// openReport opens --output for a command that streams pkg/report's
// NDJSON format (validate, apply, refresh), writes the mandatory first
// "meta" line, and returns the writer to use plus a close function to defer.
//
// This is deliberately separate from `infra plan`'s own --output handling
// (plan.go): a plan artifact is a single JSON document written once at the
// end, read back by a future apply, and is unaffected by anything in this
// file. opts.Output == "" returns a nil *report.Writer and a no-op close —
// every caller below already treats a nil Writer as "do not report", the
// same nil-means-no-hook convention executor.Options.OnEvent uses.
//
// 0600, matching the plan artifact (internal/cli/plan.go): even with every
// value redacted, this file names the caller's infrastructure by address and
// type.
func openReport(opts *GlobalOptions, command, environment string, errOut io.Writer) (*report.Writer, func(), error) {
	if opts.Output == "" {
		return nil, func() {}, nil
	}
	f, err := os.OpenFile(opts.Output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, func() {}, fmt.Errorf("opening --output %q: %w", opts.Output, err)
	}
	rw := report.NewWriter(f)
	if err := rw.Meta(command, environment, time.Now()); err != nil {
		f.Close()
		return nil, func() {}, fmt.Errorf("writing --output meta line: %w", err)
	}
	return rw, func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(errOut, "warning: failed to close --output file %q: %v\n", opts.Output, cerr)
		}
	}, nil
}

// renderDiagnostics is diagnostics.Render's caller-facing sibling for every
// command in this package that also streams a report: it renders ds to
// stderr exactly as every command already did, and — when rw is non-nil —
// additionally writes one "diagnostic" line per entry. A write failure here
// is reported, not fatal: the diagnostics themselves already reached the
// user on stderr, so a broken --output stream must not mask that.
func renderDiagnostics(errOut io.Writer, rw *report.Writer, ds diag.Diagnostics) {
	ds.Render(errOut)
	if rw == nil {
		return
	}
	for _, d := range ds {
		if err := rw.WriteDiagnostic(toReportDiagnostic(d)); err != nil {
			fmt.Fprintf(errOut, "warning: failed to write --output diagnostic: %v\n", err)
			return
		}
	}
}

// toReportDiagnostic converts one diag.Diagnostic to the wire shape, keeping
// spec §44's four parts — summary, detail, action, origin — as four separate
// fields rather than flattening them (see report.Diagnostic's doc comment).
func toReportDiagnostic(d diag.Diagnostic) report.Diagnostic {
	rd := report.Diagnostic{
		Severity: severityWire(d.Severity),
		Summary:  d.Summary,
		Detail:   d.Detail,
		Action:   d.Action,
	}
	if d.Origin.File != "" || d.Origin.Line != 0 || d.Origin.Column != 0 || len(d.Origin.Module) > 0 {
		rd.Origin = &report.DiagnosticOrigin{
			File:   d.Origin.File,
			Line:   d.Origin.Line,
			Column: d.Origin.Column,
			Module: append([]string(nil), d.Origin.Module...),
		}
	}
	if len(d.Related) > 0 {
		// Sort a copy — the same "encoding must never reorder the caller's
		// data, but must still be deterministic" rule planner.Plan.encode
		// applies to Dependents and Related.
		related := append([]address.Address(nil), d.Related...)
		address.Sort(related)
		rd.Related = make([]string, 0, len(related))
		for _, a := range related {
			rd.Related = append(rd.Related, a.String())
		}
	}
	return rd
}

// severityWire is the wire spelling for diag.Severity — lowercase, per the
// design ("severity":"error|warning") — kept independent of
// diag.Severity.String(), which is capitalized for human-readable rendering
// ("Error: ..."). Like Severity.String() itself, this is an explicit switch
// with a fixed default rather than a boolean check: SeverityError is the
// zero value, and a Severity that is neither constant must still report as
// the most actionable answer ("error") rather than silently downgrading to
// "warning" and letting real problem go unnoticed by a consumer filtering on
// severity.
func severityWire(s diag.Severity) string {
	if s == diag.SeverityWarning {
		return "warning"
	}
	return "error"
}

// toReportEvent converts one executor.Event to the wire shape. Event.Message
// is already redacted by whatever set it (executor.Event's own doc
// comment), so it is carried through verbatim rather than re-derived.
func toReportEvent(e executor.Event) report.Event {
	ev := report.Event{
		Event:   e.Kind.String(),
		Address: e.Address.String(),
		Op:      e.Op.String(),
		Attempt: e.Attempt,
		Message: e.Message,
		At:      e.At,
	}
	if e.Err != nil {
		ev.Error = e.Err.Error()
	}
	return ev
}

// finishApply writes an apply-shaped command's final "result" line (when rw
// is non-nil) and returns err unchanged, so every RunE return site can read
// `return finishApply(cmd, rw, result, err)` instead of duplicating the
// nil-check and error-string extraction at each one. Shared by apply and
// destroy — both run through executor.Apply and produce an
// executor.Result, so both produce a report.ApplyResult (see
// applyResultFrom).
//
// err is folded into result.Error UNLESS it is errChanges: a successful
// apply (or destroy) that found and applied changes returns errChanges
// purely to drive the process exit code (spec §16, exit 2), and reporting
// that as a failure in the persisted artifact would tell a frontend the run
// failed when it did exactly what it was asked to.
func finishApply(errOut io.Writer, rw *report.Writer, result report.ApplyResult, err error) error {
	if rw == nil {
		return err
	}
	if err != nil && err != errChanges {
		result.Error = err.Error()
	}
	if werr := rw.WriteApplyResult(result); werr != nil {
		fmt.Fprintf(errOut, "warning: failed to write --output result: %v\n", werr)
	}
	return err
}

// finishRefresh is finishApply's counterpart for `infra refresh`.
func finishRefresh(errOut io.Writer, rw *report.Writer, result report.RefreshResult, err error) error {
	if rw == nil {
		return err
	}
	if err != nil {
		result.Error = err.Error()
	}
	if werr := rw.WriteRefreshResult(result); werr != nil {
		fmt.Fprintf(errOut, "warning: failed to write --output result: %v\n", werr)
	}
	return err
}

// applyResultFrom builds report.ApplyResult from one executor.Result. It is
// the report package's read of the exact same fields executor.Render (task
// 12) already renders to the terminal — see executor/summary.go — so the
// two can never disagree about what was applied, forgotten, failed or
// skipped.
func applyResultFrom(res executor.Result) report.ApplyResult {
	applied := append([]address.Address(nil), res.Applied...)
	address.Sort(applied)

	out := report.ApplyResult{}
	for _, a := range applied {
		out.Applied = append(out.Applied, a.String())
	}

	forgotten := append([]address.Address(nil), res.Forgotten...)
	address.Sort(forgotten)
	for _, a := range forgotten {
		out.Forgotten = append(out.Forgotten, a.String())
	}

	if len(res.Failed) > 0 {
		out.Failed = make(map[string]string, len(res.Failed))
		for id, ferr := range res.Failed {
			out.Failed[id] = ferr.Error()
		}
	}

	skipped := append([]string(nil), res.Skipped...)
	sort.Strings(skipped)
	out.Skipped = skipped

	return out
}

// classifyObservation turns one refresh.Observation into a report.
// Observation, comparing it against what st recorded for the same address
// BEFORE this refresh ran. Called both as the streaming per-resource hook
// (concurrently, from inside refresh.Refresh, while st is still exactly as
// loaded — Refresh never mutates it) and again, sequentially, to build the
// final aggregate result — see buildRefreshResult. Both calls must run
// before internal/cli/refresh.go's applyObservations mutates st; after that
// point st.Get would report the NEW state as "before", comparing a
// resource's post-refresh state against itself.
func classifyObservation(st *state.State, o refresh.Observation) report.Observation {
	out := report.Observation{Address: o.Address.String(), At: time.Now()}
	switch {
	case o.Err != nil:
		out.Status = "error"
		out.Error = o.Err.Error()
	case o.State == nil:
		out.Status = "removed"
	default:
		before, _ := st.Get(o.Address)
		changes := attributeChanges(before, o.State)
		if len(changes) == 0 {
			out.Status = "unchanged"
		} else {
			out.Status = "changed"
			out.Changes = changes
		}
	}
	return out
}

// attributeChanges reports every attribute that differs between before and
// after, both already run through report.Format so a sensitive attribute
// reaches this line only as "<sensitive>". It is deliberately a plain
// before/after diff over the union of attribute names — refresh has no
// schema-driven ForceNew/computed distinction to make the way
// internal/planner's diffAttributes does (that compares desired CONFIGURATION
// against actual; this compares two actual reads of the same resource, taken
// at different times), so it does not reuse that unexported function.
func attributeChanges(before, after *resource.ResourceState) []report.AttributeChange {
	var beforeAttrs, afterAttrs map[string]value.Value
	if before != nil {
		beforeAttrs = before.Attributes
	}
	if after != nil {
		afterAttrs = after.Attributes
	}

	var out []report.AttributeChange
	for _, name := range unionAttributeNames(beforeAttrs, afterAttrs) {
		b, bok := beforeAttrs[name]
		a, aok := afterAttrs[name]
		if bok && aok && b.Equal(a) {
			continue
		}
		change := report.AttributeChange{Attribute: name}
		if bok {
			change.Before = report.Format(b)
		}
		if aok {
			change.After = report.Format(a)
		}
		out = append(out, change)
	}
	return out
}

// unionAttributeNames lists every key across the given maps, sorted — the
// same map-to-slice determinism rule internal/planner's unionKeys applies,
// kept as its own copy here for the reason attributeChanges is its own
// function rather than a reuse of diffAttributes.
func unionAttributeNames(maps ...map[string]value.Value) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for name := range m {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// buildRefreshResult aggregates every observation into the four buckets
// refresh's final "result" line reports: three in sorted address order
// (Drifted, Removed, Errors) and Unchanged as a bare count — see
// report.RefreshResult's doc comment for why that one field is a count and
// the other three are not. Called after refresh.Refresh returns but BEFORE
// applyObservations mutates st — see classifyObservation's doc comment for
// why that order matters.
func buildRefreshResult(st *state.State, obs refresh.Observations) report.RefreshResult {
	var result report.RefreshResult
	addrs := make([]string, 0, len(obs))
	for k := range obs {
		addrs = append(addrs, k)
	}
	sort.Strings(addrs)

	for _, k := range addrs {
		line := classifyObservation(st, obs[k])
		switch line.Status {
		case "changed":
			result.Drifted = append(result.Drifted, line.Address)
		case "removed":
			result.Removed = append(result.Removed, line.Address)
		case "unchanged":
			result.Unchanged++
		case "error":
			result.Errors = append(result.Errors, line.Address)
		}
	}
	return result
}
